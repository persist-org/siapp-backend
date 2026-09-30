# Load local settings, if present, and pass them to every command.
-include .env
export

.PHONY: up up-db down run migrate seed generate lint test check

# Mailpit and MinIO only. Use native MySQL/Redis, or `make up-db` to run them in containers.
up:
	docker compose up -d --wait

up-db:
	docker compose --profile db up -d --wait

down:
	docker compose --profile db down

run:
	go run ./cmd/api

migrate:
	goose -dir db/migrations mysql "$$MYSQL_DSN" up

# Creates the first admin from SEED_ADMIN_EMAIL / SEED_ADMIN_PASSWORD; safe to repeat.
seed:
	go run ./cmd/seed

generate:
	sqlc generate

lint:
	golangci-lint run

test:
	go test -race ./...

# sqlc diff fails when internal/db is stale; run `make generate` and commit the result.
check:
	sqlc diff
	go vet ./...
	golangci-lint run
	go test -race ./...
