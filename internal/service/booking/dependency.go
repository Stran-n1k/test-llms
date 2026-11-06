package booking

import (
	"context"

	model "booking/internal/model/booking"
	"booking/internal/model/page"

	"github.com/google/uuid"
)

//go:generate go tool mockgen -source=dependency.go -destination=mocks/mock.go -package=mocks

// Repository stores bookings. CreateBooking and CancelBooking are atomic and
// keep the event's seat count consistent with its bookings.
type Repository interface {
	CreateBooking(ctx context.Context, cmd model.CreateRepositoryCommand) (model.CreateResponse, error)
	FetchBooking(ctx context.Context, id uuid.UUID) (model.Booking, error)
	SearchBookings(ctx context.Context, qry model.SearchRepositoryQuery) (page.Page[model.Booking], error)
	CancelBooking(ctx context.Context, id uuid.UUID) (model.Booking, error)
}
