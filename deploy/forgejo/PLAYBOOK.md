# Bureau Forgejo Deployment Playbook

Self-hosted git forge for Bureau development. Provides repository hosting,
issue tracking, pull requests, and a web UI. Uses SQLite and runs as a
single rootless container.

## Prerequisites

- Docker and Docker Compose

## 1. Configure

```bash
cd deploy/forgejo
cp .env.example .env
cp app.ini.example app.ini
```

Edit `.env` if ports 3000 or 2222 conflict with other services.

For LAN access, edit `app.ini` and update the domain settings:

```ini
[server]
DOMAIN = workstation.local
SSH_DOMAIN = workstation.local
ROOT_URL = http://workstation.local:3000/
```

Forgejo modifies `app.ini` on first boot, adding generated secrets and
install wizard settings. The file is gitignored because it contains
instance-specific secrets after setup. The template (`app.ini.example`)
stays clean in version control.

## 2. Start

```bash
docker compose up -d
```

Wait for the server to be reachable (first boot takes ~10s):

```bash
until curl -sf http://localhost:3000/ > /dev/null; do
  echo "Waiting for Forgejo..."
  sleep 2
done
echo "Forgejo is ready."
```

## 3. Create Admin Account

Open `http://localhost:3000` in a browser. Forgejo shows an install
wizard on first boot. The database and server settings are pre-configured
by `app.ini` — scroll to the bottom and create the administrator account.

After submitting, **restart the container** — Forgejo stays in install
mode until restarted:

```bash
docker compose restart
```

Registration is disabled by default (per `app.ini`). Create additional
accounts through **Site Administration > User Accounts** in the web UI,
or via the CLI inside the container:

```bash
docker exec bureau-forgejo forgejo admin user create \
  --username alice \
  --email alice@bureau.local \
  --password "..." \
  --must-change-password=false
```

## 4. Create an Organization

Organizations group related repositories. Create one for Bureau work
via the web UI (**+ > New Organization**) or via the API:

```bash
# Create a personal access token first:
# Settings > Applications > Generate New Token (select all scopes)
TOKEN="your-token-here"

curl -X POST http://localhost:3000/api/v1/orgs \
  -H "Authorization: token ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"username": "bureau-foundation", "visibility": "private"}'
```

## 5. Push a Repository

```bash
# HTTP (token auth).
git remote add forgejo http://localhost:3000/bureau-foundation/bureau.git
git push forgejo main

# Or SSH (requires adding your public key in Settings > SSH Keys).
git remote add forgejo ssh://git@localhost:2222/bureau-foundation/bureau.git
git push forgejo main
```

For HTTP pushes, configure a credential helper or use a personal
access token in the URL:

```bash
git remote set-url forgejo http://TOKEN@localhost:3000/bureau-foundation/bureau.git
```

## 6. Mirror to GitHub (Optional)

Forgejo supports push mirrors that automatically sync to an external
remote on every push. Configure per-repository:

**Repository Settings > Mirror Settings > Push Mirror**

- Address: `https://github.com/bureau-foundation/bureau.git`
- Authorization: a GitHub personal access token with `repo` scope

Forgejo will push to GitHub on a configurable interval (default: 8 hours,
minimum: 10 minutes).

## Teardown

```bash
docker compose -f deploy/forgejo/docker-compose.yaml down -v
```

The `-v` flag deletes the `forgejo-data` volume and all repositories.
Omit it to preserve data across container rebuilds.
