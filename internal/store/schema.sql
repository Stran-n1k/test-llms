CREATE TABLE events (
    id uuid PRIMARY KEY,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    capacity integer NOT NULL CHECK (capacity BETWEEN 1 AND 1000000),
    available integer NOT NULL CHECK (available >= 0 AND available <= capacity),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE bookings (
    id uuid PRIMARY KEY,
    event_id uuid NOT NULL REFERENCES events(id),
    quantity integer NOT NULL CHECK (quantity BETWEEN 1 AND 1000000),
    idempotency_key text NOT NULL UNIQUE
        CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{1,128}$'),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    cancelled_at timestamptz,
    CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
);

CREATE INDEX bookings_event_id_idx ON bookings(event_id);
