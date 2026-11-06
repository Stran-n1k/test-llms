#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
# A fresh project name avoids touching an existing container, volume or network.
project="booking-test-$(date +%s)-$$"
cleanup() {
  docker compose -f compose.test.yaml -p "$project" down --volumes
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker compose -f compose.test.yaml -p "$project" up -d --wait --wait-timeout 60 db
address=$(docker compose -f compose.test.yaml -p "$project" port db 5432)
export TEST_DATABASE_URL="postgres://booking_test:booking_test@${address}/booking_test?sslmode=disable"
export REQUIRE_INTEGRATION=1
go test -race -count=1 -timeout=120s ./... "$@"
