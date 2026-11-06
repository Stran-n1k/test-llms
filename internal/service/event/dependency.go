package event

import (
	"context"

	model "booking/internal/model/event"
	"booking/internal/model/page"

	"github.com/google/uuid"
)

//go:generate go tool mockgen -source=dependency.go -destination=mocks/mock.go -package=mocks

// Repository stores events.
type Repository interface {
	CreateEvent(ctx context.Context, cmd model.CreateRepositoryCommand) (model.Event, error)
	FetchEvent(ctx context.Context, id uuid.UUID) (model.Event, error)
	SearchEvents(ctx context.Context, qry model.SearchRepositoryQuery) (page.Page[model.Event], error)
}
