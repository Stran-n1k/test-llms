#!/usr/bin/env bash
# Runs unit + integration tests (with the race detector) against a
# disposable PostgreSQL container. Requires docker and go.
set -euo pipefail
cd "$(dirname "$0")/.."

PORT="${BOOKING_TEST_PG_PORT:-55432}"
CONTAINER="booking-itest-$$"
DSN="postgres://booking:booking@localhost:${PORT}/booking_test?sslmode=disable"

cleanup() { docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker run -d --rm --name "${CONTAINER}" -p "${PORT}:5432" \
	-e POSTGRES_USER=booking -e POSTGRES_PASSWORD=booking -e POSTGRES_DB=booking_test \
	postgres:16-alpine >/dev/null

echo "Waiting for PostgreSQL on port ${PORT}..."
for _ in $(seq 1 120); do
	if docker exec "${CONTAINER}" pg_isready -U booking -d booking_test >/dev/null 2>&1; then
		break
	fi
	sleep 0.5
done

export BOOKING_TEST_DATABASE_URL="${DSN}"
go test -race -count=1 -timeout 10m ./...
