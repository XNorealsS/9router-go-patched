# 9Router Unified Gateway — Linux Deployment Guide (1 Gateway, 1 Port)

All client API traffic (`/v1/*`, `/models`, `/chat/completions`, `/messages`) and the Web Dashboard UI (`/dashboard`, `/_next/*`, `/api/*`) are served behind **one single port (`:20128`)** by `9router-go`.

## Option A — Docker Compose (Recommended 1-Command Deploy)

```bash
cp .env.example .env
# Edit .env: set INITIAL_PASSWORD, JWT_SECRET, and any *_OAUTH_CLIENT_ID / *_OAUTH_CLIENT_SECRET
docker compose up -d --build
```

Open: `http://<SERVER_IP>:20128/dashboard` (put Nginx/Caddy TLS + IP/firewall restrictions in front when exposing publicly).

## Option B — Bare-Metal Linux Bundle (`dist/9router-gateway-linux-amd64.tar.gz`)

Build the Linux package from Windows PowerShell (`scripts/package-linux.ps1`), copy it to `/opt/9router-gateway`, and install the systemd unit:

```bash
sudo mkdir -p /opt/9router-gateway
sudo tar -xzf 9router-gateway-linux-amd64.tar.gz -C /opt/9router-gateway
cd /opt/9router-gateway
cp .env.example .env
# Set HOST=0.0.0.0 (or 127.0.0.1 behind Nginx/Caddy), INITIAL_PASSWORD, and JWT_SECRET
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
- Keep `CREDENTIAL_ENCRYPTION_KEY` unset when running unified with the Next.js dashboard (both read/write the shared SQLite file). Set `CREDENTIAL_ENCRYPTION_KEY` when running `9router-go` standalone (`DASHBOARD_ENABLED=false`).
- Note SQLite location compatibility: Next.js uses `$HOME/.9router/db/data.sqlite`. When setting a custom `DATA_DIR` on bare-metal Linux, ensure `$HOME/.9router` matches or symlinks to `$DATA_DIR`.
