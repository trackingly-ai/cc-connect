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

require_cmd python3
require_cmd go

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

echo "cc-connect worker prepared"
echo "Config: ${CC_CONNECT_CONFIG_PATH}"
echo "Binary: ${CC_CONNECT_BINARY}"
echo "Run:"
echo "  ${RUN_CC_CONNECT_SCRIPT}"
