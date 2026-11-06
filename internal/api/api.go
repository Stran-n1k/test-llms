// Package api exposes the HTTP JSON API.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"booking/internal/model"
	"booking/internal/store"
)

const (
	// Maximum accepted request body size (1 MiB).
	maxBodyBytes = 1 << 20
	// Per-request server-side deadline.
	requestTimeout = 15 * time.Second
)

// Store is the persistence contract required by the HTTP layer.
type Store interface {
	CreateEvent(ctx context.Context, p store.CreateEventParams) (model.Event, error)
	GetEvent(ctx context.Context, id uuid.UUID) (model.Event, error)
	ListEvents(ctx context.Context) ([]model.Event, error)
	CreateBooking(ctx context.Context, eventID uuid.UUID, idempotencyKey string, p store.CreateBookingParams) (model.Booking, bool, error)
	GetBooking(ctx context.Context, id uuid.UUID) (model.Booking, error)
	ListEventBookings(ctx context.Context, eventID uuid.UUID) ([]model.Booking, error)
	CancelBooking(ctx context.Context, id uuid.UUID) (model.Booking, error)
	Ping(ctx context.Context) error
}

// Server holds the HTTP handlers.
type Server struct {
	store   Store
	logger  *slog.Logger
	handler http.Handler
}

// New builds the HTTP server around a store.
func New(st Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{store: st, logger: logger}
	s.handler = s.buildHandler()
	return s
}

// Handler returns the root HTTP handler including middleware.
func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("POST /events", s.handleCreateEvent)
	mux.HandleFunc("GET /events", s.handleListEvents)
	mux.HandleFunc("GET /events/{id}", s.handleGetEvent)
	mux.HandleFunc("GET /events/{id}/availability", s.handleGetAvailability)
	mux.HandleFunc("GET /events/{id}/bookings", s.handleListEventBookings)
	mux.HandleFunc("POST /events/{id}/bookings", s.handleCreateBooking)
	mux.HandleFunc("GET /bookings/{id}", s.handleGetBooking)
	mux.HandleFunc("DELETE /bookings/{id}", s.handleCancelBooking)
	mux.HandleFunc("/", s.handleNotFound)

	return s.recoverPanics(s.logRequests(s.withTimeout(mux)))
}
