# Load local settings, if present, and pass them to every command.
-include .env
export

.PHONY: up up-stub up-app up-cctv down reset-db logs ps psql psql-app run worker migrate generate lint test check

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

# Background jobs (session cleanup, ...). Needs Redis. cmd/worker arrives in Phase 01.2.
worker:
	go run ./cmd/worker

# Apply migrations from the host, as the owner. Migrations start in Phase 00.2.
migrate:
	goose -dir db/migrations postgres "$$MIGRATE_DSN" up

# sqlc code from db/queries. sqlc.yaml arrives in Phase 00.2; 00.3 adds the contract types.
generate:
	sqlc generate

lint:
	golangci-lint run

test:
	go test -race ./...

# Phase 00.2 puts `sqlc generate` + a no-diff check in front, and 00.3 adds `make contract`.
check:
	go vet ./...
	golangci-lint run
	go test -race ./...
