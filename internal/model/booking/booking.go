// Package booking holds the booking entity and its operation contracts.
package booking

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a booking. The only transition is
// StatusConfirmed to StatusCancelled.
type Status string

const (
	// StatusConfirmed means the booking holds its seats.
	StatusConfirmed Status = "confirmed"
	// StatusCancelled means the booking has returned its seats.
	StatusCancelled Status = "cancelled"
)

var (
	// ErrNotFound reports that the booking does not exist.
	ErrNotFound = errors.New("booking not found")
	// ErrIdempotencyKeyReused reports that an idempotency key already belongs
	// to a booking made by a different request.
	ErrIdempotencyKeyReused = errors.New("idempotency key was used for a different request")
)

// Booking is a number of seats held for a customer at one event.
type Booking struct {
	ID           uuid.UUID
	EventID      uuid.UUID
	CustomerName string
	Seats        int
	Status       Status
	CreatedAt    time.Time
	CancelledAt  *time.Time
}
