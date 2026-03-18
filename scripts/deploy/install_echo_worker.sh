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

wait_for_launch_agent_absent() {
  local label="$1"
  local attempt
  for attempt in {1..20}; do
    if ! launchctl print "gui/$(id -u)/${label}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "timed out waiting for launch agent ${label} to unload" >&2
  return 1
}

install_launch_agent() {
  local label="$1"
  local plist_path="$2"
  local attempt

  launchctl bootout "gui/$(id -u)/${label}" >/dev/null 2>&1 || true
  wait_for_launch_agent_absent "${label}"

  for attempt in {1..5}; do
    if launchctl bootstrap "gui/$(id -u)" "${plist_path}"; then
      launchctl kickstart -kp "gui/$(id -u)/${label}"
      return 0
    fi
    sleep 1
  done

  echo "failed to bootstrap launch agent ${label}" >&2
  return 1
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
  install_launch_agent "com.echo.cc_connect" "${LAUNCHD_AGENT_DIR}/com.echo.cc_connect.plist"
else
  echo "Installing systemd user unit for cc-connect"
  mkdir -p "${SYSTEMD_USER_DIR}"
  cp "${CC_CONNECT_UNIT_PATH}" "${SYSTEMD_USER_DIR}/cc-connect.service"
  systemctl --user daemon-reload
  systemctl --user enable --now cc-connect.service
fi

echo "cc-connect deployment completed"
