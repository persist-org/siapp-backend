# Putting SiAPP on a server

This guide takes you from "I have an empty server" to "the API is running with HTTPS".
Do the steps in order. Every step says **what** it does and **why**.

> Words used here
> - **Server** = the Linux computer you rent (a VPS). We assume **Ubuntu 22.04 or 24.04**.
> - **SSH** = how you open a terminal on the server from your own computer.
> - **Docker** = a tool that runs programs in small boxes ("containers"). SiAPP, the database and Redis each run in their own box.
> - **Compose** = a file that tells Docker which boxes to start.
> - **Domain** = a name like `api.example.com` that points to your server.

---

## 0. What is running, and what you need

SiAPP is **not alone**. These pieces work together:

| Piece | Where it runs | You set it up in |
|---|---|---|
| **api** (the SiAPP server) | a box on your server | this guide |
| **worker** (background jobs) | a box on your server | this guide |
| **postgres** (database) | a box on your server | this guide |
| **redis** (fast storage for sessions and jobs) | a box on your server | this guide |
| **HTTPS proxy** (gives you HTTPS and sits in front of the api) | your server's **existing** nginx/Caddy (recommended), or our own caddy box if the server has none | this guide, step 6b |
| **kpi-personalia-api** (the Personalia / Persist Personel service: login and people data) | **somewhere else** | NOT this guide. It must already be running and reachable from your server. |

Before you start, have these ready:

1. The server's **IP address**, and a way to log in with SSH.
2. A **domain** that points to the server's IP (an "A record" in your DNS). HTTPS needs it.
   No domain? You can still try with the IP, but production mode wants an `https://` address, so a domain is strongly recommended.
3. The **address of kpi-personalia-api** (example: `https://personalia.example.com/api/v3/integrations/siapp`).
4. The **integration key** and **webhook secret** that Personalia and SiAPP share.
   They must be the same on both sides. Ask whoever runs Personalia.
5. The **company ids** of your group in Personalia (`GROUP_COMPANY_IDS`). Only people from these companies can log in.
6. Access to the GitHub repo: `https://github.com/persist-org/siapp-backend`.

---

## 1. Log in to the server

On your own computer:

```bash
ssh root@YOUR_SERVER_IP
```

(Use your own user name if it is not `root`. If it is not root, put `sudo` in front of commands that need it.)

Update the server first:

```bash
apt update && apt upgrade -y
```

---

## 2. Open only the doors you need (firewall)

The server should only accept SSH (22), HTTP (80) and HTTPS (443). Nothing else, so the database is never exposed.

Your server already runs other projects, so **look first**:

```bash
ufw status
```

- If it says `Status: active`, the firewall is already set up. Do **nothing** here. SiAPP will not open any new port (it only listens on `127.0.0.1`, see step 3b). Just make sure 80 and 443 are in the list.
- If it says `inactive`, turn it on. **Before** that, also allow the ports your other projects use, or they go offline:

```bash
ufw allow 22
ufw allow 80
ufw allow 443
ufw --force enable
ufw status
```

> Do not skip `ufw allow 22`. If you do, you lock yourself out.

---

## 3. Install Docker and Git

```bash
apt install -y git curl ca-certificates
curl -fsSL https://get.docker.com | sh
```

Check that both work:

```bash
docker --version
docker compose version
git --version
```

All three should print a version. (If Docker was already installed for your other projects, the install line is skipped by you. Just check the versions.)

---

## 3b. Choose the folder and the port

Your server already has projects. SiAPP must not step on them, so decide two things now and write them down.

### The folder (where the project lives)ls

See what is already there:

```bash
ls /srv /opt /home 2>/dev/null
docker ps --format 'table {{.Names}}\t{{.Ports}}'
```

Use the same place your other projects use. If there is no habit, `/srv/siapp` is a good default. In this guide:

> **`/srv/siapp` = your project folder.** If you chose another folder, use it instead. All later commands say "run inside the project folder", so `cd` there first.

### The port (where the api listens on the server)

The api listens on **one port on this server**, only for the proxy on the same machine. The internet never connects to it directly. Find a free one:

```bash
ss -tlnp | grep -E ':(8080|8081|8082|8090|18080)\b'
docker ps --format '{{.Ports}}' | grep -o '[0-9]*->' | sort -u
```

Any line printed means that port is **taken**. Pick one that appears in **neither** output, for example `18080`. Confirm it is free (no output = free):

```bash
ss -tln | grep ':18080 '
```

Write it down. In this guide it is called **`SIAPP_PORT`**, and the default used in examples is `18080`. It goes in `.env` (step 5) and the proxy (step 6b).

> Avoid ports 80, 443, 5432 (Postgres), 6379 (Redis) and anything you saw in the output above.
> Postgres and Redis are **not** published at all. They are only reachable by the other SiAPP boxes, so they cannot clash with a database your other projects already run.

---

## 4. Get the code from GitHub

The repo is `persist-org/siapp-backend`. If it is **private**, the server needs permission to read it. The cleanest way is a **deploy key** (read-only, belongs to this one repo).

### 4a. Make a key on the server

```bash
ssh-keygen -t ed25519 -C "siapp-server" -f ~/.ssh/siapp_deploy -N ""
cat ~/.ssh/siapp_deploy.pub
```

Copy the line that starts with `ssh-ed25519 ...`.

### 4b. Give the key to GitHub

1. Open the repo on GitHub, then **Settings**, then **Deploy keys**, then **Add deploy key**.
2. Title: `siapp-server`. Paste the key. Leave **Allow write access** unticked. Save.

(You need to be an admin of the repo to see this page. If you are not, ask one. Another option is a Personal Access Token and the `https://` clone URL.)

### 4c. Tell Git to use that key

```bash
cat >> ~/.ssh/config <<'EOF'
Host github.com
  IdentityFile ~/.ssh/siapp_deploy
  IdentitiesOnly yes
EOF
chmod 600 ~/.ssh/config
ssh -T git@github.com
```

The last command should say `Hi persist-org/siapp-backend! You've successfully authenticated`. (A message that GitHub "does not provide shell access" is normal.)
The first time it asks "Are you sure you want to continue connecting?" Type `yes`.

### 4d. Download the code

```bash
mkdir -p /srv && cd /srv        # use the parent folder you chose in step 3b
git clone git@github.com:persist-org/siapp-backend.git siapp
cd siapp                        # this is the project folder
git checkout main
```

> **Which branch?** `main` is the stable one. If the features you need are still on another branch (for example `jonathan/feat/attendance`), that branch must be **pushed to GitHub first**, then use `git checkout <branch-name>` here. Work that only exists on your laptop is not on the server.

From now on, **every command is run inside the project folder** (`/srv/siapp`, or the one you chose). Run `cd /srv/siapp` first each time you log in.

---

## 5. Make the settings file (`.env`)

Secrets live in a file called `.env`. It is **not** in GitHub (on purpose). You make it on the server.

### 5a. Make strong random secrets

Run this. It prints five values. Keep this terminal open and copy them as you go.

```bash
echo "JWT_SECRET=$(openssl rand -base64 48 | tr -d '\n')"
echo "STORAGE_LOCAL_SIGNING_KEY=$(openssl rand -base64 48 | tr -d '\n')"
echo "ATTENDANCE_QR_SECRET=$(openssl rand -base64 48 | tr -d '\n')"
echo "DB_OWNER_PASSWORD=$(openssl rand -hex 24)"
echo "DB_APP_PASSWORD=$(openssl rand -hex 24)"
```

> The two database passwords use `-hex` on purpose: letters and numbers only, so they never break the database address.
> Do **not** reuse the example values from `.env.example`. In production, the app refuses to start with them.

### 5b. Write the file

```bash
nano .env
```

Paste this, then **replace every `<...>`**:

```env
APP_ENV=production
HTTP_ADDR=:8080
LOG_LEVEL=info
CORS_ORIGINS=https://<your-web-or-app-address>

# The port you chose in step 3b. Only the proxy on this server uses it.
SIAPP_PORT=18080
# Only if you use OUR caddy box (step 6b, option B). Otherwise leave this line out.
# COMPOSE_PROFILES=caddy

# Database passwords (used by docker-compose.prod.yml)
DB_OWNER_PASSWORD=<from step 5a>
DB_APP_PASSWORD=<from step 5a>

# Secrets
JWT_SECRET=<from step 5a>
STORAGE_LOCAL_SIGNING_KEY=<from step 5a>
ATTENDANCE_QR_SECRET=<from step 5a>

# Photos are saved on the server's disk
STORAGE_DRIVER=local
STORAGE_LOCAL_DIR=/var/lib/siapp/media
# The public address of THIS api, as phones see it. Must be https.
MEDIA_PUBLIC_BASE_URL=https://<your-domain>

# Caddy tells the api the real visitor address in this header (see Caddyfile in step 6)
CLIENT_IP_HEADER=X-Real-IP

# Personalia (login and people). Given to you by whoever runs it.
PERSONEL_BASE_URL=<https://.../api/v3/integrations/siapp>
PERSONEL_INTEGRATION_KEY=<shared key>
PERSONEL_WEBHOOK_SECRET=<shared secret, same value on the Personalia side>

# Your group
GROUP_COMPANY_IDS=<for example 3,45,89>
GROUP_ORGANIZATION_ID=1

# Push notifications. Leave empty to only log them.
FCM_SERVICE_ACCOUNT_BASE64=
```

Save in nano: `Ctrl+O`, `Enter`, then `Ctrl+X`.

Lock the file so only you can read it:

```bash
chmod 600 .env
```

> `HTTP_ADDR=:8080` is the port **inside** the box. Leave it. Your chosen `SIAPP_PORT` is what the server sees.
> Do **not** set `MIGRATE_ON_START`. It is refused in production. Migrations run as their own step below.

---

## 6. Make the server files

The repo has a `docker-compose.yml`, but that one is for a laptop (open ports, weak passwords). For the server make a separate one.

### 6a. `docker-compose.prod.yml`

```bash
nano docker-compose.prod.yml
```

Paste:

```yaml
name: siapp

x-app: &app
  build: .
  restart: unless-stopped
  env_file: .env
  environment:
    # Inside Docker, boxes find each other by name (postgres, redis).
    DATABASE_URL: postgres://siapp_app:${DB_APP_PASSWORD}@postgres:5432/siapp?sslmode=disable
    MIGRATE_DSN: postgres://siapp_owner:${DB_OWNER_PASSWORD}@postgres:5432/siapp?sslmode=disable
    REDIS_URL: redis://redis:6379/0
  depends_on:
    postgres: { condition: service_healthy }
    redis: { condition: service_healthy }

services:
  postgres:
    image: postgres:16
    restart: unless-stopped
    environment:
      POSTGRES_USER: siapp_owner
      POSTGRES_PASSWORD: ${DB_OWNER_PASSWORD}
      POSTGRES_DB: siapp
      TZ: UTC
    # No "ports": the database is NOT reachable from the internet.
    volumes:
      - postgres-data:/var/lib/postgresql/data
      - ./deploy/postgres/init.sql:/docker-entrypoint-initdb.d/01-roles.sql:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U siapp_owner -d siapp"]
      interval: 5s
      timeout: 3s
      retries: 20

  redis:
    image: redis:7
    restart: unless-stopped
    volumes:
      - redis-data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 20

  # Runs once to update the database tables, then stops. This is normal.
  migrate:
    <<: *app
    restart: "no"
    entrypoint: ["/migrate", "up"]

  api:
    <<: *app
    # Reachable only from this server (127.0.0.1), on the port you chose in step 3b.
    ports: ["127.0.0.1:${SIAPP_PORT}:8080"]
    # Same folder for api and worker, kept on the server's disk (./media = inside the project folder).
    volumes: ["./media:/var/lib/siapp/media"]
    depends_on:
      migrate: { condition: service_completed_successfully }
      redis: { condition: service_healthy }

  worker:
    <<: *app
    entrypoint: ["/worker"]
    volumes: ["./media:/var/lib/siapp/media"]
    depends_on:
      migrate: { condition: service_completed_successfully }
      redis: { condition: service_healthy }

  # OPTIONAL. Only starts if COMPOSE_PROFILES=caddy is in .env (step 6b, option B).
  # Do NOT use it if another program on the server already uses port 80/443.
  caddy:
    profiles: ["caddy"]
    image: caddy:2
    restart: unless-stopped
    ports: ["80:80", "443:443"]
    volumes:
      - ./deploy/Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config
    depends_on: [api]

volumes:
  postgres-data:
  redis-data:
  caddy-data:
  caddy-config:
```

### 6b. HTTPS: connect your domain to the port

First find out who already owns ports 80/443:

```bash
ss -tlnp | grep -E ':(80|443) '
```

- Shows `nginx` (or `apache`, `traefik`, `caddy`, a docker proxy) => **Option A**.
- Shows nothing => **Option B**.

**Option A (recommended): use the proxy you already have.**
Add a new site for your domain that forwards to `127.0.0.1:18080` (use your `SIAPP_PORT`). The domain's DNS must already point to this server.

nginx: create `/etc/nginx/sites-available/siapp` and link it:

```nginx
server {
    server_name api.example.com;
    client_max_body_size 20m;          # photo uploads
    location / {
        proxy_pass http://127.0.0.1:18080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

```bash
ln -s /etc/nginx/sites-available/siapp /etc/nginx/sites-enabled/siapp
nginx -t && systemctl reload nginx
certbot --nginx -d api.example.com     # free HTTPS certificate
```

Caddy (already on the server): add to its Caddyfile, then reload it:

```
api.example.com {
	reverse_proxy 127.0.0.1:18080 {
		header_up X-Real-IP {remote_host}
	}
}
```

> If your existing proxy itself runs **in Docker**, `127.0.0.1` means "inside that proxy's box" and will not work. In that case ask whoever set it up, or put the proxy and SiAPP on a shared Docker network.
> The `X-Real-IP` line matters: SiAPP uses it as the visitor's address (`CLIENT_IP_HEADER` in `.env`).

**Option B: nothing uses 80/443, so use our own caddy box.**
Uncomment `COMPOSE_PROFILES=caddy` in `.env`, then create the file (replace `api.example.com` with **your domain**):

```bash
mkdir -p deploy
nano deploy/Caddyfile
```

```
api.example.com {
	reverse_proxy api:8080 {
		header_up X-Real-IP {remote_host}
	}
}
```

Caddy gets a free HTTPS certificate by itself. DNS must already point to this server and ports 80 and 443 must be open (step 2).

### 6c. Make the photo folder

The app runs as a non-root user (number 65532). The folder must belong to that user or photo uploads fail.

```bash
mkdir -p media
chown 65532:65532 media
```

---

## 7. Start it (in this exact order)

### 7a. Start only the database first

```bash
cd /srv/siapp        # your project folder
docker compose -f docker-compose.prod.yml up -d --build postgres redis
docker compose -f docker-compose.prod.yml ps
```

Wait until `postgres` and `redis` say `healthy`.

### 7b. Set the app's database password

The first-time setup file creates the `siapp_app` database user with a **weak** password. Replace it with your strong one from step 5a.

Use your real `DB_APP_PASSWORD` value in place of `PASTE_DB_APP_PASSWORD`:

```bash
docker compose -f docker-compose.prod.yml exec postgres \
  psql -U siapp_owner -d siapp -c "ALTER ROLE siapp_app PASSWORD 'PASTE_DB_APP_PASSWORD';"
```

It should print `ALTER ROLE`.

> This must be done **before** you start the api. Do it once, right after the database is created.

### 7c. Start everything else

```bash
docker compose -f docker-compose.prod.yml up -d --build
docker compose -f docker-compose.prod.yml ps
```

The first build takes a few minutes (it compiles the Go code). When it is done you should see:

- `postgres`, `redis`, `api`, `worker` are **Up** (and `caddy` too, only if you chose option B). `api` shows `127.0.0.1:18080->8080`.
- `migrate` is **Exited (0)**. That is correct: it ran, updated the tables, and finished.

---

## 8. Check that it works

From the server:

```bash
docker compose -f docker-compose.prod.yml logs --tail 50 api
curl http://127.0.0.1:18080/api/v1/health     # use your SIAPP_PORT. Tests the api before HTTPS.
```

If this works but the `https://` check below does not, the problem is the proxy (step 6b), not SiAPP.

From **any** computer or your browser:

```bash
curl https://YOUR-DOMAIN/api/v1/health
```

If you get an answer (not an error), the API is alive and HTTPS works.

---

## 9. First-time data: make the first admin

Someone has to be the first admin. People come from Personalia, so Personalia must be reachable (step 5 settings).

Replace `<NIK>` with the employee number of the person who should be the first admin:

```bash
docker compose -f docker-compose.prod.yml run --rm --entrypoint /seed api grant-admin --nik <NIK>
```

Then load the starting shifts and permissions:

```bash
docker compose -f docker-compose.prod.yml run --rm --entrypoint /seed api shifts
docker compose -f docker-compose.prod.yml run --rm --entrypoint /seed api permissions
```

It is safe to run these again. They only add what is missing.

---

## 10. Every day: handy commands

Always run them in `/srv/siapp`. To type less, you can make a short name once:

```bash
alias dc='docker compose -f /srv/siapp/docker-compose.prod.yml'   # change the path if your folder is different
```

| I want to... | Command |
|---|---|
| See what is running | `docker compose -f docker-compose.prod.yml ps` |
| Read the api logs (live) | `docker compose -f docker-compose.prod.yml logs -f api` |
| Read the worker logs | `docker compose -f docker-compose.prod.yml logs -f worker` |
| Restart the api | `docker compose -f docker-compose.prod.yml restart api` |
| Stop everything | `docker compose -f docker-compose.prod.yml down` |
| Open the database | `docker compose -f docker-compose.prod.yml exec postgres psql -U siapp_owner -d siapp` |

> `down` keeps your data. Never add `-v` to it: that **deletes the database**.

---

## 11. Updating to a new version

```bash
cd /srv/siapp
git pull
docker compose -f docker-compose.prod.yml up -d --build
```

This downloads the new code, rebuilds, runs any new database changes (the `migrate` box), and restarts the api and worker. The worker and api pick up the new version together.

To go back to an older version: `git checkout <older-commit>` and run the same `up -d --build` line.

---

## 12. Backups (do this now, not later)

Two things hold your data:

1. **The database.** Make a copy every night:

   ```bash
   mkdir -p /srv/backups
   crontab -e
   ```

   Add this line (runs every day at 02:00):

   ```
   0 2 * * * cd /srv/siapp && docker compose -f docker-compose.prod.yml exec -T postgres pg_dump -U siapp_owner siapp | gzip > /srv/backups/siapp-$(date +\%F).sql.gz
   ```

2. **The photos** in the `media` folder inside your project folder (`/srv/siapp/media`). Nothing copies them for you. Back this folder up too (for example with `rsync` to another machine, or your provider's snapshot).

Copy backups **off the server** as well. A backup on the same disk dies with the disk.

---

## 13. When something goes wrong

| What you see | Most likely reason | What to do |
|---|---|---|
| `api` keeps restarting | A setting is wrong. The app prints exactly which one. | `docker compose -f docker-compose.prod.yml logs api` and read the first error. |
| Log says a secret "looks like a placeholder" | You used an example value. | Make a new one with `openssl rand -base64 48` and put it in `.env`. |
| Log says `MIGRATE_ON_START ... refused` | You set it in `.env`. | Delete that line. |
| `migrate` fails | Wrong database password in `.env`, or step 7b skipped. | Check `DB_OWNER_PASSWORD`. If the database was already created with another password, see the note below. |
| Login always fails | Personalia unreachable, or the key is wrong. | Check `PERSONEL_BASE_URL` and `PERSONEL_INTEGRATION_KEY`. Test from the server: `curl <PERSONEL_BASE_URL>`. |
| Someone from your company cannot log in | Their company id is not in the list. | Add it to `GROUP_COMPANY_IDS`, then `docker compose -f docker-compose.prod.yml up -d api worker`. |
| HTTPS does not work | Domain not pointing to the server yet, or ports closed, or proxy not forwarding. | Check DNS, and `ufw status`. Test `curl http://127.0.0.1:<SIAPP_PORT>/api/v1/health` first. Then read your proxy's log (nginx: `journalctl -u nginx`; our caddy: `docker compose -f docker-compose.prod.yml logs caddy`). |
| `port is already allocated` / `address already in use` when starting `api` | Another project uses your `SIAPP_PORT`. | Pick another free port (step 3b), change `SIAPP_PORT` in `.env` and in the proxy, then `up -d` again. |
| `caddy` fails: `bind: address already in use` | Something already owns 80/443. | You should use option A. Remove `COMPOSE_PROFILES=caddy` from `.env`. |
| Photo upload fails | The media folder has the wrong owner. | `chown 65532:65532 /srv/siapp/media` |
| `Permission denied (publickey)` on git | The deploy key is missing or wrong. | Redo step 4. |

**Note about database passwords:** `DB_OWNER_PASSWORD` is only used **the first time** the database is created. If you change it in `.env` later, the database keeps the old one. To change it for real, run `ALTER ROLE siapp_owner PASSWORD '...'` inside the database, then update `.env`.

---

## 14. Short checklist

- [ ] Server updated, firewall checked (22, 80, 443 allowed, other projects not broken)
- [ ] Docker, Git installed
- [ ] Project folder and a **free port** chosen and written down (step 3b)
- [ ] Repo cloned to the project folder, right branch checked out
- [ ] `.env` filled in with real secrets and `SIAPP_PORT`, `chmod 600`
- [ ] `docker-compose.prod.yml` created; proxy site added (option A) or `deploy/Caddyfile` created (option B)
- [ ] `media` folder created in the project folder and owned by `65532`
- [ ] postgres + redis started, `siapp_app` password changed (step 7b)
- [ ] everything started, `migrate` exited 0
- [ ] `curl https://YOUR-DOMAIN/api/v1/health` answers
- [ ] first admin made, shifts and permissions loaded
- [ ] nightly database backup set up, photos backed up
