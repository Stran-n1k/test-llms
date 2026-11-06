package store

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrSoldOut     = errors.New("not enough available seats")
	ErrKeyConflict = errors.New("idempotency key was already used with a different request")
)

//go:embed schema.sql
var schema string

type Store struct{ Pool *pgxpool.Pool }

type Event struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Capacity  int       `json:"capacity"`
	Available int       `json:"available"`
	CreatedAt time.Time `json:"created_at"`
}

type Booking struct {
	ID          string     `json:"id"`
	EventID     string     `json:"event_id"`
	Quantity    int        `json:"quantity"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	CancelledAt *time.Time `json:"cancelled_at"`
}

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Migrate serializes startup across processes; DDL and version commit together.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(683401927351)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY)`); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	switch version {
	case 0:
		if _, err = tx.Exec(ctx, schema); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES (1)`); err != nil {
			return err
		}
	case 1:
	default:
		return fmt.Errorf("unsupported database schema version %d", version)
	}
	return tx.Commit(ctx)
}

func newID() string {
	var b [16]byte
	// crypto/rand.Read fills the buffer or terminates the process on entropy failure.
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

const eventColumns = `id::text, name, capacity, available, created_at`
const bookingColumns = `id::text, event_id::text, quantity, status, created_at, cancelled_at`

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Name, &e.Capacity, &e.Available, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return e, err
}

func scanBooking(row pgx.Row) (Booking, error) {
	var b Booking
	err := row.Scan(&b.ID, &b.EventID, &b.Quantity, &b.Status, &b.CreatedAt, &b.CancelledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return b, err
}

func (s *Store) CreateEvent(ctx context.Context, name string, capacity int) (Event, error) {
	return scanEvent(s.Pool.QueryRow(ctx, `INSERT INTO events (id,name,capacity,available) VALUES ($1,$2,$3,$3) RETURNING `+eventColumns, newID(), name, capacity))
}

func (s *Store) GetEvent(ctx context.Context, id string) (Event, error) {
	return scanEvent(s.Pool.QueryRow(ctx, `SELECT `+eventColumns+` FROM events WHERE id=$1`, id))
}

func (s *Store) GetBooking(ctx context.Context, id string) (Booking, error) {
	return scanBooking(s.Pool.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE id=$1`, id))
}

// CreateBooking claims the key before touching availability. A competing INSERT
// waits for this transaction. A rollback releases both the key and the seats.
func (s *Store) CreateBooking(ctx context.Context, key, eventID string, quantity int) (Booking, bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Booking{}, false, err
	}
	defer tx.Rollback(context.Background())
	b, err := scanBooking(tx.QueryRow(ctx, `INSERT INTO bookings (id,event_id,quantity,idempotency_key)
		VALUES ($1,$2,$3,$4) ON CONFLICT (idempotency_key) DO NOTHING RETURNING `+bookingColumns,
		newID(), eventID, quantity, key))
	if errors.Is(err, ErrNotFound) {
		var previousEvent string
		var previousQuantity int
		if err = tx.QueryRow(ctx, `SELECT event_id::text,quantity FROM bookings WHERE idempotency_key=$1`, key).Scan(&previousEvent, &previousQuantity); err != nil {
			return Booking{}, false, err
		}
		if previousEvent != eventID || previousQuantity != quantity {
			return Booking{}, false, ErrKeyConflict
		}
		b, err = scanBooking(tx.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE idempotency_key=$1`, key))
		if err != nil {
			return Booking{}, false, err
		}
		return b, false, tx.Commit(ctx)
	}
	if err != nil {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23503" {
			return Booking{}, false, ErrNotFound
		}
		return Booking{}, false, err
	}
	// PostgreSQL rechecks the predicate after waiting for concurrent updates.
	result, err := tx.Exec(ctx, `UPDATE events SET available=available-$2 WHERE id=$1 AND available >= $2`, eventID, quantity)
	if err != nil {
		return Booking{}, false, err
	}
	if result.RowsAffected() != 1 {
		return Booking{}, false, ErrSoldOut
	}
	return b, true, tx.Commit(ctx)
}

// CancelBooking changes state once. Only the transaction that changes the row
// returns seats, so repeated/concurrent cancellations are harmless.
func (s *Store) CancelBooking(ctx context.Context, id string) (Booking, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Booking{}, err
	}
	defer tx.Rollback(context.Background())
	b, err := scanBooking(tx.QueryRow(ctx, `UPDATE bookings SET status='cancelled', cancelled_at=now()
		WHERE id=$1 AND status='active' RETURNING `+bookingColumns, id))
	if errors.Is(err, ErrNotFound) {
		b, err = scanBooking(tx.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE id=$1`, id))
		if err != nil {
			return Booking{}, err
		}
		return b, tx.Commit(ctx)
	}
	if err != nil {
		return Booking{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE events SET available=available+$2 WHERE id=$1`, b.EventID, b.Quantity); err != nil {
		return Booking{}, err
	}
	return b, tx.Commit(ctx)
}
