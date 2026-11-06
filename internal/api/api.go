package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"booking/internal/store"
)

const maxBody = 16 * 1024

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type API struct {
	store  *store.Store
	logger *slog.Logger
}
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func New(s *store.Store, logger *slog.Logger) http.Handler {
	a := &API{store: s, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events", a.createEvent)
	mux.HandleFunc("GET /events/{id}", a.getEvent)
	mux.HandleFunc("POST /bookings", a.createBooking)
	mux.HandleFunc("GET /bookings/{id}", a.getBooking)
	mux.HandleFunc("POST /bookings/{id}/cancel", a.cancelBooking)
	mux.HandleFunc("GET /healthz", a.health)
	// Keep routing errors JSON as well (ServeMux otherwise emits plain text).
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		_, pattern := mux.Handler(r)
		if pattern == "" {
			if allowed := allowedMethods(r.URL.Path); allowed != "" {
				w.Header().Set("Allow", allowed)
				writeError(w, 405, "method_not_allowed", "method is not allowed for this resource")
			} else {
				writeError(w, 404, "not_found", "route not found")
			}
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func allowedMethods(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if path == "/events" || path == "/bookings" {
		return "POST"
	}
	if path == "/healthz" {
		return "GET, HEAD"
	}
	if len(parts) == 2 && parts[1] != "" && (parts[0] == "events" || parts[0] == "bookings") {
		return "GET, HEAD"
	}
	if len(parts) == 3 && parts[0] == "bookings" && parts[1] != "" && parts[2] == "cancel" {
		return "POST"
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error apiError `json:"error"`
	}{apiError{code, message}})
}

func (a *API) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "not_found", "event or booking not found")
	case errors.Is(err, store.ErrSoldOut):
		writeError(w, 409, "insufficient_capacity", "not enough available seats")
	case errors.Is(err, store.ErrKeyConflict):
		writeError(w, 409, "idempotency_conflict", "Idempotency-Key was already used with a different request")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, 503, "timeout", "request timed out; retry with the same Idempotency-Key")
	default:
		a.logger.Error("request failed", "error", err)
		writeError(w, 500, "internal_error", "internal server error; retry booking creation with the same Idempotency-Key")
	}
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, 415, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	// Reject invalid UTF-8 instead of accepting encoding/json's replacement text.
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, 413, "body_too_large", "JSON body exceeds 16 KiB")
		} else {
			writeError(w, 400, "invalid_json", "could not read JSON body")
		}
		return false
	}
	if !utf8.Valid(data) {
		writeError(w, 400, "invalid_json", "JSON must contain valid UTF-8")
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		writeError(w, 400, "invalid_json", "expected a JSON object with only documented fields and valid value types")
		return false
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, 400, "invalid_json", "expected exactly one JSON object")
		return false
	}
	return true
}

func parseID(id string) (string, bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return "", false
	}
	compact := strings.ReplaceAll(id, "-", "")
	if len(compact) != 32 {
		return "", false
	}
	if _, err := hex.DecodeString(compact); err != nil {
		return "", false
	}
	return strings.ToLower(id), true
}
func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		writeError(w, 400, "invalid_id", "id must be a UUID in canonical hyphenated form")
	}
	return id, ok
}

func (a *API) createEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Capacity int    `json:"capacity"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if n := utf8.RuneCountInString(req.Name); n < 1 || n > 200 || strings.ContainsRune(req.Name, 0) {
		writeError(w, 400, "invalid_name", "name must contain 1 to 200 characters after trimming, without NUL")
		return
	}
	if req.Capacity < 1 || req.Capacity > 1000000 {
		writeError(w, 400, "invalid_capacity", "capacity must be an integer between 1 and 1000000")
		return
	}
	e, err := a.store.CreateEvent(r.Context(), req.Name, req.Capacity)
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Location", "/events/"+e.ID)
	writeJSON(w, 201, e)
}
func (a *API) getEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	e, err := a.store.GetEvent(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, e)
}
func (a *API) createBooking(w http.ResponseWriter, r *http.Request) {
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || !keyPattern.MatchString(keys[0]) {
		writeError(w, 400, "invalid_idempotency_key", "provide exactly one Idempotency-Key: 1 to 128 characters from A-Z, a-z, 0-9, dot, underscore, colon, hyphen")
		return
	}
	var req struct {
		EventID  string `json:"event_id"`
		Quantity int    `json:"quantity"`
	}
	if !decode(w, r, &req) {
		return
	}
	id, ok := parseID(req.EventID)
	if !ok {
		writeError(w, 400, "invalid_event_id", "event_id must be a UUID in canonical hyphenated form")
		return
	}
	if req.Quantity < 1 || req.Quantity > 1000000 {
		writeError(w, 400, "invalid_quantity", "quantity must be an integer between 1 and 1000000")
		return
	}
	b, created, err := a.store.CreateBooking(r.Context(), keys[0], id, req.Quantity)
	if err != nil {
		a.fail(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	w.Header().Set("Location", "/bookings/"+b.ID)
	writeJSON(w, status, b)
}
func (a *API) getBooking(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	b, err := a.store.GetBooking(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, b)
}
func (a *API) cancelBooking(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// Cancellation has no parameters; reject accidental payloads.
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(data) != 0 {
		writeError(w, 400, "unexpected_body", "cancellation expects an empty body")
		return
	}
	b, err := a.store.CancelBooking(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, b)
}
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Pool.Ping(r.Context()); err != nil {
		writeError(w, 503, "database_unavailable", "database is unavailable")
		return
	}
	writeJSON(w, 200, struct {
		Status string `json:"status"`
	}{"ok"})
}
