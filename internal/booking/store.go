package booking

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) CreateEvent(ctx context.Context, title string, capacity int) (Event, error) {
	event := Event{ID: uuid.New(), Title: title, Capacity: capacity}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO events (id, title, capacity)
		VALUES ($1, $2, $3)
		RETURNING created_at`, event.ID, event.Title, event.Capacity).Scan(&event.CreatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("create event: %w", err)
	}
	event.AvailableSeats = event.Capacity
	return event, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(row rowScanner) (Event, error) {
	var event Event
	err := row.Scan(
		&event.ID,
		&event.Title,
		&event.Capacity,
		&event.BookedSeats,
		&event.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("scan event: %w", err)
	}
	event.AvailableSeats = event.Capacity - event.BookedSeats
	return event, nil
}

const eventSelect = `SELECT id, title, capacity, booked_seats, created_at FROM events`

func (s *Store) GetEvent(ctx context.Context, id uuid.UUID) (Event, error) {
	event, err := scanEvent(s.pool.QueryRow(ctx, eventSelect+` WHERE id = $1`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Event{}, fmt.Errorf("get event: %w", err)
	}
	return event, err
}

func (s *Store) GetAvailability(ctx context.Context, eventID uuid.UUID) (Availability, error) {
	event, err := s.GetEvent(ctx, eventID)
	if err != nil {
		return Availability{}, err
	}
	return Availability{
		EventID:        event.ID,
		Capacity:       event.Capacity,
		BookedSeats:    event.BookedSeats,
		AvailableSeats: event.AvailableSeats,
	}, nil
}

func scanBooking(row rowScanner) (Booking, error) {
	var booking Booking
	err := row.Scan(
		&booking.ID,
		&booking.EventID,
		&booking.Seats,
		&booking.IdempotencyKey,
		&booking.Status,
		&booking.CreatedAt,
		&booking.CancelledAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, ErrNotFound
	}
	if err != nil {
		return Booking{}, fmt.Errorf("scan booking: %w", err)
	}
	return booking, nil
}

const bookingSelect = `
	SELECT id, event_id, seats, idempotency_key, status, created_at, cancelled_at
	FROM bookings`

func (s *Store) GetBooking(ctx context.Context, id uuid.UUID) (Booking, error) {
	booking, err := scanBooking(s.pool.QueryRow(ctx, bookingSelect+` WHERE id = $1`, id))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Booking{}, fmt.Errorf("get booking: %w", err)
	}
	return booking, err
}

// CreateBooking returns created=true only for the request that inserted the
// booking. A retry with the same key and payload returns the original booking,
// including its current cancellation status.
func (s *Store) CreateBooking(
	ctx context.Context,
	eventID uuid.UUID,
	seats int,
	idempotencyKey string,
) (booking Booking, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Booking{}, false, fmt.Errorf("begin create booking: %w", err)
	}
	defer tx.Rollback(ctx)

	// This database lock serializes equal keys across all service instances.
	// It is taken before the event lock to keep lock ordering deterministic.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, idempotencyKey); err != nil {
		return Booking{}, false, fmt.Errorf("lock idempotency key: %w", err)
	}

	booking, err = scanBooking(tx.QueryRow(ctx, bookingSelect+` WHERE idempotency_key = $1`, idempotencyKey))
	if err == nil {
		if booking.EventID != eventID || booking.Seats != seats {
			return Booking{}, false, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Booking{}, false, fmt.Errorf("commit booking retry: %w", err)
		}
		return booking, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Booking{}, false, fmt.Errorf("find booking by idempotency key: %w", err)
	}

	var capacity, booked int
	err = tx.QueryRow(ctx, `
		SELECT capacity, booked_seats FROM events WHERE id = $1 FOR UPDATE`, eventID).Scan(&capacity, &booked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, false, ErrNotFound
	}
	if err != nil {
		return Booking{}, false, fmt.Errorf("lock event: %w", err)
	}
	available := capacity - booked
	if seats > available {
		return Booking{}, false, &CapacityError{Requested: seats, Available: available}
	}

	booking = Booking{
		ID:             uuid.New(),
		EventID:        eventID,
		Seats:          seats,
		IdempotencyKey: idempotencyKey,
		Status:         BookingActive,
	}
	if _, err = tx.Exec(ctx, `
		UPDATE events SET booked_seats = booked_seats + $2 WHERE id = $1`, eventID, seats); err != nil {
		return Booking{}, false, fmt.Errorf("reserve seats: %w", err)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO bookings (id, event_id, seats, idempotency_key)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at`, booking.ID, booking.EventID, booking.Seats, booking.IdempotencyKey).Scan(&booking.CreatedAt)
	if err != nil {
		return Booking{}, false, fmt.Errorf("insert booking: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Booking{}, false, fmt.Errorf("commit create booking: %w", err)
	}
	return booking, true, nil
}

// CancelBooking uses a conditional row update. Exactly one concurrent caller
// can change active to cancelled and therefore exactly one returns the seats.
func (s *Store) CancelBooking(ctx context.Context, id uuid.UUID) (Booking, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Booking{}, fmt.Errorf("begin cancel booking: %w", err)
	}
	defer tx.Rollback(ctx)

	booking, err := scanBooking(tx.QueryRow(ctx, `
		UPDATE bookings
		SET status = 'cancelled', cancelled_at = now()
		WHERE id = $1 AND status = 'active'
		RETURNING id, event_id, seats, idempotency_key, status, created_at, cancelled_at`, id))
	if errors.Is(err, ErrNotFound) {
		booking, err = scanBooking(tx.QueryRow(ctx, bookingSelect+` WHERE id = $1`, id))
		if err == nil {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return Booking{}, fmt.Errorf("commit repeated cancellation: %w", commitErr)
			}
			return booking, nil
		}
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Booking{}, ErrNotFound
		}
		return Booking{}, fmt.Errorf("cancel booking: %w", err)
	}

	result, err := tx.Exec(ctx, `
		UPDATE events SET booked_seats = booked_seats - $2 WHERE id = $1`, booking.EventID, booking.Seats)
	if err != nil {
		return Booking{}, fmt.Errorf("release seats: %w", err)
	}
	if result.RowsAffected() != 1 {
		return Booking{}, fmt.Errorf("release seats: event %s: %w", booking.EventID, ErrNotFound)
	}
	if err := tx.Commit(ctx); err != nil {
		return Booking{}, fmt.Errorf("commit cancellation: %w", err)
	}
	return booking, nil
}
