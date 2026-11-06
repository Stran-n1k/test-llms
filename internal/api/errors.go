package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"booking/internal/store"
)

type errorBody struct {
	Error errorInfo `json:"error"`
}

type errorInfo struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Field   string         `json:"field,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

type apiError struct {
	status  int
	code    string
	message string
	field   string
	details map[string]any
}

func (s *Server) writeError(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.status, errorBody{Error: errorInfo{
		Code:    e.code,
		Message: e.message,
		Field:   e.field,
		Details: e.details,
	}})
}

func badRequest(code, message, field string) *apiError {
	return &apiError{status: http.StatusBadRequest, code: code, message: message, field: field}
}

func (s *Server) writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var notEnough *store.NotEnoughSeatsError
	switch {
	case errors.Is(err, store.ErrEventNotFound):
		s.writeError(w, &apiError{
			status:  http.StatusNotFound,
			code:    "event_not_found",
			message: "the requested event does not exist",
		})
	case errors.Is(err, store.ErrBookingNotFound):
		s.writeError(w, &apiError{
			status:  http.StatusNotFound,
			code:    "booking_not_found",
			message: "the requested booking does not exist",
		})
	case errors.As(err, &notEnough):
		s.writeError(w, &apiError{
			status:  http.StatusConflict,
			code:    "not_enough_seats",
			message: "not enough seats are available for this event",
			details: map[string]any{
				"available_seats": notEnough.Available,
				"requested_seats": notEnough.Requested,
			},
		})
	case errors.Is(err, store.ErrIdempotencyConflict):
		s.writeError(w, &apiError{
			status:  http.StatusConflict,
			code:    "idempotency_key_conflict",
			message: "this Idempotency-Key was already used with a different request payload; repeat the original request or use a new key",
		})
	default:
		s.logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		s.writeError(w, &apiError{
			status:  http.StatusInternalServerError,
			code:    "internal_error",
			message: "internal server error",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON strictly decodes a single JSON object from the request body.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apiError {
	if err := requireJSONContentType(r); err != nil {
		return err
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return &apiError{
				status:  http.StatusRequestEntityTooLarge,
				code:    "payload_too_large",
				message: fmt.Sprintf("request body must not exceed %d bytes", maxBodyBytes),
			}
		case errors.Is(err, io.EOF):
			return badRequest("invalid_json", "request body is required", "")
		default:
			return badRequest("invalid_json", "invalid JSON body: "+err.Error(), "")
		}
	}

	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return badRequest("invalid_json", "request body must contain exactly one JSON object", "")
	}
	return nil
}

func requireJSONContentType(r *http.Request) *apiError {
	ct := r.Header.Get("Content-Type")
	if strings.TrimSpace(ct) == "" {
		return &apiError{
			status:  http.StatusUnsupportedMediaType,
			code:    "unsupported_media_type",
			message: "Content-Type must be application/json",
		}
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != "application/json" {
		return &apiError{
			status:  http.StatusUnsupportedMediaType,
			code:    "unsupported_media_type",
			message: "Content-Type must be application/json",
		}
	}
	return nil
}
