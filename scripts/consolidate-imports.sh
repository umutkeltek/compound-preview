#!/usr/bin/env bash
# consolidate-imports.sh — mechanical import rewrite for Phase 3 consolidation.
# DRY RUN by default. Pass --apply to actually rewrite.
#
# Usage:
#   ./scripts/consolidate-imports.sh              # dry run: show what would change
#   ./scripts/consolidate-imports.sh --apply      # rewrite imports in place
#
# Prerequisites:
#   - Freeze protocol (ADR-0005 annex) gate checks passed
#   - No active lanes (compound coordination shows 0 others)
#   - Clean main checkout
#
# This script is idempotent: running it twice is a no-op the second time.
# Compatible with bash 3.2+ (macOS default — no associative arrays).

set -eo pipefail

APPLY=false
if [ "${1:-}" = "--apply" ]; then
  APPLY=true
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

echo "=== Compound Package Consolidation ==="
if $APPLY; then
  echo "Mode: APPLY (rewriting files)"
else
  echo "Mode: DRY RUN (no changes)"
fi
echo ""

# ── Merge map (from ADR-0005) ──────────────────────────────────────────
# Each line: "old_package new_location" (space-separated, relative to internal/)
MERGE_LIST="attention panel/attention
executorlens behaviorallens/executorlens
lensmodulator behaviorallens/lensmodulator
negatives panel/negatives
reviewqueue work/reviewqueue
statussurface harness/statussurface
stream eventstore/stream
verdictflow panel/verdictflow
laneselect routeevidence/laneselect
finding home/finding
intent work/intent
narrate panel/narrate
schema envelope/schema
knowledge candidates/knowledge"

TOTAL_FILES=0
TOTAL_PKGS=0

echo "$MERGE_LIST" | while IFS=' ' read -r old_pkg new_loc; do
  # Skip blank lines
  [ -z "$old_pkg" ] && continue

  old_import="compound/internal/${old_pkg}"
  new_import="compound/internal/${new_loc}"

  # Find all .go files that import the old package (exclude worktrees/.git)
  affected_files=$(grep -rl "\"${old_import}\"" --include="*.go" \
    --exclude-dir=".git" --exclude-dir=".claude" --exclude-dir=".codex" \
    --exclude-dir="node_modules" --exclude-dir="console" \
    ./cmd ./internal ./adapters ./tools 2>/dev/null || true)

  if [ -z "$affected_files" ]; then
    echo "  [skip] ${old_pkg} -> ${new_loc}: no imports found"
    continue
  fi

  file_count=$(echo "$affected_files" | wc -l | tr -d ' ')
  TOTAL_FILES=$((TOTAL_FILES + file_count))
  TOTAL_PKGS=$((TOTAL_PKGS + 1))

  echo "  [rewrite] ${old_pkg} -> ${new_loc}: ${file_count} files"

  if $APPLY; then
    # Rewrite imports in all affected files
    echo "$affected_files" | xargs sed -i '' "s|\"${old_import}\"|\"${new_import}\"|g"

    # Move the actual package directory
    old_dir="internal/${old_pkg}"
    new_dir="internal/${new_loc}"
    if [ -d "$old_dir" ] && [ ! -d "$new_dir" ]; then
      mkdir -p "$(dirname "$new_dir")"
      mv "$old_dir" "$new_dir"
      echo "    moved: ${old_dir} -> ${new_dir}"
    fi
  fi
done

echo ""
echo "=== Summary ==="
echo "Packages on merge list: 14"

if ! $APPLY; then
  echo ""
  echo "DRY RUN complete. Re-run with --apply to execute."
  echo "Remember: freeze protocol gate checks must pass first:"
  echo "  1. compound coordination --json   -> 0 others active"
  echo "  2. compound workbench --json      -> 0 running/verifying"
  echo "  3. compound land-lease status     -> not held"
  echo "  4. git worktree prune             -> clean stale worktrees"
fi
