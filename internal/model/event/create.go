package event

import "time"

type (
	// CreateServiceCommand is the caller's request to create an event.
	CreateServiceCommand struct {
		Name     string
		StartsAt time.Time
		Capacity int
	}

	// CreateRepositoryCommand is a validated and normalized event to store.
	CreateRepositoryCommand struct {
		Name     string
		StartsAt time.Time
		Capacity int
	}
)
