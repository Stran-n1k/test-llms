package e2e_test

import (
	"net/http"

	"booking/internal/handler/rest/gen"

	"github.com/google/uuid"
)

func (s *E2ESuite) TestWorkflow_BookViewCancelAcrossInstances() {
	ctx := s.T().Context()

	// Arrange
	event := s.createEvent(5)
	s.Require().EqualValues(5, event.AvailableSeats)

	// Act: book on one instance.
	created, err := s.book(ctx, s.first, event.Id, uuid.NewString(), 3)
	s.Require().NoError(err)

	// Assert: the other instance sees it.
	s.Require().Equal(http.StatusCreated, created.status, "body: %s", created.body)
	s.Require().NotEmpty(created.header.Get("Location"))

	booking := decode[gen.Booking](s, created)
	s.Equal(gen.BookingStatus("confirmed"), booking.Status)
	s.EqualValues(2, s.fetchEvent(s.second, event.Id).AvailableSeats)

	fetched, err := s.do(ctx, http.MethodGet, s.second.url(created.header.Get("Location")), nil, nil)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, fetched.status)
	s.Equal(booking, decode[gen.Booking](s, fetched))

	listed := s.listBookings(s.second, event.Id)
	s.Require().Len(listed, 1)
	s.Equal(booking.Id, listed[0].Id)

	// Act: more seats than are left.
	refused, err := s.book(ctx, s.second, event.Id, uuid.NewString(), 3)
	s.Require().NoError(err)

	// Assert
	s.Equal(http.StatusConflict, refused.status)
	s.Equal("insufficient_seats", decode[gen.Error](s, refused).Error.Code)

	// Act: cancel on the other instance.
	cancelled, err := s.do(ctx, http.MethodPost, s.second.url("/bookings/"+booking.Id.String()+"/cancel"), nil, nil)
	s.Require().NoError(err)

	// Assert
	s.Require().Equal(http.StatusOK, cancelled.status, "body: %s", cancelled.body)
	s.Equal(gen.BookingStatus("cancelled"), decode[gen.Booking](s, cancelled).Status)
	s.EqualValues(5, s.fetchEvent(s.first, event.Id).AvailableSeats)
}

func (s *E2ESuite) TestWorkflow_RetryWithSameKeyOnOtherInstanceReplays() {
	ctx := s.T().Context()

	// Arrange
	event := s.createEvent(5)
	key := uuid.NewString()

	first, err := s.book(ctx, s.first, event.Id, key, 2)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, first.status)

	// Act
	retry, err := s.book(ctx, s.second, event.Id, key, 2)
	s.Require().NoError(err)

	// Assert
	s.Equal(http.StatusOK, retry.status)
	s.Equal("true", retry.header.Get("Idempotent-Replayed"))
	s.Equal(decode[gen.Booking](s, first), decode[gen.Booking](s, retry))
	s.EqualValues(3, s.fetchEvent(s.first, event.Id).AvailableSeats)

	// Act: the same key with another payload is a client error, not a replay.
	reused, err := s.book(ctx, s.second, event.Id, key, 3)
	s.Require().NoError(err)

	// Assert
	s.Equal(http.StatusUnprocessableEntity, reused.status)
	s.Equal("idempotency_key_reused", decode[gen.Error](s, reused).Error.Code)
	s.EqualValues(3, s.fetchEvent(s.first, event.Id).AvailableSeats)
}

func (s *E2ESuite) TestWorkflow_InvalidRequestsAreExplainedAndChangeNothing() {
	ctx := s.T().Context()

	// Arrange
	event := s.createEvent(5)
	bookingsPath := "/events/" + event.Id.String() + "/bookings"

	tests := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		body       any
		wantStatus int
		wantCode   string
	}{
		{
			name:       "zero seats",
			method:     http.MethodPost,
			path:       bookingsPath,
			headers:    map[string]string{"Idempotency-Key": uuid.NewString()},
			body:       gen.CreateBookingRequest{CustomerName: "Anna", Seats: 0},
			wantStatus: http.StatusBadRequest,
			wantCode:   "validation_failed",
		},
		{
			name:       "negative seats",
			method:     http.MethodPost,
			path:       bookingsPath,
			headers:    map[string]string{"Idempotency-Key": uuid.NewString()},
			body:       gen.CreateBookingRequest{CustomerName: "Anna", Seats: -2},
			wantStatus: http.StatusBadRequest,
			wantCode:   "validation_failed",
		},
		{
			name:       "no idempotency key",
			method:     http.MethodPost,
			path:       bookingsPath,
			body:       gen.CreateBookingRequest{CustomerName: "Anna", Seats: 1},
			wantStatus: http.StatusBadRequest,
			wantCode:   "validation_failed",
		},
		{
			name:       "NUL in a name",
			method:     http.MethodPost,
			path:       bookingsPath,
			headers:    map[string]string{"Idempotency-Key": uuid.NewString()},
			body:       gen.CreateBookingRequest{CustomerName: "An\x00na", Seats: 1},
			wantStatus: http.StatusBadRequest,
			wantCode:   "validation_failed",
		},
		{
			name:       "unknown event",
			method:     http.MethodPost,
			path:       "/events/" + uuid.NewString() + "/bookings",
			headers:    map[string]string{"Idempotency-Key": uuid.NewString()},
			body:       gen.CreateBookingRequest{CustomerName: "Anna", Seats: 1},
			wantStatus: http.StatusNotFound,
			wantCode:   "event_not_found",
		},
		{
			name:       "unknown booking",
			method:     http.MethodPost,
			path:       "/bookings/" + uuid.NewString() + "/cancel",
			wantStatus: http.StatusNotFound,
			wantCode:   "booking_not_found",
		},
		{
			name:       "event without capacity",
			method:     http.MethodPost,
			path:       "/events",
			body:       map[string]any{"name": "Concert", "starts_at": "2030-01-31T19:00:00Z"},
			wantStatus: http.StatusBadRequest,
			wantCode:   "validation_failed",
		},
	}

	for _, test := range tests {
		s.Run(test.name, func() {
			// Act
			answer, err := s.do(ctx, test.method, s.first.url(test.path), test.headers, test.body)

			// Assert
			s.Require().NoError(err)
			s.Equal(test.wantStatus, answer.status, "body: %s", answer.body)

			problem := decode[gen.Error](s, answer)
			s.Equal(test.wantCode, problem.Error.Code)
			s.NotEmpty(problem.Error.Message)
		})
	}

	s.EqualValues(5, s.fetchEvent(s.first, event.Id).AvailableSeats)
	s.Empty(s.listBookings(s.first, event.Id))
}

// These timestamps parse, but their UTC year cannot be written back as JSON. If
// one were stored, every response containing that event would break.
func (s *E2ESuite) TestWorkflow_UnrepresentableTimestampsAreRejected() {
	ctx := s.T().Context()

	for _, startsAt := range []string{"9999-12-31T23:59:59-03:00", "0000-01-01T00:00:00+03:00"} {
		s.Run(startsAt, func() {
			// Act
			answer, err := s.do(ctx, http.MethodPost, s.first.url("/events"), nil, map[string]any{
				"name": "Concert", "starts_at": startsAt, "capacity": 1,
			})

			// Assert
			s.Require().NoError(err)
			s.Equal(http.StatusBadRequest, answer.status, "body: %s", answer.body)
			s.Equal("validation_failed", decode[gen.Error](s, answer).Error.Code)

			listed, err := s.do(ctx, http.MethodGet, s.first.url("/events"), nil, nil)
			s.Require().NoError(err)
			s.Equal(http.StatusOK, listed.status)
			s.NotEmpty(listed.body)
		})
	}
}
