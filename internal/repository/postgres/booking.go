package postgres

import (
	"context"
	"errors"
	"fmt"

	model "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
	"booking/internal/model/page"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const bookingColumns = "id, event_id, customer_name, seats, status, created_at, cancelled_at"

// CreateBooking takes seats and stores the booking atomically, or resolves the
// command to the existing booking with the same idempotency key.
//
// It locks the event row first, so all bookings of one event run one at a time
// across every service instance. Each statement below then sees everything the
// previous lock holder committed, including a booking with the same key.
func (r *Repository) CreateBooking(ctx context.Context, cmd model.CreateRepositoryCommand) (model.CreateResponse, error) {
	var response model.CreateResponse

	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var capacity, bookedSeats int

		err := tx.QueryRow(ctx,
			`SELECT capacity, booked_seats FROM events WHERE id = $1 FOR NO KEY UPDATE`, cmd.EventID,
		).Scan(&capacity, &bookedSeats)
		if errors.Is(err, pgx.ErrNoRows) {
			return eventmodel.ErrNotFound
		}

		if err != nil {
			return fmt.Errorf("lock event: %w", err)
		}

		response, err = replayBooking(ctx, tx, cmd)
		if err == nil {
			return nil
		}

		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		if available := capacity - bookedSeats; cmd.Seats > available {
			return &model.InsufficientSeatsError{Requested: cmd.Seats, Available: available}
		}

		response.Booking, err = scanBooking(tx.QueryRow(ctx, `
			INSERT INTO bookings (event_id, idempotency_key, customer_name, seats)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (idempotency_key) DO NOTHING
			RETURNING `+bookingColumns,
			cmd.EventID, cmd.IdempotencyKey, cmd.CustomerName, cmd.Seats,
		))
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent request for another event has just committed this
			// key: the event lock does not cover that case, the constraint does.
			response, err = replayBooking(ctx, tx, cmd)
			if err != nil {
				return fmt.Errorf("resolve conflicting key: %w", err)
			}

			return nil
		}

		if err != nil {
			return fmt.Errorf("insert booking: %w", err)
		}

		_, err = tx.Exec(ctx, `UPDATE events SET booked_seats = booked_seats + $2 WHERE id = $1`, cmd.EventID, cmd.Seats)
		if err != nil {
			return fmt.Errorf("take seats: %w", err)
		}

		return nil
	})
	if err != nil {
		return model.CreateResponse{}, fmt.Errorf("create booking transaction: %w", err)
	}

	return response, nil
}

// replayBooking returns the booking stored under the command's idempotency key,
// pgx.ErrNoRows if there is none, or model.ErrIdempotencyKeyReused if the key
// belongs to a different request.
func replayBooking(ctx context.Context, tx pgx.Tx, cmd model.CreateRepositoryCommand) (model.CreateResponse, error) {
	existing, err := scanBooking(tx.QueryRow(ctx,
		`SELECT `+bookingColumns+` FROM bookings WHERE idempotency_key = $1`, cmd.IdempotencyKey,
	))
	if err != nil {
		return model.CreateResponse{}, fmt.Errorf("select booking by key: %w", err)
	}

	sameRequest := existing.EventID == cmd.EventID &&
		existing.CustomerName == cmd.CustomerName &&
		existing.Seats == cmd.Seats
	if !sameRequest {
		return model.CreateResponse{}, model.ErrIdempotencyKeyReused
	}

	return model.CreateResponse{Booking: existing, Replayed: true}, nil
}

// FetchBooking returns one booking or model.ErrNotFound.
func (r *Repository) FetchBooking(ctx context.Context, id uuid.UUID) (model.Booking, error) {
	found, err := scanBooking(r.pool.QueryRow(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Booking{}, model.ErrNotFound
	}

	if err != nil {
		return model.Booking{}, fmt.Errorf("select booking: %w", err)
	}

	return found, nil
}

// SearchBookings returns one page of an event's bookings, oldest first, or
// eventmodel.ErrNotFound if the event does not exist.
func (r *Repository) SearchBookings(ctx context.Context, qry model.SearchRepositoryQuery) (page.Page[model.Booking], error) {
	var exists bool

	err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE id = $1)`, qry.EventID).Scan(&exists)
	if err != nil {
		return page.Page[model.Booking]{}, fmt.Errorf("check event: %w", err)
	}

	if !exists {
		return page.Page[model.Booking]{}, eventmodel.ErrNotFound
	}

	rows, err := r.pool.Query(ctx,
		`SELECT `+bookingColumns+` FROM bookings WHERE event_id = $1 ORDER BY created_at, id LIMIT $2 OFFSET $3`,
		qry.EventID, qry.Window.Limit+1, qry.Window.Offset,
	)
	if err != nil {
		return page.Page[model.Booking]{}, fmt.Errorf("select bookings: %w", err)
	}

	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Booking, error) {
		return scanBooking(row)
	})
	if err != nil {
		return page.Page[model.Booking]{}, fmt.Errorf("read bookings: %w", err)
	}

	return cutPage(items, qry.Window), nil
}

// CancelBooking moves a confirmed booking to cancelled and returns its seats in
// one transaction. Under concurrency the booking's row lock lets one caller win;
// the others wait, then see the new status, match no row and return the
// cancelled booking without touching the seat count.
func (r *Repository) CancelBooking(ctx context.Context, id uuid.UUID) (model.Booking, error) {
	var cancelled model.Booking

	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error

		cancelled, err = scanBooking(tx.QueryRow(ctx, `
			UPDATE bookings
			SET status = 'cancelled', cancelled_at = now()
			WHERE id = $1 AND status = 'confirmed'
			RETURNING `+bookingColumns, id,
		))
		if err != nil {
			return fmt.Errorf("update booking: %w", err)
		}

		_, err = tx.Exec(ctx,
			`UPDATE events SET booked_seats = booked_seats - $2 WHERE id = $1`,
			cancelled.EventID, cancelled.Seats,
		)
		if err != nil {
			return fmt.Errorf("return seats: %w", err)
		}

		return nil
	})
	if err == nil {
		return cancelled, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return model.Booking{}, fmt.Errorf("cancel booking transaction: %w", err)
	}

	// Nothing to cancel: the booking is either already cancelled or missing.
	return r.FetchBooking(ctx, id)
}

func scanBooking(row pgx.Row) (model.Booking, error) {
	var found model.Booking

	err := row.Scan(
		&found.ID, &found.EventID, &found.CustomerName, &found.Seats,
		&found.Status, &found.CreatedAt, &found.CancelledAt,
	)
	if err != nil {
		return model.Booking{}, err //nolint:wrapcheck // Callers add the operation and match pgx.ErrNoRows.
	}

	found.CreatedAt = found.CreatedAt.UTC()

	if found.CancelledAt != nil {
		cancelledAt := found.CancelledAt.UTC()
		found.CancelledAt = &cancelledAt
	}

	return found, nil
}
