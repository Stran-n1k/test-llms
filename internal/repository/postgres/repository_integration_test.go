package postgres_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	bookingmodel "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
	"booking/internal/repository/postgres"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	postgresImage      = "postgres:18.1-alpine"
	concurrentPoolSize = 32
)

// RepositorySuite shares one PostgreSQL container; every test owns a database.
type RepositorySuite struct {
	suite.Suite

	container *tcpostgres.PostgresContainer
	baseURL   string
	admin     *pgxpool.Pool
}

// TestRepositorySuite runs against a real PostgreSQL. It needs Docker; use
// go test -short to skip it.
func TestRepositorySuite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: needs Docker")
	}

	suite.Run(t, new(RepositorySuite))
}

func (s *RepositorySuite) SetupSuite() {
	ctx := s.T().Context()

	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("admin"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
		// Every test creates a database; durability of the throwaway container
		// is not under test, and without fsync that takes far less time.
		testcontainers.WithCmdArgs("-c", "fsync=off"),
	)
	s.Require().NoError(err, "start PostgreSQL container; is Docker running?")

	s.container = container

	s.baseURL, err = container.ConnectionString(ctx, "sslmode=disable")
	s.Require().NoError(err)

	s.admin, err = pgxpool.New(ctx, s.baseURL)
	s.Require().NoError(err)
}

func (s *RepositorySuite) TearDownSuite() {
	if s.admin != nil {
		s.admin.Close()
	}

	if s.container != nil {
		s.Require().NoError(testcontainers.TerminateContainer(s.container))
	}
}

// newDatabase creates an empty database owned by the current test and returns
// its connection URL.
func (s *RepositorySuite) newDatabase() string {
	s.T().Helper()

	name := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	_, err := s.admin.Exec(s.T().Context(), fmt.Sprintf(`CREATE DATABASE %q`, name))
	s.Require().NoError(err)

	s.T().Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_, err := s.admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE %q WITH (FORCE)`, name))
		s.NoError(err)
	})

	parsed, err := url.Parse(s.baseURL)
	s.Require().NoError(err)

	parsed.Path = "/" + name
	query := parsed.Query()
	query.Set("pool_max_conns", fmt.Sprint(concurrentPoolSize))
	parsed.RawQuery = query.Encode()

	return parsed.String()
}

func (s *RepositorySuite) newPool(databaseURL string) *pgxpool.Pool {
	s.T().Helper()

	pool, err := pgxpool.New(s.T().Context(), databaseURL)
	s.Require().NoError(err)
	s.T().Cleanup(pool.Close)

	return pool
}

// newRepository returns a repository on a migrated database owned by the test.
func (s *RepositorySuite) newRepository() (*postgres.Repository, *pgxpool.Pool) {
	s.T().Helper()

	pool := s.newPool(s.newDatabase())
	s.Require().NoError(postgres.Migrate(s.T().Context(), pool))

	return postgres.NewRepository(pool), pool
}

// newTwoInstances returns two repositories with separate connection pools on
// one migrated database, the same way two service instances share it.
func (s *RepositorySuite) newTwoInstances() (first, second *postgres.Repository, pool *pgxpool.Pool) {
	s.T().Helper()

	databaseURL := s.newDatabase()
	pool = s.newPool(databaseURL)
	s.Require().NoError(postgres.Migrate(s.T().Context(), pool))

	return postgres.NewRepository(pool), postgres.NewRepository(s.newPool(databaseURL)), pool
}

func (s *RepositorySuite) createEvent(repository *postgres.Repository, capacity int) eventmodel.Event {
	s.T().Helper()

	created, err := repository.CreateEvent(s.T().Context(), eventmodel.CreateRepositoryCommand{
		Name:     "Concert",
		StartsAt: time.Date(2030, time.January, 31, 19, 0, 0, 0, time.UTC),
		Capacity: capacity,
	})
	s.Require().NoError(err)

	return created
}

func (s *RepositorySuite) createBooking(repository *postgres.Repository, eventID uuid.UUID, seats int) bookingmodel.Booking {
	s.T().Helper()

	created, err := repository.CreateBooking(s.T().Context(), bookingCommand(eventID, uuid.NewString(), seats))
	s.Require().NoError(err)

	return created.Booking
}

func bookingCommand(eventID uuid.UUID, key string, seats int) bookingmodel.CreateRepositoryCommand {
	return bookingmodel.CreateRepositoryCommand{
		EventID:        eventID,
		IdempotencyKey: key,
		CustomerName:   "Anna",
		Seats:          seats,
	}
}

// requireSeats asserts the event's seat counter and that it agrees with the sum
// of its confirmed bookings, which is the invariant every operation must keep.
func (s *RepositorySuite) requireSeats(pool *pgxpool.Pool, eventID uuid.UUID, wantBooked int) {
	s.T().Helper()

	var booked, confirmed int

	err := pool.QueryRow(s.T().Context(), `
		SELECT e.booked_seats, COALESCE(SUM(b.seats) FILTER (WHERE b.status = 'confirmed'), 0)
		FROM events e LEFT JOIN bookings b ON b.event_id = e.id
		WHERE e.id = $1
		GROUP BY e.id`, eventID,
	).Scan(&booked, &confirmed)
	s.Require().NoError(err)
	s.Require().Equal(wantBooked, booked, "seat counter")
	s.Require().Equal(wantBooked, confirmed, "sum of confirmed bookings")
}

func (s *RepositorySuite) countBookings(pool *pgxpool.Pool, eventID uuid.UUID) int {
	s.T().Helper()

	var count int

	err := pool.QueryRow(s.T().Context(), `SELECT count(*) FROM bookings WHERE event_id = $1`, eventID).Scan(&count)
	s.Require().NoError(err)

	return count
}

// runConcurrently starts count calls at the same moment on the two instances in
// turn and returns once all of them finished.
func runConcurrently[T any](
	first, second *postgres.Repository,
	count int,
	call func(repository *postgres.Repository, index int) T,
) []T {
	results := make([]T, count)
	start := make(chan struct{})
	done := make(chan struct{}, count)

	for index := range count {
		repository := first
		if index%2 == 1 {
			repository = second
		}

		go func() {
			defer func() { done <- struct{}{} }()

			<-start

			results[index] = call(repository, index)
		}()
	}

	close(start)

	for range count {
		<-done
	}

	return results
}
