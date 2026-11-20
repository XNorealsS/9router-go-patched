#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

if [ -f "${ROOT_DIR}/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "${ROOT_DIR}/.env"
  set +a
fi

export HOST="${HOST:-127.0.0.1}"
export PORT="${PORT:-20128}"
export DATA_DIR="${DATA_DIR:-${HOME}/.9router}"
export DASHBOARD_PORT="${DASHBOARD_PORT:-3000}"
export DASHBOARD_ENABLED="${DASHBOARD_ENABLED:-true}"

mkdir -p "${DATA_DIR}/db"
chmod 700 "${DATA_DIR}" "${DATA_DIR}/db" || true

DASH_PID=""
cleanup() {
  if [ -n "${DASH_PID}" ] && kill -0 "${DASH_PID}" 2>/dev/null; then
    kill -TERM "${DASH_PID}" 2>/dev/null || true
    wait "${DASH_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

if [ "${DASHBOARD_ENABLED}" = "true" ]; then
  export DASHBOARD_URL="${DASHBOARD_URL:-http://127.0.0.1:${DASHBOARD_PORT}}"
  if command -v 9router >/dev/null 2>&1; then
    9router --port "${DASHBOARD_PORT}" --host 127.0.0.1 --no-browser --skip-update --log &
    DASH_PID=$!
  elif command -v npx >/dev/null 2>&1; then
    npx --yes 9router@0.5.119 --port "${DASHBOARD_PORT}" --host 127.0.0.1 --no-browser --skip-update --log &
    DASH_PID=$!
  else
    echo "[gateway] WARNING: Node/9router dashboard not found; running headless Go proxy only" >&2
    unset DASHBOARD_URL
  fi

  if [ -n "${DASH_PID}" ] && command -v node >/dev/null 2>&1; then
    for _ in $(seq 1 60); do
      if node -e "const n=require('net');const s=n.connect(${DASHBOARD_PORT},'127.0.0.1',()=>{s.end();process.exit(0)});s.on('error',()=>process.exit(1));" 2>/dev/null; then
        break
      fi
      sleep 0.25
    done
  fi
fi

"${ROOT_DIR}/bin/9router-go" &
GO_PID=$!
wait "${GO_PID}"
