# ADR-0006: Package Consolidation Freeze Protocol

**Status:** Proposed; deferred until an operator-scheduled zero-active-lane window
**Date:** 2026-07-31
**Related:** ADR-0005 (Postgres+pgvector Storage Experiment)

This decision is independent of the storage experiment. Nothing in ADR-0005
ratifies package moves, and this protocol must not be executed as part of the
current additive persistence burst.

The freeze cannot change the storage experiment's admission contract:
`COMPOUND_DSN` remains a process-memory-only DSN; unknown/partial configuration
continues to fail closed without file or breadth fallback.

## Problem

Package consolidation (Phase 3) rewrites import paths across ~90K files.
Any active lane that has not yet landed will fail to compile after the
consolidation commit. With 6+ concurrent agent sessions and 80+ worktrees,
an uncoordinated consolidation would break every active lane.

## Protocol

### Pre-freeze (T-24h)

1. Operator announces freeze via:
   ```bash
   compound finding record \
     --title "CONSOLIDATION FREEZE: no new claims after <timestamp>" \
     --body "Phase 3 package consolidation lands at <window>. All lanes must land or park before <deadline>. No new work claims after this notice." \
     --source-class evidence
   ```

2. Every active lane checks `compound coordination` and sees the freeze notice.

3. Lanes that cannot land in time must:
   - `git stash` or commit WIP to their branch
   - Record their worktree path + branch in the freeze notice response
   - Park their work envelope: `compound work set-aside begin <work_id> --candidate <full-sha> --file <path> --by <actor> --reason "<why the dirt is foreign>"` (reverse with `compound work set-aside end <work_id>`)

### Freeze gate (T-0)

The consolidation lane runs:

```bash
# 1. Verify no active claims
compound coordination --json | jq '.me + .others | length'
# MUST be 0 (only the consolidation lane may hold a claim)

# 2. Verify no running work
compound workbench --json | jq '[.work[] | select(.state == "running" or .state == "verifying")] | length'
# MUST be 0

# 3. Verify no held leases
compound land-lease status --json | jq '.held'
# MUST be false

# 4. Prune stale worktrees
git worktree list | grep prunable | awk '{print $1}' | xargs -I{} git worktree remove --force {}
```

### Mechanical consolidation (single commit)

```bash
# 1. Run the import rewrite script (generated per the merge list approved for this decision)
./scripts/consolidate-imports.sh

# 2. Verify compilation
go build ./...

# 3. Run the full battery
compound proof-check --base $(git rev-parse main) \
  --work <consolidation-work-id> \
  --actor operator:<name> \
  --out /tmp/consolidation-proof.json

# 4. Architecture doctor
compound architecture doctor --strict

# 5. Land via ff-only
git merge --ff-only <consolidation-branch>
```

### Post-freeze (T+1h)

1. Stashed lanes rebase onto the new main:
   ```bash
   git fetch origin && git rebase origin/main
   ```

2. The consolidation work envelope records which packages moved where,
   so agents can resolve stale imports mechanically:
   ```bash
   compound work show <consolidation-work-id> --json | jq '.artifacts'
   # Contains: {"import_map": {"internal/attention": "internal/panel/attention", ...}}
   ```

3. Operator lifts the freeze.

## Agent instruction (add to AGENTS.md at Phase 3)

```markdown
## Consolidation import map (landed <date>)

The following packages moved. If your branch predates the consolidation,
run: `./scripts/fix-stale-imports.sh` before building.

| Old import | New import |
|-----------|-----------|
| compound/internal/attention | compound/internal/panel/attention |
| ... | ... |
```

## Why not gradual?

A gradual package-by-package migration would mean weeks of mixed import
paths, during which every agent must know "is this package pre- or
post-move?" That ambiguity is worse than a single clean break with a
mechanical fixup script. The freeze is short (1-2 hours); the ambiguity
would be long.
