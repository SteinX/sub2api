#!/usr/bin/env bash
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-mock}"
case "$mode" in mock|plan|live) ;; *) echo 'Usage: bash deploy/devin-agent-e2e.sh [mock|plan|live]' >&2; exit 2 ;; esac
if [ "$mode" != plan ] && [ ! -d "$repo_dir/tools/devin-e2e-clients/node_modules" ]; then
  echo 'Install pinned clients first: npm ci --prefix tools/devin-e2e-clients --ignore-scripts --omit=optional --no-audit --no-fund' >&2
  exit 2
fi
export DEVIN_E2E_AGENT_SUITE=true
export DEVIN_E2E_RUN="${DEVIN_E2E_RUN:-^TestDevinAgentClients$}"
exec bash "$repo_dir/deploy/devin-e2e.sh" "$mode"
