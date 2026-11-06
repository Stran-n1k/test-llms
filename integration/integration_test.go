// Package integration contains end-to-end tests that run the full HTTP
// stack against a real PostgreSQL database.
//
// The tests are skipped unless BOOKING_TEST_DATABASE_URL is set, e.g.:
//
//	BOOKING_TEST_DATABASE_URL=postgres://booking:booking@localhost:55432/booking_test?sslmode=disable go test -race ./...
//
// scripts/integration.sh starts a disposable PostgreSQL container and runs
// them with the race detector.
package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"booking/internal/httpapi"
	"booking/internal/service"
	"booking/internal/store"
)

var dsn = os.Getenv("BOOKING_TEST_DATABASE_URL")

// --- stack helpers ---

type stack struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.PostgresStore
}

func setup(t *testing.T) *stack {
	t.Helper()
	if dsn == "" {
		t.Skip("BOOKING_TEST_DATABASE_URL is not set; skipping integration test")
	}
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
	})
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := service.New(st)
	srv := httptest.NewServer(httpapi.NewRouter(svc))
	t.Cleanup(srv.Close)
	return &stack{t: t, srv: srv, st: st}
}

// twoStacks simulates two service instances sharing one database.
func twoStacks(t *testing.T) (*stack, *stack) {
	t.Helper()
	return setup(t), setup(t)
}

// --- HTTP helpers ---

type apiEvent struct {
	ID                   int64     `json:"id"`
	Name                 string    `json:"name"`
	Capacity             int       `json:"capacity"`
	CreatedAt            time.Time `json:"created_at"`
	BookedSeats          int       `json:"booked_seats"`
	AvailableSeats       int       `json:"available_seats"`
	AvailableSeatNumbers []int     `json:"available_seat_numbers"`
}

type apiBooking struct {
	ID          int64      `json:"id"`
	EventID     int64      `json:"event_id"`
	Seats       []int      `json:"seats"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	CancelledAt *time.Time `json:"cancelled_at"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func (s *stack) do(method, path string, body any, headers map[string]string) (int, []byte) {
	s.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.srv.URL+path, reader)
	if err != nil {
		s.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, raw
}

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	return v
}

func (s *stack) createEvent(t *testing.T, name string, capacity int) apiEvent {
	t.Helper()
	status, raw := s.do("POST", "/events", map[string]any{"name": name, "capacity": capacity}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create event: status %d, body %s", status, raw)
	}
	return decode[apiEvent](t, raw)
}

func (s *stack) createBooking(t *testing.T, eventID int64, token string, seats []int) (int, apiBooking, apiError) {
	t.Helper()
	status, raw := s.do("POST", "/bookings", map[string]any{
		"event_id":        eventID,
		"seats":           seats,
		"idempotency_key": token,
	}, nil)
	if status >= 400 {
		return status, apiBooking{}, decode[apiError](t, raw)
	}
	return status, decode[apiBooking](t, raw), apiError{}
}

// --- DB invariant helpers ---

func activeSeatCount(t *testing.T, db *sql.DB, eventID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`
		SELECT count(*) FROM booking_seats WHERE event_id = $1 AND active`, eventID).Scan(&n); err != nil {
		t.Fatalf("count active seats: %v", err)
	}
	return n
}

// duplicateActiveSeats returns seats held by more than one active booking.
func duplicateActiveSeats(t *testing.T, db *sql.DB, eventID int64) []int {
	t.Helper()
	rows, err := db.Query(`
		SELECT seat_no FROM booking_seats
		WHERE event_id = $1 AND active
		GROUP BY seat_no HAVING count(*) > 1`, eventID)
	if err != nil {
		t.Fatalf("check duplicates: %v", err)
	}
	defer rows.Close()
	var seats []int
	for rows.Next() {
		var seat int
		if err := rows.Scan(&seat); err != nil {
			t.Fatalf("check duplicates: %v", err)
		}
		seats = append(seats, seat)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("check duplicates: %v", err)
	}
	return seats
}

// bookingSeatsActive returns how many of the booking's seats are active.
func bookingSeatsActive(t *testing.T, db *sql.DB, bookingID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`
		SELECT count(*) FROM booking_seats WHERE booking_id = $1 AND active`, bookingID).Scan(&n); err != nil {
		t.Fatalf("count booking seats: %v", err)
	}
	return n
}

func runConcurrently(n int, fn func(i int)) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// --- tests ---

func TestEventsAndAvailability(t *testing.T) {
	s := setup(t)

	event := s.createEvent(t, "Concert", 5)
	if event.Capacity != 5 || event.AvailableSeats != 5 || event.BookedSeats != 0 {
		t.Fatalf("unexpected event: %+v", event)
	}

	status, raw := s.do("GET", fmt.Sprintf("/events/%d", event.ID), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("get event: status %d, body %s", status, raw)
	}
	detail := decode[apiEvent](t, raw)
	if detail.BookedSeats != 0 || detail.AvailableSeats != 5 {
		t.Fatalf("unexpected availability: %+v", detail)
	}
	if len(detail.AvailableSeatNumbers) != 5 || detail.AvailableSeatNumbers[0] != 1 || detail.AvailableSeatNumbers[4] != 5 {
		t.Fatalf("unexpected free seats: %v", detail.AvailableSeatNumbers)
	}

	// Book two seats and check availability again.
	status, _, apiErr := s.createBooking(t, event.ID, "tok-avail", []int{1, 4})
	if status != http.StatusCreated {
		t.Fatalf("create booking: status %d, err %+v", status, apiErr)
	}
	_, raw = s.do("GET", fmt.Sprintf("/events/%d", event.ID), nil, nil)
	detail = decode[apiEvent](t, raw)
	if detail.BookedSeats != 2 || detail.AvailableSeats != 3 {
		t.Fatalf("unexpected availability after booking: %+v", detail)
	}
	want := []int{2, 3, 5}
	if fmt.Sprint(detail.AvailableSeatNumbers) != fmt.Sprint(want) {
		t.Fatalf("free seats = %v, want %v", detail.AvailableSeatNumbers, want)
	}

	// List contains the event.
	status, raw = s.do("GET", "/events", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list events: status %d", status)
	}
	events := decode[[]apiEvent](t, raw)
	found := false
	for _, e := range events {
		if e.ID == event.ID {
			found = true
			if e.BookedSeats != 2 || e.AvailableSeats != 3 {
				t.Fatalf("list availability mismatch: %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("created event not listed")
	}

	// Unknown entities are 404.
	if status, _ := s.do("GET", "/events/99999999", nil, nil); status != http.StatusNotFound {
		t.Fatalf("unknown event status = %d, want 404", status)
	}
	if status, _ := s.do("GET", "/bookings/99999999", nil, nil); status != http.StatusNotFound {
		t.Fatalf("unknown booking status = %d, want 404", status)
	}
}

func TestValidationErrors(t *testing.T) {
	s := setup(t)
	event := s.createEvent(t, "Conf", 5)

	cases := []struct {
		name    string
		method  string
		path    string
		body    any
		headers map[string]string
		want    int
		wantErr string
	}{
		{"empty name", "POST", "/events", map[string]any{"name": "", "capacity": 5}, nil, 400, "invalid_request"},
		{"zero capacity", "POST", "/events", map[string]any{"name": "x", "capacity": 0}, nil, 400, "invalid_request"},
		{"unknown field", "POST", "/events", map[string]any{"name": "x", "capacity": 5, "extra": 1}, nil, 400, "invalid_request"},
		{"bad path id", "GET", "/events/abc", nil, nil, 400, "invalid_request"},
		{"no seats", "POST", "/bookings", map[string]any{"event_id": event.ID, "seats": []int{}, "idempotency_key": "t"}, nil, 400, "invalid_request"},
		{"duplicate seats", "POST", "/bookings", map[string]any{"event_id": event.ID, "seats": []int{2, 2}, "idempotency_key": "t"}, nil, 400, "invalid_request"},
		{"zero seat", "POST", "/bookings", map[string]any{"event_id": event.ID, "seats": []int{0}, "idempotency_key": "t"}, nil, 400, "invalid_request"},
		{"no idempotency key", "POST", "/bookings", map[string]any{"event_id": event.ID, "seats": []int{1}}, nil, 400, "invalid_request"},
		{"seat above capacity", "POST", "/bookings", map[string]any{"event_id": event.ID, "seats": []int{6}, "idempotency_key": "t"}, nil, 400, "seat_out_of_range"},
		{"unknown event", "POST", "/bookings", map[string]any{"event_id": 99999999, "seats": []int{1}, "idempotency_key": "t"}, nil, 404, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := s.do(tc.method, tc.path, tc.body, tc.headers)
			if status != tc.want {
				t.Fatalf("status = %d, want %d, body %s", status, tc.want, raw)
			}
			apiErr := decode[apiError](t, raw)
			if apiErr.Error.Code != tc.wantErr {
				t.Fatalf("error code = %q, want %q (body %s)", apiErr.Error.Code, tc.wantErr, raw)
			}
		})
	}

	// Malformed JSON.
	req, _ := http.NewRequest("POST", s.srv.URL+"/events", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("malformed request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want 400", resp.StatusCode)
	}

	// Seat conflict returns 409 seat_taken.
	s.createBooking(t, event.ID, "tok-conflict-1", []int{2})
	status, _, apiErr := s.createBooking(t, event.ID, "tok-conflict-2", []int{2, 3})
	if status != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", status)
	}
	if apiErr.Error.Code != "seat_taken" {
		t.Fatalf("conflict code = %q, want seat_taken", apiErr.Error.Code)
	}
	// The failed booking must not have consumed seats.
	if n := activeSeatCount(t, s.st.DB(), event.ID); n != 1 {
		t.Fatalf("active seats = %d, want 1", n)
	}
}

func TestIdempotentCreateBooking(t *testing.T) {
	s := setup(t)
	event := s.createEvent(t, "Idem", 5)

	status, first, _ := s.createBooking(t, event.ID, "tok-repeat", []int{1, 2})
	if status != http.StatusCreated {
		t.Fatalf("first create status = %d", status)
	}

	// Exact repeat: 200 with the same booking.
	status, second, _ := s.createBooking(t, event.ID, "tok-repeat", []int{1, 2})
	if status != http.StatusOK {
		t.Fatalf("repeat status = %d, want 200", status)
	}
	if second.ID != first.ID || second.Status != "confirmed" {
		t.Fatalf("repeat returned %+v, want same booking as %+v", second, first)
	}

	// Repeat with different seats: still the same original booking, no new rows.
	status, third, _ := s.createBooking(t, event.ID, "tok-repeat", []int{3})
	if status != http.StatusOK || third.ID != first.ID {
		t.Fatalf("repeat with other seats: status %d, booking %+v", status, third)
	}
	if fmt.Sprint(third.Seats) != fmt.Sprint([]int{1, 2}) {
		t.Fatalf("repeat must return original seats, got %v", third.Seats)
	}

	// Only one booking exists for the token.
	var bookings int
	if err := s.st.DB().QueryRow(`
		SELECT count(*) FROM bookings WHERE event_id = $1 AND client_token = $2`,
		event.ID, "tok-repeat").Scan(&bookings); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if bookings != 1 {
		t.Fatalf("bookings for token = %d, want 1", bookings)
	}

	// Repeat after cancel returns the cancelled booking (documented behaviour).
	if status, raw := s.do("POST", fmt.Sprintf("/bookings/%d/cancel", first.ID), nil, nil); status != http.StatusOK {
		t.Fatalf("cancel status = %d, body %s", status, raw)
	}
	status, after, _ := s.createBooking(t, event.ID, "tok-repeat", []int{1, 2})
	if status != http.StatusOK || after.Status != "cancelled" || after.ID != first.ID {
		t.Fatalf("repeat after cancel: status %d, booking %+v", status, after)
	}
}

func TestCancelIsIdempotentAndReleasesOnce(t *testing.T) {
	s := setup(t)
	event := s.createEvent(t, "Cancel", 3)

	_, booking, _ := s.createBooking(t, event.ID, "tok-cancel", []int{1, 2, 3})

	status, raw := s.do("POST", fmt.Sprintf("/bookings/%d/cancel", booking.ID), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("cancel status = %d, body %s", status, raw)
	}
	cancelled := decode[apiBooking](t, raw)
	if cancelled.Status != "cancelled" || cancelled.CancelledAt == nil {
		t.Fatalf("unexpected cancelled booking: %+v", cancelled)
	}

	// Repeat cancel: same booking, same cancelled_at, seats released once.
	status, raw = s.do("POST", fmt.Sprintf("/bookings/%d/cancel", booking.ID), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("repeat cancel status = %d, want 200", status)
	}
	again := decode[apiBooking](t, raw)
	if again.ID != cancelled.ID || again.Status != "cancelled" {
		t.Fatalf("repeat cancel returned %+v", again)
	}
	if !again.CancelledAt.Equal(*cancelled.CancelledAt) {
		t.Fatalf("cancelled_at changed on repeat: %v vs %v", again.CancelledAt, cancelled.CancelledAt)
	}

	if n := activeSeatCount(t, s.st.DB(), event.ID); n != 0 {
		t.Fatalf("active seats after cancel = %d, want 0", n)
	}

	// Seats are free and can be rebooked.
	status, reb, _ := s.createBooking(t, event.ID, "tok-rebook", []int{1, 2, 3})
	if status != http.StatusCreated {
		t.Fatalf("rebook status = %d", status)
	}
	if n := bookingSeatsActive(t, s.st.DB(), reb.ID); n != 3 {
		t.Fatalf("rebooked seats active = %d, want 3", n)
	}

	// Cancel of unknown booking is 404.
	if status, _ := s.do("POST", "/bookings/99999999/cancel", nil, nil); status != http.StatusNotFound {
		t.Fatalf("cancel unknown status = %d, want 404", status)
	}
}

func TestConcurrentSameSeat(t *testing.T) {
	s := setup(t)
	event := s.createEvent(t, "Hot seat", 10)

	const workers = 24
	var mu sync.Mutex
	var successes, conflicts int
	runConcurrently(workers, func(i int) {
		status, booking, _ := s.createBooking(t, event.ID, fmt.Sprintf("tok-hot-%d", i), []int{7})
		mu.Lock()
		defer mu.Unlock()
		switch status {
		case http.StatusCreated:
			successes++
			if booking.Status != "confirmed" {
				t.Errorf("booking %d status %q", booking.ID, booking.Status)
			}
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d for worker %d", status, i)
		}
	})
	if successes != 1 || conflicts != workers-1 {
		t.Fatalf("successes = %d, conflicts = %d; want 1 and %d", successes, conflicts, workers-1)
	}
	if n := activeSeatCount(t, s.st.DB(), event.ID); n != 1 {
		t.Fatalf("active seats = %d, want 1", n)
	}
	if dups := duplicateActiveSeats(t, s.st.DB(), event.ID); len(dups) != 0 {
		t.Fatalf("duplicate active seats: %v", dups)
	}
}

func TestConcurrentNoOversell(t *testing.T) {
	s := setup(t)
	const capacity = 20
	event := s.createEvent(t, "Bulk", capacity)

	const workers = 60
	var mu sync.Mutex
	var confirmedSeats int
	runConcurrently(workers, func(i int) {
		rng := rand.New(rand.NewPCG(uint64(i), uint64(time.Now().UnixNano())))
		// Two distinct random seats per worker.
		a := rng.IntN(capacity) + 1
		b := rng.IntN(capacity) + 1
		for b == a {
			b = rng.IntN(capacity) + 1
		}
		status, _, _ := s.createBooking(t, event.ID, fmt.Sprintf("tok-bulk-%d", i), []int{a, b})
		mu.Lock()
		defer mu.Unlock()
		if status == http.StatusCreated {
			confirmedSeats += 2
		} else if status != http.StatusConflict {
			t.Errorf("worker %d: unexpected status %d", i, status)
		}
	})

	if n := activeSeatCount(t, s.st.DB(), event.ID); n > capacity {
		t.Fatalf("active seats = %d exceeds capacity %d — oversell!", n, capacity)
	}
	if dups := duplicateActiveSeats(t, s.st.DB(), event.ID); len(dups) != 0 {
		t.Fatalf("duplicate active seats: %v", dups)
	}
	// Every active seat belongs to exactly one confirmed booking.
	var active, seatRows int
	if err := s.st.DB().QueryRow(`
		SELECT
			(SELECT count(DISTINCT seat_no) FROM booking_seats WHERE event_id = $1 AND active),
			(SELECT count(*) FROM booking_seats WHERE event_id = $1 AND active)`,
		event.ID).Scan(&active, &seatRows); err != nil {
		t.Fatalf("invariant query: %v", err)
	}
	if active != seatRows {
		t.Fatalf("seat rows %d != distinct active seats %d", seatRows, active)
	}
}

func TestConcurrentIdempotentCreate(t *testing.T) {
	s := setup(t)
	event := s.createEvent(t, "Idem race", 10)

	const workers = 16
	ids := make(chan int64, workers)
	runConcurrently(workers, func(i int) {
		status, booking, apiErr := s.createBooking(t, event.ID, "tok-same", []int{1, 2})
		if status != http.StatusCreated && status != http.StatusOK {
			t.Errorf("worker %d: status %d, err %+v", i, status, apiErr)
			return
		}
		ids <- booking.ID
	})
	close(ids)
	var firstID int64
	count := 0
	for id := range ids {
		if firstID == 0 {
			firstID = id
		}
		if id != firstID {
			t.Fatalf("workers got different booking ids: %d vs %d", id, firstID)
		}
		count++
	}
	if count != workers {
		t.Fatalf("successful responses = %d, want %d", count, workers)
	}
	var bookings int
	if err := s.st.DB().QueryRow(`
		SELECT count(*) FROM bookings WHERE event_id = $1 AND client_token = 'tok-same'`,
		event.ID).Scan(&bookings); err != nil {
		t.Fatalf("count bookings: %v", err)
	}
	if bookings != 1 {
		t.Fatalf("bookings for token = %d, want 1", bookings)
	}
	if n := activeSeatCount(t, s.st.DB(), event.ID); n != 2 {
		t.Fatalf("active seats = %d, want 2", n)
	}
}

func TestConcurrentCancelReleasesExactlyOnce(t *testing.T) {
	s := setup(t)
	event := s.createEvent(t, "Cancel race", 5)

	_, booking, _ := s.createBooking(t, event.ID, "tok-cancel-race", []int{1, 2, 3})

	const workers = 16
	runConcurrently(workers, func(i int) {
		status, raw := s.do("POST", fmt.Sprintf("/bookings/%d/cancel", booking.ID), nil, nil)
		if status != http.StatusOK {
			t.Errorf("worker %d: cancel status %d, body %s", i, status, raw)
			return
		}
		got := decode[apiBooking](t, raw)
		if got.Status != "cancelled" || got.ID != booking.ID {
			t.Errorf("worker %d: unexpected booking %+v", i, got)
		}
	})

	if n := activeSeatCount(t, s.st.DB(), event.ID); n != 0 {
		t.Fatalf("active seats after concurrent cancels = %d, want 0", n)
	}

	// Seats released exactly once: rebooking all of them succeeds.
	if status, _, apiErr := s.createBooking(t, event.ID, "tok-after-cancel", []int{1, 2, 3}); status != http.StatusCreated {
		t.Fatalf("rebook after cancel: status %d, err %+v", status, apiErr)
	}
}

// TestCancelRebookRace races cancellation on one instance against booking
// the same seat on another instance. Whatever interleaving happens, a seat
// must never be held by two active bookings, and a cancelled booking must
// hold no active seats.
func TestCancelRebookRace(t *testing.T) {
	a, b := twoStacks(t)

	const iterations = 30
	for i := 0; i < iterations; i++ {
		event := a.createEvent(t, fmt.Sprintf("race-%d", i), 4)
		_, booking, _ := a.createBooking(t, event.ID, fmt.Sprintf("tok-race-%d", i), []int{1, 2, 3})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if status, raw := a.do("POST", fmt.Sprintf("/bookings/%d/cancel", booking.ID), nil, nil); status != http.StatusOK {
				t.Errorf("cancel: status %d, body %s", status, raw)
			}
		}()
		go func() {
			defer wg.Done()
			status, _, _ := b.createBooking(t, event.ID, fmt.Sprintf("tok-race-new-%d", i), []int{2})
			if status != http.StatusCreated && status != http.StatusConflict {
				t.Errorf("rebook: unexpected status %d", status)
			}
		}()
		wg.Wait()

		// Invariant: seat 2 is active at most once.
		if dups := duplicateActiveSeats(t, a.st.DB(), event.ID); len(dups) != 0 {
			t.Fatalf("iteration %d: duplicate active seats %v", i, dups)
		}
		// Invariant: booking status matches its active seats.
		var status string
		if err := a.st.DB().QueryRow(`SELECT status FROM bookings WHERE id = $1`, booking.ID).Scan(&status); err != nil {
			t.Fatalf("iteration %d: load booking: %v", i, err)
		}
		active := bookingSeatsActive(t, a.st.DB(), booking.ID)
		switch status {
		case "confirmed":
			if active != 3 {
				t.Fatalf("iteration %d: confirmed booking holds %d active seats, want 3", i, active)
			}
		case "cancelled":
			if active != 0 {
				t.Fatalf("iteration %d: cancelled booking holds %d active seats, want 0", i, active)
			}
		default:
			t.Fatalf("iteration %d: unexpected status %q", i, status)
		}
	}
}

// TestTwoInstancesSharedDB runs conflicting bookings against two instances
// with separate connection pools over one database.
func TestTwoInstancesSharedDB(t *testing.T) {
	a, b := twoStacks(t)
	const capacity = 30
	event := a.createEvent(t, "Shared", capacity)

	const workers = 40
	var mu sync.Mutex
	var successes int
	runConcurrently(workers, func(i int) {
		stack := a
		if i%2 == 1 {
			stack = b
		}
		rng := rand.New(rand.NewPCG(uint64(i), uint64(time.Now().UnixNano())))
		seats := make([]int, 0, 3)
		for len(seats) < 1+rng.IntN(3) {
			seat := rng.IntN(capacity) + 1
			dup := false
			for _, s := range seats {
				if s == seat {
					dup = true
					break
				}
			}
			if !dup {
				seats = append(seats, seat)
			}
		}
		status, _, apiErr := stack.createBooking(t, event.ID, fmt.Sprintf("tok-shared-%d", i), seats)
		mu.Lock()
		defer mu.Unlock()
		switch status {
		case http.StatusCreated:
			successes++
		case http.StatusConflict:
			// fine
		default:
			t.Errorf("worker %d: status %d, err %+v", i, status, apiErr)
		}
	})

	if successes == 0 {
		t.Fatalf("no bookings succeeded — suspicious")
	}
	if n := activeSeatCount(t, a.st.DB(), event.ID); n > capacity {
		t.Fatalf("active seats = %d exceeds capacity %d — oversell!", n, capacity)
	}
	if dups := duplicateActiveSeats(t, a.st.DB(), event.ID); len(dups) != 0 {
		t.Fatalf("duplicate active seats: %v", dups)
	}
	// Confirmed bookings keep their seats active; every active seat row
	// must belong to exactly one distinct seat (no duplicates).
	var activeRows, distinctSeats int
	if err := a.st.DB().QueryRow(`
		SELECT
			(SELECT count(*) FROM booking_seats WHERE event_id = $1 AND active),
			(SELECT count(DISTINCT seat_no) FROM booking_seats WHERE event_id = $1 AND active)`,
		event.ID).Scan(&activeRows, &distinctSeats); err != nil {
		t.Fatalf("invariant query: %v", err)
	}
	if activeRows != distinctSeats {
		t.Fatalf("active seat rows %d != distinct seats %d", activeRows, distinctSeats)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	first := setup(t)
	event := first.createEvent(t, "Restart", 6)
	_, booking, _ := first.createBooking(t, event.ID, "tok-restart", []int{1, 6})
	_, extra, _ := first.createBooking(t, event.ID, "tok-restart-2", []int{2})

	// Simulate a restart: close the pool and the server, then bring up a
	// "new process" against the same database.
	first.srv.Close()
	first.st.Close()

	second := setup(t)
	status, raw := second.do("GET", fmt.Sprintf("/bookings/%d", booking.ID), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("booking after restart: status %d, body %s", status, raw)
	}
	got := decode[apiBooking](t, raw)
	if got.ID != booking.ID || fmt.Sprint(got.Seats) != "[1 6]" || got.Status != "confirmed" {
		t.Fatalf("booking after restart: %+v", got)
	}

	status, raw = second.do("GET", fmt.Sprintf("/events/%d", event.ID), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("event after restart: status %d", status)
	}
	avail := decode[apiEvent](t, raw)
	if avail.BookedSeats != 3 || avail.AvailableSeats != 3 {
		t.Fatalf("availability after restart: %+v", avail)
	}
	_ = extra
}

func TestMigrationsIdempotentAndConcurrent(t *testing.T) {
	s := setup(t)
	event := s.createEvent(t, "Migrations", 3)
	if _, booking, _ := s.createBooking(t, event.ID, "tok-mig", []int{1}); booking.ID == 0 {
		t.Fatalf("booking failed")
	}

	// Re-running migrations on a populated DB must not fail or lose data.
	if err := s.st.Migrate(context.Background()); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
	if n := activeSeatCount(t, s.st.DB(), event.ID); n != 1 {
		t.Fatalf("active seats after re-migration = %d, want 1", n)
	}

	// Concurrent migration attempts from "several instances" must not fail.
	const instances = 4
	errCh := make(chan error, instances)
	var wg sync.WaitGroup
	wg.Add(instances)
	for i := 0; i < instances; i++ {
		go func() {
			defer wg.Done()
			st, err := store.Open(context.Background(), dsn)
			if err != nil {
				errCh <- err
				return
			}
			defer st.Close()
			if err := st.Migrate(context.Background()); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent migration: %v", err)
	}
	if n := activeSeatCount(t, s.st.DB(), event.ID); n != 1 {
		t.Fatalf("active seats after concurrent migrations = %d, want 1", n)
	}
}

func TestHealthz(t *testing.T) {
	s := setup(t)
	status, raw := s.do("GET", "/healthz", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("healthz status = %d, body %s", status, raw)
	}
}
