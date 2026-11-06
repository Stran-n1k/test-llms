package store

import (
	"context"
	"fmt"
)

// migrationsLockKey is an arbitrary constant identifying the migration
// advisory lock so that two instances migrating at the same time serialize.
const migrationsLockKey = 724389001233

// migrations are applied in order inside transactions; every statement is
// idempotent (IF NOT EXISTS), so re-running on an existing database is a
// no-op and never drops or loses data.
var migrations = []string{
	// 1: initial schema
	`
CREATE TABLE IF NOT EXISTS events (
	id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	name       text        NOT NULL,
	capacity   integer     NOT NULL CHECK (capacity > 0),
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bookings (
	id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	event_id     bigint      NOT NULL REFERENCES events(id),
	client_token text        NOT NULL,
	status       text        NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'cancelled')),
	created_at   timestamptz NOT NULL DEFAULT now(),
	cancelled_at timestamptz,
	CONSTRAINT bookings_event_token_unique UNIQUE (event_id, client_token)
);

CREATE TABLE IF NOT EXISTS booking_seats (
	booking_id bigint  NOT NULL REFERENCES bookings(id),
	event_id   bigint  NOT NULL REFERENCES events(id),
	seat_no    integer NOT NULL CHECK (seat_no > 0),
	active     boolean NOT NULL DEFAULT true,
	PRIMARY KEY (booking_id, seat_no)
);

-- A seat can be held by at most one *active* (not cancelled) booking.
-- This partial unique index is the core oversell guard: it is enforced by
-- the database regardless of how many service instances are running.
CREATE UNIQUE INDEX IF NOT EXISTS booking_seats_active_seat_unique
	ON booking_seats (event_id, seat_no) WHERE active;

CREATE INDEX IF NOT EXISTS booking_seats_event_active_idx
	ON booking_seats (event_id) WHERE active;
`,
}

// Migrate applies all pending migrations. It is safe to call concurrently
// from several instances (protected by a PostgreSQL advisory lock) and safe
// to re-run on a database that already has data.
func (s *PostgresStore) Migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration: acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationsLockKey); err != nil {
		return fmt.Errorf("migration: acquire lock: %w", err)
	}
	defer func() {
		// Unlock even if ctx is already cancelled.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationsLockKey)
	}()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     NOT NULL PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("migration: create schema_migrations: %w", err)
	}

	var current int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("migration: read version: %w", err)
	}

	for i, migration := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migration %d: begin: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: apply: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`,
			version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: record: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: commit: %w", version, err)
		}
	}
	return nil
}
