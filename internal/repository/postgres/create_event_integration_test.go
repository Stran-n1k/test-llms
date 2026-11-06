package postgres_test

import (
	"time"

	model "booking/internal/model/event"

	"github.com/google/uuid"
)

func (s *RepositorySuite) TestCreateEvent_StoresEventWithNoBookedSeats() {
	// Arrange
	repository, _ := s.newRepository()
	startsAt := time.Date(2030, time.January, 31, 19, 0, 0, 0, time.FixedZone("MSK", 3*60*60))

	// Act
	created, err := repository.CreateEvent(s.T().Context(), model.CreateRepositoryCommand{
		Name: "Концерт", StartsAt: startsAt, Capacity: 10,
	})

	// Assert
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, created.ID)
	s.Equal("Концерт", created.Name)
	s.True(startsAt.Equal(created.StartsAt))
	s.Equal(time.UTC, created.StartsAt.Location())
	s.Equal(10, created.Capacity)
	s.Equal(0, created.BookedSeats)
	s.Equal(10, created.AvailableSeats())
	s.False(created.CreatedAt.IsZero())
}

func (s *RepositorySuite) TestCreateEvent_RejectsNonPositiveCapacityInTheDatabase() {
	// Arrange
	repository, _ := s.newRepository()

	// Act
	_, err := repository.CreateEvent(s.T().Context(), model.CreateRepositoryCommand{
		Name: "Concert", StartsAt: time.Now(), Capacity: 0,
	})

	// Assert
	s.Require().ErrorContains(err, "events_capacity_positive")
}
