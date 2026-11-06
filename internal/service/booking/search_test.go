package booking_test

import (
	"testing"

	model "booking/internal/model/booking"
	"booking/internal/model/page"
	service "booking/internal/service/booking"
	"booking/internal/service/booking/mocks"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestService_Search(t *testing.T) {
	t.Parallel()

	eventID := uuid.New()
	limit, offset, invalidOffset := 5, 10, -1
	found := page.Page[model.Booking]{Items: []model.Booking{{EventID: eventID}}, Limit: 5, Offset: 10}

	tests := []struct {
		name          string
		setupMocks    func(*mocks.MockRepository)
		qry           model.SearchServiceQuery
		want          page.Page[model.Booking]
		wantBadFields []string
	}{
		{
			name: "requested pagination reaches the repository",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					SearchBookings(gomock.Any(), model.SearchRepositoryQuery{EventID: eventID, Window: page.Window{Limit: 5, Offset: 10}}).
					Return(found, nil)
			},
			qry:  model.SearchServiceQuery{EventID: eventID, Page: page.Request{Limit: &limit, Offset: &offset}},
			want: found,
		},
		{
			name:          "invalid pagination is rejected without a repository call",
			setupMocks:    func(*mocks.MockRepository) {},
			qry:           model.SearchServiceQuery{EventID: eventID, Page: page.Request{Offset: &invalidOffset}},
			wantBadFields: []string{"offset"},
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
			got, err := subject.Search(t.Context(), test.qry)

			// Assert
			require.Equal(t, test.want, got)
			requireOutcome(t, err, nil, test.wantBadFields)
		})
	}
}
