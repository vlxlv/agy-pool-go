#!/usr/bin/env bash
# dual-test.sh: Comprehensive differential test runner for agy-pool Go migration
# Verifies zero-tolerance behavioral parity against Python v0.1.0-beta.2 reference

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
if [ -n "$AGY_PYTHON_ROOT" ] && [ -d "$AGY_PYTHON_ROOT" ]; then
    PYTHON_ROOT="$AGY_PYTHON_ROOT"
elif [ -d "$GO_ROOT/../agy-pool" ]; then
    PYTHON_ROOT="$(cd "$GO_ROOT/../agy-pool" && pwd)"
elif [ -d "$GO_ROOT/../../agy-pool" ]; then
    PYTHON_ROOT="$(cd "$GO_ROOT/../../agy-pool" && pwd)"
else
    PYTHON_ROOT="/home/codex/agy-pool"
fi

echo -e "\033[1;36m======================================================"
echo -e "       agy-pool Differential Parity Verification      "
echo -e "======================================================\033[0m"
echo -e "Python Reference: \033[1m$PYTHON_ROOT\033[0m"
echo -e "Go Candidate    : \033[1m$GO_ROOT\033[0m"
echo ""

# 1. Ensure isolated test state root
TMP_STATE="$(mktemp -d -t agy_dual_test_XXXXXX)"
cleanup() {
    rm -rf "$TMP_STATE"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

export AGY_TEST_MODE=1
export AGY_GEMINI_DIR="$TMP_STATE/.gemini"

# 2. Build Go development binary
echo -e "\033[33m[*] Building Go candidate binary...\033[0m"
cd "$GO_ROOT"
mkdir -p bin
go build -o bin/agy-pool ./cmd/agy-pool
echo -e "\033[32m[✓] Go binary built successfully.\033[0m"

# 3. Regenerate and verify golden fixtures from Python oracle
echo -e "\n\033[33m[*] Regenerating golden fixtures from Python reference...\033[0m"
python3 "$SCRIPT_DIR/generate_fixtures.py"
echo -e "\033[32m[✓] Golden fixtures generated.\033[0m"

# 4. Run Go unit and race tests
echo -e "\n\033[33m[*] Running Go test suite with race detector...\033[0m"
gofmt -w .
go vet ./...
go test -v -race ./...
echo -e "\033[32m[✓] All Go unit and race tests passed.\033[0m"

# 5. Run automated differential comparison suite
echo -e "\n\033[33m[*] Running Python vs Go differential test suite...\033[0m"
python3 "$SCRIPT_DIR/differential_test.py"
echo -e "\033[32m[✓] Differential test suite passed with 100% parity.\033[0m"

echo -e "\n\033[1;32m======================================================"
echo -e " ALL G1, G2A, G2B, G2C & G3 DIFFERENTIAL CHECKS PASSED! "
echo -e "======================================================\033[0m"
