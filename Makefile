# Load local settings for `make run`, if present.
-include .env
export

.PHONY: up down run lint test check

up:
	docker compose up -d --wait

down:
	docker compose down

run:
	go run ./cmd/api

lint:
	golangci-lint run

test:
	go test -race ./...

check:
	go vet ./...
	golangci-lint run
	go test -race ./...
