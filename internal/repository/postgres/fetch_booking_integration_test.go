package postgres_test

import (
	model "booking/internal/model/booking"

	"github.com/google/uuid"
)

func (s *RepositorySuite) TestFetchBooking_ReturnsStoredBooking() {
	// Arrange
	repository, _ := s.newRepository()
	event := s.createEvent(repository, 10)
	booking := s.createBooking(repository, event.ID, 2)

	// Act
	found, err := repository.FetchBooking(s.T().Context(), booking.ID)

	// Assert
	s.Require().NoError(err)
	s.Equal(booking, found)
}

func (s *RepositorySuite) TestFetchBooking_MissingBookingIsNotFound() {
	// Arrange
	repository, _ := s.newRepository()

	// Act
	_, err := repository.FetchBooking(s.T().Context(), uuid.New())

	// Assert
	s.Require().ErrorIs(err, model.ErrNotFound)
}
