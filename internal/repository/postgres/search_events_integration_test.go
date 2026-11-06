package postgres_test

import (
	model "booking/internal/model/event"
	"booking/internal/model/page"
)

func (s *RepositorySuite) TestSearchEvents_EmptyDatabaseGivesEmptyItems() {
	// Arrange
	repository, _ := s.newRepository()

	// Act
	found, err := repository.SearchEvents(s.T().Context(), model.SearchRepositoryQuery{Window: page.Window{Limit: 10}})

	// Assert
	s.Require().NoError(err)
	s.NotNil(found.Items)
	s.Empty(found.Items)
	s.False(found.HasMore)
}

func (s *RepositorySuite) TestSearchEvents_PagesCoverAllEventsInCreationOrder() {
	// Arrange
	repository, _ := s.newRepository()
	first := s.createEvent(repository, 1)
	second := s.createEvent(repository, 2)
	third := s.createEvent(repository, 3)

	// Act
	firstPage, err := repository.SearchEvents(s.T().Context(), model.SearchRepositoryQuery{Window: page.Window{Limit: 2}})
	s.Require().NoError(err)

	secondPage, err := repository.SearchEvents(s.T().Context(), model.SearchRepositoryQuery{Window: page.Window{Limit: 2, Offset: 2}})
	s.Require().NoError(err)

	// Assert
	s.Equal([]model.Event{first, second}, firstPage.Items)
	s.True(firstPage.HasMore)
	s.Equal([]model.Event{third}, secondPage.Items)
	s.False(secondPage.HasMore)
	s.Equal(2, secondPage.Offset)
}
