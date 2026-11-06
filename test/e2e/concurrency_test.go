package e2e_test

import (
	"context"
	"net/http"

	"booking/internal/handler/rest/gen"

	"github.com/google/uuid"
)

func (s *E2ESuite) TestConcurrency_TwoInstancesNeverOversell() {
	// Arrange
	event := s.createEvent(30)

	// Act: 100 requests for 2 seats each compete for 30 seats.
	answers := s.inParallel(100, func(ctx context.Context, target *instance, _ int) (response, error) {
		return s.book(ctx, target, event.Id, uuid.NewString(), 2)
	})

	// Assert
	created, refused := 0, 0

	for _, answer := range answers {
		switch answer.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			refused++
		default:
			s.Failf("unexpected status", "%d: %s", answer.status, answer.body)
		}
	}

	s.Equal(15, created)
	s.Equal(85, refused)
	s.Len(s.listBookings(s.first, event.Id), 15)

	current := s.fetchEvent(s.second, event.Id)
	s.EqualValues(30, current.BookedSeats)
	s.EqualValues(0, current.AvailableSeats)
}

func (s *E2ESuite) TestConcurrency_SameKeyOnTwoInstancesCreatesOneBooking() {
	// Arrange
	event := s.createEvent(10)
	key := uuid.NewString()

	// Act
	answers := s.inParallel(40, func(ctx context.Context, target *instance, _ int) (response, error) {
		return s.book(ctx, target, event.Id, key, 4)
	})

	// Assert
	created := 0
	ids := map[uuid.UUID]struct{}{}

	for _, answer := range answers {
		if answer.status == http.StatusCreated {
			created++
		} else {
			s.Require().Equal(http.StatusOK, answer.status, "body: %s", answer.body)
		}

		ids[decode[gen.Booking](s, answer).Id] = struct{}{}
	}

	s.Equal(1, created)
	s.Len(ids, 1)
	s.Len(s.listBookings(s.first, event.Id), 1)
	s.EqualValues(4, s.fetchEvent(s.second, event.Id).BookedSeats)
}

func (s *E2ESuite) TestConcurrency_CancelOnTwoInstancesReturnsSeatsOnce() {
	// Arrange
	event := s.createEvent(10)

	kept, err := s.book(s.T().Context(), s.first, event.Id, uuid.NewString(), 4)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, kept.status)

	created, err := s.book(s.T().Context(), s.first, event.Id, uuid.NewString(), 3)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusCreated, created.status)

	path := "/bookings/" + decode[gen.Booking](s, created).Id.String() + "/cancel"

	// Act
	answers := s.inParallel(40, func(ctx context.Context, target *instance, _ int) (response, error) {
		return s.do(ctx, http.MethodPost, target.url(path), nil, nil)
	})

	// Assert
	for _, answer := range answers {
		s.Require().Equal(http.StatusOK, answer.status, "body: %s", answer.body)
		s.Equal(gen.BookingStatus("cancelled"), decode[gen.Booking](s, answer).Status)
	}

	current := s.fetchEvent(s.second, event.Id)
	s.EqualValues(4, current.BookedSeats)
	s.EqualValues(6, current.AvailableSeats)
}
