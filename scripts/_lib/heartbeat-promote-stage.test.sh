#!/usr/bin/env bash
# scripts/_lib/heartbeat-promote-stage.test.sh
#
# Shell-level tests for the promote-driver stage validation that the
# Compound autonomy heartbeat uses to decide rc. Exercises one stage case
# per test. Exits 0 on success, non-zero on first failure.
#
# Stages tested:
#   GREEN:
#     - committed (stage=committed, terminal receipt present; stdout is the
#       CLI's indented rendering, the receipt line is the producer's compact
#       record — identity binds, bytes do not)
#     - skipped_locked with FRESH heartbeat evidence (lock owner + recent receipts.jsonl)
#   RED:
#     - identity_mismatch (receipt line lacks receipt_path / names another
#       work id / report bound to a work id or home the caller did not open)
#     - unparseable (malformed JSON)
#     - partial    (stage=partial)
#     - failed     (stage=failed)
#     - recovery_required (stage=recovery_required)
#     - unknown    (stage=planned|operation_recorded|finalization_recorded|empty)
#     - missing-pass-id (pass_id blank)
#     - missing-receipt (stage=committed but receipt_path absent)
#     - stale-lock (stage=skipped_locked but heartbeat evidence older than threshold)

set -eu

LIB_DIR=$(CDPATH= dirname -- "$0")
case "$LIB_DIR" in
  /*) ;;
  *)  LIB_DIR="$(pwd)/$LIB_DIR"
      [ -d "$LIB_DIR" ] || LIB_DIR="$(pwd)"
      ;;
esac

TMPDIR=$(mktemp -d 2>/dev/null) || TMPDIR=/tmp/heartbeat-promote-stage.test.$$
mkdir -p "$TMPDIR"
trap 'rm -rf "$TMPDIR"' EXIT

# Use a 5-minute freshness window in tests so we can stamp receipts.jsonl to
# 1 minute ago (fresh) or 30 minutes ago (stale) without timing flakes.
export HEARTBEAT_PROMOTE_STAGE_FRESH_MIN="${HEARTBEAT_PROMOTE_STAGE_FRESH_MIN:-5}"

. "$LIB_DIR/heartbeat-promote-stage.sh"

# The heartbeat's stage function is called directly (no command substitution)
# so its globals (PD_RC, PD_STAGE, PD_PASS, ...) land in THIS shell. The
# test then asserts on $PD_RC and the supporting globals.
call_stage() {
  _json_path="$1"
  _home="$2"
  _pd_rc="$3"
  _payload="$4"
  _expect_work="${5:-}"
  # The helper reads the rendered report from the path — exactly the file
  # the heartbeat captured the driver's stdout into — never from stdin.
  printf '%s\n' "$_payload" > "$_json_path"
  # Disable set -e around the call so the failure can be asserted rather
  # than killing the script.
  set +e
  promote_driver_stage_decide "$_json_path" "$_home" "$_pd_rc" "$_expect_work"
  PD_RC=$?
  set -e
}

assert_stage() {
  _expect="$1"
  if [ "$_expect" != "$PD_STAGE" ]; then
    echo "FAIL: stage"
    echo "  expected stage=$_expect"
    echo "  actual   stage=$PD_STAGE (rc=$PD_RC pass=$PD_PASS)"
    exit 1
  fi
}

assert_proof() {
  _expect="$1"
  if [ "$_expect" != "$PD_PROOF" ]; then
    echo "FAIL: proof"
    echo "  expected proof=$_expect"
    echo "  actual   proof=$PD_PROOF"
    exit 1
  fi
}

# assert_outcome checks the fields the heartbeat actually closes work with.
# These MUST be derived from the stored terminal receipt, never from the
# rendered stdout, so a report cannot cite a claim its own evidence lacks.
assert_outcome() {
  _e_promoted="$1"
  _e_n="$2"
  if [ "$_e_promoted" != "$PD_PROMOTED" ] || [ "$_e_n" != "$PD_PROMOTED_N" ]; then
    echo "FAIL: outcome"
    echo "  expected promoted=[$_e_promoted] promoted_n=$_e_n"
    echo "  actual   promoted=[$PD_PROMOTED] promoted_n=$PD_PROMOTED_N"
    exit 1
  fi
}

# indent renders a compact JSON document the way the CLI prints a report
# (indented), so the committed case exercises the real producer/CLI shape.
indent() {
  printf '%s' "$1" | python3 -c 'import json, sys; print(json.dumps(json.load(sys.stdin), indent=1))'
}

sha256_of() {
  printf '%s' "$1" | python3 -c 'import hashlib, sys; print(hashlib.sha256(sys.stdin.read().encode("utf-8")).hexdigest())'
}

assert_rc() {
  _expect="$1"
  _msg="$2"
  if [ "$_expect" != "$PD_RC" ]; then
    echo "FAIL: $_msg"
    echo "  expected rc=$_expect"
    echo "  actual   rc=$PD_RC (stage=$PD_STAGE pass=$PD_PASS)"
    exit 1
  fi
  printf "ok: %s -> rc=%s stage=%s\n" "$_msg" "$PD_RC" "$PD_STAGE"
}

setup_home() {
  _home="$TMPDIR/home-$$-$RANDOM"
  mkdir -p "$_home/state/promote-driver" "$_home/lock"
  printf '%s' "$_home"
}

write_receipts() {
  _home="$1"
  _pass="$2"
  _stage="$3"
  printf '{"pass_id":"%s","stage":"%s"}\n' "$_pass" "$_stage" \
    > "$_home/state/promote-driver/receipts.jsonl"
}

# write_receipt_line writes one compact terminal record into receipts.jsonl
# the way the producer appends it (committed cases).
write_receipt_line() {
  _home="$1"
  _json_line="$2"
  printf '%s\n' "$_json_line" \
    > "$_home/state/promote-driver/receipts.jsonl"
}

freshen_receipts() {
  _home="$1"
  # Set mtime to 1 minute ago -> fresh under default 5-min window.
  touch -t "$(date -v-1M +%Y%m%d%H%M.%S 2>/dev/null || date -d '1 minute ago' +%Y%m%d%H%M.%S)" \
    "$_home/state/promote-driver/receipts.jsonl"
}

stale_receipts() {
  _home="$1"
  # Set mtime to 30 minutes ago -> stale under default 5-min window.
  touch -t "$(date -v-30M +%Y%m%d%H%M.%S 2>/dev/null || date -d '30 minutes ago' +%Y%m%d%H%M.%S)" \
    "$_home/state/promote-driver/receipts.jsonl"
}

take_lock() {
  _home="$1"
  # Just create the lock file. The function under test only checks file
  # presence + receipts.jsonl mtime for staleness — it does NOT take a flock
  # itself, so we don't need to hold one here either. The driver's own
  # single-flight is a separate concern.
  : > "$_home/lock/promote-driver.lock"
}

release_lock() {
  : # no-op; lock is just a file in the test fixture
}

# ----------------------------------------------------------------------

echo "==> GREEN: committed with terminal receipt (indented stdout, compact receipt)"

HOME_C=$(setup_home)
PASS_C="pass-committed-001"
WORK_C="work_promote_committed_001"
# The producer appends the COMPACT terminal record, naming its own file via
# receipt_path and the tick's work id via intent.work_id ...
RECEIPT_C="{\"pass_id\":\"$PASS_C\",\"stage\":\"committed\",\"home\":\"$HOME_C\",\"receipt_path\":\"$HOME_C/state/promote-driver/receipts.jsonl\",\"started_at\":\"2026-09-05T00:00:00Z\",\"finished_at\":\"2026-09-05T00:00:01Z\",\"intent\":{\"work_id\":\"$WORK_C\"},\"promote\":{\"promoted\":0},\"sweep\":{\"adjudicated\":0},\"pending_after\":0}"
write_receipt_line "$HOME_C" "$RECEIPT_C"
# ... while the CLI renders the same report INDENTED on stdout. The bytes
# differ; the identity must still bind, and the proof digest must be the
# stored line's, not the rendering's.
call_stage "$TMPDIR/driver-committed.json" "$HOME_C" "0" "$(indent "$RECEIPT_C")" "$WORK_C"
assert_rc 0 "committed -> rc=0"
assert_stage "committed"
assert_proof "evidence:promote-driver-pass:$PASS_C:committed:receipt-sha256=$(sha256_of "$RECEIPT_C")"

# ----------------------------------------------------------------------

echo "==> RED: committed but the stored line lacks receipt_path (older producer)"

HOME_O=$(setup_home)
PASS_O="pass-committed-legacy-001"
WORK_O="work_promote_legacy_001"
RENDERED_O="{\"pass_id\":\"$PASS_O\",\"stage\":\"committed\",\"home\":\"$HOME_O\",\"receipt_path\":\"$HOME_O/state/promote-driver/receipts.jsonl\",\"intent\":{\"work_id\":\"$WORK_O\"}}"
write_receipt_line "$HOME_O" "{\"pass_id\":\"$PASS_O\",\"stage\":\"committed\",\"home\":\"$HOME_O\",\"intent\":{\"work_id\":\"$WORK_O\"}}"
call_stage "$TMPDIR/driver-legacy.json" "$HOME_O" "0" "$RENDERED_O" "$WORK_O"
assert_rc 1 "stored line without receipt_path -> rc=1"
assert_stage "identity_mismatch:receipt_path"

# ----------------------------------------------------------------------

echo "==> RED: committed but the stored line belongs to another work id"

HOME_W=$(setup_home)
PASS_W="pass-committed-otherwork-001"
RENDERED_W="{\"pass_id\":\"$PASS_W\",\"stage\":\"committed\",\"home\":\"$HOME_W\",\"receipt_path\":\"$HOME_W/state/promote-driver/receipts.jsonl\",\"intent\":{\"work_id\":\"work_promote_this_tick\"}}"
write_receipt_line "$HOME_W" "{\"pass_id\":\"$PASS_W\",\"stage\":\"committed\",\"home\":\"$HOME_W\",\"receipt_path\":\"$HOME_W/state/promote-driver/receipts.jsonl\",\"intent\":{\"work_id\":\"work_promote_earlier_tick\"}}"
call_stage "$TMPDIR/driver-otherwork.json" "$HOME_W" "0" "$RENDERED_W" "work_promote_this_tick"
assert_rc 1 "stored line for another work id -> rc=1"
assert_stage "identity_mismatch:work_id"

# ----------------------------------------------------------------------

echo "==> RED: stdout claims a promotion the stored receipt never recorded"

# The heartbeat closes maintenance work with --memory-promoted <ids> and
# anchors that close to the stored receipt's SHA-256. Before this case
# existed, the ids were read off the RENDERED report while the digest was
# read off the STORED line, so a report could say "I promoted claim X; here
# is my receipt" while the cited receipt recorded no such promotion. The
# identity fields all match here — only the outcome diverges.
HOME_OC=$(setup_home)
PASS_OC="pass-committed-outcome-001"
WORK_OC="work_promote_outcome_001"
RCPT_OC="$HOME_OC/state/promote-driver/receipts.jsonl"
STORED_OC="{\"pass_id\":\"$PASS_OC\",\"stage\":\"committed\",\"home\":\"$HOME_OC\",\"receipt_path\":\"$RCPT_OC\",\"intent\":{\"work_id\":\"$WORK_OC\"},\"promote\":{\"promoted\":0,\"promotions\":[]},\"sweep\":{\"adjudicated\":0},\"pending_after\":0}"
RENDERED_OC="{\"pass_id\":\"$PASS_OC\",\"stage\":\"committed\",\"home\":\"$HOME_OC\",\"receipt_path\":\"$RCPT_OC\",\"intent\":{\"work_id\":\"$WORK_OC\"},\"promote\":{\"promoted\":1,\"promotions\":[{\"claim_id\":\"claim_never_stored\"}]},\"sweep\":{\"adjudicated\":0},\"pending_after\":0}"
write_receipt_line "$HOME_OC" "$STORED_OC"
call_stage "$TMPDIR/driver-outcome.json" "$HOME_OC" "0" "$(indent "$RENDERED_OC")" "$WORK_OC"
assert_rc 1 "stdout promotion absent from receipt -> rc=1"
assert_stage "identity_mismatch:outcome"

# ----------------------------------------------------------------------

echo "==> RED: stdout inflates the swept count the stored receipt recorded"

HOME_SC=$(setup_home)
PASS_SC="pass-committed-count-001"
WORK_SC="work_promote_count_001"
RCPT_SC="$HOME_SC/state/promote-driver/receipts.jsonl"
STORED_SC="{\"pass_id\":\"$PASS_SC\",\"stage\":\"committed\",\"home\":\"$HOME_SC\",\"receipt_path\":\"$RCPT_SC\",\"intent\":{\"work_id\":\"$WORK_SC\"},\"promote\":{\"promoted\":0},\"sweep\":{\"adjudicated\":2},\"pending_after\":7}"
RENDERED_SC="{\"pass_id\":\"$PASS_SC\",\"stage\":\"committed\",\"home\":\"$HOME_SC\",\"receipt_path\":\"$RCPT_SC\",\"intent\":{\"work_id\":\"$WORK_SC\"},\"promote\":{\"promoted\":0},\"sweep\":{\"adjudicated\":99},\"pending_after\":7}"
write_receipt_line "$HOME_SC" "$STORED_SC"
call_stage "$TMPDIR/driver-count.json" "$HOME_SC" "0" "$(indent "$RENDERED_SC")" "$WORK_SC"
assert_rc 1 "stdout count disagrees with receipt -> rc=1"
assert_stage "identity_mismatch:outcome"

# ----------------------------------------------------------------------

echo "==> GREEN: exported promotion ids are read off the stored receipt"

# The honest shape: producer marshals ONE report into the compact receipt
# line and the CLI renders that same report indented. Presentation differs,
# outcome agrees, and what the heartbeat exports comes from the receipt.
HOME_PG=$(setup_home)
PASS_PG="pass-committed-promoted-001"
WORK_PG="work_promote_promoted_001"
RCPT_PG="$HOME_PG/state/promote-driver/receipts.jsonl"
RECEIPT_PG="{\"pass_id\":\"$PASS_PG\",\"stage\":\"committed\",\"home\":\"$HOME_PG\",\"receipt_path\":\"$RCPT_PG\",\"intent\":{\"work_id\":\"$WORK_PG\"},\"promote\":{\"promoted\":2,\"promotions\":[{\"claim_id\":\"claim_a\"},{\"claim_id\":\"claim_b\"}]},\"sweep\":{\"adjudicated\":0},\"pending_after\":4}"
write_receipt_line "$HOME_PG" "$RECEIPT_PG"
call_stage "$TMPDIR/driver-promoted.json" "$HOME_PG" "0" "$(indent "$RECEIPT_PG")" "$WORK_PG"
assert_rc 0 "agreeing outcome -> rc=0"
assert_stage "committed"
assert_outcome "claim_a,claim_b" "2"

# ----------------------------------------------------------------------

echo "==> RED: committed but the report is for a work id this tick did not open"

HOME_B=$(setup_home)
PASS_B="pass-committed-stalework-001"
RECEIPT_B="{\"pass_id\":\"$PASS_B\",\"stage\":\"committed\",\"home\":\"$HOME_B\",\"receipt_path\":\"$HOME_B/state/promote-driver/receipts.jsonl\",\"intent\":{\"work_id\":\"work_promote_earlier_tick\"}}"
write_receipt_line "$HOME_B" "$RECEIPT_B"
call_stage "$TMPDIR/driver-stalework.json" "$HOME_B" "0" "$(indent "$RECEIPT_B")" "work_promote_this_tick"
assert_rc 1 "report bound to a different work id than the caller's -> rc=1"
assert_stage "identity_mismatch:work_binding"

# ----------------------------------------------------------------------

echo "==> RED: committed but the report's home is not the caller's home"

HOME_H=$(setup_home)
PASS_H="pass-committed-otherhome-001"
OTHER_HOME="$HOME_H/elsewhere"
mkdir -p "$OTHER_HOME/state/promote-driver"
RECEIPT_H="{\"pass_id\":\"$PASS_H\",\"stage\":\"committed\",\"home\":\"$OTHER_HOME\",\"receipt_path\":\"$OTHER_HOME/state/promote-driver/receipts.jsonl\",\"intent\":{\"work_id\":\"work_promote_this_tick\"}}"
printf '%s\n' "$RECEIPT_H" > "$OTHER_HOME/state/promote-driver/receipts.jsonl"
call_stage "$TMPDIR/driver-otherhome.json" "$HOME_H" "0" "$RECEIPT_H" "work_promote_this_tick"
assert_rc 1 "report for another home -> rc=1"
assert_stage "identity_mismatch:home_binding"

# ----------------------------------------------------------------------

echo "==> GREEN: skipped_locked with FRESH heartbeat evidence"

HOME_L=$(setup_home)
PASS_L="pass-locked-001"
take_lock "$HOME_L"
write_receipts "$HOME_L" "pass-prior-000" "committed"
freshen_receipts "$HOME_L"
call_stage "$TMPDIR/driver-locked-fresh.json" "$HOME_L" "0" \
  "{\"pass_id\":\"$PASS_L\",\"locked\":true,\"home\":\"$HOME_L\",\"started_at\":\"2026-09-05T00:00:00Z\",\"finished_at\":\"2026-09-05T00:00:01Z\"}"
release_lock
assert_rc 0 "skipped_locked + fresh heartbeat -> rc=0"

# ----------------------------------------------------------------------

echo "==> RED: unparseable JSON"

HOME_U=$(setup_home)
call_stage "$TMPDIR/driver-unparseable.json" "$HOME_U" "0" \
  "{ this is not valid json"
assert_rc 1 "unparseable -> rc=1"

# ----------------------------------------------------------------------

echo "==> RED: stage=partial"

HOME_P=$(setup_home)
PASS_P="pass-partial-001"
call_stage "$TMPDIR/driver-partial.json" "$HOME_P" "0" \
  "{\"pass_id\":\"$PASS_P\",\"stage\":\"partial\",\"home\":\"$HOME_P\",\"receipt_path\":\"$HOME_P/state/promote-driver/receipts.jsonl\",\"started_at\":\"2026-09-05T00:00:00Z\",\"finished_at\":\"2026-09-05T00:00:01Z\"}"
assert_rc 1 "partial -> rc=1"

# ----------------------------------------------------------------------

echo "==> RED: stage=failed"

HOME_F=$(setup_home)
PASS_F="pass-failed-001"
call_stage "$TMPDIR/driver-failed.json" "$HOME_F" "0" \
  "{\"pass_id\":\"$PASS_F\",\"stage\":\"failed\",\"home\":\"$HOME_F\",\"receipt_path\":\"$HOME_F/state/promote-driver/receipts.jsonl\",\"started_at\":\"2026-09-05T00:00:00Z\",\"finished_at\":\"2026-09-05T00:00:01Z\",\"error\":\"phase foo exploded\"}"
assert_rc 1 "failed -> rc=1"

# ----------------------------------------------------------------------

echo "==> RED: stage=recovery_required"

HOME_R=$(setup_home)
PASS_R="pass-recovery-001"
call_stage "$TMPDIR/driver-recovery.json" "$HOME_R" "0" \
  "{\"pass_id\":\"$PASS_R\",\"stage\":\"recovery_required\",\"home\":\"$HOME_R\",\"started_at\":\"2026-09-05T00:00:00Z\",\"finished_at\":\"2026-09-05T00:00:01Z\",\"error\":\"terminal receipt unavailable\"}"
assert_rc 1 "recovery_required -> rc=1"

# ----------------------------------------------------------------------

echo "==> RED: stage=unknown (empty stage)"

HOME_K=$(setup_home)
PASS_K="pass-unknown-001"
call_stage "$TMPDIR/driver-unknown.json" "$HOME_K" "0" \
  "{\"pass_id\":\"$PASS_K\",\"home\":\"$HOME_K\",\"started_at\":\"2026-09-05T00:00:00Z\",\"finished_at\":\"2026-09-05T00:00:01Z\"}"
assert_rc 1 "unknown (no stage) -> rc=1"

# ----------------------------------------------------------------------

echo "==> RED: missing pass_id"

HOME_M=$(setup_home)
call_stage "$TMPDIR/driver-no-pass.json" "$HOME_M" "0" \
  "{\"pass_id\":\"\",\"stage\":\"committed\",\"home\":\"$HOME_M\",\"receipt_path\":\"$HOME_M/state/promote-driver/receipts.jsonl\"}"
assert_rc 1 "missing-pass-id -> rc=1"

# ----------------------------------------------------------------------

echo "==> RED: missing terminal receipt (committed but receipt_path absent)"

HOME_N=$(setup_home)
PASS_N="pass-no-receipt-001"
call_stage "$TMPDIR/driver-no-receipt.json" "$HOME_N" "0" \
  "{\"pass_id\":\"$PASS_N\",\"stage\":\"committed\",\"home\":\"$HOME_N\",\"started_at\":\"2026-09-05T00:00:00Z\",\"finished_at\":\"2026-09-05T00:00:01Z\"}"
assert_rc 1 "missing-receipt -> rc=1"

# ----------------------------------------------------------------------

echo "==> RED: skipped_locked but heartbeat is STALE"

HOME_S=$(setup_home)
PASS_S="pass-locked-stale-001"
take_lock "$HOME_S"
write_receipts "$HOME_S" "pass-prior-001" "committed"
stale_receipts "$HOME_S"
call_stage "$TMPDIR/driver-locked-stale.json" "$HOME_S" "0" \
  "{\"pass_id\":\"$PASS_S\",\"locked\":true,\"home\":\"$HOME_S\",\"started_at\":\"2026-09-05T00:00:00Z\",\"finished_at\":\"2026-09-05T00:00:01Z\"}"
release_lock
assert_rc 1 "skipped_locked + stale heartbeat -> rc=1"

# ----------------------------------------------------------------------

echo ""
echo "==========================================="
echo "  ALL TESTS PASSED"
echo "==========================================="
