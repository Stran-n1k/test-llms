package validation_test

import (
	"strings"
	"testing"

	"booking/internal/model/validation"

	"github.com/stretchr/testify/require"
)

func TestCollector_NormalizeText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		value       string
		want        string
		wantProblem string
	}{
		{name: "surrounding whitespace is trimmed", value: "  Anna  ", want: "Anna"},
		{name: "length is counted in characters", value: strings.Repeat("ю", validation.MaxTextLength), want: strings.Repeat("ю", validation.MaxTextLength)},
		{name: "blank is rejected", value: " \t ", want: "", wantProblem: "is required"},
		{
			name:        "too long is rejected",
			value:       strings.Repeat("a", validation.MaxTextLength+1),
			want:        strings.Repeat("a", validation.MaxTextLength+1),
			wantProblem: "must be at most 200 characters",
		},
		{name: "NUL is rejected because PostgreSQL text cannot store it", value: "a\x00b", want: "a\x00b", wantProblem: "must not contain control characters"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// Arrange
			var problems validation.Collector

			// Act
			got := problems.NormalizeText("name", test.value)

			// Assert
			require.Equal(t, test.want, got)

			err := problems.Err()
			if test.wantProblem == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, "name: "+test.wantProblem)
		})
	}
}
