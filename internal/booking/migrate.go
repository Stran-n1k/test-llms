package booking

import (
	"context"
	"fmt"
)

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS events (
		id uuid PRIMARY KEY,
		title text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 200),
		capacity integer NOT NULL CHECK (capacity > 0),
		booked_seats integer NOT NULL DEFAULT 0
			CHECK (booked_seats >= 0 AND booked_seats <= capacity),
		created_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS bookings (
		id uuid PRIMARY KEY,
		event_id uuid NOT NULL REFERENCES events (id),
		seats integer NOT NULL CHECK (seats > 0),
		idempotency_key text NOT NULL UNIQUE
			CHECK (char_length(idempotency_key) BETWEEN 1 AND 200),
		status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled')),
		created_at timestamptz NOT NULL DEFAULT now(),
		cancelled_at timestamptz,
		CHECK (
			(status = 'active' AND cancelled_at IS NULL) OR
			(status = 'cancelled' AND cancelled_at IS NOT NULL)
		)
	)`,
	`CREATE INDEX IF NOT EXISTS bookings_event_status_idx
		ON bookings (event_id, status)`,
}

// Migrate creates the initial schema. The advisory lock also makes first startup
// safe when several application instances start against the same empty database.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(842731905)`); err != nil {
		return fmt.Errorf("lock migration: %w", err)
	}
	for _, statement := range schemaStatements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
