package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"booking/internal/model"
)

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status":   "unavailable",
			"database": "down",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   "ready",
		"database": "up",
	})
}

func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	var req createEventRequest
	if aerr := decodeJSON(w, r, &req); aerr != nil {
		s.writeError(w, aerr)
		return
	}
	params, aerr := validateCreateEvent(req)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}

	event, err := s.store.CreateEvent(r.Context(), params)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	w.Header().Set("Location", "/events/"+event.ID.String())
	writeJSON(w, http.StatusCreated, event)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListEvents(r.Context())
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Event]{Items: events})
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id, aerr := pathUUID(r, "id")
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	event, err := s.store.GetEvent(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *Server) handleGetAvailability(w http.ResponseWriter, r *http.Request) {
	id, aerr := pathUUID(r, "id")
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	event, err := s.store.GetEvent(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, availabilityResponse{
		EventID:        event.ID,
		Capacity:       event.Capacity,
		ReservedSeats:  event.ReservedSeats,
		AvailableSeats: event.AvailableSeats,
	})
}

func (s *Server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	eventID, aerr := pathUUID(r, "id")
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	key, aerr := idempotencyKey(r)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}

	var req createBookingRequest
	if aerr := decodeJSON(w, r, &req); aerr != nil {
		s.writeError(w, aerr)
		return
	}
	params, aerr := validateCreateBooking(req)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}

	booking, replayed, err := s.store.CreateBooking(r.Context(), eventID, key, params)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}

	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, booking)
		return
	}
	w.Header().Set("Location", "/bookings/"+booking.ID.String())
	writeJSON(w, http.StatusCreated, booking)
}

func (s *Server) handleListEventBookings(w http.ResponseWriter, r *http.Request) {
	eventID, aerr := pathUUID(r, "id")
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	bookings, err := s.store.ListEventBookings(r.Context(), eventID)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse[model.Booking]{Items: bookings})
}

func (s *Server) handleGetBooking(w http.ResponseWriter, r *http.Request) {
	id, aerr := pathUUID(r, "id")
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	booking, err := s.store.GetBooking(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

func (s *Server) handleCancelBooking(w http.ResponseWriter, r *http.Request) {
	id, aerr := pathUUID(r, "id")
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	booking, err := s.store.CancelBooking(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, booking)
}

func (s *Server) handleNotFound(w http.ResponseWriter, _ *http.Request) {
	s.writeError(w, &apiError{
		status:  http.StatusNotFound,
		code:    "not_found",
		message: "route not found",
	})
}

func pathUUID(r *http.Request, name string) (uuid.UUID, *apiError) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, badRequest(
			"invalid_id",
			"path parameter "+name+" must be a valid UUID",
			name)
	}
	return id, nil
}
