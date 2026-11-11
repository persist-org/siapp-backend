# CI/CD for SiAPP

This guide sets up:

- **A pull request to `development`** runs the checks: formatting, generated code in sync, vet, lint, build, unit and integration tests, and a Docker build.
- **A push to `development`** (a merged pull request) runs the same checks again, then updates the server: rebuild, database migrations, health check, and the Swagger page.

```
PR -> development      verify  (format, generated code, vet, lint, build, tests, docker build)
push to development    verify  ->  deploy (SSH to the server -> deploy.sh -> rebuild, migrate, health check, swagger)
```

> Words used here
> - **CI** = checks that run automatically on every pull request.
> - **CD** = the deploy that runs automatically after a merge.
> - **Runner** = the temporary GitHub machine that runs the workflow.
> - **Secret** = a value stored in GitHub settings that the workflow can read but nobody can view again.

---

## 0. Before you start

1. **A secret may be in `DEPLOY.md`.** Line 473 holds what looks like a real `DB_APP_PASSWORD`. Replace it with `PASTE_DB_APP_PASSWORD` before the file is ever committed. Because the value was pasted into a file, also change it on the server: `ALTER ROLE siapp_app PASSWORD '<new>'`, then update `.env`.
2. **Fix the one file that is not formatted.** `pkg/media/flow_test.go` fails `gofmt` and the linter, so CI would be red from day one. Run `gofmt -w pkg/media/flow_test.go` in the same pull request that adds CI.
3. **Never use `make check` or `make contract` in CI.** They copy the older contract from the persist-ecosystem checkout over `api/openapi.yaml` and destroy earlier phases' contract work. The workflow below runs the same steps separately.
4. **Use a branch of its own for these files**, for example `jonathan/chore/ci-cd`, with its own pull request to `development`. Another open pull request gets no CI checks until the workflow exists on `development`. After the CI pull request merges, update the other branch with `development` and the checks run.

---

## 1. Server (once)

Use the Linux user that owns the project folder and has the git deploy key from `DEPLOY.md` step 4. This guide calls it `DEPLOYUSER` (root, if you followed `DEPLOY.md` as root).

The guide writes `/srv/siapp` for the project folder, as `DEPLOY.md` does. **Use your real folder instead** (for example `/home/dev-app-workshop/siapp`). `deploy.sh` finds its own folder, so it needs no edit; only the `authorized_keys` line (1e) and your commands use the real path. Check that folder first:

```bash
cd /home/dev-app-workshop/siapp
git branch --show-current && git remote -v     # it must be the siapp-backend clone
ls -a                                          # .env must be here
git fetch origin development                   # must work as this user (git deploy key)
```

### 1a. Put `docker-compose.prod.yml` in the repo

It has no secrets, and the deploy script needs it. Use the contents from `DEPLOY.md` step 6a, plus the swagger service from 1c.

On the server, delete the copy you made by hand first, or `git reset --hard` will refuse because an untracked file would be overwritten:

```bash
rm /srv/siapp/docker-compose.prod.yml
```

### 1b. Add `deploy/deploy.sh` to the repo

Make it executable: `chmod +x deploy/deploy.sh`.

```bash
#!/usr/bin/env bash
# Run by the GitHub Actions SSH key only (forced command, see authorized_keys).
set -euo pipefail
cd "$(dirname "$(readlink -f "$0")")/.."     # the project folder, wherever it is

SHA="${SSH_ORIGINAL_COMMAND:-}"
[[ "$SHA" =~ ^[0-9a-f]{40}$ ]] || { echo "usage: a 40-character commit sha"; exit 2; }

exec 9>/tmp/siapp-deploy.lock
flock -n 9 || { echo "another deploy is running"; exit 1; }

DC="docker compose -f docker-compose.prod.yml"
PORT="$(grep -E '^SIAPP_PORT=' .env | cut -d= -f2)"; PORT="${PORT:-18080}"
PREV="$(git rev-parse HEAD)"

git fetch --prune origin development
# Only commits that are on development may be deployed.
git merge-base --is-ancestor "$SHA" origin/development || { echo "$SHA is not on development"; exit 1; }

# Backup before the migration runs; keep the last 10.
mkdir -p /srv/backups
$DC exec -T postgres pg_dump -U siapp_owner siapp | gzip > "/srv/backups/predeploy-$(date +%F-%H%M).sql.gz" \
  || echo "backup skipped (database not running?)"
ls -1t /srv/backups/predeploy-* 2>/dev/null | tail -n +11 | xargs -r rm --

git checkout -q development
git reset -q --hard "$SHA"
$DC up -d --build --remove-orphans      # migrate runs first, then api and worker
$DC restart swagger >/dev/null 2>&1 || true

for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${PORT}/api/v1/health" >/dev/null; then
    echo "deployed $SHA"; docker image prune -f >/dev/null; exit 0
  fi
  sleep 2
done

echo "health check failed, rolling back to $PREV"
git reset -q --hard "$PREV"
$DC up -d --build --remove-orphans
exit 1
```

What it does, in order: checks the commit is on `development`, backs up the database, updates the code, rebuilds and restarts, waits for the health check, and rolls back the code if the check never passes.

> **The rollback restores the old code, not the old database.** Keep migrations backward compatible: add columns and tables, and do not drop or rename in the same release. The pre-deploy dump in `/srv/backups` is the safety net.

### 1c. Add Swagger to the production compose file

Add this service to `docker-compose.prod.yml`:

```yaml
  swagger:
    image: swaggerapi/swagger-ui:v5.17.14
    restart: unless-stopped
    environment:
      SWAGGER_JSON: /spec/openapi.dev.yaml
      PERSIST_AUTHORIZATION: "true"
    volumes: ["./api:/spec:ro"]            # the folder, not the single file
    ports: ["127.0.0.1:18081:8080"]
```

Mount the **folder**, not `api/openapi.dev.yaml` alone. A single-file bind mount keeps pointing at the old file after `git reset` replaces it.

### 1d. Give Swagger a host name

The dev spec uses a same-origin server `/api/v1`, so "Try it out" only works when the page and the API are on the same host. Add an nginx site, for example `docs.example.com`:

```nginx
server {
    server_name docs.example.com;
    location /api/ { proxy_pass http://127.0.0.1:18080; proxy_set_header Host $host; proxy_set_header X-Real-IP $remote_addr; }
    location /    { proxy_pass http://127.0.0.1:18081; proxy_set_header Host $host; }
}
```

Then get a certificate:

```bash
certbot --nginx -d docs.example.com
```

Consider adding `auth_basic` to the site, because Swagger shows your whole API surface.

### 1e. Make the CI key, locked to the deploy script

On your own computer:

```bash
ssh-keygen -t ed25519 -C "github-actions-siapp" -f gh-deploy -N ""
```

On the server, add one line to `~DEPLOYUSER/.ssh/authorized_keys`. The line is the options below followed by the contents of `gh-deploy.pub`:

```
command="/home/dev-app-workshop/siapp/deploy/deploy.sh",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAA... github-actions-siapp
```

(Use the real path of your folder in `command=`. For root the file is `/root/.ssh/authorized_keys`.) This key can now run only that script. The script receives the commit sha as `SSH_ORIGINAL_COMMAND` and rejects anything that is not 40 hex characters.

Get the server's host key for GitHub. Copy the output:

```bash
ssh-keyscan -p 22 YOUR_SERVER_IP
```

---

## 2. GitHub settings

1. **Settings, Environments, New environment `development`.** Optionally add a required reviewer if you want a manual approval before each deploy. Add these as **environment secrets**:

   | Secret | Value |
   |---|---|
   | `SSH_HOST` | the server's IP or host name |
   | `SSH_USER` | `DEPLOYUSER` |
   | `SSH_PORT` | usually `22` |
   | `SSH_KEY` | the full contents of the private file `gh-deploy` |
   | `SSH_KNOWN_HOSTS` | the `ssh-keyscan` output |

   Add one **environment variable**: `API_URL`, for example `https://api.example.com`.

2. **Settings, Rules, Rulesets (or Branches).** Protect `development`:
   - require a pull request with at least 1 approval
   - require the status check `verify`
   - block force pushes
   - optionally restrict who can push

   Then "push to development" always means "a pull request was merged".

3. **Settings, Actions, General.** Set workflow permissions to **read only**.

---

## 3. The workflow

Create `.github/workflows/ci-cd.yml`. The tool versions match what the project uses locally: sqlc v1.31.1, oapi-codegen v2.8.0, golangci-lint v2.14.0.

```yaml
name: CI-CD

on:
  pull_request:
    branches: [development]
  push:
    branches: [development]

permissions:
  contents: read

concurrency:
  group: ci-${{ github.event_name }}-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

jobs:
  verify:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    services:
      postgres:
        image: postgres:16
        env:
          POSTGRES_USER: siapp_owner
          POSTGRES_PASSWORD: siapp_owner
          POSTGRES_DB: siapp
          TZ: UTC
        ports: ["5432:5432"]
        options: >-
          --health-cmd "pg_isready -U siapp_owner -d siapp"
          --health-interval 5s --health-timeout 3s --health-retries 20
      redis:
        image: redis:7
        ports: ["6379:6379"]
        options: >-
          --health-cmd "redis-cli ping"
          --health-interval 5s --health-timeout 3s --health-retries 20
      minio:
        image: bitnamilegacy/minio
        env:
          MINIO_ROOT_USER: minioadmin
          MINIO_ROOT_PASSWORD: minioadmin
          MINIO_DEFAULT_BUCKETS: persist-media
        ports: ["9000:9000"]
    env:
      MIGRATE_DSN: postgres://siapp_owner:siapp_owner@localhost:5432/siapp?sslmode=disable
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Create the app database role
        run: PGPASSWORD=siapp_owner psql -h localhost -U siapp_owner -d siapp -v ON_ERROR_STOP=1 -f deploy/postgres/init.sql

      - name: Wait for MinIO
        run: |
          for _ in $(seq 1 30); do curl -fsS http://localhost:9000/minio/health/live && exit 0; sleep 2; done
          echo "MinIO did not start"; exit 1

      - name: Formatting (gofmt)
        run: |
          out="$(gofmt -l $(git ls-files '*.go' | grep -v '^_legacy/'))"
          test -z "$out" || { echo "Not formatted:"; echo "$out"; exit 1; }

      - name: go.mod is tidy
        run: go mod tidy && git diff --exit-code go.mod go.sum

      - name: Install code generators
        run: |
          go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
          go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0
          echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"

      - name: Generated code is committed (sqlc, contract models)
        run: |
          sqlc generate
          (cd api && go generate ./...)
          git diff --exit-code -- internal/db internal/apigen

      - name: Dev swagger spec is committed
        run: |
          pip install pyyaml==6.0.1
          python3 scripts/make-dev-spec.py
          git diff --exit-code -- api/openapi.dev.yaml

      - run: go vet ./...
      - run: go build ./...

      - uses: golangci/golangci-lint-action@v8
        with:
          version: v2.14.0

      - name: Tests
        run: go test -race -count=1 ./...

      - name: Docker image builds
        run: docker build -t siapp:ci .

  deploy:
    needs: verify
    if: github.event_name == 'push' && github.ref == 'refs/heads/development'
    runs-on: ubuntu-latest
    timeout-minutes: 20
    environment: development
    concurrency:
      group: deploy-development
      cancel-in-progress: false
    steps:
      - name: Deploy over SSH
        env:
          SSH_HOST: ${{ secrets.SSH_HOST }}
          SSH_USER: ${{ secrets.SSH_USER }}
          SSH_PORT: ${{ secrets.SSH_PORT }}
          SSH_KEY: ${{ secrets.SSH_KEY }}
          SSH_KNOWN_HOSTS: ${{ secrets.SSH_KNOWN_HOSTS }}
        run: |
          install -m 700 -d ~/.ssh
          printf '%s\n' "$SSH_KEY" > ~/.ssh/deploy_key && chmod 600 ~/.ssh/deploy_key
          printf '%s\n' "$SSH_KNOWN_HOSTS" > ~/.ssh/known_hosts
          ssh -i ~/.ssh/deploy_key -p "${SSH_PORT:-22}" -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes \
            "$SSH_USER@$SSH_HOST" "$GITHUB_SHA"

      - name: Smoke test
        run: curl -fsS --retry 5 --retry-delay 3 "${{ vars.API_URL }}/api/v1/health"
```

Notes on the workflow:

- **The first run may expose gaps.** Some tests may depend on `.env` values that CI does not have. The suite has been run locally with `.env` loaded, not in a bare CI environment, so expect to adjust after the first run.
- **Go version.** CI uses the version in `go.mod` (1.26), the same as the Dockerfile. Your laptop may have a newer one.
- **MinIO.** Its tests fail (not skip) when MinIO is missing, which is why it is a service container.
- **Secrets** reach the shell as environment variables, never inside the script text.
- **Why one file.** The deploy job lives in the same workflow as `verify`, so it only runs after the checks pass on that exact push.

---

## 4. Check that it works

1. Merge the CI pull request. Add the environment, its secrets and the branch protection from section 2 **before** the first push, or the deploy job will fail.
2. **PR check.** Open a test pull request to `development` with a deliberate format error. `verify` should fail and block the merge. Fix it and it should go green.
3. **Deploy check.** Merge a pull request. In the Actions tab watch `verify`, then `deploy`. On the server:
   ```bash
   docker compose -f docker-compose.prod.yml ps
   ```
   The new containers should be up, and `migrate` should show **Exited (0)**.
4. **Swagger check.** Open the docs host. The operations from the latest `api/openapi.dev.yaml` should be listed.
5. **Rollback check (optional).** On a throwaway commit, break the health check and confirm the deploy log says `rolling back`.

---

## 5. When something goes wrong

| What you see | Most likely reason | What to do |
|---|---|---|
| `verify` fails at formatting | A Go file is not gofmt-ed | `gofmt -w <file>` and push |
| `verify` fails at "Generated code is committed" | `db/queries`, `api/openapi.yaml` or the migrations changed but the generated code was not committed | Run `sqlc generate` and `cd api && go generate ./...` locally, commit the result |
| `verify` fails at "Dev swagger spec is committed" | `api/openapi.yaml` or `api/built-routes.txt` changed | Run `python3 scripts/make-dev-spec.py` and commit `api/openapi.dev.yaml` |
| Tests fail only in CI | A test relies on `.env` or on a local service | Read the failing test's setup; set the missing variable in the job's `env:` |
| MinIO tests fail | MinIO was not ready | Check the "Wait for MinIO" step output |
| `deploy` fails: `Permission denied (publickey)` | Wrong `SSH_KEY`, or the line in `authorized_keys` is wrong | Redo 1e; the key must be the private file `gh-deploy` |
| `deploy` fails: `Host key verification failed` | `SSH_KNOWN_HOSTS` is wrong or the server changed | Run `ssh-keyscan` again and update the secret |
| Deploy log: `... is not on development` | The commit is not on `development` | Deploy only from merges to `development` |
| Deploy log: `another deploy is running` | Two deploys overlapped | Wait and re-run the job |
| `git reset` refuses: untracked files would be overwritten | A hand-made `docker-compose.prod.yml` is still on the server | Delete it (step 1a) |
| Deploy log: `rolling back` | The health check never passed | On the server: `docker compose -f docker-compose.prod.yml logs --tail 100 api`, fix, push again |
| Swagger shows the old spec | The folder is not mounted, or the container did not restart | Check the volume in 1c, run `docker compose -f docker-compose.prod.yml restart swagger` |

---

## 6. Good to know

- **The server follows `development`.** The script switches it from `main` automatically on the first deploy.
- **The server builds the image itself** (`up -d --build`), as in `DEPLOY.md`. If builds become slow, the usual next step is to build in CI, push the image to GitHub Container Registry and have the server pull it.
- **Action versions are pinned by tag.** For tighter supply-chain safety, pin them by commit SHA and turn on Dependabot for `github-actions`.
- **Update `DEPLOY.md` section 11** to say that deploys now go through CI, so nobody runs `git pull` by hand.

---

## Checklist

- [ ] Password removed from `DEPLOY.md` and rotated on the server
- [ ] `pkg/media/flow_test.go` formatted
- [ ] `docker-compose.prod.yml` (with swagger) and `deploy/deploy.sh` committed; the hand-made copy deleted on the server
- [ ] Docs host in nginx, certificate issued
- [ ] CI key made, locked to `deploy.sh` in `authorized_keys`
- [ ] GitHub environment `development` with the five secrets and `API_URL`
- [ ] Branch protection on `development` requiring `verify`
- [ ] `.github/workflows/ci-cd.yml` added
- [ ] Test PR goes red then green; a merge deploys; Swagger shows the new spec
