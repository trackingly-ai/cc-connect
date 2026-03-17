#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENV_FILE="${1:-}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

PLATFORM="$(uname -s)"

require_cmd python3
require_cmd go
if [[ "${PLATFORM}" == "Darwin" ]]; then
  require_cmd launchctl
  require_cmd plutil
else
  require_cmd systemctl
fi

RENDER_ARGS=()
if [[ -n "${ENV_FILE}" ]]; then
  RENDER_ARGS+=(--env-file "${ENV_FILE}")
fi

python3 "${ROOT_DIR}/scripts/deploy/render_echo_single_host.py" "${RENDER_ARGS[@]}"

DEPLOY_BASE_DIR="${HOME}/.local/share/echo-single-host"
if [[ -n "${ENV_FILE}" && -f "${ENV_FILE}" ]]; then
  while IFS='=' read -r key value; do
    [[ -z "${key}" || "${key}" == \#* ]] && continue
    if [[ "${key}" == "DEPLOY_BASE_DIR" ]]; then
      DEPLOY_BASE_DIR="${value}"
    fi
  done <"${ENV_FILE}"
fi

DEPLOY_ENV="${DEPLOY_BASE_DIR}/config/cc-connect/deploy.env"
source "${DEPLOY_ENV}"

echo "Building cc-connect binary"
cd "${ROOT_DIR}"
go build -o "${CC_CONNECT_BINARY}" ./cmd/cc-connect

if [[ "${PLATFORM}" == "Darwin" ]]; then
  echo "Installing launchd user agent for cc-connect"
  mkdir -p "${LAUNCHD_AGENT_DIR}"
  cp "${CC_CONNECT_PLIST_PATH}" "${LAUNCHD_AGENT_DIR}/com.echo.cc_connect.plist"
  plutil -lint "${LAUNCHD_AGENT_DIR}/com.echo.cc_connect.plist"
  launchctl bootout "gui/$(id -u)/com.echo.cc_connect" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/$(id -u)" "${LAUNCHD_AGENT_DIR}/com.echo.cc_connect.plist"
  launchctl kickstart -kp "gui/$(id -u)/com.echo.cc_connect"
else
  echo "Installing systemd user unit for cc-connect"
  mkdir -p "${SYSTEMD_USER_DIR}"
  cp "${CC_CONNECT_UNIT_PATH}" "${SYSTEMD_USER_DIR}/cc-connect.service"
  systemctl --user daemon-reload
  systemctl --user enable --now cc-connect.service
fi

echo "cc-connect deployment completed"
