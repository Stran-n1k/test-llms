package page_test

import (
	"testing"

	"booking/internal/model/page"
	"booking/internal/model/validation"

	"github.com/stretchr/testify/require"
)

func intPointer(value int) *int {
	return &value
}

func TestRequest_Resolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		request    page.Request
		want       page.Window
		wantFields []string
	}{
		{
			name:    "omitted values get defaults",
			request: page.Request{},
			want:    page.Window{Limit: page.DefaultLimit, Offset: 0},
		},
		{
			name:    "largest limit is accepted",
			request: page.Request{Limit: intPointer(page.MaxLimit), Offset: intPointer(7)},
			want:    page.Window{Limit: page.MaxLimit, Offset: 7},
		},
		{
			name:       "explicit zero limit is rejected instead of defaulted",
			request:    page.Request{Limit: intPointer(0)},
			want:       page.Window{Limit: 0, Offset: 0},
			wantFields: []string{"limit"},
		},
		{
			name:       "too large limit and negative offset are both reported",
			request:    page.Request{Limit: intPointer(page.MaxLimit + 1), Offset: intPointer(-1)},
			want:       page.Window{Limit: page.MaxLimit + 1, Offset: -1},
			wantFields: []string{"limit", "offset"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			var problems validation.Collector

			// Act
			got := test.request.Resolve(&problems)

			// Assert
			require.Equal(t, test.want, got)

			err := problems.Err()
			if test.wantFields == nil {
				require.NoError(t, err)

				return
			}

			var invalid *validation.Error

			require.ErrorAs(t, err, &invalid)

			fields := make([]string, 0, len(invalid.Fields))
			for _, field := range invalid.Fields {
				fields = append(fields, field.Field)
			}

			require.Equal(t, test.wantFields, fields)
		})
	}
}
