#!/usr/bin/env bash
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ "${1:-mock}" != mock ]; then
  echo 'This command supports simulated upstreams only. It cannot authorize or certify real-upstream E2E.' >&2
  exit 2
fi
export DEVIN_E2E_ARTIFACTS="${DEVIN_E2E_ARTIFACTS:-$repo_dir/.cache/devin-client-journeys/$(date -u +%Y%m%dT%H%M%SZ)-$$}"
mkdir -p "$DEVIN_E2E_ARTIFACTS"
export DEVIN_JOURNEY_UI_DIST="$DEVIN_E2E_ARTIFACTS/product-ui"
pnpm --dir "$repo_dir/frontend" exec vite build --outDir "$DEVIN_JOURNEY_UI_DIST" --emptyOutDir
export DEVIN_JOURNEY_SUITE=true
export DEVIN_E2E_RUN='^TestDevinClientUserJourneysMock$'
exec bash "$repo_dir/deploy/devin-e2e.sh" mock
