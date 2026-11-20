#!/usr/bin/env bash
set -euo pipefail

export DATA_DIR="${DATA_DIR:-/data}"
export HOST="${HOST:-0.0.0.0}"
export PORT="${PORT:-20128}"
export DASHBOARD_PORT="${DASHBOARD_PORT:-3000}"
export DASHBOARD_URL="${DASHBOARD_URL:-http://127.0.0.1:${DASHBOARD_PORT}}"

mkdir -p "${DATA_DIR}/db"
chmod 700 "${DATA_DIR}" "${DATA_DIR}/db" || true

# Next.js standalone reads its default DB inside $HOME/.9router/db/data.sqlite when DATA_DIR is not mapped.
# Symlink ~/.9router -> $DATA_DIR so Next.js and 9router-go always share the exact same SQLite WAL database.
if [ "${HOME:-}" != "" ] && [ "${HOME}/.9router" != "${DATA_DIR}" ]; then
  if [ ! -e "${HOME}/.9router" ]; then
    ln -s "${DATA_DIR}" "${HOME}/.9router"
  fi
fi

DASH_PID=""
cleanup() {
  if [ -n "${DASH_PID}" ] && kill -0 "${DASH_PID}" 2>/dev/null; then
    kill -TERM "${DASH_PID}" 2>/dev/null || true
    wait "${DASH_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

if [ -f "/opt/9router-ui/app/custom-server.js" ]; then
  PORT="${DASHBOARD_PORT}" HOSTNAME="127.0.0.1" node "/opt/9router-ui/app/custom-server.js" &
  DASH_PID=$!
elif [ -f "/opt/9router-ui/app/server.js" ]; then
  PORT="${DASHBOARD_PORT}" HOSTNAME="127.0.0.1" node "/opt/9router-ui/app/server.js" &
  DASH_PID=$!
fi

if [ -n "${DASH_PID}" ]; then
  for _ in $(seq 1 40); do
    if node -e "const n=require('net');const s=n.connect(${DASHBOARD_PORT},'127.0.0.1',()=>{s.end();process.exit(0)});s.on('error',()=>process.exit(1));" 2>/dev/null; then
      break
    fi
    sleep 0.25
  done
fi

/usr/local/bin/9router-go &
GO_PID=$!
wait "${GO_PID}"
