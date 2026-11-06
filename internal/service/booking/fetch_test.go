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

func TestService_Fetch(t *testing.T) {
	t.Parallel()

	id := uuid.New()

	tests := []struct {
		name       string
		setupMocks func(*mocks.MockRepository)
		want       model.Booking
		wantErr    error
	}{
		{
			name: "stored booking is returned",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					FetchBooking(gomock.Any(), id).
					Return(model.Booking{ID: id}, nil)
			},
			want: model.Booking{ID: id},
		},
		{
			name: "missing booking stays identifiable",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					FetchBooking(gomock.Any(), id).
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
			got, err := subject.Fetch(t.Context(), id)

			// Assert
			require.Equal(t, test.want, got)
			requireOutcome(t, err, test.wantErr, nil)
		})
	}
}
