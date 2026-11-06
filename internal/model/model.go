// Package model contains the domain types shared by the storage and HTTP layers.
package model

import (
	"time"

	"github.com/google/uuid"
)

// Booking statuses.
const (
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
)

// Event is an event with a fixed seat capacity.
type Event struct {
	ID             uuid.UUID `json:"id"`
	Name           string    `json:"name"`
	Capacity       int       `json:"capacity"`
	ReservedSeats  int       `json:"reserved_seats"`
	AvailableSeats int       `json:"available_seats"`
	StartsAt       time.Time `json:"starts_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// Booking is a reservation of one or more seats on an event.
type Booking struct {
	ID            uuid.UUID  `json:"id"`
	EventID       uuid.UUID  `json:"event_id"`
	Seats         int        `json:"seats"`
	Status        string     `json:"status"`
	CustomerName  string     `json:"customer_name,omitempty"`
	CustomerEmail string     `json:"customer_email,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	CancelledAt   *time.Time `json:"cancelled_at,omitempty"`
}
