#!/usr/bin/env bash
# Single launcher for the 1-Gateway package (Docker ENTRYPOINT and systemd ExecStart).
# Starts the Next.js dashboard on loopback only, then the Go gateway as the sole public port.
# Configuration comes from the process environment only (systemd EnvironmentFile, compose environment:).
# If either process exits, everything is stopped so Docker/systemd restart a consistent pair.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATEWAY_BIN="${GATEWAY_BIN:-${ROOT_DIR}/bin/9router-go}"
[ -x "${GATEWAY_BIN}" ] || GATEWAY_BIN="$(command -v 9router-go)"

export HOST="${HOST:-127.0.0.1}"
export PORT="${PORT:-20128}"
export DATA_DIR="${DATA_DIR:-${HOME}/.9router}"
DASHBOARD_PORT="${DASHBOARD_PORT:-3000}"

if [ "${HOST}" != "127.0.0.1" ] && [ -z "${INITIAL_PASSWORD:-}" ]; then
  echo "[gateway] WARNING: listening on ${HOST} without INITIAL_PASSWORD; dashboard falls back to its default login" >&2
fi

mkdir -p "${DATA_DIR}/db"
chmod 700 "${DATA_DIR}" "${DATA_DIR}/db" || true

# The dashboard resolves its database under $HOME/.9router; link it to DATA_DIR
# so both processes always open the same SQLite file.
if [ "${HOME}/.9router" != "${DATA_DIR}" ] && [ ! -e "${HOME}/.9router" ]; then
  ln -s "${DATA_DIR}" "${HOME}/.9router"
fi

PIDS=()
stop() {
  trap - EXIT
  for pid in "${PIDS[@]}"; do kill -TERM "${pid}" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap stop EXIT
trap 'exit 143' INT TERM

if command -v 9router >/dev/null 2>&1; then
  9router --port "${DASHBOARD_PORT}" --host 127.0.0.1 --no-browser --skip-update --log &
  PIDS+=($!)
  export DASHBOARD_URL="http://127.0.0.1:${DASHBOARD_PORT}"
  for _ in $(seq 1 120); do
    kill -0 "${PIDS[0]}" 2>/dev/null || { echo "[gateway] dashboard exited during startup" >&2; exit 1; }
    node -e "require('net').connect(${DASHBOARD_PORT},'127.0.0.1').on('connect',()=>process.exit(0)).on('error',()=>process.exit(1))" 2>/dev/null && break
    sleep 0.25
  done
else
  echo "[gateway] 9router dashboard not installed; running headless Go gateway" >&2
  unset DASHBOARD_URL
fi

"${GATEWAY_BIN}" &
PIDS+=($!)

set +e
wait -n
code=$?
echo "[gateway] a child process exited (code ${code}); stopping so the supervisor can restart" >&2
exit $(( code == 0 ? 1 : code ))