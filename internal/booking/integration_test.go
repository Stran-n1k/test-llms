package booking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type databaseFixture struct {
	schema string
	config *pgxpool.Config
	admin  *pgxpool.Pool
	pools  []*pgxpool.Pool
}

func newDatabaseFixture(t *testing.T) *databaseFixture {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	fixture := &databaseFixture{
		schema: "booking_test_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		admin:  admin,
	}
	identifier := pgx.Identifier{fixture.schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		for _, pool := range fixture.pools {
			pool.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA `+identifier+` CASCADE`); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
		admin.Close()
	})

	fixture.config, err = pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse test database config: %v", err)
	}
	fixture.config.ConnConfig.RuntimeParams["search_path"] = fixture.schema
	fixture.config.MaxConns = 25
	store := fixture.openStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	// Repeating startup migration must preserve the schema and its data.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("repeat test schema migration: %v", err)
	}
	return fixture
}

func (f *databaseFixture) openStore(t *testing.T) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	config := f.config.Copy()
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping store: %v", err)
	}
	f.pools = append(f.pools, pool)
	return NewStore(pool)
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func createTestEvent(t *testing.T, ctx context.Context, store *Store, capacity int) Event {
	t.Helper()
	event, err := store.CreateEvent(ctx, "Concurrency test", capacity)
	if err != nil {
		t.Fatalf("create event: %v", err)
	}
	return event
}

func TestMigrationAndPersistence(t *testing.T) {
	fixture := newDatabaseFixture(t)
	ctx := testContext(t)
	firstPool := fixture.pools[0]
	store := NewStore(firstPool)
	event := createTestEvent(t, ctx, store, 12)
	firstPool.Close()

	reopened := fixture.openStore(t)
	got, err := reopened.GetEvent(ctx, event.ID)
	if err != nil {
		t.Fatalf("get event after reopening pool: %v", err)
	}
	if got.ID != event.ID || got.Title != event.Title || got.Capacity != 12 || got.AvailableSeats != 12 {
		t.Fatalf("event did not survive reopening: got %+v, want %+v", got, event)
	}
}

func TestConcurrentBookingsNeverOversellAcrossInstances(t *testing.T) {
	fixture := newDatabaseFixture(t)
	ctx := testContext(t)
	stores := []*Store{fixture.openStore(t), fixture.openStore(t)}
	event := createTestEvent(t, ctx, stores[0], 11)

	const requests = 40
	var wg sync.WaitGroup
	results := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := stores[i%len(stores)].CreateBooking(ctx, event.ID, 1, fmt.Sprintf("capacity-%d", i))
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	succeeded, capacityErrors := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		var capacityErr *CapacityError
		if errors.As(err, &capacityErr) {
			capacityErrors++
		} else {
			t.Errorf("unexpected concurrent booking error: %v", err)
		}
	}
	if succeeded != event.Capacity || capacityErrors != requests-event.Capacity {
		t.Fatalf("success/capacity errors = %d/%d, want %d/%d", succeeded, capacityErrors, event.Capacity, requests-event.Capacity)
	}
	availability, err := stores[0].GetAvailability(ctx, event.ID)
	if err != nil {
		t.Fatalf("get availability: %v", err)
	}
	if availability.BookedSeats != event.Capacity || availability.AvailableSeats != 0 {
		t.Fatalf("invalid availability after concurrent bookings: %+v", availability)
	}
	var activeBookings int
	if err := fixture.pools[0].QueryRow(ctx, `SELECT count(*) FROM bookings WHERE event_id = $1 AND status = 'active'`, event.ID).Scan(&activeBookings); err != nil {
		t.Fatalf("count active bookings: %v", err)
	}
	if activeBookings != event.Capacity {
		t.Fatalf("active bookings = %d, want %d", activeBookings, event.Capacity)
	}
}

func TestConcurrentIdempotentCreateAcrossInstances(t *testing.T) {
	fixture := newDatabaseFixture(t)
	ctx := testContext(t)
	stores := []*Store{fixture.openStore(t), fixture.openStore(t)}
	event := createTestEvent(t, ctx, stores[0], 5)
	otherEvent := createTestEvent(t, ctx, stores[0], 5)

	const requests = 24
	var wg sync.WaitGroup
	type result struct {
		booking Booking
		created bool
		err     error
	}
	results := make(chan result, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			booking, created, err := stores[i%2].CreateBooking(ctx, event.ID, 3, "shared-request-key")
			results <- result{booking: booking, created: created, err: err}
		}(i)
	}
	wg.Wait()
	close(results)

	var bookingID uuid.UUID
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("idempotent create failed: %v", result.err)
			continue
		}
		if bookingID == uuid.Nil {
			bookingID = result.booking.ID
		}
		if result.booking.ID != bookingID {
			t.Errorf("retry returned booking %s, want %s", result.booking.ID, bookingID)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want exactly 1", createdCount)
	}
	availability, err := stores[0].GetAvailability(ctx, event.ID)
	if err != nil {
		t.Fatalf("get availability: %v", err)
	}
	if availability.BookedSeats != 3 || availability.AvailableSeats != 2 {
		t.Fatalf("idempotent retries changed availability: %+v", availability)
	}
	if _, _, err := stores[0].CreateBooking(ctx, event.ID, 2, "shared-request-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed seats error = %v, want ErrIdempotencyConflict", err)
	}
	if _, _, err := stores[0].CreateBooking(ctx, otherEvent.ID, 3, "shared-request-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed event error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestConcurrentCancellationReleasesSeatsExactlyOnce(t *testing.T) {
	fixture := newDatabaseFixture(t)
	ctx := testContext(t)
	stores := []*Store{fixture.openStore(t), fixture.openStore(t)}
	event := createTestEvent(t, ctx, stores[0], 10)
	booking, created, err := stores[0].CreateBooking(ctx, event.ID, 4, "cancel-me")
	if err != nil || !created {
		t.Fatalf("create booking: created=%v err=%v", created, err)
	}

	const requests = 32
	var wg sync.WaitGroup
	results := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cancelled, err := stores[i%2].CancelBooking(ctx, booking.ID)
			if err == nil && cancelled.Status != BookingCancelled {
				err = fmt.Errorf("status = %q, want cancelled", cancelled.Status)
			}
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent cancellation failed: %v", err)
		}
	}
	availability, err := stores[0].GetAvailability(ctx, event.ID)
	if err != nil {
		t.Fatalf("get availability: %v", err)
	}
	if availability.BookedSeats != 0 || availability.AvailableSeats != event.Capacity {
		t.Fatalf("seats were not restored exactly once: %+v", availability)
	}

	first, err := stores[0].CancelBooking(ctx, booking.ID)
	if err != nil {
		t.Fatalf("repeat cancellation: %v", err)
	}
	second, err := stores[1].CancelBooking(ctx, booking.ID)
	if err != nil {
		t.Fatalf("second repeat cancellation: %v", err)
	}
	if first.CancelledAt == nil || second.CancelledAt == nil || !first.CancelledAt.Equal(*second.CancelledAt) {
		t.Fatalf("repeated cancellation changed cancelled_at: %v vs %v", first.CancelledAt, second.CancelledAt)
	}
	if _, created, err := stores[1].CreateBooking(ctx, event.ID, event.Capacity, "all-seats-after-cancel"); err != nil || !created {
		t.Fatalf("book all restored seats: created=%v err=%v", created, err)
	}
}

func TestHTTPAPIEndToEndAndValidation(t *testing.T) {
	fixture := newDatabaseFixture(t)
	store := fixture.openStore(t)
	handler := NewHandler(store)

	response := apiRequest(t, handler, http.MethodPost, "/events", `{"title":"  Concert  ","capacity":10}`, http.StatusCreated)
	var event Event
	decodeResponse(t, response, &event)
	if event.Title != "Concert" || event.AvailableSeats != 10 {
		t.Fatalf("unexpected created event: %+v", event)
	}
	eventPath := "/events/" + event.ID.String()

	response = apiRequest(t, handler, http.MethodPost, "/bookings", fmt.Sprintf(`{"event_id":%q,"seats":4,"idempotency_key":"http-key"}`, event.ID), http.StatusCreated)
	var booking Booking
	decodeResponse(t, response, &booking)
	if booking.Status != BookingActive || booking.Seats != 4 {
		t.Fatalf("unexpected booking: %+v", booking)
	}

	response = apiRequest(t, handler, http.MethodPost, "/bookings", fmt.Sprintf(`{"event_id":%q,"seats":4,"idempotency_key":"http-key"}`, event.ID), http.StatusOK)
	var replay Booking
	decodeResponse(t, response, &replay)
	if replay.ID != booking.ID {
		t.Fatalf("HTTP retry created another booking: %s != %s", replay.ID, booking.ID)
	}
	apiRequest(t, handler, http.MethodPost, "/bookings", fmt.Sprintf(`{"event_id":%q,"seats":3,"idempotency_key":"http-key"}`, event.ID), http.StatusConflict)
	apiRequest(t, handler, http.MethodPost, "/bookings", fmt.Sprintf(`{"event_id":%q,"seats":7,"idempotency_key":"too-many"}`, event.ID), http.StatusConflict)

	response = apiRequest(t, handler, http.MethodGet, eventPath+"/availability", "", http.StatusOK)
	var availability Availability
	decodeResponse(t, response, &availability)
	if availability.BookedSeats != 4 || availability.AvailableSeats != 6 {
		t.Fatalf("unexpected availability: %+v", availability)
	}

	bookingPath := "/bookings/" + booking.ID.String()
	response = apiRequest(t, handler, http.MethodDelete, bookingPath, "", http.StatusOK)
	var cancelled Booking
	decodeResponse(t, response, &cancelled)
	if cancelled.Status != BookingCancelled || cancelled.CancelledAt == nil {
		t.Fatalf("unexpected cancellation response: %+v", cancelled)
	}
	response = apiRequest(t, handler, http.MethodDelete, bookingPath, "", http.StatusOK)
	var repeatedCancellation Booking
	decodeResponse(t, response, &repeatedCancellation)
	if !repeatedCancellation.CancelledAt.Equal(*cancelled.CancelledAt) {
		t.Fatalf("repeated HTTP cancellation changed timestamp")
	}

	apiRequest(t, handler, http.MethodPost, "/events", `{"title":"","capacity":10}`, http.StatusBadRequest)
	apiRequest(t, handler, http.MethodPost, "/events", `{"title":"Concert","capacity":10,"unknown":true}`, http.StatusBadRequest)
	apiRequest(t, handler, http.MethodPost, "/events", `{"title":`, http.StatusBadRequest)
	apiRequest(t, handler, http.MethodGet, "/events/not-a-uuid", "", http.StatusBadRequest)
	apiRequest(t, handler, http.MethodGet, "/bookings/"+uuid.NewString(), "", http.StatusNotFound)
	apiRequest(t, handler, http.MethodPatch, bookingPath, `{}`, http.StatusMethodNotAllowed)
	apiRequest(t, handler, http.MethodGet, "/missing", "", http.StatusNotFound)

	response = apiRequest(t, handler, http.MethodGet, eventPath+"/availability", "", http.StatusOK)
	decodeResponse(t, response, &availability)
	if availability.BookedSeats != 0 || availability.AvailableSeats != 10 {
		t.Fatalf("invalid requests or retries changed data: %+v", availability)
	}
}

func apiRequest(t *testing.T, handler http.Handler, method, path, body string, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s: status = %d, want %d; body = %s", method, path, response.Code, wantStatus, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("%s %s: Content-Type = %q, want JSON", method, path, contentType)
	}
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, response.Body.String())
	}
}
