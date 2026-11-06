// Package service contains business logic and validation on top of a Store.
package service

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"booking/internal/domain"
)

// Limits for requests; documented in README.
const (
	DefaultMaxCapacity        = 100_000
	DefaultMaxSeatsPerBooking = 100
	maxNameLength             = 200
	maxTokenLength            = 128
)

// Store is the persistence interface the service depends on.
// Implementations must provide the concurrency guarantees documented
// on each method (the PostgreSQL implementation does this via
// transactions, a unique constraint and a partial unique index).
type Store interface {
	Ping(ctx context.Context) error
	Migrate(ctx context.Context) error
	CreateEvent(ctx context.Context, name string, capacity int) (domain.Event, error)
	ListEvents(ctx context.Context) ([]domain.EventAvailability, error)
	GetEvent(ctx context.Context, id int64) (domain.EventAvailability, error)
	// CreateBooking books the given seats atomically. It is idempotent per
	// (eventID, clientToken): a repeat returns the existing booking with
	// created=false. It returns SeatTakenError on conflicts and
	// ErrNotFound if the event does not exist.
	CreateBooking(ctx context.Context, eventID int64, clientToken string, seats []int) (domain.Booking, bool, error)
	GetBooking(ctx context.Context, id int64) (domain.Booking, error)
	// CancelBooking releases the booking's seats exactly once and is
	// idempotent: repeating it returns the already cancelled booking.
	CancelBooking(ctx context.Context, id int64) (domain.Booking, error)
}

// Service validates input and delegates to the Store.
type Service struct {
	store              Store
	maxCapacity        int
	maxSeatsPerBooking int
}

// New builds a Service with default limits.
func New(s Store) *Service {
	return &Service{
		store:              s,
		maxCapacity:        DefaultMaxCapacity,
		maxSeatsPerBooking: DefaultMaxSeatsPerBooking,
	}
}

func (s *Service) Ping(ctx context.Context) error { return s.store.Ping(ctx) }

func (s *Service) Migrate(ctx context.Context) error { return s.store.Migrate(ctx) }

func (s *Service) CreateEvent(ctx context.Context, name string, capacity int) (domain.Event, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.Event{}, &domain.ValidationError{Msg: "name is required"}
	}
	if len(name) > maxNameLength {
		return domain.Event{}, &domain.ValidationError{Msg: fmt.Sprintf("name must be at most %d characters", maxNameLength)}
	}
	if capacity < 1 {
		return domain.Event{}, &domain.ValidationError{Msg: "capacity must be at least 1"}
	}
	if capacity > s.maxCapacity {
		return domain.Event{}, &domain.ValidationError{Msg: fmt.Sprintf("capacity must be at most %d", s.maxCapacity)}
	}
	return s.store.CreateEvent(ctx, name, capacity)
}

func (s *Service) ListEvents(ctx context.Context) ([]domain.EventAvailability, error) {
	return s.store.ListEvents(ctx)
}

func (s *Service) GetEvent(ctx context.Context, id int64) (domain.EventAvailability, error) {
	return s.store.GetEvent(ctx, id)
}

func (s *Service) CreateBooking(ctx context.Context, eventID int64, clientToken string, seats []int) (domain.Booking, bool, error) {
	clientToken = strings.TrimSpace(clientToken)
	if clientToken == "" {
		return domain.Booking{}, false, &domain.ValidationError{
			Msg: "idempotency key is required (Idempotency-Key header or idempotency_key field)",
		}
	}
	if len(clientToken) > maxTokenLength {
		return domain.Booking{}, false, &domain.ValidationError{
			Msg: fmt.Sprintf("idempotency key must be at most %d characters", maxTokenLength),
		}
	}
	if len(seats) == 0 {
		return domain.Booking{}, false, &domain.ValidationError{Msg: "seats must contain at least one seat number"}
	}
	if len(seats) > s.maxSeatsPerBooking {
		return domain.Booking{}, false, &domain.ValidationError{
			Msg: fmt.Sprintf("at most %d seats can be booked in a single request", s.maxSeatsPerBooking),
		}
	}
	sorted := slices.Clone(seats)
	slices.Sort(sorted)
	for i, seat := range sorted {
		if seat < 1 {
			return domain.Booking{}, false, &domain.ValidationError{
				Msg: fmt.Sprintf("seat numbers must be positive, got %d", seat),
			}
		}
		if i > 0 && seat == sorted[i-1] {
			return domain.Booking{}, false, &domain.ValidationError{
				Msg: fmt.Sprintf("duplicate seat number %d in request", seat),
			}
		}
	}
	return s.store.CreateBooking(ctx, eventID, clientToken, sorted)
}

func (s *Service) GetBooking(ctx context.Context, id int64) (domain.Booking, error) {
	return s.store.GetBooking(ctx, id)
}

func (s *Service) CancelBooking(ctx context.Context, id int64) (domain.Booking, error) {
	return s.store.CancelBooking(ctx, id)
}
