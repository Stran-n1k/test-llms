package api

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectedRequests(t *testing.T) {
	h := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const id = "69ecf5e3-8529-4fe6-b052-e8726751754e"
	tests := []struct {
		name, method, path, body, contentType, key string
		status                                     int
	}{
		{"unknown route", "GET", "/missing", "", "", "", 404},
		{"wrong method", "DELETE", "/events/" + id, "", "", "", 405},
		{"wrong collection method", "GET", "/events", "", "", "", 405},
		{"bad event id", "GET", "/events/nope", "", "", "", 400},
		{"bad booking id", "GET", "/bookings/nope", "", "", "", 400},
		{"bad cancel id", "POST", "/bookings/nope/cancel", "", "", "", 400},
		{"cancel payload", "POST", "/bookings/" + id + "/cancel", "{}", "application/json", "", 400},
		{"missing content type", "POST", "/events", `{}`, "", "", 415},
		{"wrong content type", "POST", "/events", `{}`, "text/plain", "", 415},
		{"broken JSON", "POST", "/events", `{"name":`, "application/json", "", 400},
		{"unknown field", "POST", "/events", `{"name":"A","capacity":2,"typo":1}`, "application/json", "", 400},
		{"two objects", "POST", "/events", `{} {}`, "application/json", "", 400},
		{"null", "POST", "/events", `null`, "application/json", "", 400},
		{"array", "POST", "/events", `[]`, "application/json", "", 400},
		{"empty name", "POST", "/events", `{"name":"  ","capacity":2}`, "application/json", "", 400},
		{"long name", "POST", "/events", `{"name":"` + strings.Repeat("я", 201) + `","capacity":2}`, "application/json", "", 400},
		{"NUL name", "POST", "/events", `{"name":"a\u0000b","capacity":2}`, "application/json", "", 400},
		{"bad UTF8", "POST", "/events", "{\"name\":\"\xff\",\"capacity\":2}", "application/json", "", 400},
		{"zero capacity", "POST", "/events", `{"name":"A","capacity":0}`, "application/json", "", 400},
		{"negative capacity", "POST", "/events", `{"name":"A","capacity":-2}`, "application/json", "", 400},
		{"large capacity", "POST", "/events", `{"name":"A","capacity":1000001}`, "application/json", "", 400},
		{"float capacity", "POST", "/events", `{"name":"A","capacity":1.5}`, "application/json", "", 400},
		{"overflow", "POST", "/events", `{"name":"A","capacity":999999999999999999999999}`, "application/json", "", 400},
		{"too big", "POST", "/events", strings.Repeat(" ", maxBody+1), "application/json", "", 413},
		{"missing key", "POST", "/bookings", `{}`, "application/json", "", 400},
		{"invalid key", "POST", "/bookings", `{}`, "application/json", "with spaces", 400},
		{"long key", "POST", "/bookings", `{}`, "application/json", strings.Repeat("k", 129), 400},
		{"invalid event id", "POST", "/bookings", `{"event_id":"oops","quantity":1}`, "application/json", "k", 400},
		{"zero quantity", "POST", "/bookings", `{"event_id":"` + id + `","quantity":0}`, "application/json", "k", 400},
		{"negative quantity", "POST", "/bookings", `{"event_id":"` + id + `","quantity":-1}`, "application/json", "k", 400},
		{"large quantity", "POST", "/bookings", `{"event_id":"` + id + `","quantity":1000001}`, "application/json", "k", 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.contentType != "" {
				r.Header.Set("Content-Type", tt.contentType)
			}
			if tt.key != "" {
				r.Header.Set("Idempotency-Key", tt.key)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status=%d want %d: %s", w.Code, tt.status, w.Body.String())
			}
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), `"error":{"code":`) {
				t.Fatalf("not a JSON error: %s", w.Body.String())
			}
		})
	}
}

func TestDuplicateIdempotencyHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/bookings", strings.NewReader(`{}`))
	r.Header.Add("Idempotency-Key", "one")
	r.Header.Add("Idempotency-Key", "two")
	w := httptest.NewRecorder()
	New(nil, slog.Default()).ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestParseID(t *testing.T) {
	valid := "69ecf5e3-8529-4fe6-b052-e8726751754e"
	if got, ok := parseID(strings.ToUpper(valid)); !ok || got != valid {
		t.Fatalf("normalization: %q %v", got, ok)
	}
	for _, id := range []string{"", strings.ReplaceAll(valid, "-", ""), valid + "a", "69ecf5e3-8529-4fe6-b052-e8726751754g", "69ecf5e3-8529-4fe6-b05--e8726751754"} {
		if _, ok := parseID(id); ok {
			t.Errorf("accepted %q", id)
		}
	}
}
