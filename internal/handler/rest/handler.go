// Package rest is the HTTP JSON adapter generated from api/rest/openapi.yaml. It
// maps requests to model contracts and results or errors to responses.
package rest

import (
	"fmt"
	"log/slog"
	"net/http"

	"booking/internal/handler/rest/gen"
	bookingmodel "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
	"booking/internal/model/page"
)

// Handler implements the operations of the generated server interface.
type Handler struct {
	events   EventService
	bookings BookingService
	health   HealthChecker
	log      *slog.Logger
}

var _ gen.ServerInterface = (*Handler)(nil)

// NewHandler returns a Handler using the services.
func NewHandler(events EventService, bookings BookingService, health HealthChecker, log *slog.Logger) *Handler {
	return &Handler{events: events, bookings: bookings, health: health, log: log}
}

// CreateEvent implements POST /events.
func (h *Handler) CreateEvent(w http.ResponseWriter, r *http.Request) {
	var body gen.CreateEventRequest
	if err := decodeJSON(w, r, &body); err != nil {
		h.writeError(w, r, err)

		return
	}

	created, err := h.events.Create(r.Context(), eventmodel.CreateServiceCommand{
		Name:     body.Name,
		StartsAt: body.StartsAt,
		Capacity: int(body.Capacity),
	})
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	w.Header().Set("Location", fmt.Sprintf("/events/%s", created.ID))
	h.writeJSON(w, r, http.StatusCreated, mapEvent(created))
}

// ListEvents implements GET /events.
func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request, params gen.ListEventsParams) {
	found, err := h.events.Search(r.Context(), eventmodel.SearchServiceQuery{
		Page: page.Request{Limit: mapOptionalInt(params.Limit), Offset: mapOptionalInt(params.Offset)},
	})
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, r, http.StatusOK, gen.EventPage{
		Items:   mapItems(found.Items, mapEvent),
		Limit:   int32(found.Limit),  //nolint:gosec // Bounded by page.MaxLimit.
		Offset:  int32(found.Offset), //nolint:gosec // Parsed from an int32 parameter.
		HasMore: found.HasMore,
	})
}

// GetEvent implements GET /events/{event_id}.
func (h *Handler) GetEvent(w http.ResponseWriter, r *http.Request, eventID gen.EventID) {
	found, err := h.events.Fetch(r.Context(), eventID)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, r, http.StatusOK, mapEvent(found))
}

// CreateBooking implements POST /events/{event_id}/bookings.
func (h *Handler) CreateBooking(w http.ResponseWriter, r *http.Request, eventID gen.EventID, params gen.CreateBookingParams) {
	var body gen.CreateBookingRequest
	if err := decodeJSON(w, r, &body); err != nil {
		h.writeError(w, r, err)

		return
	}

	created, err := h.bookings.Create(r.Context(), bookingmodel.CreateServiceCommand{
		EventID:        eventID,
		IdempotencyKey: params.IdempotencyKey,
		CustomerName:   body.CustomerName,
		Seats:          int(body.Seats),
	})
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	status := http.StatusCreated
	if created.Replayed {
		status = http.StatusOK

		w.Header().Set("Idempotent-Replayed", "true")
	}

	w.Header().Set("Location", fmt.Sprintf("/bookings/%s", created.Booking.ID))
	h.writeJSON(w, r, status, mapBooking(created.Booking))
}

// ListEventBookings implements GET /events/{event_id}/bookings.
func (h *Handler) ListEventBookings(w http.ResponseWriter, r *http.Request, eventID gen.EventID, params gen.ListEventBookingsParams) {
	found, err := h.bookings.Search(r.Context(), bookingmodel.SearchServiceQuery{
		EventID: eventID,
		Page:    page.Request{Limit: mapOptionalInt(params.Limit), Offset: mapOptionalInt(params.Offset)},
	})
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, r, http.StatusOK, gen.BookingPage{
		Items:   mapItems(found.Items, mapBooking),
		Limit:   int32(found.Limit),  //nolint:gosec // Bounded by page.MaxLimit.
		Offset:  int32(found.Offset), //nolint:gosec // Parsed from an int32 parameter.
		HasMore: found.HasMore,
	})
}

// GetBooking implements GET /bookings/{booking_id}.
func (h *Handler) GetBooking(w http.ResponseWriter, r *http.Request, bookingID gen.BookingID) {
	found, err := h.bookings.Fetch(r.Context(), bookingID)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, r, http.StatusOK, mapBooking(found))
}

// CancelBooking implements POST /bookings/{booking_id}/cancel.
func (h *Handler) CancelBooking(w http.ResponseWriter, r *http.Request, bookingID gen.BookingID) {
	cancelled, err := h.bookings.Cancel(r.Context(), bookingID)
	if err != nil {
		h.writeError(w, r, err)

		return
	}

	h.writeJSON(w, r, http.StatusOK, mapBooking(cancelled))
}

// GetHealth implements GET /healthz.
func (h *Handler) GetHealth(w http.ResponseWriter, r *http.Request) {
	if err := h.health.Ping(r.Context()); err != nil {
		h.log.ErrorContext(r.Context(), "health check failed", slog.Any("error", err))
		h.writeError(w, r, &requestError{
			status:  http.StatusServiceUnavailable,
			code:    codeServiceUnavailable,
			message: "the database is unavailable",
		})

		return
	}

	h.writeJSON(w, r, http.StatusOK, gen.Health{Status: "ok"})
}
