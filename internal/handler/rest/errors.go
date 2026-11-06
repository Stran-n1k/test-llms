package rest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"booking/internal/handler/rest/gen"
	bookingmodel "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
	"booking/internal/model/validation"
)

// statusClientClosedRequest is the conventional status for a caller that hung
// up. It only reaches the request log, because nobody is left to read it.
const statusClientClosedRequest = 499

// Stable error codes of the API contract.
const (
	codeInvalidJSON          = "invalid_json"
	codeValidationFailed     = "validation_failed"
	codeUnsupportedMediaType = "unsupported_media_type"
	codePayloadTooLarge      = "payload_too_large"
	codeNotFound             = "not_found"
	codeMethodNotAllowed     = "method_not_allowed"
	codeEventNotFound        = "event_not_found"
	codeBookingNotFound      = "booking_not_found"
	codeInsufficientSeats    = "insufficient_seats"
	codeIdempotencyKeyReused = "idempotency_key_reused"
	codeServiceUnavailable   = "service_unavailable"
	codeInternal             = "internal_error"
)

// requestError is a failure the HTTP layer itself detected and described.
type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string {
	return e.message
}

// writeError is the one place that turns an error into a response.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		requestErr *requestError
		invalid    *validation.Error
		seats      *bookingmodel.InsufficientSeatsError
	)

	switch {
	case errors.As(err, &requestErr):
		h.writeProblem(w, r, requestErr.status, requestErr.code, requestErr.message, nil)
	case errors.As(err, &invalid):
		h.writeValidationProblem(w, r, invalid.Fields)
	case errors.Is(err, eventmodel.ErrNotFound):
		h.writeProblem(w, r, http.StatusNotFound, codeEventNotFound, "event not found", nil)
	case errors.Is(err, bookingmodel.ErrNotFound):
		h.writeProblem(w, r, http.StatusNotFound, codeBookingNotFound, "booking not found", nil)
	case errors.As(err, &seats):
		h.writeProblem(w, r, http.StatusConflict, codeInsufficientSeats, seats.Error(), nil)
	case errors.Is(err, bookingmodel.ErrIdempotencyKeyReused):
		h.writeProblem(w, r, http.StatusUnprocessableEntity, codeIdempotencyKeyReused,
			"this Idempotency-Key was already used for a different request; use a new key for a new booking", nil)
	case errors.Is(r.Context().Err(), context.Canceled):
		w.WriteHeader(statusClientClosedRequest)
	case errors.Is(r.Context().Err(), context.DeadlineExceeded):
		h.log.ErrorContext(r.Context(), "request timed out", slog.Any("error", err))
		h.writeProblem(w, r, http.StatusServiceUnavailable, codeServiceUnavailable,
			"the request took too long; it is safe to retry", nil)
	default:
		h.log.ErrorContext(r.Context(), "request failed", slog.Any("error", err))
		h.writeProblem(w, r, http.StatusInternalServerError, codeInternal, "internal server error", nil)
	}
}

// writeParamError answers a path, query or header parameter the generated
// binding rejected.
func (h *Handler) writeParamError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		invalidParam  *gen.InvalidParamFormatError
		missingHeader *gen.RequiredHeaderError
		missingParam  *gen.RequiredParamError
		tooMany       *gen.TooManyValuesForParamError
	)

	field := validation.FieldError{Field: "request", Message: "has invalid parameters"}

	switch {
	case errors.As(err, &invalidParam):
		field = validation.FieldError{Field: invalidParam.ParamName, Message: "has an invalid format"}
	case errors.As(err, &missingHeader):
		field = validation.FieldError{Field: missingHeader.ParamName, Message: "is required"}
	case errors.As(err, &missingParam):
		field = validation.FieldError{Field: missingParam.ParamName, Message: "is required"}
	case errors.As(err, &tooMany):
		field = validation.FieldError{Field: tooMany.ParamName, Message: "must be given only once"}
	}

	h.writeValidationProblem(w, r, []validation.FieldError{field})
}

func (h *Handler) writeValidationProblem(w http.ResponseWriter, r *http.Request, fields []validation.FieldError) {
	mapped := make([]gen.FieldError, 0, len(fields))
	for _, field := range fields {
		mapped = append(mapped, gen.FieldError{Field: field.Field, Message: field.Message})
	}

	h.writeProblem(w, r, http.StatusBadRequest, codeValidationFailed, "the request is invalid; see fields", &mapped)
}

func (h *Handler) writeProblem(w http.ResponseWriter, r *http.Request, status int, code, message string, fields *[]gen.FieldError) {
	var body gen.Error

	body.Error.Code = code
	body.Error.Message = message
	body.Error.Fields = fields

	h.writeJSON(w, r, status, body)
}

// writeJSON encodes before it commits to a status, so a value that cannot be
// encoded becomes an explicit error instead of a success with an empty body.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		h.log.ErrorContext(r.Context(), "encode response", slog.Any("error", err))

		status = http.StatusInternalServerError
		encoded = []byte(`{"error":{"code":"` + codeInternal + `","message":"internal server error"}}`)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if _, err := w.Write(append(encoded, '\n')); err != nil {
		h.log.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}
