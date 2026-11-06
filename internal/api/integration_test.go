package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"booking/internal/api"
	"booking/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

type instance struct {
	server *httptest.Server
	store  *store.Store
}

func (i *instance) close() { i.server.Close(); i.store.Pool.Close() }

type fixture struct {
	config    *pgxpool.Config
	instances []*instance
}

// Every test gets a unique schema. Nothing outside that schema is modified.
func setup(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			t.Fatal("TEST_DATABASE_URL is required")
		}
		t.Skip("set TEST_DATABASE_URL or run scripts/test-integration.sh for real PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var random [12]byte
	_, _ = rand.Read(random[:])
	schema := "booking_test_" + hex.EncodeToString(random[:])
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	f := &fixture{}
	t.Cleanup(func() {
		for _, i := range f.instances {
			i.close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("cleanup schema: %v", err)
		}
		admin.Close()
	})
	f.config, err = pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	f.config.ConnConfig.RuntimeParams["search_path"] = schema
	f.config.MaxConns = 8
	// Separate pools emulate two application processes. Run startup migrations
	// concurrently to exercise their cross-process lock even on an empty schema.
	stores := make([]*store.Store, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for idx := range stores {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pool, err := pgxpool.NewWithConfig(ctx, f.config.Copy())
			if err != nil {
				errs[idx] = err
				return
			}
			stores[idx] = &store.Store{Pool: pool}
			errs[idx] = stores[idx].Migrate(ctx)
		}(idx)
	}
	wg.Wait()
	for idx, s := range stores {
		if s != nil {
			f.add(s)
		}
		if errs[idx] != nil {
			t.Fatalf("parallel migration: %v", errs[idx])
		}
	}
	return f
}
func (f *fixture) add(s *store.Store) *instance {
	i := &instance{store: s, server: httptest.NewServer(api.New(s, slog.New(slog.NewTextHandler(io.Discard, nil))))}
	f.instances = append(f.instances, i)
	return i
}
func (f *fixture) restart(t *testing.T) *instance {
	t.Helper()
	for _, i := range f.instances {
		i.close()
	}
	f.instances = nil
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, f.config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	s := &store.Store{Pool: pool}
	i := f.add(s)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return i
}

type reply struct {
	status int
	body   []byte
	header http.Header
	err    error
}

var client = &http.Client{Timeout: 20 * time.Second}

func request(base, method, path, key string, body any) reply {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return reply{err: err}
		}
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(data))
	if err != nil {
		return reply{err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return reply{err: err}
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, body: data, header: resp.Header, err: err}
}
func expect(t *testing.T, r reply, status int) {
	t.Helper()
	if r.err != nil || r.status != status {
		t.Fatalf("got status=%d err=%v body=%s; want %d", r.status, r.err, r.body, status)
	}
}
func decoded[T any](t *testing.T, r reply) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(r.body, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func event(t *testing.T, base string, capacity int) store.Event {
	t.Helper()
	r := request(base, "POST", "/events", "", map[string]any{"name": "  Концерт  ", "capacity": capacity})
	expect(t, r, 201)
	e := decoded[store.Event](t, r)
	if e.Name != "Концерт" || e.Capacity != capacity || e.Available != capacity || e.ID == "" || e.CreatedAt.IsZero() || r.header.Get("Location") != "/events/"+e.ID {
		t.Fatalf("bad event: %+v", e)
	}
	return e
}
func bookingBody(eventID string, quantity int) map[string]any {
	return map[string]any{"event_id": eventID, "quantity": quantity}
}
func available(t *testing.T, base, id string, want int) {
	t.Helper()
	r := request(base, "GET", "/events/"+id, "", nil)
	expect(t, r, 200)
	if e := decoded[store.Event](t, r); e.Available != want {
		t.Fatalf("availability=%d want %d", e.Available, want)
	}
}
func invariant(t *testing.T, s *store.Store, id string) {
	t.Helper()
	var capacity, avail, active int
	err := s.Pool.QueryRow(context.Background(), `SELECT e.capacity,e.available,COALESCE(SUM(b.quantity) FILTER (WHERE b.status='active'),0)
		FROM events e LEFT JOIN bookings b ON b.event_id=e.id WHERE e.id=$1 GROUP BY e.id`, id).Scan(&capacity, &avail, &active)
	if err != nil {
		t.Fatal(err)
	}
	if avail < 0 || avail+active != capacity {
		t.Fatalf("broken invariant: available=%d active=%d capacity=%d", avail, active, capacity)
	}
}
func concurrent(n int, fn func(int) reply) []reply {
	results := make([]reply, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for idx := range results {
		wg.Add(1)
		go func(idx int) { defer wg.Done(); <-start; results[idx] = fn(idx) }(idx)
	}
	close(start)
	wg.Wait()
	return results
}

func TestIntegrationLifecycleAndPersistence(t *testing.T) {
	f := setup(t)
	a, b := f.instances[0].server.URL, f.instances[1].server.URL
	expect(t, request(a, "GET", "/healthz", "", nil), 200)
	e := event(t, a, 5)
	r := request(a, "POST", "/bookings", "first", bookingBody(e.ID, 3))
	expect(t, r, 201)
	first := decoded[store.Booking](t, r)
	if first.Quantity != 3 || first.Status != "active" || first.EventID != e.ID || first.CancelledAt != nil || first.CreatedAt.IsZero() {
		t.Fatalf("bad booking: %+v", first)
	}
	if r.header.Get("Location") != "/bookings/"+first.ID {
		t.Fatal("missing booking location")
	}
	available(t, b, e.ID, 2)
	r = request(b, "POST", "/bookings", "first", bookingBody(strings.ToUpper(e.ID), 3))
	expect(t, r, 200)
	if got := decoded[store.Booking](t, r); got != first {
		t.Fatalf("replay changed booking: %+v", got)
	}
	expect(t, request(b, "POST", "/bookings", "first", bookingBody(e.ID, 2)), 409)
	other := event(t, b, 1)
	expect(t, request(b, "POST", "/bookings", "first", bookingBody(other.ID, 3)), 409)
	expect(t, request(b, "POST", "/bookings", "second", bookingBody(e.ID, 3)), 409)
	available(t, a, e.ID, 2)
	// Failed requests do not claim a key or leave a booking behind.
	r = request(b, "POST", "/bookings", "second", bookingBody(e.ID, 2))
	expect(t, r, 201)
	second := decoded[store.Booking](t, r)
	available(t, a, e.ID, 0)
	missing := "00000000-0000-0000-0000-000000000001"
	expect(t, request(a, "GET", "/events/"+missing, "", nil), 404)
	expect(t, request(a, "GET", "/bookings/"+missing, "", nil), 404)
	expect(t, request(a, "POST", "/bookings/"+missing+"/cancel", "", nil), 404)
	expect(t, request(a, "POST", "/bookings", "missing", bookingBody(missing, 1)), 404)
	for _, base := range []string{a, b} {
		r = request(base, "POST", "/bookings/"+first.ID+"/cancel", "", nil)
		expect(t, r, 200)
		if got := decoded[store.Booking](t, r); got.Status != "cancelled" || got.CancelledAt == nil {
			t.Fatalf("bad cancellation: %+v", got)
		}
	}
	available(t, a, e.ID, 3)
	r = request(a, "POST", "/bookings", "first", bookingBody(e.ID, 3))
	expect(t, r, 200)
	if got := decoded[store.Booking](t, r); got.ID != first.ID || got.Status != "cancelled" {
		t.Fatalf("recreated cancelled booking: %+v", got)
	}
	available(t, b, e.ID, 3)
	// Drop every pool/server, then reopen the same schema and rerun migration.
	restarted := f.restart(t)
	base := restarted.server.URL
	available(t, base, e.ID, 3)
	r = request(base, "GET", "/bookings/"+first.ID, "", nil)
	expect(t, r, 200)
	if got := decoded[store.Booking](t, r); got.Status != "cancelled" {
		t.Fatal("cancel state lost")
	}
	r = request(base, "GET", "/bookings/"+second.ID, "", nil)
	expect(t, r, 200)
	if got := decoded[store.Booking](t, r); got.Status != "active" {
		t.Fatal("active state lost")
	}
	r = request(base, "POST", "/bookings", "second", bookingBody(e.ID, 2))
	expect(t, r, 200)
	if got := decoded[store.Booking](t, r); got.ID != second.ID {
		t.Fatal("idempotency lost on restart")
	}
	invariant(t, restarted.store, e.ID)
}

func TestIntegrationConcurrentCapacity(t *testing.T) {
	f := setup(t)
	e := event(t, f.instances[0].server.URL, 37)
	results := concurrent(60, func(idx int) reply {
		return request(f.instances[idx%2].server.URL, "POST", "/bookings", fmt.Sprintf("capacity-%d", idx), bookingBody(e.ID, 2))
	})
	created := 0
	for _, r := range results {
		if r.status == 201 {
			created++
		} else {
			expect(t, r, 409)
			if !bytes.Contains(r.body, []byte("insufficient_capacity")) {
				t.Fatalf("wrong conflict: %s", r.body)
			}
		}
		if r.err != nil {
			t.Fatal(r.err)
		}
	}
	if created != 18 {
		t.Fatalf("created=%d want 18", created)
	}
	available(t, f.instances[0].server.URL, e.ID, 1)
	invariant(t, f.instances[0].store, e.ID)
	var count int
	if err := f.instances[0].store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM bookings`).Scan(&count); err != nil || count != 18 {
		t.Fatalf("failed requests leaked rows: count=%d err=%v", count, err)
	}
}

func TestIntegrationConcurrentIdempotency(t *testing.T) {
	f := setup(t)
	e := event(t, f.instances[0].server.URL, 30)
	results := concurrent(50, func(idx int) reply {
		return request(f.instances[idx%2].server.URL, "POST", "/bookings", "same-key", bookingBody(e.ID, 4))
	})
	created := 0
	id := ""
	for _, r := range results {
		if r.status == 201 {
			created++
		} else {
			expect(t, r, 200)
		}
		got := decoded[store.Booking](t, r)
		if id == "" {
			id = got.ID
		}
		if got.ID != id {
			t.Fatal("duplicate booking for same key")
		}
	}
	if created != 1 {
		t.Fatalf("created=%d want 1", created)
	}
	available(t, f.instances[1].server.URL, e.ID, 26)
	invariant(t, f.instances[0].store, e.ID)
	// Same key racing with different payloads: only the winning payload can replay.
	results = concurrent(40, func(idx int) reply {
		return request(f.instances[idx%2].server.URL, "POST", "/bookings", "conflicting-key", bookingBody(e.ID, idx%2+1))
	})
	created = 0
	winner := 0
	for _, r := range results {
		if r.status == 201 {
			created++
			winner = decoded[store.Booking](t, r).Quantity
		}
	}
	if created != 1 {
		t.Fatalf("conflicting requests created=%d", created)
	}
	for idx, r := range results {
		if idx%2+1 == winner {
			if r.status != 201 {
				expect(t, r, 200)
			}
		} else {
			expect(t, r, 409)
			if !bytes.Contains(r.body, []byte("idempotency_conflict")) {
				t.Fatalf("wrong error: %s", r.body)
			}
		}
	}
	available(t, f.instances[1].server.URL, e.ID, 26-winner)
	invariant(t, f.instances[0].store, e.ID)
}

func TestIntegrationConcurrentCancellation(t *testing.T) {
	f := setup(t)
	base := f.instances[0].server.URL
	e := event(t, base, 10)
	r := request(base, "POST", "/bookings", "cancel-me", bookingBody(e.ID, 7))
	expect(t, r, 201)
	b := decoded[store.Booking](t, r)
	results := concurrent(50, func(idx int) reply {
		return request(f.instances[idx%2].server.URL, "POST", "/bookings/"+b.ID+"/cancel", "", nil)
	})
	var cancelledAt time.Time
	for _, r := range results {
		expect(t, r, 200)
		got := decoded[store.Booking](t, r)
		if got.Status != "cancelled" || got.CancelledAt == nil {
			t.Fatalf("not cancelled: %+v", got)
		}
		if cancelledAt.IsZero() {
			cancelledAt = *got.CancelledAt
		}
		if !cancelledAt.Equal(*got.CancelledAt) {
			t.Fatal("cancellation timestamp changed")
		}
	}
	available(t, base, e.ID, 10)
	// All seats can be sold again, but no extra seats were introduced.
	r = request(base, "POST", "/bookings", "refill", bookingBody(e.ID, 10))
	expect(t, r, 201)
	expect(t, request(base, "POST", "/bookings", "extra", bookingBody(e.ID, 1)), 409)
	available(t, base, e.ID, 0)
	invariant(t, f.instances[0].store, e.ID)
}

func TestIntegrationMixedBookingAndCancellation(t *testing.T) {
	f := setup(t)
	base := f.instances[0].server.URL
	e := event(t, base, 20)
	r := request(base, "POST", "/bookings", "original", bookingBody(e.ID, 20))
	expect(t, r, 201)
	original := decoded[store.Booking](t, r)
	results := concurrent(80, func(idx int) reply {
		base := f.instances[idx%2].server.URL
		switch idx % 4 {
		case 0:
			return request(base, "POST", "/bookings/"+original.ID+"/cancel", "", nil)
		case 1:
			return request(base, "POST", "/bookings", "original", bookingBody(e.ID, 20))
		default:
			return request(base, "POST", "/bookings", fmt.Sprintf("mixed-%d", idx), bookingBody(e.ID, 2))
		}
	})
	for idx, r := range results {
		if idx%4 < 2 {
			expect(t, r, 200)
		} else if r.status != 201 {
			expect(t, r, 409)
		}
	}
	invariant(t, f.instances[0].store, e.ID)
}

func TestIntegrationDatabaseConstraints(t *testing.T) {
	f := setup(t)
	e := event(t, f.instances[0].server.URL, 2)
	pool := f.instances[0].store.Pool
	for _, query := range []string{
		`UPDATE events SET available=-1 WHERE id=$1`,
		`UPDATE events SET available=capacity+1 WHERE id=$1`,
		`UPDATE events SET capacity=0 WHERE id=$1`,
		`INSERT INTO bookings(id,event_id,quantity,idempotency_key) VALUES ('00000000-0000-0000-0000-000000000002',$1,0,'bad-quantity')`,
		`INSERT INTO bookings(id,event_id,quantity,idempotency_key,status) VALUES ('00000000-0000-0000-0000-000000000003',$1,1,'bad-status','cancelled')`,
	} {
		if _, err := pool.Exec(context.Background(), query, e.ID); err == nil {
			t.Fatalf("database accepted invalid data: %s", query)
		}
	}
	available(t, f.instances[0].server.URL, e.ID, 2)
	invariant(t, f.instances[0].store, e.ID)
}
