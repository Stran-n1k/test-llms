// Package app is the composition root: it connects to the database, prepares the
// schema, wires the layers and runs the HTTP server until its context ends.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"booking/internal/handler/rest"
	"booking/internal/repository/postgres"
	bookingservice "booking/internal/service/booking"
	eventservice "booking/internal/service/event"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	databaseWaitTimeout  = 60 * time.Second
	databaseWaitInterval = 500 * time.Millisecond
	migrationTimeout     = 60 * time.Second
	shutdownTimeout      = 15 * time.Second

	requestTimeout    = 10 * time.Second
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 20 * time.Second
	idleTimeout       = 60 * time.Second
)

// Config is the process configuration.
type Config struct {
	DatabaseURL string
	HTTPAddr    string
}

// Run serves HTTP until ctx is cancelled, then stops accepting requests, lets
// running ones finish and closes the database pool.
func Run(ctx context.Context, cfg Config, log *slog.Logger) error {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}
	defer pool.Close()

	if err := waitForDatabase(ctx, pool, log); err != nil {
		return err
	}

	migrateCtx, cancelMigrate := context.WithTimeout(ctx, migrationTimeout)
	defer cancelMigrate()

	if err := postgres.Migrate(migrateCtx, pool); err != nil {
		return fmt.Errorf("prepare schema: %w", err)
	}

	repository := postgres.NewRepository(pool)
	handler := rest.NewHandler(
		eventservice.NewService(repository),
		bookingservice.NewService(repository),
		repository,
		log,
	)

	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}

	server := &http.Server{
		Handler:           rest.NewHTTPHandler(handler, requestTimeout),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	log.InfoContext(ctx, "serving", slog.String("addr", listener.Addr().String()))

	return serve(ctx, server, listener, log)
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, log *slog.Logger) error {
	stopped := make(chan error, 1)

	go func() {
		stopped <- server.Serve(listener)
	}()

	select {
	case err := <-stopped:
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
	}

	log.InfoContext(ctx, "shutting down")

	// The process context is already cancelled, so shutdown needs its own.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down http: %w", err)
	}

	if err := <-stopped; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}

	return nil
}

// waitForDatabase tolerates a database that is still starting, which is normal
// when the service and PostgreSQL start together.
func waitForDatabase(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, databaseWaitTimeout)
	defer cancel()

	ticker := time.NewTicker(databaseWaitInterval)
	defer ticker.Stop()

	for {
		err := pool.Ping(ctx)
		if err == nil {
			return nil
		}

		log.WarnContext(ctx, "database is not ready yet", slog.Any("error", err))

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for database: %w", err)
		case <-ticker.C:
		}
	}
}
