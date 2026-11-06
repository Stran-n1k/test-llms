package rest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"booking/internal/handler/rest"
	"booking/internal/handler/rest/gen"
	"booking/internal/handler/rest/mocks"
	bookingmodel "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
	"booking/internal/model/page"
	"booking/internal/model/validation"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

const (
	validEventBody   = `{"name":"Concert","starts_at":"2030-01-31T19:00:00+03:00","capacity":10}`
	validBookingBody = `{"customer_name":"Anna","seats":2}`
)

type dependencies struct {
	events   *mocks.MockEventService
	bookings *mocks.MockBookingService
	health   *mocks.MockHealthChecker
}

func newServer(t *testing.T, setupMocks func(dependencies)) http.Handler {
	t.Helper()

	controller := gomock.NewController(t)
	deps := dependencies{
		events:   mocks.NewMockEventService(controller),
		bookings: mocks.NewMockBookingService(controller),
		health:   mocks.NewMockHealthChecker(controller),
	}
	setupMocks(deps)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return rest.NewHTTPHandler(rest.NewHandler(deps.events, deps.bookings, deps.health, log), time.Minute)
}

func newRequest(method, target, body string, headers map[string]string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}

	for name, value := range headers {
		request.Header.Set(name, value)
	}

	return request
}

func decodeBody[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()

	var body T

	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), "body: %s", recorder.Body.String())

	return body
}

func TestServer_RejectedRequestsDoNotReachServices(t *testing.T) {
	t.Parallel()

	eventsPath := "/events"
	bookingsPath := "/events/" + uuid.NewString() + "/bookings"
	idempotent := map[string]string{"Idempotency-Key": "key-1"}

	tests := []struct {
		name        string
		method      string
		target      string
		body        string
		headers     map[string]string
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{name: "empty body", method: http.MethodPost, target: eventsPath, headers: map[string]string{"Content-Type": "application/json"}, wantStatus: 400, wantCode: "invalid_json", wantMessage: "empty"},
		{name: "malformed JSON", method: http.MethodPost, target: eventsPath, body: `{"name":`, wantStatus: 400, wantCode: "invalid_json", wantMessage: "not valid JSON"},
		{name: "JSON of the wrong shape", method: http.MethodPost, target: eventsPath, body: `[1,2]`, wantStatus: 400, wantCode: "invalid_json", wantMessage: "wrong type"},
		{name: "trailing data", method: http.MethodPost, target: eventsPath, body: validEventBody + ` {}`, wantStatus: 400, wantCode: "invalid_json", wantMessage: "single JSON value"},
		{name: "unknown field", method: http.MethodPost, target: eventsPath, body: `{"nam":"x"}`, wantStatus: 400, wantCode: "invalid_json", wantMessage: `unknown field "nam"`},
		{name: "text capacity", method: http.MethodPost, target: eventsPath, body: `{"capacity":"ten"}`, wantStatus: 400, wantCode: "invalid_json", wantMessage: `"capacity"`},
		{name: "fractional capacity", method: http.MethodPost, target: eventsPath, body: `{"capacity":1.5}`, wantStatus: 400, wantCode: "invalid_json", wantMessage: `"capacity"`},
		{name: "capacity above int32", method: http.MethodPost, target: eventsPath, body: `{"capacity":2147483648}`, wantStatus: 400, wantCode: "invalid_json", wantMessage: `"capacity"`},
		{name: "timestamp not in RFC 3339", method: http.MethodPost, target: eventsPath, body: `{"starts_at":"tomorrow"}`, wantStatus: 400, wantCode: "invalid_json", wantMessage: "RFC 3339"},
		{name: "numeric timestamp", method: http.MethodPost, target: eventsPath, body: `{"starts_at":123}`, wantStatus: 400, wantCode: "invalid_json", wantMessage: `"starts_at"`},
		{name: "oversized body", method: http.MethodPost, target: eventsPath, body: `{"name":"` + strings.Repeat("a", 1<<20) + `"}`, wantStatus: 413, wantCode: "payload_too_large", wantMessage: "must not exceed"},
		{name: "wrong content type", method: http.MethodPost, target: eventsPath, body: validEventBody, headers: map[string]string{"Content-Type": "text/plain"}, wantStatus: 415, wantCode: "unsupported_media_type", wantMessage: "application/json"},
		{name: "booking without idempotency key", method: http.MethodPost, target: bookingsPath, body: validBookingBody, wantStatus: 400, wantCode: "validation_failed", wantMessage: "fields"},
		{name: "booking with text seats", method: http.MethodPost, target: bookingsPath, body: `{"customer_name":"Anna","seats":"2"}`, headers: idempotent, wantStatus: 400, wantCode: "invalid_json", wantMessage: `"seats"`},
		{name: "booking for a malformed event id", method: http.MethodPost, target: "/events/not-a-uuid/bookings", body: validBookingBody, headers: idempotent, wantStatus: 400, wantCode: "validation_failed", wantMessage: "fields"},
		{name: "fetch with a malformed id", method: http.MethodGet, target: "/events/not-a-uuid", wantStatus: 400, wantCode: "validation_failed", wantMessage: "fields"},
		{name: "cancel with a malformed id", method: http.MethodPost, target: "/bookings/not-a-uuid/cancel", wantStatus: 400, wantCode: "validation_failed", wantMessage: "fields"},
		{name: "text limit", method: http.MethodGet, target: "/events?limit=many", wantStatus: 400, wantCode: "validation_failed", wantMessage: "fields"},
		{name: "unknown path", method: http.MethodGet, target: "/nope", wantStatus: 404, wantCode: "not_found", wantMessage: "no such path"},
		{name: "unsupported method", method: http.MethodDelete, target: eventsPath, wantStatus: 405, wantCode: "method_not_allowed", wantMessage: "not allowed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange: no expectations, so any service call fails the test.
			server := newServer(t, func(dependencies) {})
			recorder := httptest.NewRecorder()

			// Act
			server.ServeHTTP(recorder, newRequest(test.method, test.target, test.body, test.headers))

			// Assert
			require.Equal(t, test.wantStatus, recorder.Code, "body: %s", recorder.Body.String())
			require.Equal(t, "application/json", recorder.Header().Get("Content-Type"))

			body := decodeBody[gen.Error](t, recorder)
			require.Equal(t, test.wantCode, body.Error.Code)
			require.Contains(t, body.Error.Message, test.wantMessage)
		})
	}
}

func TestServer_ServiceErrorsMapToResponses(t *testing.T) {
	t.Parallel()

	bookingID := uuid.New()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantText   string
	}{
		{name: "missing event", err: eventmodel.ErrNotFound, wantStatus: 404, wantCode: "event_not_found"},
		{name: "missing booking", err: bookingmodel.ErrNotFound, wantStatus: 404, wantCode: "booking_not_found"},
		{name: "reused key", err: bookingmodel.ErrIdempotencyKeyReused, wantStatus: 422, wantCode: "idempotency_key_reused"},
		{
			name:       "not enough seats reports what is left",
			err:        &bookingmodel.InsufficientSeatsError{Requested: 5, Available: 2},
			wantStatus: 409,
			wantCode:   "insufficient_seats",
			wantText:   "requested 5, available 2",
		},
		{
			name:       "validation error lists fields",
			err:        &validation.Error{Fields: []validation.FieldError{{Field: "seats", Message: "must be at least 1"}}},
			wantStatus: 400,
			wantCode:   "validation_failed",
			wantText:   "fields",
		},
		{name: "unexpected failure hides its details", err: errors.New("password=secret"), wantStatus: 500, wantCode: "internal_error", wantText: "internal server error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			server := newServer(t, func(deps dependencies) {
				deps.bookings.EXPECT().
					Fetch(gomock.Any(), bookingID).
					Return(bookingmodel.Booking{}, test.err)
			})
			recorder := httptest.NewRecorder()

			// Act
			server.ServeHTTP(recorder, newRequest(http.MethodGet, "/bookings/"+bookingID.String(), "", nil))

			// Assert
			require.Equal(t, test.wantStatus, recorder.Code)

			body := decodeBody[gen.Error](t, recorder)
			require.Equal(t, test.wantCode, body.Error.Code)
			require.Contains(t, body.Error.Message, test.wantText)
			require.NotContains(t, recorder.Body.String(), "secret")
		})
	}
}

func TestServer_CreateEventMapsRequestAndResponse(t *testing.T) {
	t.Parallel()

	// Arrange
	id := uuid.New()
	startsAt := time.Date(2030, time.January, 31, 16, 0, 0, 0, time.UTC)
	server := newServer(t, func(deps dependencies) {
		deps.events.EXPECT().
			Create(gomock.Any(), gomock.Cond(func(cmd eventmodel.CreateServiceCommand) bool {
				return cmd.Name == "Concert" && cmd.Capacity == 10 && cmd.StartsAt.Equal(startsAt)
			})).
			Return(eventmodel.Event{ID: id, Name: "Concert", StartsAt: startsAt, Capacity: 10, BookedSeats: 3}, nil)
	})
	recorder := httptest.NewRecorder()

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodPost, "/events", validEventBody, nil))

	// Assert
	require.Equal(t, http.StatusCreated, recorder.Code)
	require.Equal(t, "/events/"+id.String(), recorder.Header().Get("Location"))

	body := decodeBody[gen.Event](t, recorder)
	require.Equal(t, id, body.Id)
	require.EqualValues(t, 10, body.Capacity)
	require.EqualValues(t, 3, body.BookedSeats)
	require.EqualValues(t, 7, body.AvailableSeats)
}

func TestServer_CreateEventAcceptsMediaTypeParameters(t *testing.T) {
	t.Parallel()

	// Arrange
	server := newServer(t, func(deps dependencies) {
		deps.events.EXPECT().
			Create(gomock.Any(), gomock.Any()).
			Return(eventmodel.Event{}, nil)
	})
	recorder := httptest.NewRecorder()
	headers := map[string]string{"Content-Type": "application/json; charset=utf-8"}

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodPost, "/events", validEventBody, headers))

	// Assert
	require.Equal(t, http.StatusCreated, recorder.Code)
}

func TestServer_CreateBookingSignalsReplay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		replayed     bool
		wantStatus   int
		wantReplayed string
	}{
		{name: "first request answers created", replayed: false, wantStatus: http.StatusCreated, wantReplayed: ""},
		{name: "repeat answers ok with a replay header", replayed: true, wantStatus: http.StatusOK, wantReplayed: "true"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			eventID, bookingID := uuid.New(), uuid.New()
			server := newServer(t, func(deps dependencies) {
				deps.bookings.EXPECT().
					Create(gomock.Any(), bookingmodel.CreateServiceCommand{
						EventID: eventID, IdempotencyKey: "key-1", CustomerName: "Anna", Seats: 2,
					}).
					Return(bookingmodel.CreateResponse{Booking: bookingmodel.Booking{ID: bookingID}, Replayed: test.replayed}, nil)
			})
			recorder := httptest.NewRecorder()
			headers := map[string]string{"Idempotency-Key": "key-1"}

			// Act
			server.ServeHTTP(recorder, newRequest(http.MethodPost, "/events/"+eventID.String()+"/bookings", validBookingBody, headers))

			// Assert
			require.Equal(t, test.wantStatus, recorder.Code)
			require.Equal(t, test.wantReplayed, recorder.Header().Get("Idempotent-Replayed"))
			require.Equal(t, "/bookings/"+bookingID.String(), recorder.Header().Get("Location"))
		})
	}
}

func TestServer_CancelReturnsTheCancelledBooking(t *testing.T) {
	t.Parallel()

	// Arrange
	id := uuid.New()
	cancelledAt := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	server := newServer(t, func(deps dependencies) {
		deps.bookings.EXPECT().
			Cancel(gomock.Any(), id).
			Return(bookingmodel.Booking{ID: id, Status: bookingmodel.StatusCancelled, CancelledAt: &cancelledAt}, nil)
	})
	recorder := httptest.NewRecorder()

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodPost, "/bookings/"+id.String()+"/cancel", "", nil))

	// Assert
	require.Equal(t, http.StatusOK, recorder.Code)

	body := decodeBody[gen.Booking](t, recorder)
	require.Equal(t, gen.BookingStatus("cancelled"), body.Status)
	require.NotNil(t, body.CancelledAt)
}

func TestServer_ListsMapPaginationAndEmptyItems(t *testing.T) {
	t.Parallel()

	// Arrange
	wantLimit, wantOffset := 5, 10
	server := newServer(t, func(deps dependencies) {
		deps.events.EXPECT().
			Search(gomock.Any(), eventmodel.SearchServiceQuery{Page: page.Request{Limit: &wantLimit, Offset: &wantOffset}}).
			Return(page.Page[eventmodel.Event]{Items: []eventmodel.Event{}, Limit: 5, Offset: 10}, nil)
	})
	recorder := httptest.NewRecorder()

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodGet, "/events?limit=5&offset=10", "", nil))

	// Assert
	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"items":[],"limit":5,"offset":10,"has_more":false}`, recorder.Body.String())
}

func TestServer_ListBookingsPassesEventAndPagination(t *testing.T) {
	t.Parallel()

	// Arrange
	eventID := uuid.New()
	server := newServer(t, func(deps dependencies) {
		deps.bookings.EXPECT().
			Search(gomock.Any(), bookingmodel.SearchServiceQuery{EventID: eventID}).
			Return(page.Page[bookingmodel.Booking]{Items: []bookingmodel.Booking{{EventID: eventID}}, Limit: 50, HasMore: true}, nil)
	})
	recorder := httptest.NewRecorder()

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodGet, "/events/"+eventID.String()+"/bookings", "", nil))

	// Assert
	require.Equal(t, http.StatusOK, recorder.Code)

	body := decodeBody[gen.BookingPage](t, recorder)
	require.Len(t, body.Items, 1)
	require.True(t, body.HasMore)
}

func TestServer_HealthReflectsTheDatabase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
	}{
		{name: "reachable database is healthy", pingErr: nil, wantStatus: http.StatusOK},
		{name: "unreachable database is unavailable", pingErr: errors.New("connection refused"), wantStatus: http.StatusServiceUnavailable},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			server := newServer(t, func(deps dependencies) {
				deps.health.EXPECT().
					Ping(gomock.Any()).
					Return(test.pingErr)
			})
			recorder := httptest.NewRecorder()

			// Act
			server.ServeHTTP(recorder, newRequest(http.MethodGet, "/healthz", "", nil))

			// Assert
			require.Equal(t, test.wantStatus, recorder.Code)
		})
	}
}

func TestServer_PanicBecomesJSONInternalError(t *testing.T) {
	t.Parallel()

	// Arrange
	server := newServer(t, func(deps dependencies) {
		deps.events.EXPECT().
			Search(gomock.Any(), gomock.Any()).
			DoAndReturn(func(context.Context, eventmodel.SearchServiceQuery) (page.Page[eventmodel.Event], error) {
				panic("boom")
			})
	})
	recorder := httptest.NewRecorder()

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodGet, "/events", "", nil))

	// Assert
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, "internal_error", decodeBody[gen.Error](t, recorder).Error.Code)
	require.NotContains(t, recorder.Body.String(), "boom")
}

func TestServer_UnencodableResponseIsAnExplicitError(t *testing.T) {
	t.Parallel()

	// Arrange
	id := uuid.New()
	server := newServer(t, func(deps dependencies) {
		deps.events.EXPECT().
			Fetch(gomock.Any(), id).
			Return(eventmodel.Event{ID: id, StartsAt: time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)}, nil)
	})
	recorder := httptest.NewRecorder()

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodGet, "/events/"+id.String(), "", nil))

	// Assert
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, "internal_error", decodeBody[gen.Error](t, recorder).Error.Code)
}

func TestServer_TimedOutRequestIsReportedAsRetryable(t *testing.T) {
	t.Parallel()

	// Arrange
	controller := gomock.NewController(t)
	events := mocks.NewMockEventService(controller)
	events.EXPECT().
		Search(gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ eventmodel.SearchServiceQuery) (page.Page[eventmodel.Event], error) {
			<-ctx.Done()

			return page.Page[eventmodel.Event]{}, ctx.Err()
		})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := rest.NewHTTPHandler(
		rest.NewHandler(events, mocks.NewMockBookingService(controller), mocks.NewMockHealthChecker(controller), log),
		time.Millisecond,
	)
	recorder := httptest.NewRecorder()

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodGet, "/events", "", nil))

	// Assert
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Equal(t, "service_unavailable", decodeBody[gen.Error](t, recorder).Error.Code)
}

func TestServer_CallerHangingUpIsNotAnInternalError(t *testing.T) {
	t.Parallel()

	// Arrange
	ctx, cancel := context.WithCancel(t.Context())
	server := newServer(t, func(deps dependencies) {
		deps.events.EXPECT().
			Search(gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, _ eventmodel.SearchServiceQuery) (page.Page[eventmodel.Event], error) {
				cancel()

				return page.Page[eventmodel.Event]{}, ctx.Err()
			})
	})
	recorder := httptest.NewRecorder()

	// Act
	server.ServeHTTP(recorder, newRequest(http.MethodGet, "/events", "", nil).WithContext(ctx))

	// Assert
	require.Equal(t, 499, recorder.Code)
}
