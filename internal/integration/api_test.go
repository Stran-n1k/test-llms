package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"booking/internal/migrate"
	"booking/internal/model"
)

func TestEventLifecycle(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)

	event := a.createEvent("Go Conference", 10)
	require.Equal(t, "Go Conference", event.Name)
	require.Equal(t, 10, event.Capacity)
	require.Equal(t, 0, event.ReservedSeats)
	require.Equal(t, 10, event.AvailableSeats)

	status, _, body := a.do(http.MethodGet, "/events/"+event.ID.String(), nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var fetched model.Event
	require.NoError(t, json.Unmarshal(body, &fetched))
	require.Equal(t, event.ID, fetched.ID)

	status, _, body = a.do(http.MethodGet, "/events", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var list eventsList
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Items, 1)
	require.Empty(t, a.listBookings(event.ID))

	booking := a.mustCreateBooking(event.ID, "lifecycle-1", 2)
	require.Equal(t, model.StatusConfirmed, booking.Status)
	require.Equal(t, 2, booking.Seats)
	require.Nil(t, booking.CancelledAt)

	status, _, body = a.do(http.MethodGet, "/bookings/"+booking.ID.String(), nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var fetchedBooking model.Booking
	require.NoError(t, json.Unmarshal(body, &fetchedBooking))
	require.Equal(t, booking.ID, fetchedBooking.ID)

	reserved, available := a.availability(event.ID)
	require.Equal(t, 2, reserved)
	require.Equal(t, 8, available)
	require.Len(t, a.listBookings(event.ID), 1)

	status, body, err := a.cancelBooking(booking.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, string(body))
	var cancelled model.Booking
	require.NoError(t, json.Unmarshal(body, &cancelled))
	require.Equal(t, model.StatusCancelled, cancelled.Status)
	require.NotNil(t, cancelled.CancelledAt)

	reserved, available = a.availability(event.ID)
	require.Equal(t, 0, reserved)
	require.Equal(t, 10, available)

	// Repeated cancellation is a harmless no-op and must not release seats twice.
	status, body, err = a.cancelBooking(booking.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, string(body))
	var cancelledAgain model.Booking
	require.NoError(t, json.Unmarshal(body, &cancelledAgain))
	require.Equal(t, model.StatusCancelled, cancelledAgain.Status)
	require.NotNil(t, cancelledAgain.CancelledAt)
	require.True(t, cancelledAgain.CancelledAt.Equal(*cancelled.CancelledAt))

	reserved, available = a.availability(event.ID)
	require.Equal(t, 0, reserved)
	require.Equal(t, 10, available)

	status, body, err = a.cancelBooking(uuid.New())
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, status, string(body))
	require.Equal(t, "booking_not_found", decodeError(t, body).Error.Code)

	status, _, body = a.do(http.MethodGet, "/events/"+uuid.New().String(), nil, nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	require.Equal(t, "event_not_found", decodeError(t, body).Error.Code)

	status, _, body = a.do(http.MethodGet, "/events/"+uuid.New().String()+"/bookings", nil, nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	require.Equal(t, "event_not_found", decodeError(t, body).Error.Code)
}

func TestValidationErrors(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)

	validStartsAt := "2026-12-01T18:00:00Z"

	t.Run("content type required", func(t *testing.T) {
		status, _, body, err := a.doRaw(http.MethodPost, "/events",
			[]byte(`{"name":"x","capacity":1,"starts_at":"`+validStartsAt+`"}`), "", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnsupportedMediaType, status, string(body))
		require.Equal(t, "unsupported_media_type", decodeError(t, body).Error.Code)
	})

	t.Run("malformed json", func(t *testing.T) {
		status, _, body, err := a.doRaw(http.MethodPost, "/events", []byte(`{`), "application/json", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		require.Equal(t, "invalid_json", decodeError(t, body).Error.Code)
	})

	t.Run("unknown field", func(t *testing.T) {
		status, _, body, err := a.doRaw(http.MethodPost, "/events",
			[]byte(`{"name":"x","capacity":1,"starts_at":"`+validStartsAt+`","bogus":true}`),
			"application/json", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		require.Equal(t, "invalid_json", decodeError(t, body).Error.Code)
	})

	eventCases := []struct {
		name  string
		body  string
		field string
	}{
		{
			name:  "zero capacity",
			body:  `{"name":"x","capacity":0,"starts_at":"` + validStartsAt + `"}`,
			field: "capacity",
		},
		{
			name:  "blank name",
			body:  `{"name":"   ","capacity":1,"starts_at":"` + validStartsAt + `"}`,
			field: "name",
		},
		{
			name:  "bad timestamp",
			body:  `{"name":"x","capacity":1,"starts_at":"tomorrow"}`,
			field: "starts_at",
		},
	}
	for _, tc := range eventCases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, body, err := a.doRaw(http.MethodPost, "/events", []byte(tc.body), "application/json", nil)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, status, string(body))
			resp := decodeError(t, body)
			require.Equal(t, "validation_error", resp.Error.Code)
			require.Equal(t, tc.field, resp.Error.Field)
		})
	}

	// None of the rejected requests above may have created data.
	status, _, body := a.do(http.MethodGet, "/events", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var list eventsList
	require.NoError(t, json.Unmarshal(body, &list))
	require.Empty(t, list.Items)

	event := a.createEvent("Validation", 2)

	t.Run("invalid event id", func(t *testing.T) {
		status, _, body := a.do(http.MethodGet, "/events/not-a-uuid", nil, nil)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		resp := decodeError(t, body)
		require.Equal(t, "invalid_id", resp.Error.Code)
		require.Equal(t, "id", resp.Error.Field)
	})

	t.Run("idempotency key required", func(t *testing.T) {
		status, _, body := a.do(http.MethodPost, "/events/"+event.ID.String()+"/bookings",
			bookingRequest{Seats: 1}, nil)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		require.Equal(t, "idempotency_key_required", decodeError(t, body).Error.Code)
	})

	t.Run("zero seats", func(t *testing.T) {
		status, _, body, err := a.createBooking(event.ID, "v-zero", 0)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		resp := decodeError(t, body)
		require.Equal(t, "validation_error", resp.Error.Code)
		require.Equal(t, "seats", resp.Error.Field)
	})

	t.Run("invalid email", func(t *testing.T) {
		payload := mustJSON(t, bookingRequest{Seats: 1, CustomerEmail: "not-an-email"})
		status, _, body, err := a.doRaw(http.MethodPost, "/events/"+event.ID.String()+"/bookings",
			payload, "application/json", map[string]string{"Idempotency-Key": "v-email"})
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		resp := decodeError(t, body)
		require.Equal(t, "validation_error", resp.Error.Code)
		require.Equal(t, "customer_email", resp.Error.Field)
	})

	t.Run("unknown event", func(t *testing.T) {
		status, _, body, err := a.createBooking(uuid.New(), "v-unknown", 1)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, status, string(body))
		require.Equal(t, "event_not_found", decodeError(t, body).Error.Code)
	})

	t.Run("not enough seats", func(t *testing.T) {
		status, _, body, err := a.createBooking(event.ID, "v-too-many", 3)
		require.NoError(t, err)
		require.Equal(t, http.StatusConflict, status, string(body))
		resp := decodeError(t, body)
		require.Equal(t, "not_enough_seats", resp.Error.Code)
		require.EqualValues(t, 2, resp.Error.Details["available_seats"])
		require.EqualValues(t, 3, resp.Error.Details["requested_seats"])
	})

	t.Run("invalid booking id on cancel", func(t *testing.T) {
		status, _, body, err := a.doRaw(http.MethodDelete, "/bookings/nope", nil, "", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		require.Equal(t, "invalid_id", decodeError(t, body).Error.Code)
	})

	t.Run("unknown route", func(t *testing.T) {
		status, _, body := a.do(http.MethodGet, "/nope", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(body))
		require.Equal(t, "not_found", decodeError(t, body).Error.Code)
	})

	// Nothing was booked and no seats were consumed by invalid requests.
	reserved, available := a.availability(event.ID)
	require.Equal(t, 0, reserved)
	require.Equal(t, 2, available)
	require.Empty(t, a.listBookings(event.ID))
}

func TestBookingIdempotency(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)
	event := a.createEvent("Idempotency", 10)

	status, header, body, err := a.createBooking(event.ID, "key-1", 2)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status, string(body))
	require.Empty(t, header.Get("Idempotent-Replayed"))
	var first model.Booking
	require.NoError(t, json.Unmarshal(body, &first))

	status, header, body, err = a.createBooking(event.ID, "key-1", 2)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Equal(t, "true", header.Get("Idempotent-Replayed"))
	var replay model.Booking
	require.NoError(t, json.Unmarshal(body, &replay))
	require.Equal(t, first.ID, replay.ID)

	reserved, available := a.availability(event.ID)
	require.Equal(t, 2, reserved)
	require.Equal(t, 8, available)
	require.Len(t, a.listBookings(event.ID), 1)

	// Same key, different payload is a client error, never a silent duplicate.
	status, _, body, err = a.createBooking(event.ID, "key-1", 3)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, status, string(body))
	require.Equal(t, "idempotency_key_conflict", decodeError(t, body).Error.Code)

	payload := mustJSON(t, bookingRequest{Seats: 2, CustomerName: "Bob", CustomerEmail: "bob@example.com"})
	status, _, body, err = a.doRaw(http.MethodPost, "/events/"+event.ID.String()+"/bookings",
		payload, "application/json", map[string]string{"Idempotency-Key": "key-1"})
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, status, string(body))
	require.Equal(t, "idempotency_key_conflict", decodeError(t, body).Error.Code)

	reserved, available = a.availability(event.ID)
	require.Equal(t, 2, reserved)
	require.Equal(t, 8, available)

	// A different key creates a new booking.
	second := a.mustCreateBooking(event.ID, "key-2", 2)
	require.NotEqual(t, first.ID, second.ID)
	reserved, available = a.availability(event.ID)
	require.Equal(t, 4, reserved)
	require.Equal(t, 6, available)
	require.Len(t, a.listBookings(event.ID), 2)

	// Keys are scoped per event, so the same key on another event is fine.
	otherEvent := a.createEvent("Other event", 5)
	otherBooking := a.mustCreateBooking(otherEvent.ID, "key-1", 1)
	require.NotEqual(t, first.ID, otherBooking.ID)
}

func TestConcurrentBookingsAcrossTwoInstances(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)
	b := newInstanceWithPool(t, schema.NewPool(t))

	const (
		capacity = 10
		attempts = 100
	)
	event := a.createEvent("Concurrency", capacity)

	statuses := make([]int, attempts)
	bodies := make([][]byte, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		target := a
		if i%2 == 1 {
			target = b
		}
		wg.Add(1)
		go func(i int, target *instance) {
			defer wg.Done()
			statuses[i], _, bodies[i], errs[i] = target.createBooking(
				event.ID, fmt.Sprintf("concurrent-%d", i), 1)
		}(i, target)
	}
	wg.Wait()

	successes := 0
	conflicts := 0
	var bookingIDs []uuid.UUID
	for i := range statuses {
		require.NoError(t, errs[i])
		switch statuses[i] {
		case http.StatusCreated:
			successes++
			var booking model.Booking
			require.NoError(t, json.Unmarshal(bodies[i], &booking))
			bookingIDs = append(bookingIDs, booking.ID)
		case http.StatusConflict:
			conflicts++
			require.Equal(t, "not_enough_seats", decodeError(t, bodies[i]).Error.Code)
		default:
			t.Fatalf("unexpected status %d: %s", statuses[i], string(bodies[i]))
		}
	}
	require.Equal(t, capacity, successes)
	require.Equal(t, attempts-capacity, conflicts)
	require.Len(t, bookingIDs, capacity)

	// Read through the other instance: both see the same committed state.
	reserved, available := b.availability(event.ID)
	require.Equal(t, capacity, reserved)
	require.Equal(t, 0, available)
	require.Equal(t, capacity, a.reservedInDB(event.ID))

	bookings := a.listBookings(event.ID)
	require.Len(t, bookings, capacity)
	totalSeats := 0
	for _, booking := range bookings {
		require.Equal(t, model.StatusConfirmed, booking.Status)
		totalSeats += booking.Seats
	}
	require.Equal(t, capacity, totalSeats)

	// Cancel every booking several times concurrently through both instances.
	const cancelsPerBooking = 4
	cancelStatuses := make([]int, len(bookingIDs)*cancelsPerBooking)
	cancelErrs := make([]error, len(bookingIDs)*cancelsPerBooking)
	var cancelWG sync.WaitGroup
	for idx, bookingID := range bookingIDs {
		for j := 0; j < cancelsPerBooking; j++ {
			pos := idx*cancelsPerBooking + j
			target := a
			if pos%2 == 1 {
				target = b
			}
			cancelWG.Add(1)
			go func(pos int, target *instance, bookingID uuid.UUID) {
				defer cancelWG.Done()
				cancelStatuses[pos], _, cancelErrs[pos] = target.cancelBooking(bookingID)
			}(pos, target, bookingID)
		}
	}
	cancelWG.Wait()

	for i := range cancelStatuses {
		require.NoError(t, cancelErrs[i])
		require.Equal(t, http.StatusOK, cancelStatuses[i])
	}

	reserved, available = b.availability(event.ID)
	require.Equal(t, 0, reserved)
	require.Equal(t, capacity, available)
	require.Equal(t, 0, b.reservedInDB(event.ID))

	// A second wave of cancellations must not release seats again.
	for _, bookingID := range bookingIDs {
		status, body, err := a.cancelBooking(bookingID)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status, string(body))
	}
	reserved, available = a.availability(event.ID)
	require.Equal(t, 0, reserved)
	require.Equal(t, capacity, available)
}

func TestConcurrentMultiSeatBookings(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)
	b := newInstanceWithPool(t, schema.NewPool(t))

	const (
		capacity = 25
		attempts = 20
		seats    = 3
	)
	event := a.createEvent("Multi seat", capacity)

	statuses := make([]int, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		target := a
		if i%2 == 1 {
			target = b
		}
		wg.Add(1)
		go func(i int, target *instance) {
			defer wg.Done()
			statuses[i], _, _, errs[i] = target.createBooking(
				event.ID, fmt.Sprintf("multi-%d", i), seats)
		}(i, target)
	}
	wg.Wait()

	successes := 0
	for i := range statuses {
		require.NoError(t, errs[i])
		switch statuses[i] {
		case http.StatusCreated:
			successes++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected status %d", statuses[i])
		}
	}
	// 8 * 3 = 24 seats fit into 25; a 9th request would exceed capacity.
	require.Equal(t, 8, successes)

	reserved, available := b.availability(event.ID)
	require.Equal(t, 24, reserved)
	require.Equal(t, 1, available)
	require.Equal(t, 24, a.reservedInDB(event.ID))

	total := 0
	for _, booking := range a.listBookings(event.ID) {
		require.Equal(t, model.StatusConfirmed, booking.Status)
		total += booking.Seats
	}
	require.Equal(t, 24, total)
}

func TestConcurrentIdempotentCreate(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)
	b := newInstanceWithPool(t, schema.NewPool(t))

	event := a.createEvent("Duplicate requests", 10)

	const attempts = 30
	statuses := make([]int, attempts)
	bodies := make([][]byte, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		target := a
		if i%2 == 1 {
			target = b
		}
		wg.Add(1)
		go func(i int, target *instance) {
			defer wg.Done()
			statuses[i], _, bodies[i], errs[i] = target.createBooking(event.ID, "same-key", 3)
		}(i, target)
	}
	wg.Wait()

	created := 0
	var bookingIDs []uuid.UUID
	for i := range statuses {
		require.NoError(t, errs[i])
		switch statuses[i] {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("unexpected status %d: %s", statuses[i], string(bodies[i]))
		}
		var booking model.Booking
		require.NoError(t, json.Unmarshal(bodies[i], &booking))
		bookingIDs = append(bookingIDs, booking.ID)
	}
	require.Equal(t, 1, created, "exactly one request creates the booking")
	for _, id := range bookingIDs {
		require.Equal(t, bookingIDs[0], id, "all replays return the same booking")
	}

	reserved, available := b.availability(event.ID)
	require.Equal(t, 3, reserved)
	require.Equal(t, 7, available)
	require.Len(t, a.listBookings(event.ID), 1)

	status, _, body, err := a.createBooking(event.ID, "same-key", 4)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, status, string(body))
	require.Equal(t, "idempotency_key_conflict", decodeError(t, body).Error.Code)
}

func TestConcurrentCancellation(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)
	b := newInstanceWithPool(t, schema.NewPool(t))

	event := a.createEvent("Cancellation", 5)
	booking := a.mustCreateBooking(event.ID, "cancel-key", 3)
	reserved, _ := a.availability(event.ID)
	require.Equal(t, 3, reserved)

	const attempts = 40
	statuses := make([]int, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		target := a
		if i%2 == 1 {
			target = b
		}
		wg.Add(1)
		go func(i int, target *instance) {
			defer wg.Done()
			statuses[i], _, errs[i] = target.cancelBooking(booking.ID)
		}(i, target)
	}
	wg.Wait()

	for i := range statuses {
		require.NoError(t, errs[i])
		require.Equal(t, http.StatusOK, statuses[i])
	}

	reserved, available := b.availability(event.ID)
	require.Equal(t, 0, reserved)
	require.Equal(t, 5, available)
	require.Equal(t, 0, a.reservedInDB(event.ID))

	// A stale cancellation after re-booking must not free the new seats.
	newBooking := b.mustCreateBooking(event.ID, "cancel-key-2", 2)
	reserved, available = a.availability(event.ID)
	require.Equal(t, 2, reserved)
	require.Equal(t, 3, available)

	status, body, err := a.cancelBooking(booking.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, string(body))
	reserved, available = b.availability(event.ID)
	require.Equal(t, 2, reserved)
	require.Equal(t, 3, available)

	status, body, err = b.cancelBooking(newBooking.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, string(body))
	reserved, available = a.availability(event.ID)
	require.Equal(t, 0, reserved)
	require.Equal(t, 5, available)
	require.Equal(t, 0, b.reservedInDB(event.ID))
}

func TestDataSurvivesInstanceRestart(t *testing.T) {
	schema := testDatabase.NewSchema(t)

	first := newInstance(t, schema)
	event := first.createEvent("Persistent event", 4)
	booking := first.mustCreateBooking(event.ID, "persist-key", 1)

	first.server.Close()
	first.pool.Close()

	second := newInstanceWithPool(t, schema.NewPool(t))
	status, _, body := second.do(http.MethodGet, "/events/"+event.ID.String(), nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var fetched model.Event
	require.NoError(t, json.Unmarshal(body, &fetched))
	require.Equal(t, event.ID, fetched.ID)

	status, _, body = second.do(http.MethodGet, "/bookings/"+booking.ID.String(), nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var fetchedBooking model.Booking
	require.NoError(t, json.Unmarshal(body, &fetchedBooking))
	require.Equal(t, booking.ID, fetchedBooking.ID)

	reserved, available := second.availability(event.ID)
	require.Equal(t, 1, reserved)
	require.Equal(t, 3, available)
	require.Len(t, second.listBookings(event.ID), 1)
}

func TestMigrationsAreIdempotentAndPreserveData(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)
	event := a.createEvent("Migrated event", 3)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 0; i < 2; i++ {
		require.NoError(t, migrate.Run(context.Background(), schema.Pool(), logger))
	}

	status, _, body := a.do(http.MethodGet, "/events/"+event.ID.String(), nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	var count int
	require.NoError(t, schema.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM schema_migrations WHERE version = '0001_init.sql'").Scan(&count))
	require.Equal(t, 1, count)
}

func TestHealthEndpoints(t *testing.T) {
	schema := testDatabase.NewSchema(t)
	a := newInstance(t, schema)

	status, _, body := a.do(http.MethodGet, "/healthz", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	status, _, body = a.do(http.MethodGet, "/readyz", nil, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var payload map[string]string
	require.NoError(t, json.Unmarshal(body, &payload))
	require.Equal(t, "ready", payload["status"])
	require.Equal(t, "up", payload["database"])
}
