package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"booking/internal/domain"
)

// fakeStore records calls; every method returns a canned value.
type fakeStore struct {
	calls           map[string]int
	lastBookingArgs struct {
		eventID int64
		token   string
		seats   []int
	}
}

func newFakeStore() *fakeStore { return &fakeStore{calls: map[string]int{}} }

func (f *fakeStore) Ping(ctx context.Context) error    { return nil }
func (f *fakeStore) Migrate(ctx context.Context) error { return nil }
func (f *fakeStore) ListEvents(ctx context.Context) ([]domain.EventAvailability, error) {
	return nil, nil
}
func (f *fakeStore) GetEvent(ctx context.Context, id int64) (domain.EventAvailability, error) {
	return domain.EventAvailability{}, nil
}
func (f *fakeStore) GetBooking(ctx context.Context, id int64) (domain.Booking, error) {
	return domain.Booking{}, nil
}
func (f *fakeStore) CancelBooking(ctx context.Context, id int64) (domain.Booking, error) {
	return domain.Booking{}, nil
}
func (f *fakeStore) CreateEvent(ctx context.Context, name string, capacity int) (domain.Event, error) {
	f.calls["CreateEvent"]++
	return domain.Event{ID: 1, Name: name, Capacity: capacity}, nil
}
func (f *fakeStore) CreateBooking(ctx context.Context, eventID int64, token string, seats []int) (domain.Booking, bool, error) {
	f.calls["CreateBooking"]++
	f.lastBookingArgs.eventID = eventID
	f.lastBookingArgs.token = token
	f.lastBookingArgs.seats = seats
	return domain.Booking{ID: 1, EventID: eventID, Seats: seats, Status: domain.StatusConfirmed}, true, nil
}

func TestCreateEventValidation(t *testing.T) {
	cases := []struct {
		name       string
		eventName  string
		capacity   int
		wantErr    bool
		wantCalled bool
	}{
		{name: "valid", eventName: "Concert", capacity: 10, wantCalled: true},
		{name: "empty name", eventName: "  ", capacity: 10, wantErr: true},
		{name: "long name", eventName: string(make([]byte, 201)), capacity: 10, wantErr: true},
		{name: "zero capacity", eventName: "Concert", capacity: 0, wantErr: true},
		{name: "negative capacity", eventName: "Concert", capacity: -5, wantErr: true},
		{name: "capacity too large", eventName: "Concert", capacity: DefaultMaxCapacity + 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			svc := New(fs)
			_, err := svc.CreateEvent(context.Background(), tc.eventName, tc.capacity)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var validationErr *domain.ValidationError
			if tc.wantErr && !errors.As(err, &validationErr) {
				t.Fatalf("expected ValidationError, got %T: %v", err, err)
			}
			if got, want := fs.calls["CreateEvent"] == 1, tc.wantCalled; got != want {
				t.Fatalf("store called = %v, want %v", got, want)
			}
		})
	}
}

func TestCreateBookingValidation(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		seats   []int
		wantErr bool
	}{
		{name: "valid", token: "tok-1", seats: []int{2, 1}},
		{name: "empty token", token: "", seats: []int{1}, wantErr: true},
		{name: "whitespace token", token: "   ", seats: []int{1}, wantErr: true},
		{name: "no seats", token: "tok-1", seats: nil, wantErr: true},
		{name: "empty seats", token: "tok-1", seats: []int{}, wantErr: true},
		{name: "negative seat", token: "tok-1", seats: []int{0, -1}, wantErr: true},
		{name: "duplicate seats", token: "tok-1", seats: []int{3, 3}, wantErr: true},
		{name: "too many seats", token: "tok-1", seats: make([]int, DefaultMaxSeatsPerBooking+1), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeStore()
			svc := New(fs)
			_, _, err := svc.CreateBooking(context.Background(), 1, tc.token, tc.seats)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var validationErr *domain.ValidationError
			if tc.wantErr && !errors.As(err, &validationErr) {
				t.Fatalf("expected ValidationError, got %T: %v", err, err)
			}
			if tc.wantErr && fs.calls["CreateBooking"] != 0 {
				t.Fatalf("store must not be called on validation error")
			}
		})
	}
}

func TestCreateBookingSortsSeats(t *testing.T) {
	fs := newFakeStore()
	svc := New(fs)
	if _, _, err := svc.CreateBooking(context.Background(), 7, "tok", []int{5, 1, 3}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []int{1, 3, 5}
	if !slices.Equal(fs.lastBookingArgs.seats, want) {
		t.Fatalf("store received seats %v, want %v", fs.lastBookingArgs.seats, want)
	}
}

func TestServiceRequiresStore(t *testing.T) {
	fs := newFakeStore()
	svc := New(fs)
	if err := svc.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := svc.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}
