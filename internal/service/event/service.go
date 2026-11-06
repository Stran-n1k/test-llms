// Package event implements the business operations on events.
package event

import (
	"context"
	"fmt"

	model "booking/internal/model/event"
	"booking/internal/model/page"
	"booking/internal/model/validation"

	"github.com/google/uuid"
)

const (
	minStartsAtYear = 1
	maxStartsAtYear = 9999
)

// Service owns the rules for creating and reading events.
type Service struct {
	repository Repository
}

// NewService returns a Service backed by the repository.
func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

// Create validates and stores a new event.
func (s *Service) Create(ctx context.Context, cmd model.CreateServiceCommand) (model.Event, error) {
	var problems validation.Collector

	name := problems.NormalizeText("name", cmd.Name)

	// Responses carry timestamps in UTC, and JSON cannot represent years outside
	// this range, so an event beyond it could be stored but never read back.
	switch year := cmd.StartsAt.UTC().Year(); {
	case cmd.StartsAt.IsZero():
		problems.Add("starts_at", "is required")
	case year < minStartsAtYear || year > maxStartsAtYear:
		problems.Add("starts_at", fmt.Sprintf("must be between years %d and %d in UTC", minStartsAtYear, maxStartsAtYear))
	}

	if cmd.Capacity < 1 {
		problems.Add("capacity", "must be at least 1")
	}

	if err := problems.Err(); err != nil {
		return model.Event{}, err
	}

	created, err := s.repository.CreateEvent(ctx, model.CreateRepositoryCommand{
		Name:     name,
		StartsAt: cmd.StartsAt,
		Capacity: cmd.Capacity,
	})
	if err != nil {
		return model.Event{}, fmt.Errorf("create event: %w", err)
	}

	return created, nil
}

// Fetch returns one event or model.ErrNotFound.
func (s *Service) Fetch(ctx context.Context, id uuid.UUID) (model.Event, error) {
	found, err := s.repository.FetchEvent(ctx, id)
	if err != nil {
		return model.Event{}, fmt.Errorf("fetch event: %w", err)
	}

	return found, nil
}

// Search returns one page of events.
func (s *Service) Search(ctx context.Context, qry model.SearchServiceQuery) (page.Page[model.Event], error) {
	var problems validation.Collector

	window := qry.Page.Resolve(&problems)
	if err := problems.Err(); err != nil {
		return page.Page[model.Event]{}, err
	}

	found, err := s.repository.SearchEvents(ctx, model.SearchRepositoryQuery{Window: window})
	if err != nil {
		return page.Page[model.Event]{}, fmt.Errorf("search events: %w", err)
	}

	return found, nil
}
