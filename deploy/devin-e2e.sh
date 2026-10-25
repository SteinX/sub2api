#!/usr/bin/env bash
set -euo pipefail
umask 077

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-mock}"
case "$mode" in
  mock) ;;
  plan|live)
    export DEVIN_E2E_PAT="${DEVIN_E2E_PAT:-${DEVIN_E2E_CREDENTIAL:-}}"
    : "${DEVIN_E2E_PAT:?runtime credential reference unavailable; check this instance binding/version, not the secret value}"
    case "$DEVIN_E2E_PAT" in *$'\r'*|*$'\n'*) echo 'Invalid credential reference: newline characters are not allowed' >&2; exit 2 ;; esac
    if [ "$mode" = live ] && [ "${DEVIN_E2E_ALLOW_PAID:-false}" != true ]; then
      echo 'Paid inference is disabled. Review make test-devin-plan, then explicitly set DEVIN_E2E_ALLOW_PAID=true after approval.' >&2
      exit 2
    fi
    ;;
  *) echo 'Usage: bash deploy/devin-e2e.sh [mock|plan|live]' >&2; exit 2 ;;
esac
export DEVIN_E2E_MODE="$mode"
export DEVIN_E2E_ARTIFACTS="${DEVIN_E2E_ARTIFACTS:-$repo_dir/.cache/devin-e2e/$(date -u +%Y%m%dT%H%M%SZ)-$$}"
mkdir -p "$DEVIN_E2E_ARTIFACTS"
export DEVIN_E2E_ARTIFACTS="$(cd "$DEVIN_E2E_ARTIFACTS" && pwd)"
export DEVIN_E2E_BINARY="$DEVIN_E2E_ARTIFACTS/server"

if [ "$mode" != mock ]; then
  # Fetch the same public catalog configured by the backend, through the inherited
  # proxy/CA. Its remote client need not have direct DNS/network access here.
  pricing_source=https://raw.githubusercontent.com/Wei-Shaw/model-price-repo/main/model_prices_and_context_window
  export DEVIN_E2E_PRICING_CATALOG="$DEVIN_E2E_ARTIFACTS/reference-prices.json"
  curl --fail --silent --show-error --max-time 30 --output "$DEVIN_E2E_PRICING_CATALOG" "$pricing_source.json"
  curl --fail --silent --show-error --max-time 30 --output "$DEVIN_E2E_ARTIFACTS/reference-prices.sha256" "$pricing_source.sha256"
  read -r expected_pricing_hash _ < "$DEVIN_E2E_ARTIFACTS/reference-prices.sha256"
  [[ "$expected_pricing_hash" =~ ^[a-fA-F0-9]{64}$ ]] || { echo 'Invalid reference pricing checksum' >&2; exit 2; }
  actual_pricing_hash="$(sha256sum "$DEVIN_E2E_PRICING_CATALOG")"
  export DEVIN_E2E_PRICING_SHA256="${actual_pricing_hash%% *}"
  [ "$DEVIN_E2E_PRICING_SHA256" = "${expected_pricing_hash,,}" ] || { echo 'Reference pricing checksum mismatch' >&2; exit 2; }
fi

# Select the managed local daemon explicitly; preserve proxy and registry config.
unset DOCKER_HOST DOCKER_CONTEXT DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH
docker_cli=(docker --host=unix:///var/run/docker.sock)
"${docker_cli[@]}" info >/dev/null
project="devin-e2e-$$"
compose=("${docker_cli[@]}" compose -p "$project" -f "$repo_dir/deploy/docker-compose.devin-e2e.yml")
export DEVIN_E2E_DB_PASSWORD
DEVIN_E2E_DB_PASSWORD="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
cleanup() {
  if [ -f "$DEVIN_E2E_ARTIFACTS/server.pid" ]; then
    read -r backend_pid < "$DEVIN_E2E_ARTIFACTS/server.pid" || true
    if [[ "${backend_pid:-}" =~ ^[0-9]+$ ]] && [ "$(readlink "/proc/$backend_pid/exe" 2>/dev/null || true)" = "$DEVIN_E2E_BINARY" ]; then
      kill "$backend_pid" 2>/dev/null || true
    fi
  fi
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"${compose[@]}" up -d --wait --wait-timeout 90
export DEVIN_E2E_PG_PORT DEVIN_E2E_REDIS_PORT
DEVIN_E2E_PG_PORT="$("${compose[@]}" port postgres 5432 | awk -F: '{print $NF}')"
DEVIN_E2E_REDIS_PORT="$("${compose[@]}" port redis 6379 | awk -F: '{print $NF}')"
cd "$repo_dir/backend"
timeout --kill-after=10s 5m go build -mod=readonly -o "$DEVIN_E2E_BINARY" ./cmd/server
{
  printf 'mode=%s\n' "$mode"
  printf 'commit=%s\n' "$(git rev-parse HEAD)"
  printf 'dirty_files=%s\n' "$(git status --porcelain | wc -l | tr -d ' ')"
  printf 'test_run=%s\n' "${DEVIN_E2E_RUN:-.}"
  printf 'test_skip=%s\n' "${DEVIN_E2E_SKIP:-^$}"
  printf 'reference_pricing_sha256=%s\n' "${DEVIN_E2E_PRICING_SHA256:-synthetic-group-prices}"
  printf 'acceptance_tier=protocol_or_client_contract\nreal_user_e2e=false\n'
  go version
  sha256sum "$DEVIN_E2E_BINARY"
} > "$DEVIN_E2E_ARTIFACTS/provenance.txt"
echo "Running Devin $mode contracts (not product user E2E); private artifacts: $DEVIN_E2E_ARTIFACTS"
test_seconds="${DEVIN_E2E_TIMEOUT_SECONDS:-600}"
[[ "$test_seconds" =~ ^[0-9]+$ ]] && (( test_seconds > 0 && test_seconds <= 3600 )) || { echo 'Invalid test wall time' >&2; exit 2; }
timeout --kill-after=15s "$((test_seconds+30))s" go test -mod=readonly -count=1 -tags=devine2e -run "${DEVIN_E2E_RUN:-.}" -skip "${DEVIN_E2E_SKIP:-^$}" -json -timeout="${test_seconds}s" ./internal/devine2e | tee "$DEVIN_E2E_ARTIFACTS/results.jsonl"
