package event_test

import (
	"testing"

	model "booking/internal/model/event"
	service "booking/internal/service/event"
	"booking/internal/service/event/mocks"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestService_Fetch(t *testing.T) {
	t.Parallel()

	id := uuid.New()

	tests := []struct {
		name       string
		setupMocks func(*mocks.MockRepository)
		want       model.Event
		wantErr    error
	}{
		{
			name: "stored event is returned",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					FetchEvent(gomock.Any(), id).
					Return(model.Event{ID: id}, nil)
			},
			want: model.Event{ID: id},
		},
		{
			name: "missing event stays identifiable",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					FetchEvent(gomock.Any(), id).
					Return(model.Event{}, model.ErrNotFound)
			},
			wantErr: model.ErrNotFound,
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
			got, err := subject.Fetch(t.Context(), id)

			// Assert
			require.Equal(t, test.want, got)
			requireOutcome(t, err, test.wantErr, nil)
		})
	}
}
