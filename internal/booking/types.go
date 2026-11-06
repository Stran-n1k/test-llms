package booking

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	MaxTitleLength    = 200
	MaxCapacity       = 1_000_000
	MaxIdempotencyKey = 200
)

var (
	ErrNotFound            = errors.New("not found")
	ErrIdempotencyConflict = errors.New("idempotency key conflict")
)

type CapacityError struct {
	Requested int
	Available int
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("not enough seats: requested %d, available %d", e.Requested, e.Available)
}

type Event struct {
	ID             uuid.UUID `json:"id"`
	Title          string    `json:"title"`
	Capacity       int       `json:"capacity"`
	BookedSeats    int       `json:"booked_seats"`
	AvailableSeats int       `json:"available_seats"`
	CreatedAt      time.Time `json:"created_at"`
}

type Availability struct {
	EventID        uuid.UUID `json:"event_id"`
	Capacity       int       `json:"capacity"`
	BookedSeats    int       `json:"booked_seats"`
	AvailableSeats int       `json:"available_seats"`
}

type BookingStatus string

const (
	BookingActive    BookingStatus = "active"
	BookingCancelled BookingStatus = "cancelled"
)

type Booking struct {
	ID             uuid.UUID     `json:"id"`
	EventID        uuid.UUID     `json:"event_id"`
	Seats          int           `json:"seats"`
	IdempotencyKey string        `json:"idempotency_key"`
	Status         BookingStatus `json:"status"`
	CreatedAt      time.Time     `json:"created_at"`
	CancelledAt    *time.Time    `json:"cancelled_at,omitempty"`
}
