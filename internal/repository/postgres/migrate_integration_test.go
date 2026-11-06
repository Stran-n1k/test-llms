package postgres_test

import (
	"booking/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

func (s *RepositorySuite) TestMigrate_CreatesSchemaOnEmptyDatabase() {
	// Arrange
	pool := s.newPool(s.newDatabase())

	// Act
	err := postgres.Migrate(s.T().Context(), pool)

	// Assert
	s.Require().NoError(err)

	var tables int

	err = pool.QueryRow(s.T().Context(), `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN ('events', 'bookings', 'schema_migrations')`,
	).Scan(&tables)
	s.Require().NoError(err)
	s.Equal(3, tables)
}

func (s *RepositorySuite) TestMigrate_RepeatedRunKeepsData() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 10)
	booking := s.createBooking(repository, event.ID, 3)

	// Act
	err := postgres.Migrate(s.T().Context(), pool)

	// Assert
	s.Require().NoError(err)

	found, err := repository.FetchBooking(s.T().Context(), booking.ID)
	s.Require().NoError(err)
	s.Equal(booking, found)
	s.requireSeats(pool, event.ID, 3)
}

func (s *RepositorySuite) TestMigrate_ConcurrentRunsOnEmptyDatabaseAllSucceed() {
	// Arrange
	databaseURL := s.newDatabase()
	pools := []*pgxpool.Pool{s.newPool(databaseURL), s.newPool(databaseURL)}

	// Act
	errs := runConcurrently(nil, nil, 8, func(_ *postgres.Repository, index int) error {
		return postgres.Migrate(s.T().Context(), pools[index%len(pools)])
	})

	// Assert
	for _, err := range errs {
		s.Require().NoError(err)
	}

	var applied int

	err := pools[0].QueryRow(s.T().Context(), `SELECT count(*) FROM schema_migrations`).Scan(&applied)
	s.Require().NoError(err)
	s.Equal(1, applied)
}

func (s *RepositorySuite) TestMigrate_RefusesAChangedAppliedMigration() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 10)

	_, err := pool.Exec(s.T().Context(), `UPDATE schema_migrations SET checksum = 'other'`)
	s.Require().NoError(err)

	// Act
	err = postgres.Migrate(s.T().Context(), pool)

	// Assert
	s.Require().ErrorContains(err, "changed after it was applied")

	_, err = repository.FetchEvent(s.T().Context(), event.ID)
	s.Require().NoError(err)
}

func (s *RepositorySuite) TestSchema_RejectsSeatCountsOutsideCapacity() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 2)

	// Act
	_, oversold := pool.Exec(s.T().Context(), `UPDATE events SET booked_seats = 3 WHERE id = $1`, event.ID)
	_, negative := pool.Exec(s.T().Context(), `UPDATE events SET booked_seats = -1 WHERE id = $1`, event.ID)

	// Assert
	s.Require().ErrorContains(oversold, "events_booked_seats_range")
	s.Require().ErrorContains(negative, "events_booked_seats_range")
}
