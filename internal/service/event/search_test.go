package event_test

import (
	"testing"

	model "booking/internal/model/event"
	"booking/internal/model/page"
	service "booking/internal/service/event"
	"booking/internal/service/event/mocks"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestService_Search(t *testing.T) {
	t.Parallel()

	invalidLimit := 0
	found := page.Page[model.Event]{Items: []model.Event{{Name: "Concert"}}, Limit: page.DefaultLimit}

	tests := []struct {
		name          string
		setupMocks    func(*mocks.MockRepository)
		qry           model.SearchServiceQuery
		want          page.Page[model.Event]
		wantBadFields []string
	}{
		{
			name: "omitted pagination reaches the repository with defaults",
			setupMocks: func(repository *mocks.MockRepository) {
				repository.EXPECT().
					SearchEvents(gomock.Any(), model.SearchRepositoryQuery{Window: page.Window{Limit: page.DefaultLimit, Offset: 0}}).
					Return(found, nil)
			},
			want: found,
		},
		{
			name:          "invalid pagination is rejected without a repository call",
			setupMocks:    func(*mocks.MockRepository) {},
			qry:           model.SearchServiceQuery{Page: page.Request{Limit: &invalidLimit}},
			wantBadFields: []string{"limit"},
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
