package e2e_test

import (
	"net/http"
	"syscall"

	"booking/internal/handler/rest/gen"

	"github.com/google/uuid"
)

func (s *E2ESuite) TestRestart_GracefulStopExitsCleanly() {
	// Arrange
	spare := s.startInstance()
	s.waitReady(spare)

	// Act
	err := s.stopInstance(spare, syscall.SIGTERM)

	// Assert
	s.Require().NoError(err, "log: %s", spare.output)
}

func (s *E2ESuite) TestRestart_DataAndIdempotencySurviveACrash() {
	ctx := s.T().Context()

	// Arrange
	event := s.createEvent(5)
	key := uuid.NewString()
	restarted := s.startInstance()
	s.waitReady(restarted)

	created, err := s.book(ctx, restarted, event.Id, key, 2)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, created.status)

	booking := decode[gen.Booking](s, created)

	// Act: kill the process abruptly and start it again on the same database,
	// which also repeats the schema preparation.
	s.Require().Error(s.stopInstance(restarted, syscall.SIGKILL))

	restarted = s.startInstanceOn(restarted.addr)
	s.waitReady(restarted)
	s.T().Cleanup(func() {
		_ = s.stopInstance(restarted, syscall.SIGTERM)
	})

	// Assert: the booking and the seat count are still there.
	current := s.fetchEvent(restarted, event.Id)
	s.EqualValues(2, current.BookedSeats)
	s.EqualValues(3, current.AvailableSeats)

	fetched, err := s.do(ctx, http.MethodGet, restarted.url("/bookings/"+booking.Id.String()), nil, nil)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, fetched.status)
	s.Equal(booking, decode[gen.Booking](s, fetched))

	// Act: a client retrying through the restart gets the same booking.
	retry, err := s.book(ctx, restarted, event.Id, key, 2)
	s.Require().NoError(err)

	// Assert
	s.Equal(http.StatusOK, retry.status)
	s.Equal(booking.Id, decode[gen.Booking](s, retry).Id)
	s.EqualValues(2, s.fetchEvent(s.first, event.Id).BookedSeats)
}
