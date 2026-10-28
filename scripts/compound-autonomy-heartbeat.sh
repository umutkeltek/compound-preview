#!/bin/sh
# nounset only, never errexit: this script is an aggregator that collects one
# rc per stage and MUST reach the final log line whatever a stage returns. An
# errexit window opened after the promote stage made a failing later stage
# (a non-zero command substitution) exit before its rc was captured and
# before the log line was written — the lost-evidence failure mode.
set -u

# Experimental Compound-owned autonomy heartbeat. Its promotion stage is the
# supported frozen candidate-only driver path; registry and lane-belief
# mutation are intentionally absent.
export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
LOG="$HOME/.compound/autonomy-heartbeat.log"
COMPOUND_HOME="$HOME/.compound"
BIN="$HOME/.local/bin/compound"
TIMEOUT_BIN=$(command -v gtimeout || command -v timeout || true)
TS=$(date -u +%FT%TZ)

# Stage-truth validation lives in a sourced lib so the heartbeat (sh) and the
# shell test (sh) share one surface. The lib parses the driver JSON strictly,
# verifies the terminal receipt, checks skipped_locked freshness, and decides
# GREEN vs RED — never trusting the binary's process exit code alone.
HEARTBEAT_LIB_DIR=$(CDPATH= dirname -- "$0")
case "$HEARTBEAT_LIB_DIR" in
  /*) ;;
  *)  HEARTBEAT_LIB_DIR="$(pwd)/$HEARTBEAT_LIB_DIR"
      [ -d "$HEARTBEAT_LIB_DIR" ] || HEARTBEAT_LIB_DIR="$(pwd)"
      ;;
esac
. "$HEARTBEAT_LIB_DIR/_lib/heartbeat-promote-stage.sh"

if [ ! -x "$BIN" ]; then
  echo "$TS preflight rc=69 ERR: installed compound unavailable at $BIN" >> "$LOG"
  exit 69
fi
if [ -z "$TIMEOUT_BIN" ]; then
  echo "$TS preflight rc=69 ERR: timeout utility unavailable; no stages executed" >> "$LOG"
  exit 69
fi
if ! "$BIN" memory promote-driver --help 2>&1 | grep -q "promote-driver"; then
  echo "$TS preflight rc=69 ERR: promote-driver unavailable in installed compound; no stages executed" >> "$LOG"
  exit 69
fi

# Stage stderr is diagnostics, never payload. Until 2026-09-10 the parsed
# stages captured with `2>&1`, so a Go `log` line on stderr (e.g.
# "eventstore: rebuilt index ...", internal/eventstore/index.go) was glued
# in front of the stage JSON and turned a green stage into rc=65
# unparseable. Every stage now captures stdout only into the variable (or
# file) that gets parsed, and its stderr is appended to the tick log
# prefixed with the stage name — visible, never parsed.
STAGE_STDERR_DIR=$(mktemp -d "${TMPDIR:-/tmp}/compound-heartbeat-stderr.XXXXXX") || {
  echo "$TS preflight rc=70 ERR: cannot create stage stderr dir; no stages executed" >> "$LOG"
  exit 70
}
trap 'rm -rf "$STAGE_STDERR_DIR"' EXIT HUP INT TERM

log_stage_stderr() {
  stderr_stage=$1
  stderr_file=$2
  [ -s "$stderr_file" ] || return 0
  awk -v ts="$TS" -v stage="$stderr_stage" '{ print ts " " stage " stderr: " $0 }' "$stderr_file" >> "$LOG"
}

# raw_head prints the first 300 bytes of a stage's raw output with line breaks
# collapsed, so an `ERR: unparseable` verdict in the one-line heartbeat log
# carries enough of the offending bytes to diagnose later. 300 bytes is enough
# to see a tool banner or an error envelope's head without bloating the log.
raw_head() { printf '%.300s' "$1" | tr -d '\n\r'; }

DIR_OUT=$("$TIMEOUT_BIN" 300 "$BIN" director propose --apply --json 2>"$STAGE_STDERR_DIR/director.err")
DIR_RC=$?
log_stage_stderr director "$STAGE_STDERR_DIR/director.err"
DIR_SUM=$(printf '%s' "$DIR_OUT" | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin)
    print("proposals=%s suppressed=%s skipped=%s" % (d.get("proposal_count"), d.get("suppressed_duplicate_count"), d.get("skipped_count")))
except Exception:
    print("unparseable")' 2>/dev/null)
[ "$DIR_RC" -ne 0 ] && DIR_SUM="ERR: $(printf '%.160s' "$DIR_OUT")"
[ "$DIR_SUM" = "unparseable" ] && DIR_RC=65 && DIR_SUM="ERR: unparseable JSON raw=$(raw_head "$DIR_OUT")"

# Landfill compaction, BEFORE the promote pass so the pass walks live work only.
# Terminal candidates (no_promote/rejected/promoted) never leave the inbox on
# their own, and the promote cursor re-walks all of them forever: measured
# 2026-09-01, 154,127 inbox entries of which 11 were `proposed`, so a 500-entry
# walk considered 0 candidates and still cost 78s. archive-history is the
# supported eviction door for exactly those three states (`parked` and
# `proposed` are never touched), and it is reversible via
# `compound home restore-history`. Without this stage the walk stays
# proportional to everything ever adjudicated instead of to what is live.
# The pass is BOUNDED: --max-files caps this tick's batch, and a durable scan
# cursor (archives/operational/scan-cursor.json) makes the next tick resume
# after the last examined entry instead of rescanning the inbox. Each file is
# journaled per-file (pending row -> move -> committed row, each fsynced), so
# even a hard kill mid-batch leaves a state the next pass reconciles — the
# historic reason for the generous timeout (move-all-then-commit-index) no
# longer exists, but the budget stays generous because the batch cost is
# dominated by per-file fsyncs, not by index rewrites. Steady state (backlog
# drained, cursor cleared) is seconds. Concurrent ticks are safe: the whole
# operation is inside the flock-based autonomy admission lock, so a second
# tick blocks and then finds nothing to do.
ARCH_OUT=$("$TIMEOUT_BIN" 3600 "$BIN" home archive-history --kind memory-candidate --max-files 2000 2>"$STAGE_STDERR_DIR/compaction.err")
ARCH_RC=$?
log_stage_stderr compaction "$STAGE_STDERR_DIR/compaction.err"
# A well-formed archive-history report ALWAYS prints its `- dry_run:` line
# (cmd/compound printHomeOperationalArchiveReport) but only prints
# `- archived: N` when N > 0 — so the empty steady state (nothing left to
# evict, tool exit 0) is a VALID `archived=0`, never `unparseable`. Only an
# output with neither the counts line nor a report shape is unparseable.
ARCH_SUM=$(printf '%s' "$ARCH_OUT" | awk -F': ' '
  /^- archived:/ { archived = $2; found = 1 }
  /^- dry_run:/ { report = 1 }
  END {
    if (found) print "archived=" archived;
    else if (report) print "archived=0";
    else print "unparseable"
  }' 2>/dev/null)
[ "$ARCH_RC" -ne 0 ] && ARCH_SUM="ERR: $(printf '%.160s' "$ARCH_OUT")"
[ "$ARCH_SUM" = "unparseable" ] && ARCH_RC=65 && ARCH_SUM="ERR: unparseable output raw=$(raw_head "$ARCH_OUT")"

PD_WORK_ID="work_promote_$(date -u +%Y%m%dT%H%M%SZ)_$$"
PD_ACTOR="scheduler:autonomy-heartbeat"
PD_RC=0
PD_SUM="not-run"
PD_TMP=$(mktemp -d "${TMPDIR:-/tmp}/compound-promote-heartbeat.XXXXXX") || exit 70
trap 'rm -rf "$PD_TMP" "$STAGE_STDERR_DIR"' EXIT HUP INT TERM

terminalize_failed_work() {
  failure_summary=$1
  failure_proof=$2
  if "$BIN" work close "$PD_WORK_ID" --outcome failed --move-type control_plane_repair --by "$PD_ACTOR" --summary "$failure_summary" --proof "$failure_proof" >"$PD_TMP/close.out" 2>"$PD_TMP/close.err"; then
    return 0
  fi
  "$BIN" work advance "$PD_WORK_ID" --to verifying --by "$PD_ACTOR" --verification "$failure_proof" --note "$failure_summary" >"$PD_TMP/recover.out" 2>"$PD_TMP/recover.err" || return 1
  "$BIN" work close "$PD_WORK_ID" --outcome failed --move-type control_plane_repair --by "$PD_ACTOR" --summary "$failure_summary" --proof "$failure_proof" >"$PD_TMP/close.out" 2>"$PD_TMP/close.err"
}

if ! "$BIN" work create \
  --work-id "$PD_WORK_ID" \
  --owner compound:memory-maintenance \
  --predicate "scheduled memory promote-driver pass terminalizes with an exact driver receipt" \
  --outcome "Run one bounded autonomous memory-maintenance pass" \
  --by "$PD_ACTOR" \
  --creator-harness script \
  --creator-model none \
  --creator-lane autonomy-heartbeat \
  --state ready \
  --canonical-surface compound://memory/promote-driver \
  --risk tier1 --privacy private >"$PD_TMP/create.out" 2>"$PD_TMP/create.err"; then
  PD_RC=1
  PD_SUM="ERR: work create $(printf '%.120s' "$(cat "$PD_TMP/create.err")")"
elif ! "$BIN" work claim "$PD_WORK_ID" --by "$PD_ACTOR" >"$PD_TMP/claim.out" 2>"$PD_TMP/claim.err"; then
  PD_RC=1
  PD_SUM="ERR: work claim $(printf '%.120s' "$(cat "$PD_TMP/claim.err")")"
  terminalize_failed_work "Scheduled memory maintenance claim failed" "failure:promote-driver:claim" || PD_SUM="$PD_SUM; terminalization failed"
elif ! "$BIN" work advance "$PD_WORK_ID" --to running --by "$PD_ACTOR" --note "scheduled bounded promote-driver pass" >"$PD_TMP/run.out" 2>"$PD_TMP/run.err"; then
  PD_RC=1
  PD_SUM="ERR: work advance $(printf '%.120s' "$(cat "$PD_TMP/run.err")")"
  terminalize_failed_work "Scheduled memory maintenance running advance failed" "failure:promote-driver:advance" || PD_SUM="$PD_SUM; terminalization failed"
else
	# --walk-scan-limit must EXCEED the whole live corpus or the dedupe phase can never
	# run: driver.go computes `walkedAll := cursor == "" && page.AtEnd && !blocked`, so
	# collapse happens only on a single pass that starts at the beginning AND reaches the
	# end. At the old 500 that was arithmetically unreachable — 104 consecutive passes
	# skipped dedupe with "resume walk". Post-compaction the corpus is ~51k (parked, which
	# is never evicted), and a full 19k walk measured 29s, so 60000 buys full coverage with
	# headroom and degrades gracefully (back to a resume walk) if parked ever outgrows it.
	"$TIMEOUT_BIN" 600 "$BIN" memory promote-driver \
		--confirm --work "$PD_WORK_ID" \
		--promote-limit 1 --scan-limit 100 --walk-scan-limit 60000 \
    --json >"$PD_TMP/driver.json" 2>"$PD_TMP/driver.err"
  PD_RC=$?
  # Strict semantic stage decision: the lib reads the driver JSON from the
  # captured file, binds a committed pass to THIS tick (home + work id) and
  # to its stored terminal receipt by identity, checks skipped_locked
  # freshness (lock owner + recent receipts.jsonl), and decides GREEN vs RED.
  # PD_RC is now the SEMANTIC rc, not the binary's process exit code.
  promote_driver_stage_decide "$PD_TMP/driver.json" "$COMPOUND_HOME" "$PD_RC" "$PD_WORK_ID"
  PD_RC=$?
  if [ "$PD_STAGE" = "committed" ] && [ "$PD_RC" -eq 0 ]; then
    "$BIN" work advance "$PD_WORK_ID" --to verifying --by "$PD_ACTOR" --verification "$PD_PROOF" --note "driver terminal receipt committed" >/dev/null 2>"$PD_TMP/verify.err" || PD_RC=1
    if [ "$PD_RC" -eq 0 ]; then
      if [ -n "$PD_PROMOTED$PD_PARKED$PD_REJECTED" ]; then
        set -- "$BIN" work close "$PD_WORK_ID" --outcome completed --move-type memory_delta --by "$PD_ACTOR" --summary "Scheduled memory maintenance committed" --changed --proof "$PD_PROOF"
        [ -n "$PD_PROMOTED" ] && set -- "$@" --memory-promoted "$PD_PROMOTED"
        [ -n "$PD_PARKED" ] && set -- "$@" --memory-parked "$PD_PARKED"
        [ -n "$PD_REJECTED" ] && set -- "$@" --memory-rejected "$PD_REJECTED"
      else
        set -- "$BIN" work close "$PD_WORK_ID" --outcome completed --move-type audit_only --by "$PD_ACTOR" --summary "Scheduled memory maintenance found no state delta" --proof "$PD_PROOF"
      fi
      "$@" >"$PD_TMP/close.out" 2>"$PD_TMP/close.err" || PD_RC=1
    fi
  elif [ "$PD_STAGE" = "skipped_locked" ] && [ "$PD_RC" -eq 0 ]; then
    # Another pass holds the single-flight lock and its heartbeat is fresh —
    # doctrine says keep waiting, never contend. We DO NOT close this envelope
    # as completed (no work was done by us); we advance to verifying with the
    # freshness proof so the next tick can pick it up.
    "$BIN" work advance "$PD_WORK_ID" --to verifying --by "$PD_ACTOR" --verification "$PD_PROOF" --note "driver skipped_locked; another pass holds the single-flight lock and is fresh" >/dev/null 2>"$PD_TMP/verify.err" || PD_RC=1
    if [ "$PD_RC" -eq 0 ]; then
      "$BIN" work close "$PD_WORK_ID" --outcome completed --move-type audit_only --by "$PD_ACTOR" --summary "Scheduled memory maintenance skipped (another pass holds lock; heartbeat fresh)" --proof "$PD_PROOF" >"$PD_TMP/close.out" 2>"$PD_TMP/close.err" || PD_RC=1
    fi
  else
    terminalize_failed_work "Scheduled memory maintenance did not commit ($PD_STAGE)" "failure:promote-driver:$PD_PASS:$PD_STAGE" || PD_SUM="ERR: driver failed and work terminalization failed"
  fi
  if [ "$PD_RC" -ne 0 ] && [ "$PD_STAGE" = "committed" ]; then
    terminalize_failed_work "Scheduled memory maintenance receipt close failed" "failure:promote-driver:$PD_PASS:work-close" || PD_SUM="ERR: committed driver pass left work nonterminal"
  fi
  if [ "$PD_RC" -eq 0 ]; then
    PD_SUM="stage=$PD_STAGE pass=$PD_PASS promoted=$PD_PROMOTED_N swept=$PD_SWEPT_N pending=$PD_PENDING work=$PD_WORK_ID"
  elif [ "$PD_STAGE" = "unparseable" ]; then
    PD_SUM="ERR: unparseable JSON stage=$PD_STAGE pass=$PD_PASS raw=$(raw_head "$(cat "$PD_TMP/driver.json" 2>/dev/null)")"
  else
    PD_SUM="ERR stage=$PD_STAGE pass=$PD_PASS $(printf '%.120s' "$(cat "$PD_TMP/driver.err" "$PD_TMP/close.err" 2>/dev/null)")"
  fi
fi
# The driver's stderr is diagnostics too: route it to the tick log like
# every other stage's (otherwise it vanished with the PD_TMP cleanup).
log_stage_stderr promote-driver "$PD_TMP/driver.err"

# Diagnosis only -- deliberately NOT --yes. The apply path terminalizes an
# envelope read before the liveness probe, so a concurrent actor that changes
# owner/metadata/attachments without changing state has those writes silently
# overwritten, and a process that becomes live after the probe can be
# terminalized under it. Unattended hourly mutation is the wrong place to carry
# that risk: the default dry-run still reports safe/reclaimed/skipped, so the
# backlog stays observable while nothing is rewritten. Restore --yes only once
# the reclaim is bound to the exact envelope generation it diagnosed and the
# envelope write + lifecycle event + lease release are one recoverable unit.
STALE_OUT=$("$TIMEOUT_BIN" 300 "$BIN" work sweep-stale --threshold 168h --json 2>"$STAGE_STDERR_DIR/stale-work.err")
STALE_RC=$?
log_stage_stderr stale-work "$STAGE_STDERR_DIR/stale-work.err"
STALE_SUM=$(printf '%s' "$STALE_OUT" | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin)
    print("safe=%s reclaimed=%s skipped=%s" % (d.get("safe"), d.get("reclaimed"), d.get("skipped")))
except Exception:
    print("unparseable")' 2>/dev/null)
[ "$STALE_RC" -ne 0 ] && STALE_SUM="ERR: $(printf '%.160s' "$STALE_OUT")"
[ "$STALE_SUM" = "unparseable" ] && STALE_RC=65 && STALE_SUM="ERR: unparseable JSON raw=$(raw_head "$STALE_OUT")"

# R6O (2026-09-11): refresh the lane-capacity sampler BEFORE the continue stage.
# The sampler is a launchd job that only runs while the machine is awake, so the
# first tick after sleep otherwise reads a window older than the seam's 10-minute
# freshness gate and refuses with capacity_unknown all morning. The kick goes
# through launchd (the sampler's owner) and is bounded: we wait at most
# SAMPLER_WAIT_S for the projection file's mtime to advance, then continue either
# way — the seam's own freshness gate stays the authority. Skipped entirely when
# the plist is not installed under this HOME (tests, foreign machines).
SAMPLER_LABEL=ai.compound.usage-supplement
SAMPLER_PLIST="$HOME/Library/LaunchAgents/$SAMPLER_LABEL.plist"
SAMPLER_FILE="$COMPOUND_HOME/projections/usage-supplement.jsonl"
SAMPLER_WAIT_S=${COMPOUND_HEARTBEAT_SAMPLER_WAIT_S:-120}
SAMPLER_SUM="skipped:no_plist"
if [ -f "$SAMPLER_PLIST" ] && command -v launchctl >/dev/null 2>&1; then
  SAMPLER_BEFORE=$(stat -f %m "$SAMPLER_FILE" 2>/dev/null || echo 0)
  if launchctl kickstart "gui/$(id -u)/$SAMPLER_LABEL" >/dev/null 2>&1; then
    SAMPLER_SUM="kicked:stale"
    SAMPLER_WAITED=0
    while [ "$SAMPLER_WAITED" -lt "$SAMPLER_WAIT_S" ]; do
      sleep 5; SAMPLER_WAITED=$((SAMPLER_WAITED + 5))
      SAMPLER_AFTER=$(stat -f %m "$SAMPLER_FILE" 2>/dev/null || echo 0)
      if [ "$SAMPLER_AFTER" -gt "$SAMPLER_BEFORE" ]; then SAMPLER_SUM="kicked:fresh_after=${SAMPLER_WAITED}s"; break; fi
    done
  else
    SAMPLER_SUM="kick_refused"
  fi
fi

CONT_OUT=$("$TIMEOUT_BIN" 600 "$BIN" work continue --actor claude-minimax --until-wait --max-steps 3 --max-execute-steps 1 --json 2>"$STAGE_STDERR_DIR/continue.err")
CONT_RC=$?
log_stage_stderr continue "$STAGE_STDERR_DIR/continue.err"
CONT_SUM=$(printf '%s' "$CONT_OUT" | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin)
    # R3A execute-stage: surface "execute=skipped:<reason>" when the
    # unattended seam ran but refused, and "execute=ok ratt=<id>" when
    # it drove a ready envelope. The seam logs `skipped` and
    # `run_attempt_ids` only when it actually triggers, so absence
    # means DispatchStep handled the tick as before.
    steps = d.get("step_count")
    # The run result mirrors the last execute verdict at the top level
    # (ContinueRunResult.Execute); older binaries only had it inside steps[].
    verdict = d.get("execute") or {}
    if not verdict:
        for step in d.get("steps") or []:
            if step.get("execute"):
                verdict = step["execute"]
    skipped = verdict.get("skipped") or d.get("skipped")
    ratts = verdict.get("run_attempt_ids") or d.get("run_attempt_ids") or []
    summary = "status=%s steps=%s" % (d.get("status"), steps)
    detached = verdict.get("detached") or d.get("detached") or {}
    if skipped:
        summary += " execute=skipped:%s" % skipped
    elif detached:
        # R6P: the launch was handed to a detached governed door; there is
        # no ratt yet on this tick. Name the door so a human can find it.
        summary += " execute=detached work=%s pid=%s log=%s" % (detached.get("work_id"), detached.get("pid"), detached.get("log_path"))
    elif ratts:
        summary += " execute=ok ratt=%s" % ratts[0]
    reconciled = verdict.get("reconciled") or d.get("detached_reconciled") or []
    if reconciled:
        summary += " reconciled=" + ",".join("%s:%s" % (r.get("work_id"), r.get("outcome")) for r in reconciled)
    # R6K: execute.skipped is only the FIRST refusal family (the seam cuts
    # the reason at ":"), so it can never answer "which envelopes were
    # refused and why?". execute.human names every refused id + reason and
    # the outcome — carry it on the one-line summary (newlines collapsed,
    # capped at 400 chars) so a nothing-launched tick is self-explaining.
    human = verdict.get("human") or ""
    if human:
        summary += " execute_human=\"%s\"" % " ".join(human.split())[:400]
    print(summary)
except Exception:
    print("unparseable")' 2>/dev/null)
[ "$CONT_RC" -ne 0 ] && CONT_SUM="ERR: $(printf '%.160s' "$CONT_OUT")"
[ "$CONT_SUM" = "unparseable" ] && CONT_RC=65 && CONT_SUM="ERR: unparseable JSON raw=$(raw_head "$CONT_OUT")"

FINAL_RC=0
if [ "$DIR_RC" -ne 0 ] || [ "$ARCH_RC" -ne 0 ] || [ "$PD_RC" -ne 0 ] || [ "$STALE_RC" -ne 0 ] || [ "$CONT_RC" -ne 0 ]; then
  FINAL_RC=1
fi
# Dead-man switch: a green tick pings the off-box check, a red tick pings its
# /fail endpoint, so a machine that stops ticking at all is noticed by
# something that is not on this machine. With no
# $COMPOUND_HOME/config/healthchecks-ping-url it is a no-op with no network.
# The ping reports the tick; it is not a stage and never changes FINAL_RC.
if [ "$FINAL_RC" -eq 0 ]; then
  DEADMAN_OUT=$("$TIMEOUT_BIN" 30 "$BIN" notice deadman ping 2>&1)
else
  DEADMAN_OUT=$("$TIMEOUT_BIN" 30 "$BIN" notice deadman ping --fail 2>&1)
fi
DEADMAN_RC=$?
DEADMAN_SUM=$(printf '%.160s' "$DEADMAN_OUT" | tr '\n\r' '  ')
echo "$TS final rc=$FINAL_RC | director rc=$DIR_RC $DIR_SUM | compaction rc=$ARCH_RC $ARCH_SUM | promote-driver rc=$PD_RC $PD_SUM | stale-work rc=$STALE_RC $STALE_SUM | sampler=$SAMPLER_SUM | continue rc=$CONT_RC $CONT_SUM | deadman rc=$DEADMAN_RC $DEADMAN_SUM" >> "$LOG"
exit "$FINAL_RC"
