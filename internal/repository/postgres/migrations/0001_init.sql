CREATE TABLE events (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text        NOT NULL,
    starts_at    timestamptz NOT NULL,
    capacity     integer     NOT NULL,
    booked_seats integer     NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT events_capacity_positive CHECK (capacity > 0),
    -- Last line of defence against overselling and double refunds.
    CONSTRAINT events_booked_seats_range CHECK (booked_seats BETWEEN 0 AND capacity)
);

CREATE INDEX events_created_at_id_idx ON events (created_at, id);

CREATE TABLE bookings (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id        uuid        NOT NULL REFERENCES events (id),
    idempotency_key text        NOT NULL,
    customer_name   text        NOT NULL,
    seats           integer     NOT NULL,
    status          text        NOT NULL DEFAULT 'confirmed',
    created_at      timestamptz NOT NULL DEFAULT now(),
    cancelled_at    timestamptz,

    CONSTRAINT bookings_idempotency_key_unique UNIQUE (idempotency_key),
    CONSTRAINT bookings_seats_positive CHECK (seats > 0),
    CONSTRAINT bookings_status_known CHECK (status IN ('confirmed', 'cancelled')),
    CONSTRAINT bookings_cancelled_at_matches_status
        CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
);

CREATE INDEX bookings_event_id_created_at_id_idx ON bookings (event_id, created_at, id);
