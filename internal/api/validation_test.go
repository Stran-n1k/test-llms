package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateCreateEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		req       createEventRequest
		wantErr   bool
		wantField string
	}{
		{
			name: "valid",
			req:  createEventRequest{Name: "  Go Conference  ", Capacity: 100, StartsAt: "2026-12-01T18:00:00Z"},
		},
		{
			name:      "missing name",
			req:       createEventRequest{Name: "   ", Capacity: 10, StartsAt: "2026-12-01T18:00:00Z"},
			wantErr:   true,
			wantField: "name",
		},
		{
			name:      "name too long",
			req:       createEventRequest{Name: strings.Repeat("x", 201), Capacity: 10, StartsAt: "2026-12-01T18:00:00Z"},
			wantErr:   true,
			wantField: "name",
		},
		{
			name:      "zero capacity",
			req:       createEventRequest{Name: "Event", Capacity: 0, StartsAt: "2026-12-01T18:00:00Z"},
			wantErr:   true,
			wantField: "capacity",
		},
		{
			name:      "negative capacity",
			req:       createEventRequest{Name: "Event", Capacity: -1, StartsAt: "2026-12-01T18:00:00Z"},
			wantErr:   true,
			wantField: "capacity",
		},
		{
			name:      "capacity too large",
			req:       createEventRequest{Name: "Event", Capacity: 1_000_001, StartsAt: "2026-12-01T18:00:00Z"},
			wantErr:   true,
			wantField: "capacity",
		},
		{
			name:      "missing starts_at",
			req:       createEventRequest{Name: "Event", Capacity: 10},
			wantErr:   true,
			wantField: "starts_at",
		},
		{
			name:      "invalid starts_at",
			req:       createEventRequest{Name: "Event", Capacity: 10, StartsAt: "tomorrow"},
			wantErr:   true,
			wantField: "starts_at",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, aerr := validateCreateEvent(tt.req)
			if !tt.wantErr {
				require.Nil(t, aerr)
				require.Equal(t, "Go Conference", got.Name)
				require.Equal(t, 100, got.Capacity)
				require.False(t, got.StartsAt.IsZero())
				return
			}
			require.NotNil(t, aerr)
			require.Equal(t, tt.wantField, aerr.field)
			require.Equal(t, "validation_error", aerr.code)
		})
	}
}

func TestValidateCreateBooking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		req       createBookingRequest
		wantErr   bool
		wantField string
	}{
		{name: "valid minimal", req: createBookingRequest{Seats: 2}},
		{name: "valid with customer", req: createBookingRequest{Seats: 2, CustomerName: " Alice ", CustomerEmail: "alice@example.com"}},
		{name: "zero seats", req: createBookingRequest{Seats: 0}, wantErr: true, wantField: "seats"},
		{name: "negative seats", req: createBookingRequest{Seats: -3}, wantErr: true, wantField: "seats"},
		{name: "too many seats", req: createBookingRequest{Seats: 1_000_001}, wantErr: true, wantField: "seats"},
		{name: "name too long", req: createBookingRequest{Seats: 1, CustomerName: strings.Repeat("x", 201)}, wantErr: true, wantField: "customer_name"},
		{name: "invalid email", req: createBookingRequest{Seats: 1, CustomerEmail: "not-an-email"}, wantErr: true, wantField: "customer_email"},
		{name: "email with display name", req: createBookingRequest{Seats: 1, CustomerEmail: "Alice <alice@example.com>"}, wantErr: true, wantField: "customer_email"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, aerr := validateCreateBooking(tt.req)
			if !tt.wantErr {
				require.Nil(t, aerr)
				require.Equal(t, tt.req.Seats, got.Seats)
				return
			}
			require.NotNil(t, aerr)
			require.Equal(t, tt.wantField, aerr.field)
			require.Equal(t, "validation_error", aerr.code)
		})
	}
}

func TestIdempotencyKey(t *testing.T) {
	t.Parallel()

	t.Run("required", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/events/1/bookings", nil)
		_, aerr := idempotencyKey(r)
		require.NotNil(t, aerr)
		require.Equal(t, "idempotency_key_required", aerr.code)
	})

	t.Run("trimmed and accepted", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/events/1/bookings", nil)
		r.Header.Set("Idempotency-Key", "  abc-123  ")
		key, aerr := idempotencyKey(r)
		require.Nil(t, aerr)
		require.Equal(t, "abc-123", key)
	})

	t.Run("too long", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/events/1/bookings", nil)
		r.Header.Set("Idempotency-Key", strings.Repeat("k", 201))
		_, aerr := idempotencyKey(r)
		require.NotNil(t, aerr)
		require.Equal(t, "validation_error", aerr.code)
	})

	t.Run("control character", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/events/1/bookings", nil)
		r.Header["Idempotency-Key"] = []string{"abc\x00def"}
		_, aerr := idempotencyKey(r)
		require.NotNil(t, aerr)
		require.Equal(t, "validation_error", aerr.code)
	})
}
