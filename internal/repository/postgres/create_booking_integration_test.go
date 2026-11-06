package postgres_test

import (
	"context"
	"time"

	model "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
	"booking/internal/repository/postgres"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type createOutcome struct {
	response model.CreateResponse
	err      error
}

func (s *RepositorySuite) TestCreateBooking_StoresBookingAndTakesSeats() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 10)

	// Act
	created, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))

	// Assert
	s.Require().NoError(err)
	s.False(created.Replayed)
	s.NotEqual(uuid.Nil, created.Booking.ID)
	s.Equal(event.ID, created.Booking.EventID)
	s.Equal("Anna", created.Booking.CustomerName)
	s.Equal(3, created.Booking.Seats)
	s.Equal(model.StatusConfirmed, created.Booking.Status)
	s.Nil(created.Booking.CancelledAt)
	s.requireSeats(pool, event.ID, 3)
}

func (s *RepositorySuite) TestCreateBooking_LastSeatsCanBeTaken() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 5)
	s.createBooking(repository, event.ID, 3)

	// Act
	_, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 2))

	// Assert
	s.Require().NoError(err)
	s.requireSeats(pool, event.ID, 5)
}

func (s *RepositorySuite) TestCreateBooking_TooManySeatsTakesNothing() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 5)
	s.createBooking(repository, event.ID, 3)

	// Act
	_, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))

	// Assert
	var insufficient *model.InsufficientSeatsError

	s.Require().ErrorAs(err, &insufficient)
	s.Equal(3, insufficient.Requested)
	s.Equal(2, insufficient.Available)
	s.Equal(1, s.countBookings(pool, event.ID))
	s.requireSeats(pool, event.ID, 3)
}

func (s *RepositorySuite) TestCreateBooking_FailedAttemptDoesNotConsumeItsKey() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 2)
	blocking := s.createBooking(repository, event.ID, 2)

	_, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 1))
	s.Require().Error(err)

	_, err = repository.CancelBooking(s.T().Context(), blocking.ID)
	s.Require().NoError(err)

	// Act
	retried, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 1))

	// Assert
	s.Require().NoError(err)
	s.False(retried.Replayed)
	s.requireSeats(pool, event.ID, 1)
}

func (s *RepositorySuite) TestCreateBooking_MissingEventIsNotFound() {
	// Arrange
	repository, _ := s.newRepository()

	// Act
	_, err := repository.CreateBooking(s.T().Context(), bookingCommand(uuid.New(), "key", 1))

	// Assert
	s.Require().ErrorIs(err, eventmodel.ErrNotFound)
}

func (s *RepositorySuite) TestCreateBooking_SameKeyReplaysWithoutTakingSeats() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 10)

	first, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))
	s.Require().NoError(err)

	// Act
	second, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))

	// Assert
	s.Require().NoError(err)
	s.True(second.Replayed)
	s.Equal(first.Booking, second.Booking)
	s.Equal(1, s.countBookings(pool, event.ID))
	s.requireSeats(pool, event.ID, 3)
}

func (s *RepositorySuite) TestCreateBooking_ReplayWorksWhenTheEventIsFull() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 3)

	first, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))
	s.Require().NoError(err)

	// Act
	second, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))

	// Assert
	s.Require().NoError(err)
	s.True(second.Replayed)
	s.Equal(first.Booking.ID, second.Booking.ID)
	s.requireSeats(pool, event.ID, 3)
}

func (s *RepositorySuite) TestCreateBooking_ReplayAfterCancelDoesNotBookAgain() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 10)

	first, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))
	s.Require().NoError(err)

	_, err = repository.CancelBooking(s.T().Context(), first.Booking.ID)
	s.Require().NoError(err)

	// Act
	replay, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))

	// Assert
	s.Require().NoError(err)
	s.True(replay.Replayed)
	s.Equal(model.StatusCancelled, replay.Booking.Status)
	s.Equal(1, s.countBookings(pool, event.ID))
	s.requireSeats(pool, event.ID, 0)
}

func (s *RepositorySuite) TestCreateBooking_KeyReusedForDifferentRequestIsRejected() {
	// Arrange
	repository, pool := s.newRepository()
	event := s.createEvent(repository, 10)
	otherEvent := s.createEvent(repository, 10)

	_, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))
	s.Require().NoError(err)

	otherCustomer := bookingCommand(event.ID, "key", 3)
	otherCustomer.CustomerName = "Boris"

	tests := map[string]model.CreateRepositoryCommand{
		"other seats":    bookingCommand(event.ID, "key", 4),
		"other customer": otherCustomer,
		"other event":    bookingCommand(otherEvent.ID, "key", 3),
	}

	for name, cmd := range tests {
		// Act
		_, err := repository.CreateBooking(s.T().Context(), cmd)

		// Assert
		s.Require().ErrorIs(err, model.ErrIdempotencyKeyReused, name)
	}

	s.requireSeats(pool, event.ID, 3)
	s.requireSeats(pool, otherEvent.ID, 0)
}

func (s *RepositorySuite) TestCreateBooking_ConcurrentRequestsNeverOversell() {
	// Arrange
	first, second, pool := s.newTwoInstances()
	event := s.createEvent(first, 25)

	// Act: 60 requests for 2 seats each compete for 25 seats.
	outcomes := runConcurrently(first, second, 60, func(repository *postgres.Repository, _ int) createOutcome {
		response, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, uuid.NewString(), 2))

		return createOutcome{response: response, err: err}
	})

	// Assert
	created, rejected := 0, 0

	for _, outcome := range outcomes {
		if outcome.err == nil {
			created++

			continue
		}

		var insufficient *model.InsufficientSeatsError

		s.Require().ErrorAs(outcome.err, &insufficient)

		rejected++
	}

	s.Equal(12, created)
	s.Equal(48, rejected)
	s.Equal(12, s.countBookings(pool, event.ID))
	s.requireSeats(pool, event.ID, 24)
}

func (s *RepositorySuite) TestCreateBooking_ConcurrentSameKeyCreatesOneBooking() {
	// Arrange
	first, second, pool := s.newTwoInstances()
	event := s.createEvent(first, 10)

	// Act
	outcomes := runConcurrently(first, second, 40, func(repository *postgres.Repository, _ int) createOutcome {
		response, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))

		return createOutcome{response: response, err: err}
	})

	// Assert
	originals := 0
	ids := map[uuid.UUID]struct{}{}

	for _, outcome := range outcomes {
		s.Require().NoError(outcome.err)

		ids[outcome.response.Booking.ID] = struct{}{}

		if !outcome.response.Replayed {
			originals++
		}
	}

	s.Equal(1, originals)
	s.Len(ids, 1)
	s.Equal(1, s.countBookings(pool, event.ID))
	s.requireSeats(pool, event.ID, 3)
}

// A waiter must find the winner's booking even though the winner took every
// seat: it has to answer with a replay, not with "not enough seats".
func (s *RepositorySuite) TestCreateBooking_ConcurrentSameKeyTakingAllSeatsAllSucceed() {
	// Arrange
	first, second, pool := s.newTwoInstances()
	event := s.createEvent(first, 3)

	// Act
	outcomes := runConcurrently(first, second, 40, func(repository *postgres.Repository, _ int) createOutcome {
		response, err := repository.CreateBooking(s.T().Context(), bookingCommand(event.ID, "key", 3))

		return createOutcome{response: response, err: err}
	})

	// Assert
	for _, outcome := range outcomes {
		s.Require().NoError(outcome.err)
	}

	s.requireSeats(pool, event.ID, 3)
}

// The event lock does not serialize this case; the unique constraint must.
func (s *RepositorySuite) TestCreateBooking_ConcurrentSameKeyOnDifferentEventsBooksOnlyOne() {
	// Arrange
	first, second, pool := s.newTwoInstances()
	eventA := s.createEvent(first, 100)
	eventB := s.createEvent(first, 100)

	// Act
	outcomes := runConcurrently(first, second, 40, func(repository *postgres.Repository, index int) createOutcome {
		eventID := eventA.ID
		if index%4 < 2 {
			eventID = eventB.ID
		}

		response, err := repository.CreateBooking(s.T().Context(), bookingCommand(eventID, "key", 1))

		return createOutcome{response: response, err: err}
	})

	// Assert
	reused := 0

	for _, outcome := range outcomes {
		if outcome.err != nil {
			s.Require().ErrorIs(outcome.err, model.ErrIdempotencyKeyReused)

			reused++
		}
	}

	s.Positive(reused)

	var total int

	err := pool.QueryRow(s.T().Context(), `SELECT count(*) FROM bookings`).Scan(&total)
	s.Require().NoError(err)
	s.Equal(1, total)

	found := s.countBookings(pool, eventA.ID)
	s.requireSeats(pool, eventA.ID, found)
	s.requireSeats(pool, eventB.ID, 1-found)
}

// Forces the moment the event lock cannot cover: another transaction holds the
// same key for another event, uncommitted, while this request is inserting it.
func (s *RepositorySuite) TestCreateBooking_WaitsForAnUncommittedSameKey() {
	tests := []struct {
		name       string
		finish     func(context.Context, pgx.Tx) error
		wantErr    error
		wantBooked int
	}{
		{
			name:    "other transaction commits and the key is reported as reused",
			finish:  func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) },
			wantErr: model.ErrIdempotencyKeyReused,
		},
		{
			name:       "other transaction rolls back and the booking goes through",
			finish:     func(ctx context.Context, tx pgx.Tx) error { return tx.Rollback(ctx) },
			wantBooked: 2,
		},
	}

	for _, test := range tests {
		s.Run(test.name, func() {
			ctx := s.T().Context()

			// Arrange
			repository, pool := s.newRepository()
			eventA := s.createEvent(repository, 10)
			eventB := s.createEvent(repository, 10)

			tx, err := pool.Begin(ctx)
			s.Require().NoError(err)

			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

			_, err = tx.Exec(ctx, `
				INSERT INTO bookings (event_id, idempotency_key, customer_name, seats)
				VALUES ($1, 'key', 'Boris', 1)`, eventA.ID)
			s.Require().NoError(err)

			// Act
			finished := make(chan error, 1)

			go func() {
				_, err := repository.CreateBooking(ctx, bookingCommand(eventB.ID, "key", 2))
				finished <- err
			}()

			s.Require().Eventually(func() bool {
				var waiting int

				err := pool.QueryRow(ctx, `
					SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'`,
				).Scan(&waiting)

				return err == nil && waiting == 1
			}, 10*time.Second, 5*time.Millisecond, "the request should be waiting for the key")

			s.Require().NoError(test.finish(ctx, tx))

			// Assert
			err = <-finished
			if test.wantErr != nil {
				s.Require().ErrorIs(err, test.wantErr)
			} else {
				s.Require().NoError(err)
			}

			var booked int

			err = pool.QueryRow(ctx, `SELECT booked_seats FROM events WHERE id = $1`, eventB.ID).Scan(&booked)
			s.Require().NoError(err)
			s.Equal(test.wantBooked, booked)
		})
	}
}
