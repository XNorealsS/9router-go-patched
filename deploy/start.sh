#!/usr/bin/env bash
# Single launcher for the 1-Gateway package (Docker ENTRYPOINT and systemd ExecStart).
# Starts the Next.js dashboard on loopback only, then the Go gateway as the sole public port.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATEWAY_BIN="${GATEWAY_BIN:-${ROOT_DIR}/bin/9router-go}"
[ -x "${GATEWAY_BIN}" ] || GATEWAY_BIN="$(command -v 9router-go)"

if [ -f "${ROOT_DIR}/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "${ROOT_DIR}/.env"
  set +a
fi

export HOST="${HOST:-127.0.0.1}"
export PORT="${PORT:-20128}"
export DATA_DIR="${DATA_DIR:-${HOME}/.9router}"
DASHBOARD_PORT="${DASHBOARD_PORT:-3000}"

mkdir -p "${DATA_DIR}/db"
chmod 700 "${DATA_DIR}" "${DATA_DIR}/db" || true

# The dashboard resolves its database under $HOME/.9router; link it to DATA_DIR
# so both processes always open the same SQLite file.
if [ "${HOME}/.9router" != "${DATA_DIR}" ] && [ ! -e "${HOME}/.9router" ]; then
  ln -s "${DATA_DIR}" "${HOME}/.9router"
fi

DASH_PID=""
cleanup() {
  if [ -n "${DASH_PID}" ] && kill -0 "${DASH_PID}" 2>/dev/null; then
    kill -TERM "${DASH_PID}" 2>/dev/null || true
    wait "${DASH_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

if command -v 9router >/dev/null 2>&1; then
  9router --port "${DASHBOARD_PORT}" --host 127.0.0.1 --no-browser --skip-update --log &
  DASH_PID=$!
  export DASHBOARD_URL="http://127.0.0.1:${DASHBOARD_PORT}"
  for _ in $(seq 1 60); do
    node -e "require('net').connect(${DASHBOARD_PORT},'127.0.0.1').on('connect',()=>process.exit(0)).on('error',()=>process.exit(1))" 2>/dev/null && break
    sleep 0.25
  done
else
  echo "[gateway] 9router dashboard not installed; running headless Go gateway" >&2
  unset DASHBOARD_URL
fi

"${GATEWAY_BIN}" &
wait $!
