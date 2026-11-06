package postgres_test

import (
	model "booking/internal/model/event"

	"github.com/google/uuid"
)

func (s *RepositorySuite) TestFetchEvent_ReturnsCurrentAvailability() {
	// Arrange
	repository, _ := s.newRepository()
	event := s.createEvent(repository, 10)
	s.createBooking(repository, event.ID, 4)

	// Act
	found, err := repository.FetchEvent(s.T().Context(), event.ID)

	// Assert
	s.Require().NoError(err)
	s.Equal(event.ID, found.ID)
	s.Equal(4, found.BookedSeats)
	s.Equal(6, found.AvailableSeats())
}

func (s *RepositorySuite) TestFetchEvent_MissingEventIsNotFound() {
	// Arrange
	repository, _ := s.newRepository()

	// Act
	_, err := repository.FetchEvent(s.T().Context(), uuid.New())

	// Assert
	s.Require().ErrorIs(err, model.ErrNotFound)
}
