#!/usr/bin/env bash
# switchback-capture-maintenance.sh — the compound-owned owner entrypoint for
# Switchback sealed-capture backup, verify, and custody-gated prune.
#
# Why this exists: the retention registry (internal/policy/retention.go,
# class "archived-artifact") names THIS script as the reclaim owner, and the
# scheduled capture-backup lane execs it. When the referenced script does not
# exist, the schedule fails with exit 127 every cycle while health surfaces
# can still look plausible — an owner record pointing at nothing is exactly
# the failure shape this registry was built to prevent.
#
# Ownership boundary: Switchback is the authority for its own capture store.
# This script never touches store files directly; it delegates to the
# Switchback transfer tool (switchback-capture-backup.ts) which plans,
# transfers with per-segment checksum receipts, and prunes only segments whose
# remote custody is content-hash verified. Reclaim refuses to delete anything
# when a sealed day is short — an unverified custody claim cannot reach a
# deletion.
#
# Modes:
#   (no args) | --backup     transfer pending sealed segments (no reclaim)
#   --status                 read-only: destination, last receipt, pending work
#   --plan | --dry-run       read-only: show what a backup would transfer
#   --verify-only            read-only: check remote custody without transfer
#   --prune                  transfer + custody-gated reclaim. Dry-run by
#                            default; pass --yes to actually reclaim.
#   --mode-d                 target the Mode D capture store (segmented v2)
#   --keep-days N            reclaim floor for --prune (default 14)
#
# Environment:
#   SWITCHBACK_ROOT          switchback checkout (default: sibling of this repo)
#   SWITCHBACK_CLI           sb executable (default: $SWITCHBACK_ROOT/cli/sb)
#   SWITCHBACK_BACKUP_REMOTE     remote host   (default: from the store's own
#   SWITCHBACK_BACKUP_REMOTE_ROOT remote root   latest receipt, else required)
#   SWITCHBACK_BODY_STATE_DIR    store to operate on (set by --mode-d)
#
# Exit codes: 0 ok (including a refused/dry-run prune), 1 operational
# failure, 2 usage error, 3 another backup run holds the lock.

set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

MODE="status"
PRUNE_CONFIRMED=0
KEEP_DAYS="${SWITCHBACK_BACKUP_KEEP_DAYS:-14}"
MODE_D=0

usage_error() {
  printf 'usage: %s [--status|--backup|--plan|--dry-run|--verify-only|--prune [--yes]] [--mode-d] [--keep-days N]\n' "$0" >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --status|--backup|--plan|--dry-run|--verify-only|--prune)
      MODE="$1"
      ;;
    --yes)
      PRUNE_CONFIRMED=1
      ;;
    --mode-d)
      MODE_D=1
      ;;
    --keep-days)
      [ "$#" -ge 2 ] || usage_error
      KEEP_DAYS="$2"
      shift
      ;;
    -h|--help)
      sed -n '2,30p' "$0"
      exit 0
      ;;
    *)
      printf 'switchback-capture-maintenance: unknown flag %s\n' "$1" >&2
      usage_error
      ;;
  esac
  shift
done

case "$KEEP_DAYS" in
  ''|*[!0-9]*) printf 'switchback-capture-maintenance: --keep-days must be a non-negative integer\n' >&2; exit 2 ;;
esac
if [ "$PRUNE_CONFIRMED" = "1" ] && [ "$MODE" != "--prune" ]; then
  printf 'switchback-capture-maintenance: --yes only applies to --prune\n' >&2
  exit 2
fi

log() {
  printf '[%s] %s\n' "$(date -u +%FT%TZ)" "$*"
}

die() {
  log "ERROR: $*" >&2
  exit 1
}

# --- locate the Switchback authority -----------------------------------------

SWITCHBACK_ROOT="${SWITCHBACK_ROOT:-$(cd "$REPO_ROOT/.." && pwd)/switchback}"
TRANSFER_TOOL="${TRANSFER_TOOL:-$SWITCHBACK_ROOT/cli/switchback-capture-backup.ts}"
SWITCHBACK_CLI="${SWITCHBACK_CLI:-$SWITCHBACK_ROOT/cli/sb}"

BUN_BIN="${BUN_BIN:-$(command -v bun || true)}"
[ -n "$BUN_BIN" ] && [ -x "$BUN_BIN" ] || die "bun is required to run the Switchback transfer tool"
[ -f "$TRANSFER_TOOL" ] || die "Switchback transfer tool is missing: $TRANSFER_TOOL (set SWITCHBACK_ROOT)"
[ -x "$SWITCHBACK_CLI" ] || die "Switchback CLI is not executable: $SWITCHBACK_CLI (set SWITCHBACK_CLI)"

if [ "$MODE_D" = "1" ]; then
  export SWITCHBACK_BODY_STATE_DIR="${SWITCHBACK_BODY_STATE_DIR:-$HOME/.switchback/state/mode-d}"
fi

BODY_STATE_DIR="${SWITCHBACK_BODY_STATE_DIR:-$HOME/.switchback/state/body}"
LATEST_RECEIPT="$BODY_STATE_DIR/backup/latest-receipt.json"

# --- destination: the store's own receipt is the default, env overrides -------
# Nothing operator-specific is baked into this tracked script; the remote is
# discovered from the store the same way every cycle discovers it.

if [ -z "${SWITCHBACK_BACKUP_REMOTE:-}" ] || [ -z "${SWITCHBACK_BACKUP_REMOTE_ROOT:-}" ]; then
  if [ -f "$LATEST_RECEIPT" ]; then
    RECEIVED_ROOT="$(python3 -c '
import json,sys
try:
    r=json.load(open(sys.argv[1]))
    print(r.get("remote_root",""))
except Exception:
    sys.exit(1)
' "$LATEST_RECEIPT" 2>/dev/null || true)"
    case "$RECEIVED_ROOT" in
      *:*)
        if [ -z "${SWITCHBACK_BACKUP_REMOTE:-}" ]; then
          export SWITCHBACK_BACKUP_REMOTE="${RECEIVED_ROOT%%:*}"
        fi
        if [ -z "${SWITCHBACK_BACKUP_REMOTE_ROOT:-}" ]; then
          export SWITCHBACK_BACKUP_REMOTE_ROOT="${RECEIVED_ROOT#*:}"
        fi
        ;;
    esac
  fi
fi
if [ -z "${SWITCHBACK_BACKUP_REMOTE:-}" ] || [ -z "${SWITCHBACK_BACKUP_REMOTE_ROOT:-}" ]; then
  die "no backup destination: set SWITCHBACK_BACKUP_REMOTE and SWITCHBACK_BACKUP_REMOTE_ROOT (no prior receipt to inherit)"
fi

# --- lock: serialized so the switchback backup front door never runs
# --- concurrently with itself or a sibling backup caller ------------------

LOCK_DIR="${LOCK_DIR:-/tmp/switchback-capture-backup.${UID}.lock}"
LOCK_OWNER_FILE="$LOCK_DIR/owner"
LOCK_ACQUIRED=0
LOCK_START=""
LOCK_OBSERVED_PID=""

process_start() {
  ps -p "$1" -o lstart= 2>/dev/null | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

lock_owner_is_live() {
  local owner_pid owner_start observed_start
  LOCK_OBSERVED_PID=""
  [ -f "$LOCK_OWNER_FILE" ] || return 1
  IFS='|' read -r owner_pid owner_start < "$LOCK_OWNER_FILE" || return 1
  case "$owner_pid" in ''|*[!0-9]*) return 1 ;; esac
  [ -n "$owner_start" ] || return 1
  observed_start="$(process_start "$owner_pid")"
  [ -n "$observed_start" ] && [ "$observed_start" = "$owner_start" ] || return 1
  LOCK_OBSERVED_PID="$owner_pid"
  return 0
}

acquire_lock() {
  local missing_owner_retries=0 quarantine
  while ! mkdir "$LOCK_DIR" 2>/dev/null; do
    if lock_owner_is_live; then
      log "another capture backup run is active: pid=$LOCK_OBSERVED_PID lock=$LOCK_DIR"
      exit 3
    fi
    if [ ! -e "$LOCK_OWNER_FILE" ] && [ "$missing_owner_retries" -lt 5 ]; then
      missing_owner_retries=$((missing_owner_retries + 1))
      sleep 0.1
      continue
    fi
    quarantine="$LOCK_DIR.stale.$$.${RANDOM:-0}"
    if mv "$LOCK_DIR" "$quarantine" 2>/dev/null; then
      rm -f "$quarantine/owner"
      if ! rmdir "$quarantine" 2>/dev/null; then
        die "stale backup lock contained unexpected entries; quarantined at $quarantine"
      fi
      log "recovered stale backup lock: $LOCK_DIR"
      missing_owner_retries=0
    else
      die "cannot quarantine stale backup lock: $LOCK_DIR"
    fi
  done
  LOCK_START="$(process_start "$$")"
  [ -n "$LOCK_START" ] || die "cannot resolve backup process start time"
  printf '%s|%s\n' "$$" "$LOCK_START" > "$LOCK_OWNER_FILE"
  LOCK_ACQUIRED=1
}

cleanup() {
  local owner_pid owner_start
  [ "$LOCK_ACQUIRED" = "1" ] && [ -f "$LOCK_OWNER_FILE" ] || return 0
  IFS='|' read -r owner_pid owner_start < "$LOCK_OWNER_FILE" || return 0
  if [ "$owner_pid" = "$$" ] && [ "$owner_start" = "$LOCK_START" ]; then
    rm -f "$LOCK_OWNER_FILE"
    rmdir "$LOCK_DIR" 2>/dev/null || true
  fi
}

# --- read-only modes need no lock ---------------------------------------------

read_only_plan() {
  "$BUN_BIN" "$TRANSFER_TOOL" --plan
}

# bounded_plan_summary — the pending backlog plan can take minutes on a large
# estate, which is fine for an explicit --plan but not for --status. Status
# bounds the wait and degrades to a pointer instead of hanging a health check.
bounded_plan_summary() {
  local seconds="${PLAN_TIMEOUT_SECONDS:-45}"
  local tmp
  tmp="$(mktemp)"
  ("$BUN_BIN" "$TRANSFER_TOOL" --plan > "$tmp") &
  local plan_pid=$!
  local waited=0
  while kill -0 "$plan_pid" 2>/dev/null && [ "$waited" -lt "$seconds" ]; do
    sleep 1
    waited=$((waited + 1))
  done
  if kill -0 "$plan_pid" 2>/dev/null; then
    kill "$plan_pid" 2>/dev/null || true
    wait "$plan_pid" 2>/dev/null || true
    rm -f "$tmp"
    printf '  plan still running after %ss (large backlog) — run --plan directly\n' "$seconds"
    return 0
  fi
  if ! wait "$plan_pid" 2>/dev/null; then
    rm -f "$tmp"
    printf '  plan failed — run --plan directly\n'
    return 0
  fi
  python3 -c '
import json,sys
try:
    r=json.load(open(sys.argv[1]))
except Exception:
    print("  plan output not understood — run --plan directly")
    sys.exit(0)
plan=r.get("plan") or {}
print("  next_generation=%s pending_segments=%s pending_bytes=%s" % (
    plan.get("next_generation"), len(plan.get("segments") or []), plan.get("total_segment_bytes")))
' "$tmp" || printf '  plan output not understood — run --plan directly\n'
  rm -f "$tmp"
}

if [ "$MODE" = "--status" ]; then
  printf 'destination: %s:%s\n' "$SWITCHBACK_BACKUP_REMOTE" "$SWITCHBACK_BACKUP_REMOTE_ROOT"
  printf 'store:       %s\n' "$BODY_STATE_DIR"
  if [ -f "$LATEST_RECEIPT" ]; then
    python3 -c '
import json,sys
r=json.load(open(sys.argv[1]))
print("last receipt: generation=%s verified_through_day=%s segments=%s completed_at_unix_ms=%s" % (
    r.get("generation"), r.get("verified_through_day"), len(r.get("segments") or []), r.get("completed_at_unix_ms")))
' "$LATEST_RECEIPT"
  else
    printf 'last receipt: NONE — no completed backup cycle recorded\n'
  fi
  "$SWITCHBACK_CLI" body status --json 2>/dev/null | python3 -c '
import json,sys
try:
    s=json.load(sys.stdin)
except Exception:
    sys.exit(0)
p=s.get("pressure") or {}
print("pressure:     mode=%s healthy_backup_cycles=%s queue_depth=%s queue_drops=%s" % (
    p.get("mode"), p.get("healthy_backup_cycles"), p.get("queue_depth"), p.get("queue_drops")))
print("local store:  segments=%s events=%s reclaimable_index_bytes=%s" % (
    s.get("local_segment_count"), s.get("events"), s.get("index_reclaimable_bytes")))
' || true
  printf 'pending (plan):\n'
  bounded_plan_summary
  exit 0
fi

if [ "$MODE" = "--plan" ] || [ "$MODE" = "--dry-run" ]; then
  read_only_plan
  exit 0
fi

if [ "$MODE" = "--verify-only" ]; then
  acquire_lock
  trap cleanup EXIT INT TERM
  "$BUN_BIN" "$TRANSFER_TOOL" --verify-only
  exit 0
fi

# --- mutating modes: prune without --yes stays a dry-run ----------------------

if [ "$MODE" = "--prune" ] && [ "$PRUNE_CONFIRMED" != "1" ]; then
  log "prune dry-run (pass --yes to transfer and reclaim after verified custody)"
  read_only_plan
  exit 0
fi

acquire_lock
trap cleanup EXIT INT TERM

export SWITCHBACK_BACKUP_KEEP_DAYS="$KEEP_DAYS"
if [ "$MODE" = "--prune" ]; then
  export SWITCHBACK_BACKUP_RECLAIM=1
  log "backup + custody-gated prune (keep_days=$KEEP_DAYS)"
else
  log "backup (transfer only; reclaim requires --prune)"
fi

"$BUN_BIN" "$TRANSFER_TOOL"
EFFECTIVE_MODE="$MODE"
if [ "$MODE" = "status" ]; then
  # Bare invocation (the scheduled lane's form) is a backup, not a status.
  EFFECTIVE_MODE="backup"
fi
log "Switchback sealed-capture maintenance complete (mode=$EFFECTIVE_MODE)"
