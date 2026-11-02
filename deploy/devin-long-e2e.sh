#!/usr/bin/env bash
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-mock}"
if [ "$mode" = live ] && [ "${DEVIN_E2E_LONG_ALLOW_PAID:-false}" != true ]; then
  echo 'Review the long-task plan and approve its limits separately before live generation.' >&2
  exit 2
fi
export DEVIN_E2E_LONG_TASK=true
export DEVIN_E2E_RUN="${DEVIN_E2E_RUN:-^TestDevinLongToolWorkflow$|^TestLongWorkflowBudgetBounds$}"
exec bash "$repo_dir/deploy/devin-e2e.sh" "$mode"
