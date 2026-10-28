#!/usr/bin/env bash
# lane-safety-check.sh — run from ANY lane to verify you won't conflict
# with the storage migration or consolidation work.
#
# Usage: ./scripts/lane-safety-check.sh [your-worktree-path]
#
# Safe if ALL of:
#   1. You don't import any package on the consolidation merge list
#      (or you've already rebased post-consolidation)
#   2. You don't write directly to COMPOUND_HOME/ledger or events
#      (those get a storage backend switch)
#   3. main compiles cleanly (no mid-consolidation state)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${1:-$REPO_ROOT}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

echo "=== Lane Safety Check ==="
echo "Checking: $(pwd)"
echo ""

WARNINGS=0
FATAL=0

# Every scan below is scoped to TRACKED files via `git grep`, never `grep -r .`.
#
# A recursive grep from a repo root descends into the agent worktree containers
# (.claude/worktrees, .agents/worktrees, .hermes/worktrees), each holding a near-identical
# checkout of this same tree. On a machine carrying 36 worktrees that reported every hit ~36
# times, which made this check slow and its output unreadable. `git grep` searches the tracked
# set by construction -- the containers are gitignored, so they are excluded without a
# hand-maintained skip list to go stale. Same instinct as the leak gate and the retention scan:
# ask git what is actually in the repo.
#
# Not being in a git worktree is FATAL rather than an empty result. A scan that silently finds
# nothing because it could not look is the silent-green failure this check exists to prevent.
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo -e "${RED}[fail]${NC} not inside a git worktree — cannot scope the scan to tracked files"
  echo "  → run this from a checkout of the repo (or pass its path as \$1)"
  exit 1
fi
GO_SCOPE=("--" "*.go" ":(exclude)vendor/*" ":(exclude)*_test.go")

# 1. Check for consolidation-sensitive imports
MERGE_PKGS="attention|executorlens|lensmodulator|negatives|reviewqueue|statussurface|stream|verdictflow|laneselect|finding|intent|narrate|schema|knowledge"
hits=$(git grep -n "\"compound/internal/\(${MERGE_PKGS}\)\"" "${GO_SCOPE[@]}" 2>/dev/null || true)

if [[ -n "$hits" ]]; then
  echo -e "${YELLOW}[warn]${NC} Your lane imports packages on the consolidation list:"
  echo "$hits" | head -10 || true
  echo "  → After consolidation lands, rebase onto main and run scripts/consolidate-imports.sh --apply"
  WARNINGS=$((WARNINGS + 1))
else
  echo -e "${GREEN}[ok]${NC} No consolidation-sensitive imports"
fi

# 2. Check for direct COMPOUND_HOME storage writes
storage_hits=$(git grep -n "filepath.Join.*\"ledger\"\|filepath.Join.*\"events\"\|filepath.Join.*\"inbox\"" "${GO_SCOPE[@]}" 2>/dev/null | grep -v "internal/home\|internal/eventstore\|internal/mem" || true)

if [[ -n "$storage_hits" ]]; then
  echo -e "${YELLOW}[warn]${NC} Your lane writes to storage directories that will get a backend switch:"
  echo "$storage_hits" | head -5 || true
  echo "  → Use internal/home or internal/eventstore APIs instead of raw paths"
  WARNINGS=$((WARNINGS + 1))
else
  echo -e "${GREEN}[ok]${NC} No direct storage path writes"
fi

# 3. Check main compiles. Compilation failure is always fatal; warnings never
# downgrade it to a successful exit.
if go build ./cmd/compound 2>/dev/null; then
  echo -e "${GREEN}[ok]${NC} cmd/compound compiles"
else
  echo -e "${RED}[fail]${NC} cmd/compound does NOT compile — do not land"
  FATAL=1
fi

echo ""
if [[ $FATAL -ne 0 ]]; then
  echo -e "${RED}UNSAFE: Fix compilation before proceeding.${NC}"
  exit 1
elif [[ $WARNINGS -eq 0 ]]; then
  echo -e "${GREEN}SAFE: Your lane has no conflicts with the migration plan.${NC}"
  exit 0
else
  echo -e "${YELLOW}WARN: ${WARNINGS} migration-plan conflict(s); compilation is healthy.${NC}"
  exit 0
fi
