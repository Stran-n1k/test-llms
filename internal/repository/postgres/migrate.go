package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey identifies the advisory lock that serializes migrations
// between service instances starting at the same time.
const migrationLockKey = 7426981530

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate applies the embedded migrations that the database does not have yet.
// It runs in one transaction under an advisory lock, so concurrent callers wait
// for each other, a failure leaves the schema untouched, and repeating it
// changes nothing. It fails if an applied migration's file has since changed.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLockKey)); err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}

		_, err := tx.Exec(ctx, `
			CREATE TABLE IF NOT EXISTS schema_migrations (
				version    text        PRIMARY KEY,
				checksum   text        NOT NULL,
				applied_at timestamptz NOT NULL DEFAULT now()
			)`)
		if err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}

		versions, err := fs.Glob(migrationFiles, "migrations/*.sql")
		if err != nil {
			return fmt.Errorf("list migrations: %w", err)
		}

		slices.Sort(versions)

		for _, version := range versions {
			if err := applyMigration(ctx, tx, version); err != nil {
				return fmt.Errorf("apply %s: %w", version, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}

	return nil
}

func applyMigration(ctx context.Context, tx pgx.Tx, version string) error {
	script, err := migrationFiles.ReadFile(version)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	sum := sha256.Sum256(script)
	checksum := hex.EncodeToString(sum[:])

	var applied string

	err = tx.QueryRow(ctx, "SELECT checksum FROM schema_migrations WHERE version = $1", version).Scan(&applied)

	switch {
	case err == nil:
		if applied != checksum {
			return errors.New("file changed after it was applied")
		}

		return nil
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("read applied version: %w", err)
	}

	if _, err := tx.Exec(ctx, string(script)); err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	_, err = tx.Exec(ctx, "INSERT INTO schema_migrations (version, checksum) VALUES ($1, $2)", version, checksum)
	if err != nil {
		return fmt.Errorf("record version: %w", err)
	}

	return nil
}
