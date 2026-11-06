package booking

import (
	"booking/internal/model/page"

	"github.com/google/uuid"
)

type (
	// SearchServiceQuery is the caller's request to list an event's bookings.
	SearchServiceQuery struct {
		EventID uuid.UUID
		Page    page.Request
	}

	// SearchRepositoryQuery selects one validated page of an event's bookings.
	SearchRepositoryQuery struct {
		EventID uuid.UUID
		Window  page.Window
	}
)
