.PHONY: help build run test test-unit test-integration fmt vet up down logs clean

help:
	@echo "build            - build all packages"
	@echo "test             - unit + integration tests (integration needs Docker or TEST_DATABASE_URL)"
	@echo "test-unit        - tests that do not need PostgreSQL"
	@echo "test-integration - integration tests against a disposable PostgreSQL"
	@echo "up               - start app and PostgreSQL with docker compose"
	@echo "down             - stop docker compose services (data volume is kept)"
	@echo "logs             - follow app logs"

build:
	go build ./...

run:
	go run ./cmd/server

test: test-unit test-integration

test-unit:
	go test ./internal/api/... ./internal/config/... ./internal/migrate/... ./internal/model/... ./internal/store/...

test-integration:
	go test ./internal/integration/... -count=1

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f app
