package rest

import (
	"context"

	bookingmodel "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
	"booking/internal/model/page"

	"github.com/google/uuid"
)

//go:generate go tool mockgen -source=dependency.go -destination=mocks/mock.go -package=mocks

type (
	// EventService is the business logic behind the event operations.
	EventService interface {
		Create(ctx context.Context, cmd eventmodel.CreateServiceCommand) (eventmodel.Event, error)
		Fetch(ctx context.Context, id uuid.UUID) (eventmodel.Event, error)
		Search(ctx context.Context, qry eventmodel.SearchServiceQuery) (page.Page[eventmodel.Event], error)
	}

	// BookingService is the business logic behind the booking operations.
	BookingService interface {
		Create(ctx context.Context, cmd bookingmodel.CreateServiceCommand) (bookingmodel.CreateResponse, error)
		Fetch(ctx context.Context, id uuid.UUID) (bookingmodel.Booking, error)
		Search(ctx context.Context, qry bookingmodel.SearchServiceQuery) (page.Page[bookingmodel.Booking], error)
		Cancel(ctx context.Context, id uuid.UUID) (bookingmodel.Booking, error)
	}

	// HealthChecker reports whether the service's dependencies are reachable.
	HealthChecker interface {
		Ping(ctx context.Context) error
	}
)
