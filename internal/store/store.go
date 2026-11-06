// Package store implements all PostgreSQL persistence for the booking service.
//
// Capacity accounting is done in a single UPDATE statement that both checks and
// increments events.reserved_seats. PostgreSQL takes a row lock for that UPDATE,
// so concurrent booking transactions for the same event are serialised by the
// database itself, even when they come from different service instances.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"booking/internal/model"
)

// Store is the PostgreSQL-backed data store.
type Store struct {
	pool *pgxpool.Pool
}

// New creates a Store using an existing connection pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateEventParams is the validated input for creating an event.
type CreateEventParams struct {
	Name     string
	Capacity int
	StartsAt time.Time
}

// CreateBookingParams is the validated input for creating a booking.
type CreateBookingParams struct {
	Seats         int
	CustomerName  string
	CustomerEmail string
}

const eventColumns = `
	id, name, capacity, reserved_seats, capacity - reserved_seats,
	starts_at, created_at`

const bookingColumns = `
	id, event_id, seats, status, customer_name, customer_email,
	created_at, cancelled_at, request_hash`

type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(s scanner) (model.Event, error) {
	var e model.Event
	err := s.Scan(
		&e.ID, &e.Name, &e.Capacity, &e.ReservedSeats, &e.AvailableSeats,
		&e.StartsAt, &e.CreatedAt,
	)
	return e, err
}

func scanBooking(s scanner) (model.Booking, string, error) {
	var (
		b           model.Booking
		requestHash string
	)
	err := s.Scan(
		&b.ID, &b.EventID, &b.Seats, &b.Status, &b.CustomerName,
		&b.CustomerEmail, &b.CreatedAt, &b.CancelledAt, &requestHash,
	)
	return b, requestHash, err
}

// CreateEvent inserts a new event.
func (s *Store) CreateEvent(ctx context.Context, p CreateEventParams) (model.Event, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO events (id, name, capacity, starts_at)
		VALUES ($1, $2, $3, $4)
		RETURNING `+eventColumns,
		uuid.New(), p.Name, p.Capacity, p.StartsAt,
	)
	event, err := scanEvent(row)
	if err != nil {
		return model.Event{}, fmt.Errorf("insert event: %w", err)
	}
	return event, nil
}

// GetEvent returns one event by ID.
func (s *Store) GetEvent(ctx context.Context, id uuid.UUID) (model.Event, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+eventColumns+` FROM events WHERE id = $1`, id)
	event, err := scanEvent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Event{}, ErrEventNotFound
	}
	if err != nil {
		return model.Event{}, fmt.Errorf("get event: %w", err)
	}
	return event, nil
}

// ListEvents returns all events ordered by start time.
func (s *Store) ListEvents(ctx context.Context) ([]model.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+` FROM events ORDER BY starts_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	events := make([]model.Event, 0)
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return events, nil
}

// GetBooking returns one booking by ID.
func (s *Store) GetBooking(ctx context.Context, id uuid.UUID) (model.Booking, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE id = $1`, id)
	booking, _, err := scanBooking(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Booking{}, ErrBookingNotFound
	}
	if err != nil {
		return model.Booking{}, fmt.Errorf("get booking: %w", err)
	}
	return booking, nil
}

// ListEventBookings returns all bookings (confirmed and cancelled) of an event.
func (s *Store) ListEventBookings(ctx context.Context, eventID uuid.UUID) ([]model.Booking, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)", eventID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check event: %w", err)
	}
	if !exists {
		return nil, ErrEventNotFound
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+bookingColumns+` FROM bookings WHERE event_id = $1 ORDER BY created_at, id`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list bookings: %w", err)
	}
	defer rows.Close()

	bookings := make([]model.Booking, 0)
	for rows.Next() {
		booking, _, err := scanBooking(rows)
		if err != nil {
			return nil, fmt.Errorf("scan booking: %w", err)
		}
		bookings = append(bookings, booking)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list bookings: %w", err)
	}
	return bookings, nil
}

// CreateBooking reserves seats on an event.
//
// The idempotency key is scoped to the event. If a booking with the same key
// already exists and the request payload is identical, the existing booking is
// returned with replayed=true and nothing is changed. Reusing the key with a
// different payload returns ErrIdempotencyConflict.
//
// The whole operation is one transaction: either seats are reserved and the
// booking row exists, or neither happened.
func (s *Store) CreateBooking(
	ctx context.Context,
	eventID uuid.UUID,
	idempotencyKey string,
	p CreateBookingParams,
) (booking model.Booking, replayed bool, err error) {
	requestHash := hashBookingRequest(eventID, p)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return model.Booking{}, false, fmt.Errorf("begin booking transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, existingHash, err := getBookingByKey(ctx, tx, eventID, idempotencyKey)
	switch {
	case err == nil:
		if existingHash != requestHash {
			return model.Booking{}, false, ErrIdempotencyConflict
		}
		return existing, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return model.Booking{}, false, fmt.Errorf("look up idempotency key: %w", err)
	}

	// Atomically check capacity and reserve the seats. A concurrent transaction
	// for the same event blocks here until the other one commits, then sees the
	// updated counter, so the capacity can never be exceeded.
	var available int
	err = tx.QueryRow(ctx, `
		UPDATE events
		SET reserved_seats = reserved_seats + $2
		WHERE id = $1 AND reserved_seats + $2 <= capacity
		RETURNING capacity - reserved_seats`,
		eventID, p.Seats,
	).Scan(&available)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)", eventID,
		).Scan(&exists); err != nil {
			return model.Booking{}, false, fmt.Errorf("check event existence: %w", err)
		}
		if !exists {
			return model.Booking{}, false, ErrEventNotFound
		}
		// Re-read the counter for a helpful error. This runs in the same
		// transaction, after the conditional UPDATE released its lock.
		if err := tx.QueryRow(ctx,
			"SELECT capacity - reserved_seats FROM events WHERE id = $1", eventID,
		).Scan(&available); err != nil {
			return model.Booking{}, false, fmt.Errorf("read availability: %w", err)
		}
		return model.Booking{}, false, &NotEnoughSeatsError{
			Available: available,
			Requested: p.Seats,
		}
	}
	if err != nil {
		return model.Booking{}, false, fmt.Errorf("reserve seats: %w", err)
	}

	booking = model.Booking{
		ID:            uuid.New(),
		EventID:       eventID,
		Seats:         p.Seats,
		Status:        model.StatusConfirmed,
		CustomerName:  p.CustomerName,
		CustomerEmail: p.CustomerEmail,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO bookings
			(id, event_id, seats, status, customer_name, customer_email, idempotency_key, request_hash)
		VALUES ($1, $2, $3, 'confirmed', $4, $5, $6, $7)
		RETURNING `+bookingColumns,
		booking.ID, eventID, p.Seats, p.CustomerName, p.CustomerEmail,
		idempotencyKey, requestHash,
	).Scan(
		&booking.ID, &booking.EventID, &booking.Seats, &booking.Status,
		&booking.CustomerName, &booking.CustomerEmail, &booking.CreatedAt,
		&booking.CancelledAt, &requestHash,
	)
	if err != nil {
		// A concurrent request with the same idempotency key may have won the
		// race and committed between our lookup and our insert. The unique
		// constraint caught it; roll back (which also releases the seats we
		// reserved) and return the winning booking as a replay.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback(ctx)
			existing, existingHash, getErr := s.getBookingByKey(ctx, eventID, idempotencyKey)
			if getErr != nil {
				return model.Booking{}, false, fmt.Errorf("read booking after idempotency race: %w", getErr)
			}
			if existingHash != requestHash {
				return model.Booking{}, false, ErrIdempotencyConflict
			}
			return existing, true, nil
		}
		return model.Booking{}, false, fmt.Errorf("insert booking: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Booking{}, false, fmt.Errorf("commit booking: %w", err)
	}
	return booking, false, nil
}

// CancelBooking cancels a booking and returns its seats exactly once.
//
// The booking row is locked with SELECT ... FOR UPDATE. Concurrent or repeated
// cancellation requests therefore observe the already-cancelled state and do
// not decrement the event counter again. Cancelling an already cancelled
// booking is idempotent and succeeds with the current booking state.
func (s *Store) CancelBooking(ctx context.Context, id uuid.UUID) (model.Booking, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return model.Booking{}, fmt.Errorf("begin cancellation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	booking, _, err := scanBooking(tx.QueryRow(ctx,
		`SELECT `+bookingColumns+` FROM bookings WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Booking{}, ErrBookingNotFound
	}
	if err != nil {
		return model.Booking{}, fmt.Errorf("lock booking: %w", err)
	}
	if booking.Status == model.StatusCancelled {
		return booking, nil
	}

	var cancelledAt time.Time
	if err := tx.QueryRow(ctx, `
		UPDATE bookings
		SET status = 'cancelled', cancelled_at = now()
		WHERE id = $1
		RETURNING cancelled_at`, id,
	).Scan(&cancelledAt); err != nil {
		return model.Booking{}, fmt.Errorf("cancel booking: %w", err)
	}

	tag, err := tx.Exec(ctx,
		"UPDATE events SET reserved_seats = reserved_seats - $2 WHERE id = $1",
		booking.EventID, booking.Seats)
	if err != nil {
		return model.Booking{}, fmt.Errorf("release seats: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return model.Booking{}, fmt.Errorf("release seats: event %s not found", booking.EventID)
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Booking{}, fmt.Errorf("commit cancellation: %w", err)
	}

	booking.Status = model.StatusCancelled
	booking.CancelledAt = &cancelledAt
	return booking, nil
}

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) getBookingByKey(ctx context.Context, eventID uuid.UUID, key string) (model.Booking, string, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+bookingColumns+` FROM bookings WHERE event_id = $1 AND idempotency_key = $2`,
		eventID, key)
	return scanBooking(row)
}

func getBookingByKey(ctx context.Context, tx pgx.Tx, eventID uuid.UUID, key string) (model.Booking, string, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+bookingColumns+` FROM bookings WHERE event_id = $1 AND idempotency_key = $2`,
		eventID, key)
	return scanBooking(row)
}

func hashBookingRequest(eventID uuid.UUID, p CreateBookingParams) string {
	// Canonical JSON avoids field-boundary ambiguity between customer strings.
	payload, _ := json.Marshal(struct {
		EventID       string `json:"event_id"`
		Seats         int    `json:"seats"`
		CustomerName  string `json:"customer_name"`
		CustomerEmail string `json:"customer_email"`
	}{
		EventID:       eventID.String(),
		Seats:         p.Seats,
		CustomerName:  p.CustomerName,
		CustomerEmail: p.CustomerEmail,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
