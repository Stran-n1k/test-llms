package api

import (
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"booking/internal/store"
)

const (
	maxNameRunes         = 200
	maxEmailLen          = 254
	maxCapacity          = 1_000_000
	maxSeatsPerBooking   = 1_000_000
	maxIdempotencyKeyLen = 200
	startsAtLayout       = time.RFC3339
)

type createEventRequest struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
	StartsAt string `json:"starts_at"`
}

type createBookingRequest struct {
	Seats         int    `json:"seats"`
	CustomerName  string `json:"customer_name"`
	CustomerEmail string `json:"customer_email"`
}

type availabilityResponse struct {
	EventID        uuid.UUID `json:"event_id"`
	Capacity       int       `json:"capacity"`
	ReservedSeats  int       `json:"reserved_seats"`
	AvailableSeats int       `json:"available_seats"`
}

type listResponse[T any] struct {
	Items []T `json:"items"`
}

func validateCreateEvent(req createEventRequest) (store.CreateEventParams, *apiError) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return store.CreateEventParams{}, badRequest(
			"validation_error", "name is required", "name")
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		return store.CreateEventParams{}, badRequest(
			"validation_error", "name must be at most 200 characters", "name")
	}
	if req.Capacity < 1 || req.Capacity > maxCapacity {
		return store.CreateEventParams{}, badRequest(
			"validation_error", "capacity must be between 1 and 1000000", "capacity")
	}
	startsAtRaw := strings.TrimSpace(req.StartsAt)
	if startsAtRaw == "" {
		return store.CreateEventParams{}, badRequest(
			"validation_error", "starts_at is required (RFC 3339, for example 2026-12-01T18:00:00Z)", "starts_at")
	}
	startsAt, err := time.Parse(startsAtLayout, startsAtRaw)
	if err != nil {
		return store.CreateEventParams{}, badRequest(
			"validation_error", "starts_at must be a valid RFC 3339 timestamp", "starts_at")
	}
	return store.CreateEventParams{
		Name:     name,
		Capacity: req.Capacity,
		StartsAt: startsAt,
	}, nil
}

func validateCreateBooking(req createBookingRequest) (store.CreateBookingParams, *apiError) {
	if req.Seats < 1 || req.Seats > maxSeatsPerBooking {
		return store.CreateBookingParams{}, badRequest(
			"validation_error", "seats must be between 1 and 1000000", "seats")
	}
	name := strings.TrimSpace(req.CustomerName)
	if utf8.RuneCountInString(name) > maxNameRunes {
		return store.CreateBookingParams{}, badRequest(
			"validation_error", "customer_name must be at most 200 characters", "customer_name")
	}
	email := strings.TrimSpace(req.CustomerEmail)
	if email != "" {
		if len(email) > maxEmailLen {
			return store.CreateBookingParams{}, badRequest(
				"validation_error", "customer_email must be at most 254 characters", "customer_email")
		}
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address != email {
			return store.CreateBookingParams{}, badRequest(
				"validation_error", "customer_email must be a valid email address", "customer_email")
		}
	}
	return store.CreateBookingParams{
		Seats:         req.Seats,
		CustomerName:  name,
		CustomerEmail: email,
	}, nil
}

func idempotencyKey(r *http.Request) (string, *apiError) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return "", badRequest(
			"idempotency_key_required",
			"the Idempotency-Key header is required to make booking creation safe to retry",
			"Idempotency-Key")
	}
	if len(key) > maxIdempotencyKeyLen {
		return "", badRequest(
			"validation_error", "Idempotency-Key must be at most 200 characters", "Idempotency-Key")
	}
	if !utf8.ValidString(key) {
		return "", badRequest(
			"validation_error", "Idempotency-Key must be valid UTF-8", "Idempotency-Key")
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return "", badRequest(
				"validation_error", "Idempotency-Key must not contain control characters", "Idempotency-Key")
		}
	}
	return key, nil
}
