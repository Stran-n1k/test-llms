package postgres

import (
	"context"
	"errors"
	"fmt"

	model "booking/internal/model/event"
	"booking/internal/model/page"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const eventColumns = "id, name, starts_at, capacity, booked_seats, created_at"

// CreateEvent stores a new event with no booked seats.
func (r *Repository) CreateEvent(ctx context.Context, cmd model.CreateRepositoryCommand) (model.Event, error) {
	row := r.pool.QueryRow(ctx,
		`INSERT INTO events (name, starts_at, capacity) VALUES ($1, $2, $3) RETURNING `+eventColumns,
		cmd.Name, cmd.StartsAt, cmd.Capacity,
	)

	created, err := scanEvent(row)
	if err != nil {
		return model.Event{}, fmt.Errorf("insert event: %w", err)
	}

	return created, nil
}

// FetchEvent returns one event or model.ErrNotFound.
func (r *Repository) FetchEvent(ctx context.Context, id uuid.UUID) (model.Event, error) {
	found, err := scanEvent(r.pool.QueryRow(ctx, `SELECT `+eventColumns+` FROM events WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Event{}, model.ErrNotFound
	}

	if err != nil {
		return model.Event{}, fmt.Errorf("select event: %w", err)
	}

	return found, nil
}

// SearchEvents returns one page of events, oldest first.
func (r *Repository) SearchEvents(ctx context.Context, qry model.SearchRepositoryQuery) (page.Page[model.Event], error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+eventColumns+` FROM events ORDER BY created_at, id LIMIT $1 OFFSET $2`,
		qry.Window.Limit+1, qry.Window.Offset,
	)
	if err != nil {
		return page.Page[model.Event]{}, fmt.Errorf("select events: %w", err)
	}

	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Event, error) {
		return scanEvent(row)
	})
	if err != nil {
		return page.Page[model.Event]{}, fmt.Errorf("read events: %w", err)
	}

	return cutPage(items, qry.Window), nil
}

func scanEvent(row pgx.Row) (model.Event, error) {
	var found model.Event

	err := row.Scan(&found.ID, &found.Name, &found.StartsAt, &found.Capacity, &found.BookedSeats, &found.CreatedAt)
	if err != nil {
		return model.Event{}, err //nolint:wrapcheck // Callers add the operation and match pgx.ErrNoRows.
	}

	found.StartsAt = found.StartsAt.UTC()
	found.CreatedAt = found.CreatedAt.UTC()

	return found, nil
}

// cutPage turns rows selected with limit+1 into a page: the extra row, when
// present, only says that more results follow.
func cutPage[T any](items []T, window page.Window) page.Page[T] {
	hasMore := len(items) > window.Limit
	if hasMore {
		items = items[:window.Limit]
	}

	if items == nil {
		items = []T{}
	}

	return page.Page[T]{Items: items, Limit: window.Limit, Offset: window.Offset, HasMore: hasMore}
}
