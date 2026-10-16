# Load local settings, if present, and pass them to every command.
-include .env
export

.PHONY: contract test-personel-live up up-app up-cctv down reset-db logs ps psql psql-app run worker migrate seed generate lint test check

# postgres, redis, minio
up:
	docker compose up -d --wait

# + migrate, api and worker in containers (no --wait: migrate exits when done)
up-app:
	docker compose --profile app up -d --build
	docker compose ps

# + MediaMTX
up-cctv:
	docker compose --profile cctv up -d --wait

down:
	docker compose --profile app --profile cctv down

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

# Background jobs (session cleanup, idempotency cleanup, audit partitions). Needs Redis.
worker:
	go run ./cmd/worker

# Apply migrations from the host, as the owner (MIGRATE_DSN). Also: go run ./cmd/migrate down|status
migrate:
	go run ./cmd/migrate up

# Development seed. For identity it only prints who can log in: people come from Personalia.
seed:
	go run ./cmd/seed

# Where the PERSIST contract lives (set CONTRACT_SRC in .env to override).
CONTRACT_SRC ?= ../persist-ecosystem/contracts/openapi.yaml

# Vendor the PERSIST contract into api/openapi.yaml and regenerate internal/apigen (models only).
contract:
	cp "$(CONTRACT_SRC)" api/openapi.yaml
	cd api && go generate ./...

# sqlc code from db/queries into internal/db. Phase 00.3 adds the contract types.
generate:
	sqlc generate

lint:
	golangci-lint run

test:
	go test -race ./...

# Contract test against a real kpi-personalia-api seeded with db/seed/40_siapp.sql. Set
# PERSONEL_LIVE_URL (the integration root) and PERSONEL_LIVE_KEY; it skips without them.
test-personel-live:
	go test -tags personel_live -count=1 -v -run Live ./pkg/platform/personel

# Generated code must be committed: regenerate, then fail if that changed anything. The
# contract is re-vendored too when the persist-ecosystem checkout is next to this repo.
# Tests need `make up` (Postgres + Redis) and MIGRATE_DSN.
check:
	sqlc generate
	@if [ -f "$(CONTRACT_SRC)" ]; then $(MAKE) contract; else cd api && go generate ./...; fi
	git diff --exit-code -- internal/db internal/apigen api
	go vet ./...
	golangci-lint run
	go test -race ./...
