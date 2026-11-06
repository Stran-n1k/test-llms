// Command booking runs the seat booking HTTP service.
//
// DATABASE_URL (required) is the PostgreSQL connection string and HTTP_ADDR
// (default ":8080") is the address to listen on.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"booking/internal/app"
)

const defaultHTTPAddr = ":8080"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log); err != nil {
		log.Error("service stopped", slog.Any("error", err))
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg := app.Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		HTTPAddr:    os.Getenv("HTTP_ADDR"),
	}

	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultHTTPAddr
	}

	return app.Run(ctx, cfg, log)
}
