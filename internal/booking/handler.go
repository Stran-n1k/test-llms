package booking

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type Handler struct {
	store *Store
	mux   *http.ServeMux
}

func NewHandler(store *Store) http.Handler {
	h := &Handler{store: store, mux: http.NewServeMux()}
	h.mux.HandleFunc("/healthz", h.health)
	h.mux.HandleFunc("/events", h.events)
	h.mux.HandleFunc("/events/", h.event)
	h.mux.HandleFunc("/bookings", h.bookings)
	h.mux.HandleFunc("/bookings/", h.booking)
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if err := h.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request struct {
		Title    string `json:"title"`
		Capacity int    `json:"capacity"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	request.Title = strings.TrimSpace(request.Title)
	if request.Title == "" || len([]rune(request.Title)) > MaxTitleLength || strings.ContainsRune(request.Title, '\x00') {
		writeError(w, http.StatusBadRequest, "validation_error", "title must contain between 1 and 200 characters and no NUL bytes")
		return
	}
	if request.Capacity < 1 || request.Capacity > MaxCapacity {
		writeError(w, http.StatusBadRequest, "validation_error", "capacity must be between 1 and 1000000")
		return
	}
	event, err := h.store.CreateEvent(r.Context(), request.Title, request.Capacity)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, event)
}

func (h *Handler) event(w http.ResponseWriter, r *http.Request) {
	parts := pathParts(r.URL.Path, "/events/")
	if len(parts) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	eventID, err := uuid.Parse(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "event id must be a UUID")
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		event, err := h.store.GetEvent(r.Context(), eventID)
		if err != nil {
			h.storeError(w, err, "event not found")
			return
		}
		writeJSON(w, http.StatusOK, event)
		return
	}
	if len(parts) == 2 && parts[1] == "availability" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		availability, err := h.store.GetAvailability(r.Context(), eventID)
		if err != nil {
			h.storeError(w, err, "event not found")
			return
		}
		writeJSON(w, http.StatusOK, availability)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "route not found")
}

func (h *Handler) bookings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request struct {
		EventID        string `json:"event_id"`
		Seats          int    `json:"seats"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decodeRequest(w, r, &request) {
		return
	}
	eventID, err := uuid.Parse(request.EventID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "event_id must be a UUID")
		return
	}
	if request.Seats < 1 || request.Seats > MaxCapacity {
		writeError(w, http.StatusBadRequest, "validation_error", "seats must be between 1 and 1000000")
		return
	}
	if len(request.IdempotencyKey) < 1 || len(request.IdempotencyKey) > MaxIdempotencyKey || strings.ContainsRune(request.IdempotencyKey, '\x00') {
		writeError(w, http.StatusBadRequest, "validation_error", "idempotency_key must contain between 1 and 200 bytes and no NUL bytes")
		return
	}
	booking, created, err := h.store.CreateBooking(r.Context(), eventID, request.Seats, request.IdempotencyKey)
	if err != nil {
		h.storeError(w, err, "event not found")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, booking)
}

func (h *Handler) booking(w http.ResponseWriter, r *http.Request) {
	parts := pathParts(r.URL.Path, "/bookings/")
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	bookingID, err := uuid.Parse(parts[0])
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "booking id must be a UUID")
		return
	}
	switch r.Method {
	case http.MethodGet:
		booking, err := h.store.GetBooking(r.Context(), bookingID)
		if err != nil {
			h.storeError(w, err, "booking not found")
			return
		}
		writeJSON(w, http.StatusOK, booking)
	case http.MethodDelete:
		booking, err := h.store.CancelBooking(r.Context(), bookingID)
		if err != nil {
			h.storeError(w, err, "booking not found")
			return
		}
		writeJSON(w, http.StatusOK, booking)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodDelete)
	}
}

func (h *Handler) storeError(w http.ResponseWriter, err error, notFoundMessage string) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", notFoundMessage)
	case errors.Is(err, ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "idempotency_key was already used with a different event_id or seats")
	default:
		var capacityErr *CapacityError
		if errors.As(err, &capacityErr) {
			writeError(w, http.StatusConflict, "insufficient_capacity", capacityErr.Error())
			return
		}
		internalError(w, err)
	}
}

func pathParts(path, prefix string) []string {
	trimmed := strings.TrimPrefix(path, prefix)
	if trimmed == "" || strings.HasSuffix(trimmed, "/") {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", fmt.Sprintf("invalid JSON: %v", err))
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain exactly one JSON object")
		return false
	}
	return true
}

type errorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: apiError{Code: code, Message: message}})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func internalError(w http.ResponseWriter, _ error) {
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
