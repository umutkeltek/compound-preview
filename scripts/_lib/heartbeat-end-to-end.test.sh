#!/usr/bin/env bash
# scripts/_lib/heartbeat-end-to-end.test.sh
#
# End-to-end test of scripts/compound-autonomy-heartbeat.sh driven by a
# stub `compound` binary that emits a controlled driver JSON. Runs the real
# script with stub `work`, `memory`, `director`, `home archive-history`,
# `work sweep-stale`, and `work continue` subcommands. Asserts the FINAL_RC
# matches the semantic stage (committed + fresh skipped_locked -> 0;
# everything else -> nonzero). All other stages pass through (their stage
# logic is NOT exercised by this test — that's what
# heartbeat-promote-stage.test.sh covers).

set -eu

LIB_DIR=$(CDPATH= dirname -- "$0")
case "$LIB_DIR" in
  /*) ;;
  *)  LIB_DIR="$(pwd)/$LIB_DIR"
      [ -d "$LIB_DIR" ] || LIB_DIR="$(pwd)"
      ;;
esac

ROOT_DIR=$(CDPATH= cd -- "$LIB_DIR/../.." && pwd)
SCRIPT="$ROOT_DIR/scripts/compound-autonomy-heartbeat.sh"

TMPDIR=$(mktemp -d 2>/dev/null) || TMPDIR=/tmp/heartbeat-e2e.test.$$
mkdir -p "$TMPDIR"
trap 'rm -rf "$TMPDIR"' EXIT

# Stub `compound` — for `memory promote-driver` it renders the JSON passed in
# $FAKE_DRIVER_JSON the way the CLI does (indented, intent.work_id stamped
# from --work) and, when FAKE_DRIVER_RECEIPT=producer|legacy, first appends
# the compact terminal record the way the producer does. Exits 0. Other
# subcommands exit 0 too.
FAKE_BIN="$TMPDIR/fake-bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/compound" <<'STUB'
#!/usr/bin/env sh
# Stub for the heartbeat tests. Behaviour is driven by env vars set per case:
#   FAKE_DRIVER_JSON  — JSON to emit on `memory promote-driver --json` (else
#                       a default committed payload).
#   FAKE_HOLD_LOCK    — when set to 1, refuse to release the single-flight
#                       lock (we never take it; the test home has no lock).
# For all other subcommands the stub exits 0 and emits a minimal JSON shape
# the script's parsers accept.
case "${1:-}" in
  memory)
    case "${2:-}" in
      promote-driver)
        # --help path: emit usage that contains "promote-driver" so the
        # heartbeat's preflight recognises the subcommand is supported.
        if [ "${3:-}" = "--help" ] || [ "${3:-}" = "help" ]; then
          printf 'Usage: compound memory promote-driver [--json] [--confirm]\n'
          exit 0
        fi
        if [ -n "${FAKE_DRIVER_JSON:-}" ]; then
          # Bind the report to the envelope the heartbeat opened: the real
          # driver stamps intent.work_id from the same --work flag.
          FAKE_WORK_ID=""
          while [ $# -gt 0 ]; do
            if [ "$1" = "--work" ] && [ $# -gt 1 ]; then FAKE_WORK_ID="$2"; fi
            shift
          done
          export FAKE_WORK_ID
          python3 - <<'PY'
import json, os
d = json.loads(os.environ["FAKE_DRIVER_JSON"])
work = os.environ.get("FAKE_WORK_ID", "")
if work:
    d.setdefault("intent", {})["work_id"] = work
shape = os.environ.get("FAKE_DRIVER_RECEIPT", "")
if shape in ("producer", "legacy") and d.get("receipt_path"):
    # Producer contract: append the COMPACT terminal record before rendering
    # the report. "legacy" mimics an older producer that never stamped
    # receipt_path on the stored line.
    rec = dict(d)
    if shape == "legacy":
        rec.pop("receipt_path", None)
    os.makedirs(os.path.dirname(d["receipt_path"]), exist_ok=True)
    with open(d["receipt_path"], "a", encoding="utf-8") as fh:
        fh.write(json.dumps(rec, separators=(",", ":")) + "\n")
# CLI contract: stdout is the INDENTED rendering of the same report.
print(json.dumps(d, indent=1))
PY
        else
          printf '{"pass_id":"unknown","stage":"unknown"}\n'
        fi
        exit 0
        ;;
    esac
    ;;
  director)
    # FAKE_DIRECTOR_STDERR_LINE: emit a Go-style log line on STDERR before
    # the JSON, mimicking e.g. internal/eventstore logging "rebuilt index"
    # mid-run. The heartbeat must parse stdout only and keep this line as
    # tick-log diagnostics (the 2>&1 capture used to glue it onto the JSON).
    if [ -n "${FAKE_DIRECTOR_STDERR_LINE:-}" ]; then
      printf '%s\n' "$FAKE_DIRECTOR_STDERR_LINE" >&2
    fi
    printf '{"proposal_count":0,"suppressed_duplicate_count":0,"skipped_count":0}\n'
    exit 0
    ;;
  home)
    printf -- '- archived: 0\n- dry_run: false\n'
    exit 0
    ;;
  notice)
    # `notice deadman ping [--fail]`: echo the exact argv so the tick log
    # proves which form the heartbeat sent (green -> ping, red -> --fail).
    printf 'deadman: stub %s\n' "$*"
    exit 0
    ;;
  work)
    # `work continue` renders a controlled payload when the case provides
    # one (FAKE_CONTINUE_JSON), so CONT_SUM parsing — e.g. the R6K
    # execute.human surfacing — is testable end to end. Every other work
    # verb keeps the minimal shape the script's parsers accept.
    if [ "${2:-}" = "continue" ] && [ -n "${FAKE_CONTINUE_JSON:-}" ]; then
      printf '%s\n' "$FAKE_CONTINUE_JSON"
      exit 0
    fi
    printf '{"status":"ok","safe":0,"reclaimed":0,"skipped":0,"step_count":0}\n'
    exit 0
    ;;
esac
exit 0
STUB
chmod +x "$FAKE_BIN/compound"

run_heartbeat() {
  _case="$1"
  _expected_rc="$2"
  _extra_setup="$3"
  # Optional 4th arg: a grep pattern the tick log must match after the run
  # (used to assert stage stderr was routed into the log as diagnostics).
  _expected_log="${4:-}"

  _home="$TMPDIR/home-$$-$_case-$RANDOM"
  mkdir -p "$_home/.compound/state/promote-driver" "$_home/.compound/lock" \
           "$_home/.local/bin" "$_home/.local/state"

  # Provide a recent receipts.jsonl so the freshness check sees a holder.
  printf '{"pass_id":"prior","stage":"committed"}\n' \
    > "$_home/.compound/state/promote-driver/receipts.jsonl"
  touch "$_home/.compound/lock/promote-driver.lock"

  eval "$_extra_setup"

  # Run the heartbeat with our stub on PATH first so PATH resolution finds
  # the stub (the heartbeat reads $HOME/.local/bin/compound which we also
  # symlink below).
  ln -sf "$FAKE_BIN/compound" "$_home/.local/bin/compound"

  set +e
  HOME="$_home" PATH="$FAKE_BIN:$PATH" timeout 30 sh "$SCRIPT" >/dev/null 2>&1
  _rc=$?
  set -e

  if [ "$_rc" = "$_expected_rc" ]; then
    if [ -n "$_expected_log" ]; then
      if grep -q "$_expected_log" "$_home/.compound/autonomy-heartbeat.log" 2>/dev/null; then
        printf "ok: %s -> FINAL_RC=%s; tick log matched: %s\n" "$_case" "$_rc" "$_expected_log"
      else
        echo "FAIL: $_case"
        echo "  expected tick log to match: $_expected_log"
        echo "  --- last log line ---"
        tail -1 "$_home/.compound/autonomy-heartbeat.log" 2>/dev/null
        exit 1
      fi
    else
      printf "ok: %s -> FINAL_RC=%s\n" "$_case" "$_rc"
    fi
  else
    # Show the heartbeat log to diagnose.
    echo "FAIL: $_case"
    echo "  expected FINAL_RC=$_expected_rc"
    echo "  actual   FINAL_RC=$_rc"
    if [ -f "$_home/.compound/autonomy-heartbeat.log" ]; then
      echo "  --- last log line ---"
      tail -1 "$_home/.compound/autonomy-heartbeat.log"
    fi
    exit 1
  fi
}

# committed_driver_json renders the committed report for the home the
# heartbeat is about to run against ($_home is the per-case home that
# run_heartbeat mints; this is evaluated inside its _extra_setup).
committed_driver_json() {
  printf '{"pass_id":"%s","stage":"committed","home":"%s/.compound","receipt_path":"%s/.compound/state/promote-driver/receipts.jsonl","started_at":"2026-09-05T00:00:00Z","finished_at":"2026-09-05T00:00:01Z","promote":{"promoted":0},"sweep":{"adjudicated":0},"pending_after":0}' \
    "$1" "$2" "$2"
}

# ----------------------------------------------------------------------

echo "==> e2e: committed driver (process exit 0) -> FINAL_RC=0"

# The stub behaves like the real producer: it appends the compact terminal
# record (naming receipt_path and the tick's work id) to the home's
# receipts.jsonl, then prints the INDENTED report on stdout. The heartbeat
# must bind the two by identity and go green.
run_heartbeat "committed" "0" \
  'FAKE_DRIVER_JSON=$(committed_driver_json pass-committed-001 "$_home"); export FAKE_DRIVER_JSON; FAKE_DRIVER_RECEIPT=producer; export FAKE_DRIVER_RECEIPT' \
  'deadman rc=0 deadman: stub notice deadman ping$'

echo "==> e2e: director stderr noise before valid JSON -> FINAL_RC=0, stderr kept in tick log"

# A Go `log` line on stderr (eventstore index rebuild notice) used to be
# glued in front of the director JSON by the stage's `2>&1` capture, turning
# a green tick into rc=65 unparseable. The heartbeat must parse stdout only
# and route the stderr line into the tick log as diagnostics.
run_heartbeat "director-stderr-noise" "0" \
  'FAKE_DRIVER_JSON=$(committed_driver_json pass-committed-stderr-001 "$_home"); export FAKE_DRIVER_JSON; FAKE_DRIVER_RECEIPT=producer; export FAKE_DRIVER_RECEIPT; FAKE_DIRECTOR_STDERR_LINE="2026/09/10 03:03:05 eventstore: rebuilt index x"; export FAKE_DIRECTOR_STDERR_LINE' \
  'director stderr: .*eventstore: rebuilt index x'

echo "==> e2e: continue execute.human names every refused id -> FINAL_RC=0, both ids in tick log"

# R6K: execute.skipped carries only the FIRST refusal's family
# ("ineligible"), so a tick where nothing launched could not say WHICH
# envelopes were refused or why — that lives in execute.human, which the
# heartbeat did not log. The stub's `work continue` refuses two envelopes
# by id; the one-line summary must surface execute.human naming BOTH ids.
CONTINUE_JSON='{"status":"waiting","step_count":1,"execute":{"stage":"execute","skipped":"ineligible","status":"waiting","human":"did nothing: work_r6k_refused_aaa1 (ineligible:harness=codex), work_r6k_refused_bbb2 (ineligible:harness=codex); no eligible ready envelope"}}'
run_heartbeat "continue-execute-human" "0" \
  'FAKE_DRIVER_JSON=$(committed_driver_json pass-committed-continue-001 "$_home"); export FAKE_DRIVER_JSON; FAKE_DRIVER_RECEIPT=producer; export FAKE_DRIVER_RECEIPT; FAKE_CONTINUE_JSON="$CONTINUE_JSON"; export FAKE_CONTINUE_JSON' \
  'execute=skipped:ineligible execute_human=".*work_r6k_refused_aaa1.*work_r6k_refused_bbb2'

echo "==> e2e: committed driver whose stored receipt lacks receipt_path -> FINAL_RC=1"

# An older producer appended terminal records without receipt_path. The
# rendered report still claims committed, but the stored line cannot be
# bound to it — the heartbeat must refuse (identity_mismatch), never green.
run_heartbeat "committed-legacy-receipt" "1" \
  'FAKE_DRIVER_JSON=$(committed_driver_json pass-committed-legacy-001 "$_home"); export FAKE_DRIVER_JSON; FAKE_DRIVER_RECEIPT=legacy; export FAKE_DRIVER_RECEIPT'

echo "==> e2e: partial driver (process exit 0) -> FINAL_RC=1"

DRIVER_JSON='{"pass_id":"pass-partial-001","stage":"partial","home":"..."}'
run_heartbeat "partial" "1" \
  'FAKE_DRIVER_JSON="$DRIVER_JSON"; export FAKE_DRIVER_JSON' \
  'deadman rc=0 deadman: stub notice deadman ping --fail$'

echo "==> e2e: failed driver (process exit 0) -> FINAL_RC=1"

DRIVER_JSON='{"pass_id":"pass-failed-001","stage":"failed","error":"phase exploded"}'
run_heartbeat "failed" "1" \
  'FAKE_DRIVER_JSON="$DRIVER_JSON"; export FAKE_DRIVER_JSON'

echo "==> e2e: recovery_required driver (process exit 0) -> FINAL_RC=1"

DRIVER_JSON='{"pass_id":"pass-recovery-001","stage":"recovery_required","error":"terminal receipt unavailable"}'
run_heartbeat "recovery_required" "1" \
  'FAKE_DRIVER_JSON="$DRIVER_JSON"; export FAKE_DRIVER_JSON'

echo "==> e2e: unknown stage (process exit 0) -> FINAL_RC=1"

DRIVER_JSON='{"pass_id":"pass-unknown-001","home":"..."}'
run_heartbeat "unknown" "1" \
  'FAKE_DRIVER_JSON="$DRIVER_JSON"; export FAKE_DRIVER_JSON'

echo ""
echo "==========================================="
echo "  ALL E2E TESTS PASSED"
echo "==========================================="
