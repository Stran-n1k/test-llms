package postgres_test

import (
	"errors"

	model "booking/internal/model/booking"
	"booking/internal/repository/postgres"

	"github.com/google/uuid"
)

type cancelOutcome struct {
	booking model.Booking
	err     error
}

func (s *RepositorySuite) TestCancelBooking_ReturnsSeatsWhichCanBeBookedAgain() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 3)
	booking := s.createBooking(repository, event.ID, 3)

	// Act
	cancelled, err := repository.CancelBooking(s.T().Context(), booking.ID)

	// Assert
	s.Require().NoError(err)
	s.Equal(booking.ID, cancelled.ID)
	s.Equal(model.StatusCancelled, cancelled.Status)
	s.Require().NotNil(cancelled.CancelledAt)
	s.requireSeats(pool, event.ID, 0)

	// Act
	rebooked := s.createBooking(repository, event.ID, 3)

	// Assert
	s.Equal(model.StatusConfirmed, rebooked.Status)
	s.requireSeats(pool, event.ID, 3)
}

func (s *RepositorySuite) TestCancelBooking_RepeatChangesNothing() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 10)
	kept := s.createBooking(repository, event.ID, 4)
	booking := s.createBooking(repository, event.ID, 3)

	first, err := repository.CancelBooking(s.T().Context(), booking.ID)
	s.Require().NoError(err)

	// Act
	second, err := repository.CancelBooking(s.T().Context(), booking.ID)

	// Assert
	s.Require().NoError(err)
	s.Equal(first, second)
	s.requireSeats(pool, event.ID, kept.Seats)
}

func (s *RepositorySuite) TestCancelBooking_MissingBookingIsNotFound() {
	// Arrange
	repository, _ := s.newRepository()

	// Act
	_, err := repository.CancelBooking(s.T().Context(), uuid.New())

	// Assert
	s.Require().ErrorIs(err, model.ErrNotFound)
}

func (s *RepositorySuite) TestCancelBooking_ConcurrentCancelsReturnSeatsOnce() {
	// Arrange
	first, second, pool := s.newTwoInstances()
	event := s.createEvent(first, 10)
	kept := s.createBooking(first, event.ID, 4)
	booking := s.createBooking(first, event.ID, 3)

	// Act
	outcomes := runConcurrently(first, second, 40, func(repository *postgres.Repository, _ int) cancelOutcome {
		cancelled, err := repository.CancelBooking(s.T().Context(), booking.ID)

		return cancelOutcome{booking: cancelled, err: err}
	})

	// Assert
	for _, outcome := range outcomes {
		s.Require().NoError(outcome.err)
		s.Equal(model.StatusCancelled, outcome.booking.Status)
		s.Equal(outcomes[0].booking.CancelledAt, outcome.booking.CancelledAt)
	}

	s.requireSeats(pool, event.ID, kept.Seats)
}

// Mixes every write on one event. Any lost update or double refund would break
// the agreement between the seat counter and the sum of confirmed bookings.
func (s *RepositorySuite) TestCancelBooking_ConcurrentWithBookingsKeepsSeatsConsistent() {
	// Arrange
	first, second, pool := s.newTwoInstances()
	event := s.createEvent(first, 40)

	bookings := make([]model.Booking, 0, 20)
	for range 20 {
		bookings = append(bookings, s.createBooking(first, event.ID, 2))
	}

	// Act: each existing booking is cancelled twice, while 40 new requests for
	// 2 seats each compete for the 40 seats that become free.
	errs := runConcurrently(first, second, 80, func(repository *postgres.Repository, index int) error {
		if index < 40 {
			_, err := repository.CancelBooking(s.T().Context(), bookings[index/2].ID)

			return err
		}

		_, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, uuid.NewString(), 2))

		var insufficient *model.InsufficientSeatsError
		if errors.As(err, &insufficient) {
			return nil
		}

		return err
	})

	// Assert
	for _, err := range errs {
		s.Require().NoError(err)
	}

	var booked int

	err := pool.QueryRow(s.T().Context(), `SELECT booked_seats FROM events WHERE id = $1`, event.ID).Scan(&booked)
	s.Require().NoError(err)
	s.LessOrEqual(booked, 40)
	s.requireSeats(pool, event.ID, booked)
}
