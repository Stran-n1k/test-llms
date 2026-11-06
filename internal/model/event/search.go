package event

import "booking/internal/model/page"

type (
	// SearchServiceQuery is the caller's request to list events.
	SearchServiceQuery struct {
		Page page.Request
	}

	// SearchRepositoryQuery selects one validated page of events.
	SearchRepositoryQuery struct {
		Window page.Window
	}
)
