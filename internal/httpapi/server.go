// Package httpapi exposes the booking service as a JSON HTTP API.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"booking/internal/domain"
	"booking/internal/service"
)

// NewRouter builds the HTTP handler with all routes.
func NewRouter(svc *service.Service) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := svc.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, newErrorBody("unavailable", "database unreachable"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /events", handleCreateEvent(svc))
	mux.HandleFunc("GET /events", handleListEvents(svc))
	mux.HandleFunc("GET /events/{id}", handleGetEvent(svc))
	mux.HandleFunc("POST /bookings", handleCreateBooking(svc))
	mux.HandleFunc("GET /bookings/{id}", handleGetBooking(svc))
	mux.HandleFunc("POST /bookings/{id}/cancel", handleCancelBooking(svc))

	return withRecovery(withLogging(mux))
}

// --- request / response types ---

type createEventRequest struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

type eventResponse struct {
	ID                   int64     `json:"id"`
	Name                 string    `json:"name"`
	Capacity             int       `json:"capacity"`
	CreatedAt            time.Time `json:"created_at"`
	BookedSeats          int       `json:"booked_seats"`
	AvailableSeats       int       `json:"available_seats"`
	AvailableSeatNumbers []int     `json:"available_seat_numbers,omitempty"`
}

type createBookingRequest struct {
	EventID        int64  `json:"event_id"`
	Seats          []int  `json:"seats"`
	IdempotencyKey string `json:"idempotency_key"`
}

type bookingResponse struct {
	ID          int64      `json:"id"`
	EventID     int64      `json:"event_id"`
	Seats       []int      `json:"seats"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newErrorBody(code, message string) errorBody {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = message
	return b
}

// --- handlers ---

func handleCreateEvent(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createEventRequest
		if err := decode(r, &req); err != nil {
			writeError(w, err)
			return
		}
		event, err := svc.CreateEvent(r.Context(), req.Name, req.Capacity)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, eventResponse{
			ID:             event.ID,
			Name:           event.Name,
			Capacity:       event.Capacity,
			CreatedAt:      event.CreatedAt,
			BookedSeats:    0,
			AvailableSeats: event.Capacity,
		})
	}
}

func handleListEvents(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		events, err := svc.ListEvents(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		resp := make([]eventResponse, 0, len(events))
		for _, e := range events {
			resp = append(resp, eventResponse{
				ID:             e.ID,
				Name:           e.Name,
				Capacity:       e.Capacity,
				CreatedAt:      e.CreatedAt,
				BookedSeats:    e.Booked,
				AvailableSeats: e.Available(),
			})
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleGetEvent(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			writeError(w, err)
			return
		}
		e, err := svc.GetEvent(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, eventResponse{
			ID:                   e.ID,
			Name:                 e.Name,
			Capacity:             e.Capacity,
			CreatedAt:            e.CreatedAt,
			BookedSeats:          e.Booked,
			AvailableSeats:       e.Available(),
			AvailableSeatNumbers: e.AvailableSeats,
		})
	}
}

func handleCreateBooking(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createBookingRequest
		if err := decode(r, &req); err != nil {
			writeError(w, err)
			return
		}
		// The idempotency key may come from the header or the body; the
		// header wins if both are present.
		token := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if token == "" {
			token = strings.TrimSpace(req.IdempotencyKey)
		}
		booking, created, err := svc.CreateBooking(r.Context(), req.EventID, token, req.Seats)
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, newBookingResponse(booking))
	}
}

func handleGetBooking(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			writeError(w, err)
			return
		}
		booking, err := svc.GetBooking(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, newBookingResponse(booking))
	}
}

func handleCancelBooking(svc *service.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			writeError(w, err)
			return
		}
		booking, err := svc.CancelBooking(r.Context(), id)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, newBookingResponse(booking))
	}
}

// --- helpers ---

func newBookingResponse(b domain.Booking) bookingResponse {
	return bookingResponse{
		ID:          b.ID,
		EventID:     b.EventID,
		Seats:       b.Seats,
		Status:      b.Status,
		CreatedAt:   b.CreatedAt,
		CancelledAt: b.CancelledAt,
	}
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, &domain.ValidationError{Msg: "invalid id in path"}
	}
	return id, nil
}

func decode(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &domain.ValidationError{Msg: "invalid JSON body: " + err.Error()}
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return &domain.ValidationError{Msg: "request body must contain a single JSON object"}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	var validationErr *domain.ValidationError
	var rangeErr *domain.SeatOutOfRangeError
	var takenErr *domain.SeatTakenError
	switch {
	case errors.As(err, &validationErr):
		writeJSON(w, http.StatusBadRequest, newErrorBody("invalid_request", err.Error()))
	case errors.As(err, &rangeErr):
		writeJSON(w, http.StatusBadRequest, newErrorBody("seat_out_of_range", err.Error()))
	case errors.Is(err, domain.ErrNotFound):
		writeJSON(w, http.StatusNotFound, newErrorBody("not_found", "requested entity does not exist"))
	case errors.As(err, &takenErr):
		writeJSON(w, http.StatusConflict, newErrorBody("seat_taken", err.Error()))
	default:
		log.Printf("internal error: %v", err)
		writeJSON(w, http.StatusInternalServerError, newErrorBody("internal", "internal server error"))
	}
}

// --- middleware ---

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Microsecond))
	})
}

func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic: %v", rec)
				writeJSON(w, http.StatusInternalServerError, newErrorBody("internal", "internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
