package postgres_test

import (
	model "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
	"booking/internal/model/page"

	"github.com/google/uuid"
)

func (s *RepositorySuite) TestSearchBookings_EventWithoutBookingsGivesEmptyItems() {
	// Arrange
	repository, _ := s.newRepository()
	event := s.createEvent(repository, 10)

	// Act
	found, err := repository.SearchBookings(s.T().Context(), model.SearchRepositoryQuery{
		EventID: event.ID, Window: page.Window{Limit: 10},
	})

	// Assert
	s.Require().NoError(err)
	s.NotNil(found.Items)
	s.Empty(found.Items)
	s.False(found.HasMore)
}

func (s *RepositorySuite) TestSearchBookings_PagesOnlyThisEventIncludingCancelled() {
	// Arrange
	repository, _ := s.newRepository()
	event := s.createEvent(repository, 10)
	otherEvent := s.createEvent(repository, 10)
	first := s.createBooking(repository, event.ID, 1)
	second := s.createBooking(repository, event.ID, 2)
	s.createBooking(repository, otherEvent.ID, 1)

	cancelled, err := repository.CancelBooking(s.T().Context(), first.ID)
	s.Require().NoError(err)

	// Act
	firstPage, err := repository.SearchBookings(s.T().Context(), model.SearchRepositoryQuery{
		EventID: event.ID, Window: page.Window{Limit: 1},
	})
	s.Require().NoError(err)

	secondPage, err := repository.SearchBookings(s.T().Context(), model.SearchRepositoryQuery{
		EventID: event.ID, Window: page.Window{Limit: 1, Offset: 1},
	})
	s.Require().NoError(err)

	// Assert
	s.Equal([]model.Booking{cancelled}, firstPage.Items)
	s.True(firstPage.HasMore)
	s.Equal([]model.Booking{second}, secondPage.Items)
	s.False(secondPage.HasMore)
}

func (s *RepositorySuite) TestSearchBookings_MissingEventIsNotFound() {
	// Arrange
	repository, _ := s.newRepository()

	// Act
	_, err := repository.SearchBookings(s.T().Context(), model.SearchRepositoryQuery{
		EventID: uuid.New(), Window: page.Window{Limit: 10},
	})

	// Assert
	s.Require().ErrorIs(err, eventmodel.ErrNotFound)
}
