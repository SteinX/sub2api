#!/usr/bin/env bash
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
case "${1:-mock}" in
  mock) ;;
  plan|live)
    exec python3 "$repo_dir/tools/devin-acceptance/authorized_journey.py" run --mode "$1" \
      --plan "${DEVIN_JOURNEY_PLAN_FILE:-}" --approval "${DEVIN_JOURNEY_APPROVAL_FILE:-}" \
      --ledger "${DEVIN_JOURNEY_RUN_LEDGER:-}" --artifacts "${DEVIN_E2E_ARTIFACTS:-}" ;;
  *) echo 'Usage: devin-user-journeys.sh mock|plan|live' >&2; exit 2 ;;
esac
export DEVIN_E2E_ARTIFACTS="${DEVIN_E2E_ARTIFACTS:-$repo_dir/.cache/devin-client-journeys/$(date -u +%Y%m%dT%H%M%SZ)-$$}"
mkdir -p "$DEVIN_E2E_ARTIFACTS"
export DEVIN_JOURNEY_UI_DIST="$DEVIN_E2E_ARTIFACTS/product-ui"
pnpm --dir "$repo_dir/frontend" exec vite build --outDir "$DEVIN_JOURNEY_UI_DIST" --emptyOutDir
export DEVIN_JOURNEY_SUITE=true
export DEVIN_E2E_RUN='^TestDevinClientUserJourneysMock$'
exec bash "$repo_dir/deploy/devin-e2e.sh" mock
