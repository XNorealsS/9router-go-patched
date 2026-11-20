#!/usr/bin/env bash
# Idempotent Linux server installer / upgrader for 9Router Unified 1-Gateway.
# Safely replaces an existing running 9router / 9router-gateway while preserving SQLite data.
set -euo pipefail

[ "$(id -u)" -eq 0 ] || exec sudo -E bash "$0" "$@"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="${SCRIPT_DIR}"
MODE="bundle"
if [ ! -f "${SCRIPT_DIR}/bin/9router-go" ] && [ -f "${SCRIPT_DIR}/../cmd/9router-go/main.go" ]; then
  SRC_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
  MODE="repo"
fi
DEST_DIR="${DEST_DIR:-/opt/9router-gateway}"
SERVICE_PATH="/etc/systemd/system/9router-gateway.service"

# 1. Discover npm/nvm Node and Go bin paths so systemd & repo builds work reliably
for d in /usr/local/go/bin /usr/local/bin /root/.npm-global/bin /root/.nvm/versions/node/*/bin; do
  [ -d "${d}" ] && export PATH="${d}:${PATH}"
done
if [ -n "${SUDO_USER:-}" ] && [ "${SUDO_USER}" != "root" ]; then
  SUDO_HOME="$(getent passwd "${SUDO_USER}" | cut -d: -f6 || true)"
  for d in "${SUDO_HOME}/.npm-global/bin" "${SUDO_HOME}/.local/bin" "${SUDO_HOME}/.nvm/versions/node"/*/bin; do
    [ -d "${d}" ] && export PATH="${PATH}:${d}"
  done
fi

# 2. Auto-detect existing SQLite DB and create timestamped backup before stopping old process
EXISTING_DB=""
for candidate in "/root/.9router/db/data.sqlite" "${SUDO_HOME:-/nonexistent}/.9router/db/data.sqlite" "/data/db/data.sqlite"; do
  if [ -f "${candidate}" ]; then
    EXISTING_DB="${candidate}"
    break
  fi
done
if [ -n "${EXISTING_DB}" ]; then
  TS="$(date +%Y%m%d-%H%M%S)"
  cp -a "${EXISTING_DB}" "${EXISTING_DB}.bak.${TS}"
  echo "[install] Backed up existing SQLite DB -> ${EXISTING_DB}.bak.${TS}"
fi

# 3. Stop previous 9router / PM2 / systemd instances gracefully
systemctl stop 9router-gateway 9router 2>/dev/null || true
if command -v pm2 >/dev/null 2>&1; then
  pm2 stop 9router 2>/dev/null || true
  pm2 save 2>/dev/null || true
fi
for proc_pat in '/9router-go' 'node.*9router' 'bin/9router '; do
  pids="$(pgrep -f "${proc_pat}" 2>/dev/null | grep -v "^$$\$" || true)"
  if [ -n "${pids}" ]; then
    echo "${pids}" | xargs -r kill -TERM 2>/dev/null || true
    sleep 1
    echo "${pids}" | xargs -r kill -KILL 2>/dev/null || true
  fi
done

# 4. Install binary + scripts into /opt/9router-gateway (bundle or git clone)
mkdir -p "${DEST_DIR}/bin" "${DEST_DIR}/systemd"
if [ "${MODE}" = "repo" ]; then
  if ! command -v go >/dev/null 2>&1; then
    echo "[install] ERROR: Go compiler not found in PATH (required when installing from git clone). Install Go 1.24+ or use Docker / prebuilt .tar.gz bundle." >&2
    exit 1
  fi
  echo "[install] Building 9router-go from git repository (${SRC_DIR})..."
  (cd "${SRC_DIR}" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${DEST_DIR}/bin/9router-go" ./cmd/9router-go/)
  cp -a "${SRC_DIR}/deploy/start.sh" "${DEST_DIR}/bin/start.sh"
  cp -a "${SRC_DIR}/deploy/9router-gateway.service" "${DEST_DIR}/systemd/9router-gateway.service"
  cp -a "${SRC_DIR}/.env.example" "${DEST_DIR}/.env.example"
  cp -a "${SRC_DIR}/deploy/README_DEPLOY.md" "${DEST_DIR}/README_DEPLOY.md"
  cp -a "${SRC_DIR}/deploy/install.sh" "${DEST_DIR}/install.sh"
elif [ "$(realpath "${SRC_DIR}")" != "$(realpath "${DEST_DIR}")" ]; then
  cp -a "${SRC_DIR}/bin/9router-go" "${DEST_DIR}/bin/9router-go"
  cp -a "${SRC_DIR}/bin/start.sh" "${DEST_DIR}/bin/start.sh"
  cp -a "${SRC_DIR}/systemd/9router-gateway.service" "${DEST_DIR}/systemd/9router-gateway.service"
  cp -a "${SRC_DIR}/.env.example" "${DEST_DIR}/.env.example"
  [ -f "${SRC_DIR}/README_DEPLOY.md" ] && cp -a "${SRC_DIR}/README_DEPLOY.md" "${DEST_DIR}/README_DEPLOY.md"
  [ -f "${SRC_DIR}/install.sh" ] && cp -a "${SRC_DIR}/install.sh" "${DEST_DIR}/install.sh"
fi
chmod 755 "${DEST_DIR}/bin/9router-go" "${DEST_DIR}/bin/start.sh" "${DEST_DIR}/install.sh" 2>/dev/null || true

# 5. Prepare .env preserving existing secrets across upgrades
if [ ! -f "${DEST_DIR}/.env" ]; then
  cp "${DEST_DIR}/.env.example" "${DEST_DIR}/.env"
  RAND_JWT="$( (openssl rand -hex 32 2>/dev/null || tr -dc 'a-f0-9' </dev/urandom | head -c 64) || echo 'change-me-random-jwt-secret')"
  sed -i "s/^JWT_SECRET=.*/JWT_SECRET=${RAND_JWT}/" "${DEST_DIR}/.env"
  sed -i "s/^# *HOST=127.0.0.1/HOST=0.0.0.0/" "${DEST_DIR}/.env"
  if [ -n "${INITIAL_PASSWORD:-}" ]; then
    sed -i "s|^INITIAL_PASSWORD=.*|INITIAL_PASSWORD=${INITIAL_PASSWORD}|" "${DEST_DIR}/.env"
  fi
  # Reuse existing non-root user SQLite directory if root DB did not exist
  if [ -n "${EXISTING_DB}" ] && [ "${EXISTING_DB}" != "/root/.9router/db/data.sqlite" ]; then
    OLD_DATA_DIR="$(dirname "$(dirname "${EXISTING_DB}")")"
    sed -i "s|^# *DATA_DIR=.*|DATA_DIR=${OLD_DATA_DIR}|" "${DEST_DIR}/.env"
    echo "[install] Configured DATA_DIR=${OLD_DATA_DIR} to preserve existing accounts/keys"
  fi
  chmod 600 "${DEST_DIR}/.env"
  echo "[install] Created ${DEST_DIR}/.env (review INITIAL_PASSWORD before public exposure)"
else
  chmod 600 "${DEST_DIR}/.env"
  echo "[install] Preserved existing ${DEST_DIR}/.env"
fi

# 6. Ensure companion Next.js dashboard CLI is installed (Node >= 20.18.1)
if ! command -v 9router >/dev/null 2>&1; then
  if command -v npm >/dev/null 2>&1; then
    echo "[install] Installing 9router@0.5.119 dashboard companion..."
    npm install -g 9router@0.5.119
  else
    echo "[install] WARNING: npm not found; Go gateway will run headless without Next.js dashboard" >&2
  fi
fi

# 7. Install/refresh systemd unit & start service
cp "${DEST_DIR}/systemd/9router-gateway.service" "${SERVICE_PATH}"
systemctl daemon-reload
systemctl enable --now 9router-gateway

PORT_VAL="$(grep -E '^PORT=' "${DEST_DIR}/.env" | tail -n1 | cut -d= -f2 || true)"
PORT_VAL="${PORT_VAL:-20128}"
echo "[install] Waiting for gateway health on 127.0.0.1:${PORT_VAL}/health ..."
for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:${PORT_VAL}/health" >/dev/null 2>&1 || \
     wget -qO- "http://127.0.0.1:${PORT_VAL}/health" >/dev/null 2>&1; then
    echo "[install] OK: 9router-gateway is healthy on port ${PORT_VAL}"
    systemctl --no-pager -l status 9router-gateway | head -n 15 || true
    exit 0
  fi
  sleep 0.5
done

echo "[install] WARNING: health probe did not respond within 30s; inspect journalctl -u 9router-gateway -n 50" >&2
exit 1
