package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"booking/internal/api"
	"booking/internal/model"
	"booking/internal/store"
	"booking/internal/testutil"
)

var (
	testDatabase *testutil.Database
	testClient   = &http.Client{Timeout: 30 * time.Second}
)

func TestMain(m *testing.M) {
	db, cleanup, err := testutil.Start(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "start test database:", err)
		os.Exit(1)
	}
	testDatabase = db
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// instance is one running HTTP server (one "service instance") sharing a schema.
type instance struct {
	t      *testing.T
	server *httptest.Server
	pool   *pgxpool.Pool
}

func newInstance(t *testing.T, schema *testutil.Schema) *instance {
	t.Helper()
	return newInstanceWithPool(t, schema.Pool())
}

func newInstanceWithPool(t *testing.T, pool *pgxpool.Pool) *instance {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(api.New(store.New(pool), logger).Handler())
	t.Cleanup(server.Close)
	return &instance{t: t, server: server, pool: pool}
}

type eventRequest struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
	StartsAt string `json:"starts_at"`
}

type bookingRequest struct {
	Seats         int    `json:"seats"`
	CustomerName  string `json:"customer_name,omitempty"`
	CustomerEmail string `json:"customer_email,omitempty"`
}

type bookingsList struct {
	Items []model.Booking `json:"items"`
}

type eventsList struct {
	Items []model.Event `json:"items"`
}

type errorResponse struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Field   string         `json:"field"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func (a *instance) doRaw(method, path string, body []byte, contentType string, headers map[string]string) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, a.server.URL+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

func (a *instance) do(method, path string, body any, headers map[string]string) (int, http.Header, []byte) {
	a.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(a.t, err)
	}
	status, header, respBody, err := a.doRaw(method, path, raw, "application/json", headers)
	require.NoError(a.t, err)
	return status, header, respBody
}

func (a *instance) createEvent(name string, capacity int) model.Event {
	a.t.Helper()
	status, header, body := a.do(http.MethodPost, "/events", eventRequest{
		Name:     name,
		Capacity: capacity,
		StartsAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}, nil)
	require.Equal(a.t, http.StatusCreated, status, string(body))
	require.Contains(a.t, header.Get("Location"), "/events/")

	var event model.Event
	require.NoError(a.t, json.Unmarshal(body, &event))
	require.NotEqual(a.t, uuid.Nil, event.ID)
	return event
}

func (a *instance) createBooking(eventID uuid.UUID, key string, seats int) (int, http.Header, []byte, error) {
	return a.doRaw(http.MethodPost, "/events/"+eventID.String()+"/bookings",
		mustJSON(a.t, bookingRequest{Seats: seats, CustomerName: "Alice", CustomerEmail: "alice@example.com"}),
		"application/json",
		map[string]string{"Idempotency-Key": key},
	)
}

func (a *instance) mustCreateBooking(eventID uuid.UUID, key string, seats int) model.Booking {
	a.t.Helper()
	status, _, body, err := a.createBooking(eventID, key, seats)
	require.NoError(a.t, err)
	require.Equal(a.t, http.StatusCreated, status, string(body))
	var booking model.Booking
	require.NoError(a.t, json.Unmarshal(body, &booking))
	return booking
}

func (a *instance) availability(eventID uuid.UUID) (reserved, available int) {
	a.t.Helper()
	status, _, body := a.do(http.MethodGet, "/events/"+eventID.String()+"/availability", nil, nil)
	require.Equal(a.t, http.StatusOK, status, string(body))
	var payload struct {
		ReservedSeats  int `json:"reserved_seats"`
		AvailableSeats int `json:"available_seats"`
	}
	require.NoError(a.t, json.Unmarshal(body, &payload))
	return payload.ReservedSeats, payload.AvailableSeats
}

func (a *instance) listBookings(eventID uuid.UUID) []model.Booking {
	a.t.Helper()
	status, _, body := a.do(http.MethodGet, "/events/"+eventID.String()+"/bookings", nil, nil)
	require.Equal(a.t, http.StatusOK, status, string(body))
	var list bookingsList
	require.NoError(a.t, json.Unmarshal(body, &list))
	return list.Items
}

func (a *instance) cancelBooking(bookingID uuid.UUID) (int, []byte, error) {
	status, _, body, err := a.doRaw(http.MethodDelete, "/bookings/"+bookingID.String(), nil, "", nil)
	return status, body, err
}

func (a *instance) reservedInDB(eventID uuid.UUID) int {
	a.t.Helper()
	var reserved int
	err := a.pool.QueryRow(context.Background(),
		"SELECT reserved_seats FROM events WHERE id = $1", eventID).Scan(&reserved)
	require.NoError(a.t, err)
	return reserved
}

func decodeError(t *testing.T, body []byte) errorResponse {
	t.Helper()
	var resp errorResponse
	require.NoError(t, json.Unmarshal(body, &resp), "body: %s", string(body))
	return resp
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}
