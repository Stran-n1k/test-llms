// Package booking implements the business operations on bookings.
package booking

import (
	"context"
	"fmt"

	model "booking/internal/model/booking"
	"booking/internal/model/page"
	"booking/internal/model/validation"

	"github.com/google/uuid"
)

// maxIdempotencyKeyLength is the maximum length of an idempotency key in bytes.
const maxIdempotencyKeyLength = 255

// Service owns the rules for creating, reading and cancelling bookings.
type Service struct {
	repository Repository
}

// NewService returns a Service backed by the repository.
func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

// Create validates a booking request and stores it, or resolves it to the
// booking an earlier request with the same idempotency key already made.
func (s *Service) Create(ctx context.Context, cmd model.CreateServiceCommand) (model.CreateResponse, error) {
	var problems validation.Collector

	customerName := problems.NormalizeText("customer_name", cmd.CustomerName)

	if cmd.Seats < 1 {
		problems.Add("seats", "must be at least 1")
	}

	if !isValidIdempotencyKey(cmd.IdempotencyKey) {
		problems.Add("Idempotency-Key", fmt.Sprintf(
			"must be 1 to %d printable ASCII characters", maxIdempotencyKeyLength,
		))
	}

	if err := problems.Err(); err != nil {
		return model.CreateResponse{}, err
	}

	created, err := s.repository.CreateBooking(ctx, model.CreateRepositoryCommand{
		EventID:        cmd.EventID,
		IdempotencyKey: cmd.IdempotencyKey,
		CustomerName:   customerName,
		Seats:          cmd.Seats,
	})
	if err != nil {
		return model.CreateResponse{}, fmt.Errorf("create booking: %w", err)
	}

	return created, nil
}

// Fetch returns one booking or model.ErrNotFound.
func (s *Service) Fetch(ctx context.Context, id uuid.UUID) (model.Booking, error) {
	found, err := s.repository.FetchBooking(ctx, id)
	if err != nil {
		return model.Booking{}, fmt.Errorf("fetch booking: %w", err)
	}

	return found, nil
}

// Search returns one page of an event's bookings.
func (s *Service) Search(ctx context.Context, qry model.SearchServiceQuery) (page.Page[model.Booking], error) {
	var problems validation.Collector

	window := qry.Page.Resolve(&problems)
	if err := problems.Err(); err != nil {
		return page.Page[model.Booking]{}, err
	}

	found, err := s.repository.SearchBookings(ctx, model.SearchRepositoryQuery{
		EventID: qry.EventID,
		Window:  window,
	})
	if err != nil {
		return page.Page[model.Booking]{}, fmt.Errorf("search bookings: %w", err)
	}

	return found, nil
}

// Cancel returns the booking's seats once and reports the cancelled booking;
// cancelling an already cancelled booking changes nothing.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID) (model.Booking, error) {
	cancelled, err := s.repository.CancelBooking(ctx, id)
	if err != nil {
		return model.Booking{}, fmt.Errorf("cancel booking: %w", err)
	}

	return cancelled, nil
}

func isValidIdempotencyKey(key string) bool {
	if key == "" || len(key) > maxIdempotencyKeyLength {
		return false
	}

	for i := range len(key) {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}

	return true
}
