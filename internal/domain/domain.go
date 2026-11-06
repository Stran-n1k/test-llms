// Package domain contains the core types and errors of the booking service.
package domain

import (
	"errors"
	"fmt"
	"time"
)

// Booking status values.
const (
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
)

// Event is a bookable event with a fixed seat capacity.
type Event struct {
	ID        int64
	Name      string
	Capacity  int
	CreatedAt time.Time
}

// EventAvailability is an event plus current seat availability information.
// AvailableSeats is only filled for single-event lookups; list endpoints omit it.
type EventAvailability struct {
	Event
	Booked         int
	AvailableSeats []int
}

// Available returns the number of free seats.
func (a EventAvailability) Available() int { return a.Capacity - a.Booked }

// Booking is a reservation of a set of seats for one event.
type Booking struct {
	ID          int64
	EventID     int64
	Seats       []int
	Status      string
	CreatedAt   time.Time
	CancelledAt *time.Time
}

// ErrNotFound is returned when the requested entity does not exist.
var ErrNotFound = errors.New("not found")

// ValidationError marks client mistakes that map to HTTP 400.
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return e.Msg }

// SeatTakenError marks a conflict over already booked seats; maps to HTTP 409.
// Seats lists the offending seats when known.
type SeatTakenError struct {
	Seats []int
}

func (e *SeatTakenError) Error() string {
	if len(e.Seats) == 0 {
		return "one or more requested seats are already booked"
	}
	return fmt.Sprintf("seats already booked: %v", e.Seats)
}

// SeatOutOfRangeError marks a seat number outside 1..capacity; maps to HTTP 400.
type SeatOutOfRangeError struct {
	Seat     int
	Capacity int
}

func (e *SeatOutOfRangeError) Error() string {
	return fmt.Sprintf("seat %d is out of range 1..%d", e.Seat, e.Capacity)
}
