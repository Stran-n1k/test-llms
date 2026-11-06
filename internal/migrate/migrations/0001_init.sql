-- Core schema for the booking service.
--
-- The events.reserved_seats counter is the single source of truth for capacity
-- accounting.  It is updated atomically as part of every booking/cancellation
-- transaction; the CHECK constraint is a last line of defence and makes it
-- impossible to persist a state where more seats are reserved than exist.

CREATE TABLE events (
    id             uuid        PRIMARY KEY,
    name           text        NOT NULL CHECK (btrim(name) <> ''),
    capacity       integer     NOT NULL CHECK (capacity > 0),
    reserved_seats integer     NOT NULL DEFAULT 0 CHECK (reserved_seats >= 0),
    starts_at      timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT events_reserved_within_capacity CHECK (reserved_seats <= capacity)
);

CREATE TABLE bookings (
    id              uuid        PRIMARY KEY,
    event_id        uuid        NOT NULL REFERENCES events (id) ON DELETE RESTRICT,
    seats           integer     NOT NULL CHECK (seats > 0),
    status          text        NOT NULL DEFAULT 'confirmed',
    customer_name   text        NOT NULL DEFAULT '',
    customer_email  text        NOT NULL DEFAULT '',
    idempotency_key text        NOT NULL CHECK (btrim(idempotency_key) <> ''),
    request_hash    text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    cancelled_at    timestamptz,
    CONSTRAINT bookings_status_valid CHECK (status IN ('confirmed', 'cancelled')),
    CONSTRAINT bookings_cancelled_at_matches_status
        CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL)),
    CONSTRAINT bookings_event_idempotency_key_unique UNIQUE (event_id, idempotency_key)
);

CREATE INDEX bookings_event_id_idx ON bookings (event_id);
CREATE INDEX bookings_created_at_idx ON bookings (created_at);
CREATE INDEX events_starts_at_idx ON events (starts_at);
