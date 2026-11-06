package event_test

import (
	"errors"
	"testing"
	"time"

	model "booking/internal/model/event"
	"booking/internal/model/validation"
	service "booking/internal/service/event"
	"booking/internal/service/event/mocks"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestService_Create(t *testing.T) {
	t.Parallel()

	startsAt := time.Date(2030, time.January, 31, 19, 0, 0, 0, time.UTC)
	stored := model.Event{Name: "Concert", StartsAt: startsAt, Capacity: 10}
	errStorage := errors.New("storage failed")

	tests := []struct {
		name          string
		setupMocks    func(*mocks.MockRepository)
		cmd           model.CreateServiceCommand
		want          model.Event
		wantErr       error
		wantBadFields []string
	}{
		{
			name: "normalized event is stored",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					CreateEvent(gomock.Any(), model.CreateRepositoryCommand{Name: "Concert", StartsAt: startsAt, Capacity: 10}).
					Return(stored, nil)
			},
			cmd:  model.CreateServiceCommand{Name: "  Concert ", StartsAt: startsAt, Capacity: 10},
			want: stored,
		},
		{
			name:          "every invalid field is reported without a repository call",
			setupMocks:    func(*mocks.MockRepository) {},
			cmd:           model.CreateServiceCommand{Name: " ", Capacity: 0},
			wantBadFields: []string{"name", "starts_at", "capacity"},
		},
		{
			name:          "negative capacity is rejected",
			setupMocks:    func(*mocks.MockRepository) {},
			cmd:           model.CreateServiceCommand{Name: "Concert", StartsAt: startsAt, Capacity: -1},
			wantBadFields: []string{"capacity"},
		},
		{
			name:          "start beyond year 9999 in UTC is rejected",
			setupMocks:    func(*mocks.MockRepository) {},
			cmd:           model.CreateServiceCommand{Name: "Concert", StartsAt: time.Date(9999, time.December, 31, 23, 59, 59, 0, time.FixedZone("", -3*60*60)), Capacity: 1},
			wantBadFields: []string{"starts_at"},
		},
		{
			name:          "start before year 1 in UTC is rejected",
			setupMocks:    func(*mocks.MockRepository) {},
			cmd:           model.CreateServiceCommand{Name: "Concert", StartsAt: time.Date(0, time.January, 1, 0, 0, 0, 0, time.UTC), Capacity: 1},
			wantBadFields: []string{"starts_at"},
		},
		{
			name: "repository failure keeps its cause",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					CreateEvent(gomock.Any(), gomock.Any()).
					Return(model.Event{}, errStorage)
			},
			cmd:     model.CreateServiceCommand{Name: "Concert", StartsAt: startsAt, Capacity: 10},
			wantErr: errStorage,
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
