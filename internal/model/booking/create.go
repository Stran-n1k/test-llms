package booking

import (
	"fmt"

	"github.com/google/uuid"
)

type (
	// CreateServiceCommand is the caller's request to book seats.
	CreateServiceCommand struct {
		EventID        uuid.UUID
		IdempotencyKey string
		CustomerName   string
		Seats          int
	}

	// CreateRepositoryCommand is a validated and normalized booking to store.
	CreateRepositoryCommand struct {
		EventID        uuid.UUID
		IdempotencyKey string
		CustomerName   string
		Seats          int
	}

	// CreateResponse is the booking a create request resolved to. Replayed is
	// true when the booking came from an earlier request with the same key.
	CreateResponse struct {
		Booking  Booking
		Replayed bool
	}

	// InsufficientSeatsError reports that an event cannot fit a booking.
	InsufficientSeatsError struct {
		Requested int
		Available int
	}
)

// Error implements error.
func (e *InsufficientSeatsError) Error() string {
	return fmt.Sprintf("not enough seats: requested %d, available %d", e.Requested, e.Available)
}
