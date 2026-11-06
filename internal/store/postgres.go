// Package store implements the service.Store interface on PostgreSQL.
//
// Concurrency guarantees rely on the database, not on in-process locks, so
// they hold for any number of service instances sharing one database:
//
//   - Oversell prevention: partial unique index booking_seats_active_seat_unique
//     allows only one active row per (event_id, seat_no).
//   - Booking idempotency: unique constraint bookings_event_token_unique on
//     (event_id, client_token) makes repeated creates return the same booking.
//   - Cancel exactly-once: seats are released by flipping active=false; a
//     repeated or concurrent cancel flips rows that are already false and
//     therefore releases seats a second time never.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver

	"booking/internal/domain"
)

// PostgresStore is a service.Store backed by PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

// Open connects to the database described by dsn and waits until it is
// reachable (up to ~30s), so startup ordering in compose is forgiving.
func Open(ctx context.Context, dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(30 * time.Minute)

	deadline := time.Now().Add(30 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			db.Close()
			return nil, fmt.Errorf("database not reachable: %w", err)
		}
		select {
		case <-ctx.Done():
			db.Close()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return &PostgresStore{db: db}, nil
}

// Close releases the connection pool.
func (s *PostgresStore) Close() error { return s.db.Close() }

// Ping checks database connectivity.
func (s *PostgresStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// DB exposes the underlying connection pool (used by integration tests and
// operational tooling).
func (s *PostgresStore) DB() *sql.DB { return s.db }

// queryer is implemented by both *sql.DB and *sql.Tx.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *PostgresStore) CreateEvent(ctx context.Context, name string, capacity int) (domain.Event, error) {
	var e domain.Event
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO events (name, capacity)
		VALUES ($1, $2)
		RETURNING id, name, capacity, created_at`, name, capacity).
		Scan(&e.ID, &e.Name, &e.Capacity, &e.CreatedAt)
	if err != nil {
		return domain.Event{}, fmt.Errorf("create event: %w", err)
	}
	return e, nil
}

const eventListSelect = `
	SELECT e.id, e.name, e.capacity, e.created_at,
	       (SELECT count(*) FROM booking_seats bs WHERE bs.event_id = e.id AND bs.active) AS booked
	FROM events e`

func (s *PostgresStore) ListEvents(ctx context.Context) ([]domain.EventAvailability, error) {
	rows, err := s.db.QueryContext(ctx, eventListSelect+` ORDER BY e.id`)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var result []domain.EventAvailability
	for rows.Next() {
		var a domain.EventAvailability
		if err := rows.Scan(&a.ID, &a.Name, &a.Capacity, &a.CreatedAt, &a.Booked); err != nil {
			return nil, fmt.Errorf("list events: %w", err)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return result, nil
}

func (s *PostgresStore) GetEvent(ctx context.Context, id int64) (domain.EventAvailability, error) {
	// A repeatable-read read-only transaction gives a consistent snapshot of
	// the event row, booked count and free seat list.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return domain.EventAvailability{}, fmt.Errorf("get event: %w", err)
	}
	defer tx.Rollback()

	var a domain.EventAvailability
	err = tx.QueryRowContext(ctx, eventListSelect+` WHERE e.id = $1`, id).
		Scan(&a.ID, &a.Name, &a.Capacity, &a.CreatedAt, &a.Booked)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.EventAvailability{}, fmt.Errorf("get event %d: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.EventAvailability{}, fmt.Errorf("get event: %w", err)
	}

	seatRows, err := tx.QueryContext(ctx, `
		SELECT s.seat_no
		FROM generate_series(1, $1) AS s(seat_no)
		LEFT JOIN booking_seats bs
		       ON bs.event_id = $2 AND bs.seat_no = s.seat_no AND bs.active
		WHERE bs.booking_id IS NULL
		ORDER BY s.seat_no`, a.Capacity, id)
	if err != nil {
		return domain.EventAvailability{}, fmt.Errorf("get event seats: %w", err)
	}
	defer seatRows.Close()
	for seatRows.Next() {
		var seat int
		if err := seatRows.Scan(&seat); err != nil {
			return domain.EventAvailability{}, fmt.Errorf("get event seats: %w", err)
		}
		a.AvailableSeats = append(a.AvailableSeats, seat)
	}
	if err := seatRows.Err(); err != nil {
		return domain.EventAvailability{}, fmt.Errorf("get event seats: %w", err)
	}
	return a, nil
}

func (s *PostgresStore) CreateBooking(ctx context.Context, eventID int64, clientToken string, seats []int) (domain.Booking, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Booking{}, false, fmt.Errorf("create booking: %w", err)
	}
	defer tx.Rollback()

	var capacity int
	err = tx.QueryRowContext(ctx, `SELECT capacity FROM events WHERE id = $1`, eventID).Scan(&capacity)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Booking{}, false, fmt.Errorf("create booking: event %d: %w", eventID, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Booking{}, false, fmt.Errorf("create booking: %w", err)
	}
	for _, seat := range seats {
		if seat < 1 || seat > capacity {
			return domain.Booking{}, false, &domain.SeatOutOfRangeError{Seat: seat, Capacity: capacity}
		}
	}

	// Idempotency: if a booking with the same token for the same event
	// already exists, return it as-is instead of creating a duplicate.
	var bookingID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO bookings (event_id, client_token)
		VALUES ($1, $2)
		ON CONFLICT (event_id, client_token) DO NOTHING
		RETURNING id`, eventID, clientToken).Scan(&bookingID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var existingID int64
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM bookings WHERE event_id = $1 AND client_token = $2`,
			eventID, clientToken).Scan(&existingID); err != nil {
			return domain.Booking{}, false, fmt.Errorf("create booking: load existing: %w", err)
		}
		existing, err := getBooking(ctx, tx, existingID)
		if err != nil {
			return domain.Booking{}, false, err
		}
		return existing, false, nil
	case err != nil:
		return domain.Booking{}, false, fmt.Errorf("create booking: %w", err)
	}

	// Pre-check for a precise error message; the unique index below is the
	// real arbiter under concurrency.
	taken, err := takenSeats(ctx, tx, eventID, seats)
	if err != nil {
		return domain.Booking{}, false, err
	}
	if len(taken) > 0 {
		return domain.Booking{}, false, &domain.SeatTakenError{Seats: taken}
	}

	if err := insertSeats(ctx, tx, bookingID, eventID, seats); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Lost the race against another booking for the same seats.
			return domain.Booking{}, false, &domain.SeatTakenError{}
		}
		return domain.Booking{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Booking{}, false, fmt.Errorf("create booking: commit: %w", err)
	}
	created, err := s.GetBooking(ctx, bookingID)
	if err != nil {
		return domain.Booking{}, false, err
	}
	return created, true, nil
}

func (s *PostgresStore) GetBooking(ctx context.Context, id int64) (domain.Booking, error) {
	return getBooking(ctx, s.db, id)
}

func (s *PostgresStore) CancelBooking(ctx context.Context, id int64) (domain.Booking, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("cancel booking: %w", err)
	}
	defer tx.Rollback()

	var bookingID int64
	err = tx.QueryRowContext(ctx, `
		UPDATE bookings
		SET status = 'cancelled', cancelled_at = now()
		WHERE id = $1 AND status = 'confirmed'
		RETURNING id`, id).Scan(&bookingID)
	if errors.Is(err, sql.ErrNoRows) {
		// Either the booking does not exist (404) or it is already
		// cancelled — an idempotent replay that must not release seats again.
		return getBooking(ctx, tx, id)
	}
	if err != nil {
		return domain.Booking{}, fmt.Errorf("cancel booking: %w", err)
	}

	// Release the seats exactly once: rows already inactive match no rows.
	if _, err := tx.ExecContext(ctx, `
		UPDATE booking_seats SET active = false
		WHERE booking_id = $1 AND active`, bookingID); err != nil {
		return domain.Booking{}, fmt.Errorf("cancel booking: release seats: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return domain.Booking{}, fmt.Errorf("cancel booking: commit: %w", err)
	}
	return s.GetBooking(ctx, bookingID)
}

func getBooking(ctx context.Context, q queryer, id int64) (domain.Booking, error) {
	var b domain.Booking
	var cancelledAt sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT id, event_id, status, created_at, cancelled_at
		FROM bookings
		WHERE id = $1`, id).
		Scan(&b.ID, &b.EventID, &b.Status, &b.CreatedAt, &cancelledAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Booking{}, fmt.Errorf("booking %d: %w", id, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Booking{}, fmt.Errorf("get booking: %w", err)
	}
	if cancelledAt.Valid {
		t := cancelledAt.Time
		b.CancelledAt = &t
	}
	rows, err := q.QueryContext(ctx, `
		SELECT seat_no FROM booking_seats
		WHERE booking_id = $1
		ORDER BY seat_no`, id)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("get booking seats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seat int
		if err := rows.Scan(&seat); err != nil {
			return domain.Booking{}, fmt.Errorf("get booking seats: %w", err)
		}
		b.Seats = append(b.Seats, seat)
	}
	if err := rows.Err(); err != nil {
		return domain.Booking{}, fmt.Errorf("get booking seats: %w", err)
	}
	return b, nil
}

// takenSeats returns the subset of seats that is currently actively booked.
func takenSeats(ctx context.Context, tx *sql.Tx, eventID int64, seats []int) ([]int, error) {
	args := []any{eventID}
	var sb strings.Builder
	sb.WriteString(`SELECT seat_no FROM booking_seats WHERE event_id = $1 AND seat_no IN (`)
	for i, seat := range seats {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "$%d", len(args)+1)
		args = append(args, seat)
	}
	sb.WriteString(`) AND active ORDER BY seat_no`)

	rows, err := tx.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("check seats: %w", err)
	}
	defer rows.Close()
	var taken []int
	for rows.Next() {
		var seat int
		if err := rows.Scan(&seat); err != nil {
			return nil, fmt.Errorf("check seats: %w", err)
		}
		taken = append(taken, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("check seats: %w", err)
	}
	return taken, nil
}

// insertSeats inserts all seats in one statement. Seats arrive sorted, so
// concurrent transactions acquire index locks in the same order and cannot
// deadlock with each other.
func insertSeats(ctx context.Context, tx *sql.Tx, bookingID, eventID int64, seats []int) error {
	args := []any{bookingID, eventID}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO booking_seats (booking_id, event_id, seat_no) VALUES `)
	for i, seat := range seats {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `($1,$2,$%d)`, len(args)+1)
		args = append(args, seat)
	}
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("insert seats: %w", err)
	}
	return nil
}
