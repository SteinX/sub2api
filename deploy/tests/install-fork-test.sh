#!/bin/bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TEST_DIR=$(mktemp -d)
trap 'rm -rf "$TEST_DIR"' EXIT

# Load definitions without running an installation or touching host services.
export GITHUB_REPO=SteinX/sub2api
source <(head -n -1 "$ROOT_DIR/deploy/install.sh")
test "$GITHUB_REPO" = SteinX/sub2api
github_api_curl() {
    printf '%s\n' "$@" > "$TEST_DIR/request"
    printf '{"tag_name":"v0.2.17-stx.1"}\n'
}
get_latest_version
test "$LATEST_VERSION" = v0.2.17-stx.1
grep -Fxq 'https://api.github.com/repos/SteinX/sub2api/releases/latest' "$TEST_DIR/request"

INSTALL_DIR="$TEST_DIR/install"
mkdir -p "$INSTALL_DIR"
cat > "$INSTALL_DIR/sub2api" <<'EOF'
#!/bin/sh
echo 'Sub2API 0.2.17-stx.1 (commit: abc, built: today)'
EOF
chmod +x "$INSTALL_DIR/sub2api"
test "$(get_current_version)" = 0.2.17-stx.1
echo 'fork installer repository and version checks passed'
