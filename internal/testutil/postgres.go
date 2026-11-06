// Package testutil starts a disposable PostgreSQL for integration tests and
// gives every test its own isolated schema.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"booking/internal/migrate"
)

const defaultPostgresImage = "postgres:16-alpine"

// Database is a running PostgreSQL instance (container or externally supplied).
type Database struct {
	adminDSN string
}

// Start launches a PostgreSQL container unless TEST_DATABASE_URL is set, in
// which case that database is used for the tests. The returned cleanup function
// stops the container.
func Start(ctx context.Context) (*Database, func(), error) {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		if _, err := pgxpool.ParseConfig(dsn); err != nil {
			return nil, nil, fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
		}
		return &Database{adminDSN: dsn}, func() {}, nil
	}

	container, err := tcpostgres.Run(ctx, defaultPostgresImage,
		tcpostgres.WithDatabase("booking_test"),
		tcpostgres.WithUsername("booking"),
		tcpostgres.WithPassword("booking"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("start postgres container: %w", err)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(context.Background())
		return nil, nil, fmt.Errorf("postgres connection string: %w", err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := container.Terminate(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "terminate postgres container:", err)
		}
	}
	return &Database{adminDSN: dsn}, cleanup, nil
}

// Schema is an isolated namespace with the service schema already migrated.
type Schema struct {
	name    string
	dsn     string
	primary *pgxpool.Pool
}

// NewSchema creates a fresh PostgreSQL schema, runs the service migrations in
// it, and drops it when the test finishes.
func (d *Database) NewSchema(t *testing.T) *Schema {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminCfg, err := pgxpool.ParseConfig(d.adminDSN)
	if err != nil {
		t.Fatalf("parse admin database URL: %v", err)
	}
	// Schema administration must not depend on a caller-provided search_path.
	delete(adminCfg.ConnConfig.RuntimeParams, "search_path")
	delete(adminCfg.ConnConfig.RuntimeParams, "options")
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}

	name := "test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		admin.Close()
		t.Fatalf("create test schema: %v", err)
	}

	primary := newPool(t, d.adminDSN, name)
	if err := migrate.Run(ctx, primary, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		primary.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE")
		admin.Close()
		t.Fatalf("migrate test schema: %v", err)
	}

	t.Cleanup(func() {
		primary.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+name+" CASCADE"); err != nil {
			t.Logf("drop test schema %s: %v", name, err)
		}
		admin.Close()
	})

	return &Schema{
		name:    name,
		dsn:     d.adminDSN,
		primary: primary,
	}
}

// Pool returns the primary pool of the test schema.
func (s *Schema) Pool() *pgxpool.Pool {
	return s.primary
}

// NewPool opens an additional independent pool on the same schema. It is used
// to simulate a second service instance sharing the database.
func (s *Schema) NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPool(t, s.dsn, s.name)
	t.Cleanup(pool.Close)
	return pool
}

// Name returns the PostgreSQL schema name.
func (s *Schema) Name() string {
	return s.name
}

func newPool(t *testing.T, dsn, schema string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse database URL: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	return pool
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generate schema suffix: %v", err)
	}
	return hex.EncodeToString(buf)
}
