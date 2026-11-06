// Package postgres stores events and bookings in PostgreSQL.
//
// All coordination between service instances happens in the database: a booking
// holds the row lock of its event while it checks and takes seats, the unique
// constraint on the idempotency key rejects duplicates, and a cancellation is a
// conditional status change that only one caller can win.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the PostgreSQL implementation of the service repositories.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository returns a Repository using the pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Ping reports whether the database is reachable.
func (r *Repository) Ping(ctx context.Context) error {
	if err := r.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	return nil
}
