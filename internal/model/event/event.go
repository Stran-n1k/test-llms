// Package event holds the event entity and its operation contracts.
package event

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound reports that the event does not exist.
var ErrNotFound = errors.New("event not found")

// Event is an occasion with a limited number of interchangeable seats.
type Event struct {
	ID          uuid.UUID
	Name        string
	StartsAt    time.Time
	Capacity    int
	BookedSeats int
	CreatedAt   time.Time
}

// AvailableSeats returns the number of seats that can still be booked.
func (e Event) AvailableSeats() int {
	return e.Capacity - e.BookedSeats
}
