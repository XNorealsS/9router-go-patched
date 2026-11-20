# 9Router Unified Gateway — Linux Deployment Guide (1 Gateway, 1 Port)

All client API traffic (`/v1/*`, `/models`, `/chat/completions`, `/messages`) and the Web Dashboard UI (`/dashboard`, `/_next/*`, `/api/*`) are served behind **one single port (`:20128`)** by `9router-go`.

## Option A — Docker Compose (Recommended 1-Command Deploy)

```bash
cp .env.example .env
# Edit .env: INITIAL_PASSWORD is required (compose refuses to start without it).
# Optional: ANTIGRAVITY_OAUTH_CLIENT_ID / ANTIGRAVITY_OAUTH_CLIENT_SECRET for Go-side token refresh.
docker compose up -d --build
```

Open: `http://<SERVER_IP>:20128/dashboard` (put Nginx/Caddy TLS + IP/firewall restrictions in front when exposing publicly).

## Option B — Direct `git clone` on Linux Server (Build & Replace)

If Go (`>= 1.24`, recommended `1.26`) and Git are installed on your Linux server:

```bash
git clone https://github.com/XNorealsS/9router-go-patched.git
cd 9router-go-patched
sudo INITIAL_PASSWORD='your-admin-password' bash deploy/install.sh
```

`deploy/install.sh` auto-detects repo mode, compiles `./cmd/9router-go/` with `CGO_ENABLED=0`, backs up your existing SQLite DB, stops the old `9router` process, and enables `9router-gateway.service`.

## Option C — Bare-Metal Linux Bundle (`dist/9router-gateway-linux-amd64.tar.gz`)

### 1-Command Replace / Upgrade Existing 9Router (Recommended)
Build the bundle from Windows PowerShell (`scripts/package-linux.ps1`), upload `dist/9router-gateway-linux-amd64.tar.gz` to the Linux server, and run `install.sh`:

```bash
mkdir -p /tmp/9router-up && tar -xzf 9router-gateway-linux-amd64.tar.gz -C /tmp/9router-up
sudo INITIAL_PASSWORD='your-admin-password' bash /tmp/9router-up/install.sh
rm -rf /tmp/9router-up
```

`install.sh` automatically:
1. Backs up any existing `~/.9router/db/data.sqlite` (`*.bak.<timestamp>`).
2. Gracefully stops previous `9router` / `pm2` / `systemd` processes.
3. Installs to `/opt/9router-gateway` (preserving an existing `/opt/9router-gateway/.env` and reusing existing SQLite DB / accounts / cookies).
4. Ensures `9router@0.5.119` dashboard runtime is present, enables `9router-gateway.service`, and verifies `/health`.

### Manual Bare-Metal Steps

```bash
sudo mkdir -p /opt/9router-gateway
sudo tar -xzf 9router-gateway-linux-amd64.tar.gz -C /opt/9router-gateway
cd /opt/9router-gateway
cp .env.example .env
# Set HOST=0.0.0.0 (or 127.0.0.1 behind Nginx/Caddy), INITIAL_PASSWORD, and DATA_DIR
chmod 600 .env
chmod +x bin/9router-go bin/start.sh

# Pre-install companion Next.js dashboard runtime once (requires Node >= 20.18.1):
npm install -g 9router@0.5.119

# Enable systemd service:
sudo cp systemd/9router-gateway.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now 9router-gateway
```

Notes:
- Keep `CREDENTIAL_ENCRYPTION_KEY` unset when running unified with the Next.js dashboard (both read/write the shared SQLite file). Set `CREDENTIAL_ENCRYPTION_KEY` only when running `9router-go` standalone (headless: do not install the `9router` package; `start.sh` then runs the Go gateway alone).
- Note SQLite location compatibility: Next.js uses `$HOME/.9router/db/data.sqlite`. When setting a custom `DATA_DIR` on bare-metal Linux, ensure `$HOME/.9router` matches or symlinks to `$DATA_DIR`.


## Runtime model
- `bin/start.sh` is the only entry point (Docker ENTRYPOINT, systemd ExecStart). It starts the dashboard on `127.0.0.1:3000` (never published), waits for it, then starts `9router-go` on `:20128`.
- If either process exits, `start.sh` stops the other and exits non-zero, so Docker (`restart: unless-stopped`) or systemd (`Restart=always`) restarts a consistent pair. `SIGTERM` stops both.
- Configuration is read only from the process environment (compose `environment:`, systemd `EnvironmentFile`). `start.sh` does not source `.env`.
- Docker image runs as non-root `node`, persists state in the `/data` volume, and has a `HEALTHCHECK` on `/health`.

## Known limitations
- Google OAuth (Antigravity) redirects to a `localhost` callback. For a remote server, connect the account through an SSH tunnel (`ssh -L 20128:127.0.0.1:20128 server`) and open `http://localhost:20128/dashboard`.
- `/health` reports the Go gateway only; it does not probe the dashboard.
