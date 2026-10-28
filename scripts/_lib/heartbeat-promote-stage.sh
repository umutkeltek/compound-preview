#!/usr/bin/env sh
# scripts/_lib/heartbeat-promote-stage.sh
#
# Stage-truth decision for the Compound autonomy heartbeat's promote-driver
# stage. Pure shell + python3 so the heartbeat (sh) and the stage test (sh)
# both depend on the same surface. Sourced by:
#   scripts/compound-autonomy-heartbeat.sh
#   scripts/_lib/heartbeat-promote-stage.test.sh
#
# Public surface:
#   promote_driver_stage_decide <driver_json_path> <home_path> <pd_rc> [expected_work_id]
#       argv1:  path the caller captured the driver's stdout (the rendered
#               report JSON) into. It is read from disk — never from stdin
#               or argv — so a large report cannot hit E2BIG and a caller
#               whose stdin is a terminal cannot block.
#       argv2:  home path; the lock file and receipts.jsonl are resolved here,
#               and a committed report's `home` must resolve to it.
#       argv3:  the binary's process exit code (PD_RC); used as a corroborating
#               signal but never as the only signal.
#       argv4:  optional work id the caller opened for this pass. When given,
#               a committed report's intent.work_id must equal it, so a stale
#               receipt from an earlier pass can never green this tick.
#       Sets globals (consumed by the heartbeat script):
#           PD_RC        0 when GREEN, 1 when RED
#           PD_STAGE     committed | skipped_locked | partial | failed
#                        | recovery_required | unparseable | unknown
#                        | missing_receipt | identity_mismatch:<fields>
#                        | stale_lock
#           PD_PASS      pass_id ("" if missing)
#           PD_PROOF     evidence:promote-driver-pass:<pass>:<stage>[:digest|fresh]
#           PD_PROMOTED  csv of promoted claim_ids
#           PD_PARKED    csv of parked candidate_ids
#           PD_REJECTED  csv of rejected candidate_ids
#           PD_PROMOTED_N PD_SWEPT_N PD_PENDING  counts
#           PD_FRESH_LOCK 1 when skipped_locked was verified FRESH, else 0
#
# GREEN only:
#   - stage == "committed" AND the receipt_path file holds a terminal line for
#     pass_id whose canonical identity (home, intent.work_id, receipt_path)
#     equals the rendered report's, AND the rendered report is bound to this
#     caller (home resolves to argv2; intent.work_id equals argv4 when given).
#     The SHA-256 of the matched receipt LINE is recorded in PD_PROOF as the
#     evidence anchor. The rendered stdout is deliberately NOT byte-compared
#     with the stored line: the CLI renders indented JSON while the producer
#     appends compact JSON, so a byte digest can never match a real pass.
#     Identity is the contract; presentation is not.
#   - OR locked=true AND lock file exists AND receipts.jsonl mtime is within
#     HEARTBEAT_PROMOTE_STAGE_FRESH_MIN minutes (a stale lock is RED).
#
# RED for everything else: malformed/unparseable, missing pass id, missing
# terminal receipt, identity mismatch, partial, failed, recovery_required,
# unknown stage, stale lock, and any failure to finalize.

# Default freshness window for skipped_locked liveness check. The hourly
# heartbeat cadence plus a 15-minute budget comfortably catches a stuck
# driver before the next tick. Tests override this via env to avoid timing
# flakes.
: "${HEARTBEAT_PROMOTE_STAGE_FRESH_MIN:=15}"

# raw_head prints the first 300 bytes of a stage's raw output with line breaks
# collapsed, so an `ERR: unparseable` verdict in the one-line heartbeat log
# carries enough of the offending bytes to diagnose later.
raw_head() { printf '%.300s' "$1" | tr -d '\n\r'; }

# decide_rc: 0 GREEN, 1 RED. Mutates globals documented above.
promote_driver_stage_decide() {
  _drv_json="$1"
  _home="$2"
  _pd_rc="$3"
  _expect_work="${4:-}"

  PD_RC=1
  PD_STAGE="unparseable"
  PD_PASS=""
  PD_PROOF=""
  PD_PROMOTED=""
  PD_PARKED=""
  PD_REJECTED=""
  PD_PROMOTED_N=0
  PD_SWEPT_N=0
  PD_PENDING=None
  PD_FRESH_LOCK=0

  # ------------------------------------------------------------------
  # 1. Parse + validate the driver JSON strictly. Malformed/missing
  #    required fields => RED with stage=unparseable. The python heredoc
  #    owns stdin, so the report travels by path (argv1), never by pipe.
  # ------------------------------------------------------------------
  _eval=$(HEARTBEAT_PROMOTE_STAGE_FRESH_MIN="$HEARTBEAT_PROMOTE_STAGE_FRESH_MIN" \
    python3 - "$_drv_json" "$_home" "$_pd_rc" "$_expect_work" <<'PY'
import hashlib, json, os, sys, time

drv_path, home, pd_rc, expect_work = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4].strip()
try:
    with open(drv_path, "r", encoding="utf-8", errors="replace") as fh:
        raw = fh.read()
except OSError as e:
    print("UNPARSEABLE|cannot read driver json %r: %s" % (drv_path, e))
    sys.exit(0)
fresh_min = int(os.environ.get("HEARTBEAT_PROMOTE_STAGE_FRESH_MIN", "15"))
try:
    d = json.loads(raw)
except Exception as e:
    print("UNPARSEABLE|" + repr(e)[:160])
    sys.exit(0)

pass_id = (d.get("pass_id") or "").strip()
stage = (d.get("stage") or "").strip()
locked = bool(d.get("locked"))
home_in_report = (d.get("home") or "").strip()
receipt_path = (d.get("receipt_path") or "").strip()
rendered_work = ((d.get("intent") or {}).get("work_id") or "").strip()

# outcome_of derives everything the parent script exports about what the
# pass DID (promoted/parked/rejected csv + counts). It is deliberately a
# function of ONE document so the identical derivation runs over the
# rendered report and over the stored terminal receipt: a divergence
# between the two is then a comparison, not a second implementation that
# can drift away from the first.
def outcome_of(doc):
    promote = doc.get("promote") or {}
    sweep = doc.get("sweep") or {}
    dedupe = doc.get("dedupe") or {}
    imports = doc.get("imports") or {}

    promoted = []
    for x in promote.get("promotions") or []:
        cid = x.get("claim_id")
        if cid:
            promoted.append(cid)
    for src in (imports.get("sources") or []):
        for cid in (src.get("claim_ids") or []):
            if cid:
                promoted.append(cid)

    changed = set(sweep.get("changed_ids") or [])
    parked = []
    for x in sweep.get("actions") or []:
        if x.get("verdict") == "park" and x.get("candidate_id") in changed:
            parked.append(x.get("candidate_id"))

    rejected = []
    for x in sweep.get("actions") or []:
        if x.get("verdict") in ("reject", "no_promote") and x.get("candidate_id") in changed:
            rejected.append(x.get("candidate_id"))
    for grp in dedupe.get("actions") or []:
        for cid in (grp.get("no_promoted") or []):
            if cid:
                rejected.append(cid)

    pending_after = doc.get("pending_after")
    return {
        "promoted": promoted,
        "parked": parked,
        "rejected": rejected,
        "promoted_n": str(promote.get("promoted", 0)),
        "swept_n": str(sweep.get("adjudicated", 0)),
        "pending": "None" if pending_after is None else str(pending_after),
    }


# The rendered report is a PRESENTATION of the pass. It is the thing the
# heartbeat would otherwise close work from, so it is the thing that must
# be checked -- never the thing that is trusted.
rendered_outcome = outcome_of(d)
# For the RED stages there is no stored terminal record to be authoritative,
# and those stages close nothing, so the rendered values stand as diagnostics.
# On the single GREEN committed path below this is REPLACED by the outcome
# read off the stored receipt.
outcome = rendered_outcome


def same_path(a, b):
    if not a or not b:
        return False
    try:
        return os.path.realpath(a) == os.path.realpath(b)
    except OSError:
        return False


# ------------------------------------------------------------------
# 2. Semantic stage decision. Anything not in the GREEN list is RED.
# ------------------------------------------------------------------
GREEN = False
verdict_stage = "unparseable"
proof_digest = ""
fresh_lock = False

if not pass_id:
    verdict_stage = "unparseable"
elif stage == "committed":
    # A real terminal receipt is required at receipt_path: a stored line for
    # this pass_id whose canonical identity equals the rendered report's,
    # bound to the home and work id this caller manages. The producer sets
    # receipt_path on the report BEFORE appending, so the stored line names
    # its own file; a line without it is an older producer or a forgery.
    if not receipt_path or not os.path.exists(receipt_path):
        verdict_stage = "missing_receipt"
    else:
        stored = None
        matching_line = None
        try:
            with open(receipt_path, "r", encoding="utf-8", errors="replace") as fh:
                for line in fh:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        rec = json.loads(line)
                    except Exception:
                        continue
                    if rec.get("pass_id") == pass_id and rec.get("stage") == "committed":
                        stored = rec
                        matching_line = line
                        break
        except OSError:
            stored = None
        if stored is None or matching_line is None:
            verdict_stage = "missing_receipt"
        else:
            stored_work = ((stored.get("intent") or {}).get("work_id") or "").strip()
            mismatch = []
            if not home_in_report or (stored.get("home") or "").strip() != home_in_report:
                mismatch.append("home")
            if not rendered_work or stored_work != rendered_work:
                mismatch.append("work_id")
            if (stored.get("receipt_path") or "").strip() != receipt_path:
                mismatch.append("receipt_path")
            if not same_path(home_in_report, home):
                mismatch.append("home_binding")
            if expect_work and rendered_work != expect_work:
                mismatch.append("work_binding")
            # Identity binding alone still let a report CLAIM an outcome its
            # own receipt never recorded: the digest anchored the stored line
            # while promoted/parked/rejected + counts were read off stdout, so
            # the heartbeat could close work citing a claim id that appears
            # nowhere in the evidence it cites. Presentation may differ from
            # the receipt (indented vs compact); the reported OUTCOME may not.
            # Safe to require equality: the producer marshals ONE report value
            # into the receipt line and returns that same value for the CLI to
            # render, so a real pass agrees with itself by construction.
            stored_outcome = outcome_of(stored)
            if stored_outcome != rendered_outcome:
                mismatch.append("outcome")
            if mismatch:
                verdict_stage = "identity_mismatch:" + ",".join(mismatch)
            else:
                # The digest is read off the stored line we matched — the
                # evidence anchor names the exact record, never a synthesized
                # string and never the presentation bytes on stdout.
                proof_digest = hashlib.sha256(matching_line.encode("utf-8")).hexdigest()
                # The stored terminal record is authoritative for what the
                # pass did. Everything the heartbeat exports downstream is now
                # derived from the same bytes proof_digest anchors.
                outcome = stored_outcome
                verdict_stage = "committed"
                GREEN = True
elif stage in ("planned", "operation_recorded", "finalization_recorded"):
    verdict_stage = "unknown"
elif stage == "partial":
    verdict_stage = "partial"
elif stage == "failed":
    verdict_stage = "failed"
elif stage == "recovery_required":
    verdict_stage = "recovery_required"
elif locked:
    # Skipped because another pass held the single-flight lock.
    # Doctrine: keep waiting, never contend — but ONLY when the holder is
    # alive. A stale lock (no recent receipts.jsonl progress) means the
    # holder is wedged and we MUST surface that.
    lock_path = os.path.join(home, "lock", "promote-driver.lock")
    rcpts = os.path.join(home, "state", "promote-driver", "receipts.jsonl")
    if not os.path.exists(lock_path):
        verdict_stage = "stale_lock"
    elif not os.path.exists(rcpts):
        verdict_stage = "stale_lock"
    else:
        age_min = (time.time() - os.path.getmtime(rcpts)) / 60.0
        if age_min > fresh_min:
            verdict_stage = "stale_lock"
        else:
            verdict_stage = "skipped_locked"
            fresh_lock = True
            GREEN = True
else:
    verdict_stage = "unknown"

print("FIELDS|" + "|".join([
    pass_id,
    stage,
    ",".join(outcome["promoted"]),
    ",".join(outcome["parked"]),
    ",".join(outcome["rejected"]),
    outcome["promoted_n"],
    outcome["swept_n"],
    outcome["pending"],
]))
print("VERDICT|" + verdict_stage + "|" + ("GREEN" if GREEN else "RED") + "|" + proof_digest + "|" + ("1" if fresh_lock else "0"))
PY
  )

  # Parse the two-line evaluation output.
  _fields_line=$(printf '%s' "$_eval" | sed -n 's/^FIELDS|//p')
  _verdict_line=$(printf '%s' "$_eval" | sed -n 's/^VERDICT|//p')

  IFS='|' read -r PD_PASS PD_STAGE_RAW PD_PROMOTED PD_PARKED PD_REJECTED PD_PROMOTED_N PD_SWEPT_N PD_PENDING <<EOF
$_fields_line
EOF

  IFS='|' read -r _stage _verdict _digest _fresh <<EOF
$_verdict_line
EOF

  PD_STAGE="$_stage"
  PD_FRESH_LOCK="$_fresh"
  # If python never produced a VERDICT line (malformed JSON), default to
  # unparseable so the assertion labels are accurate even on parse failure.
  if [ -z "$PD_STAGE" ]; then
    PD_STAGE="unparseable"
  fi
  if [ "$_verdict" = "GREEN" ]; then
    PD_RC=0
  else
    PD_RC=1
  fi

  # Build the evidence proof. For committed, include the receipt digest
  # (read off the receipt line, not synthesized). For skipped_locked,
  # include the freshness flag.
  case "$PD_STAGE" in
    committed)
      PD_PROOF="evidence:promote-driver-pass:$PD_PASS:committed:receipt-sha256=$_digest"
      ;;
    skipped_locked)
      PD_PROOF="evidence:promote-driver-pass:$PD_PASS:skipped_locked:fresh=$PD_FRESH_LOCK"
      ;;
    *)
      PD_PROOF="evidence:promote-driver-pass:$PD_PASS:$PD_STAGE"
      ;;
  esac

  # ------------------------------------------------------------------
  # 3. The process exit code is corroborating, not authoritative. If the
  #    JSON says it's green but the binary errored, we still surface a RED
  #    so the operator sees the disagreement.
  # ------------------------------------------------------------------
  if [ "$PD_RC" -eq 0 ] && [ "$_pd_rc" != "0" ]; then
    PD_RC=1
    PD_STAGE="unknown"
    PD_PROOF="evidence:promote-driver-pass:$PD_PASS:binary-nonzero"
  fi

  return "$PD_RC"
}
