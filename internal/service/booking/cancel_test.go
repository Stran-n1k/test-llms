package booking_test

import (
	"testing"

	model "booking/internal/model/booking"
	service "booking/internal/service/booking"
	"booking/internal/service/booking/mocks"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestService_Cancel(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	cancelled := model.Booking{ID: id, Status: model.StatusCancelled}

	tests := []struct {
		name       string
		setupMocks func(*mocks.MockRepository)
		want       model.Booking
		wantErr    error
	}{
		{
			name: "cancelled booking is returned",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					CancelBooking(gomock.Any(), id).
					Return(cancelled, nil)
			},
			want: cancelled,
		},
		{
			name: "missing booking stays identifiable",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					CancelBooking(gomock.Any(), id).
					Return(model.Booking{}, model.ErrNotFound)
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
			got, err := subject.Cancel(t.Context(), id)

			// Assert
			require.Equal(t, test.want, got)
			requireOutcome(t, err, test.wantErr, nil)
		})
	}
}
