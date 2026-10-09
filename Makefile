# Load local settings, if present, and pass them to every command.
-include .env
export

.PHONY: up up-stub up-app up-cctv down reset-db logs ps psql psql-app run worker migrate seed generate lint test check

# postgres, redis, minio
up:
	docker compose up -d --wait

# + the Personel stub
up-stub:
	docker compose --profile stub up -d --wait --build

# + migrate, api and worker in containers (no --wait: migrate exits when done)
up-app:
	docker compose --profile app up -d --build
	docker compose ps

# + MediaMTX
up-cctv:
	docker compose --profile cctv up -d --wait

down:
	docker compose --profile stub --profile app --profile cctv down

# Delete the database volume and start over (asks first)
reset-db:
	@read -p "Delete ALL local Postgres data? [y/N] " a && [ "$$a" = y ]
	docker compose down -v --remove-orphans

logs:
	docker compose logs -f api worker

ps:
	docker compose ps

# psql as the owner (sees every row)
psql:
	docker compose exec postgres psql -U siapp_owner -d siapp

# psql as siapp_app (RLS applies: set app.current_org first)
psql-app:
	docker compose exec -e PGPASSWORD=siapp_app postgres psql -h localhost -U siapp_app -d siapp

run:
	go run ./cmd/api

# Background jobs (session cleanup, ...). Needs Redis.
worker:
	go run ./cmd/worker

# Apply migrations from the host, as the owner
migrate:
	goose -dir db/migrations postgres "$$MIGRATE_DSN" up

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
