// Package page defines offset pagination: limit defaults to DefaultLimit and
// may be at most MaxLimit, offset defaults to 0. Results are ordered by creation
// time with the identifier as a tie-breaker.
package page

import (
	"fmt"

	"booking/internal/model/validation"
)

const (
	// DefaultLimit is the page size used when the caller does not choose one.
	DefaultLimit = 50
	// MaxLimit is the largest page size a caller may request.
	MaxLimit = 200
)

type (
	// Request is the pagination the caller asked for; nil means omitted.
	Request struct {
		Limit  *int
		Offset *int
	}

	// Window is a validated pagination with its defaults applied.
	Window struct {
		Limit  int
		Offset int
	}

	// Page is one page of results.
	Page[T any] struct {
		Items   []T
		Limit   int
		Offset  int
		HasMore bool
	}
)

// Resolve applies defaults to the request and records invalid values.
func (r Request) Resolve(problems *validation.Collector) Window {
	window := Window{Limit: DefaultLimit, Offset: 0}

	if r.Limit != nil {
		window.Limit = *r.Limit
		if window.Limit < 1 || window.Limit > MaxLimit {
			problems.Add("limit", fmt.Sprintf("must be between 1 and %d", MaxLimit))
		}
	}

	if r.Offset != nil {
		window.Offset = *r.Offset
		if window.Offset < 0 {
			problems.Add("offset", "must not be negative")
		}
	}

	return window
}
