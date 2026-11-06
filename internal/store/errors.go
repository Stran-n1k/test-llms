package store

import (
	"errors"
	"fmt"
)

// ErrEventNotFound means the requested event does not exist.
var ErrEventNotFound = errors.New("event not found")

// ErrBookingNotFound means the requested booking does not exist.
var ErrBookingNotFound = errors.New("booking not found")

// ErrIdempotencyConflict means the same idempotency key was reused with a
// different request payload.
var ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")

// NotEnoughSeatsError is returned when the event cannot accommodate the
// requested number of seats.
type NotEnoughSeatsError struct {
	Available int
	Requested int
}

func (e *NotEnoughSeatsError) Error() string {
	return fmt.Sprintf("not enough seats: requested %d, available %d", e.Requested, e.Available)
}
