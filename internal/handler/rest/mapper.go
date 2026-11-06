package rest

import (
	"booking/internal/handler/rest/gen"
	bookingmodel "booking/internal/model/booking"
	eventmodel "booking/internal/model/event"
)

func mapEvent(event eventmodel.Event) gen.Event {
	return gen.Event{
		Id:             event.ID,
		Name:           event.Name,
		StartsAt:       event.StartsAt,
		Capacity:       int32(event.Capacity),         //nolint:gosec // Stored as a PostgreSQL integer.
		BookedSeats:    int32(event.BookedSeats),      //nolint:gosec // Bounded by capacity.
		AvailableSeats: int32(event.AvailableSeats()), //nolint:gosec // Bounded by capacity.
		CreatedAt:      event.CreatedAt,
	}
}

func mapBooking(booking bookingmodel.Booking) gen.Booking {
	return gen.Booking{
		Id:           booking.ID,
		EventId:      booking.EventID,
		CustomerName: booking.CustomerName,
		Seats:        int32(booking.Seats), //nolint:gosec // Stored as a PostgreSQL integer.
		Status:       gen.BookingStatus(booking.Status),
		CreatedAt:    booking.CreatedAt,
		CancelledAt:  booking.CancelledAt,
	}
}

func mapItems[S, D any](items []S, mapItem func(S) D) []D {
	mapped := make([]D, 0, len(items))
	for _, item := range items {
		mapped = append(mapped, mapItem(item))
	}

	return mapped
}

func mapOptionalInt(value *int32) *int {
	if value == nil {
		return nil
	}

	mapped := int(*value)

	return &mapped
}
