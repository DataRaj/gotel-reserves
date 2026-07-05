.PHONY: build build-prod docker-build run dev test tidy lint clean sqlc migrateup migratedown new_migration infra-up infra-down

## Build the production binary into bin/
build:
	@go build -o bin/recallo ./cmd/api/...

## Run the already-built binary
run:
	@./bin/recallo

## Build then immediately run (hot workflow)
dev:
	@go build -o bin/recallo ./cmd/api/... && ./bin/recallo

## Run all tests with verbose output
test:
	@go test -v ./...

## Tidy and verify module dependencies
tidy:
	@go mod tidy
	@go mod verify

## Run go vet + staticcheck linters
lint:
	@go vet ./...

## Generate sqlc models and queries
sqlc:
	@sqlc generate

## Create a new migration file (e.g., make new_migration name=add_users_table)
new_migration:
	@migrate create -ext sql -dir db/migrations -seq $(name)

## Apply all up migrations
migrateup:
	@migrate -path db/migrations -database "$$DATABASE_URL" -verbose up

## Revert all down migrations
migratedown:
	@migrate -path db/migrations -database "$$DATABASE_URL" -verbose down

## Static binary for Linux/amd64 production deployment
build-prod:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -ldflags="-s -w" -trimpath -o bin/recallo ./cmd/api

## Build Docker image (scratch-based, ~5MB)
docker-build:
	docker build -t recallo/api:$(shell git rev-parse --short HEAD) .

## Start local dev infrastructure (Redis only — Postgres is external)
infra-up:
	docker compose -f deployments/docker-compose.dev.yml up -d

## Stop local dev infrastructure
infra-down:
	docker compose -f deployments/docker-compose.dev.yml down

## Remove compiled artifacts
clean:
	@rm -rf bin/
