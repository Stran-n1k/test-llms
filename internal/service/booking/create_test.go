package booking_test

import (
	"strings"
	"testing"

	model "booking/internal/model/booking"
	"booking/internal/model/validation"
	service "booking/internal/service/booking"
	"booking/internal/service/booking/mocks"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestService_Create(t *testing.T) {
	t.Parallel()

	eventID := uuid.New()
	valid := model.CreateServiceCommand{EventID: eventID, IdempotencyKey: "key-1", CustomerName: "Anna", Seats: 2}
	stored := model.CreateResponse{Booking: model.Booking{ID: uuid.New(), Seats: 2}}

	withKey := func(key string) model.CreateServiceCommand {
		cmd := valid
		cmd.IdempotencyKey = key

		return cmd
	}

	tests := []struct {
		name          string
		setupMocks    func(*mocks.MockRepository)
		cmd           model.CreateServiceCommand
		want          model.CreateResponse
		wantErr       error
		wantBadFields []string
	}{
		{
			name: "normalized booking is stored",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					CreateBooking(gomock.Any(), model.CreateRepositoryCommand{
						EventID: eventID, IdempotencyKey: "key-1", CustomerName: "Anna", Seats: 2,
					}).
					Return(stored, nil)
			},
			cmd:  model.CreateServiceCommand{EventID: eventID, IdempotencyKey: "key-1", CustomerName: " Anna ", Seats: 2},
			want: stored,
		},
		{
			name: "longest idempotency key is accepted",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					CreateBooking(gomock.Any(), gomock.Any()).
					Return(stored, nil)
			},
			cmd:  withKey(strings.Repeat("k", 255)),
			want: stored,
		},
		{
			name:          "all invalid fields are reported without a repository call",
			setupMocks:    func(*mocks.MockRepository) {},
			cmd:           model.CreateServiceCommand{EventID: eventID, Seats: 0},
			wantBadFields: []string{"customer_name", "seats", "Idempotency-Key"},
		},
		{
			name:          "too long idempotency key is rejected",
			setupMocks:    func(*mocks.MockRepository) {},
			cmd:           withKey(strings.Repeat("k", 256)),
			wantBadFields: []string{"Idempotency-Key"},
		},
		{
			name:          "idempotency key with a space is rejected",
			setupMocks:    func(*mocks.MockRepository) {},
			cmd:           withKey("key 1"),
			wantBadFields: []string{"Idempotency-Key"},
		},
		{
			name:          "non-ASCII idempotency key is rejected",
			setupMocks:    func(*mocks.MockRepository) {},
			cmd:           withKey("ключ"),
			wantBadFields: []string{"Idempotency-Key"},
		},
		{
			name: "domain failure stays identifiable",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					CreateBooking(gomock.Any(), gomock.Any()).
					Return(model.CreateResponse{}, model.ErrIdempotencyKeyReused)
			},
			cmd:     valid,
			wantErr: model.ErrIdempotencyKeyReused,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			repository := mocks.NewMockRepository(gomock.NewController(t))
			test.setupMocks(repository)
			subject := service.NewService(repository)

			// Act
			got, err := subject.Create(t.Context(), test.cmd)

			// Assert
			require.Equal(t, test.want, got)
			requireOutcome(t, err, test.wantErr, test.wantBadFields)
		})
	}
}

// requireOutcome asserts success, a wrapped cause, or the invalid input fields.
func requireOutcome(t *testing.T, err, wantErr error, wantBadFields []string) {
	t.Helper()

	if wantBadFields != nil {
		var invalid *validation.Error

		require.ErrorAs(t, err, &invalid)

		fields := make([]string, 0, len(invalid.Fields))
		for _, field := range invalid.Fields {
			fields = append(fields, field.Field)
		}

		require.Equal(t, wantBadFields, fields)

		return
	}

	if wantErr != nil {
		require.ErrorIs(t, err, wantErr)

		return
	}

	require.NoError(t, err)
}
