#!/usr/bin/env bash
set -u

# relational-dynamics-panel-e2e.sh — prove the "relational-dynamics" reading capability as a
# PANEL LENS SET, hermetically, end to end:
#
#   import (additive lens upsert) -> capture (real transcript intake) -> plan (trigger-matched
#   lens selection) -> run --fixtures (deterministic lens + judge calls) -> assert (strict-JSON
#   findings, synthesis, preserved dissent, a discriminating probe).
#
# Verified along the way (see internal/panel/import_lenses.go, internal/capgraph/capgraph.go
# Store.Add): `compound panel import-lenses` upserts capgraph lens nodes by node id — it is
# additive, never replaces the whole lens authority. This script imports ONLY the five
# relational-dynamics lenses into a fresh, throwaway COMPOUND_HOME, so the plan below selects
# exactly those five (no pre-existing lens set to preserve in a from-scratch home; at real
# landing time, importing fixtures/panel/relational-lens-manifest.yaml into the live home is a
# pure addition alongside fixtures/panel/lens-manifest.yaml, never a replacement).
#
# Verified: a `compound capture`-minted candidate is already targetable by `panel plan
# --target-kind memory_candidate` (CaptureExhaust mints into the same candidate store any other
# memory candidate uses, state=proposed) — no new target KIND was needed. What was missing: the
# transcript body itself is a content-addressed blob pointer on the candidate record, not
# inline text, so a lens would see only a cid and have nothing to quote. Fixed with one small,
# additive enrichment in internal/panel/target.go (captureExhaustTranscriptPayload, mirroring
# the existing knowledge-extraction provenance enrichment) that inlines the real transcript
# text into the lens payload for capture-minted candidates. See internal/panel/target_test.go
# for the table-driven proof of that enrichment in isolation.
#
# Governing work envelope: work_42ab5bbb682b
#
# Usage: scripts/proof/relational-dynamics-panel-e2e.sh

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT" || exit 1

section() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

fail() {
  echo "FAIL: $1"
  exit 1
}

MANIFEST="fixtures/panel/relational-lens-manifest.yaml"
MODEL_CAPS="fixtures/panel/model-capabilities.yaml"
TRANSCRIPT="fixtures/panel/relational-transcripts/mutual-indifference.txt"
# sha256("mcand_relational_mutual_indifference")[:12] — the digest the committed static
# fixtures under fixtures/panel/{lens,judge}-responses/.../e9fbae8652bc.json are keyed to.
STATIC_DIGEST="e9fbae8652bc"

section "build"
BIN="$(mktemp -d)/compound"
go build -o "$BIN" ./cmd/compound
EXIT=$?
echo "go build exit=$EXIT"
[ "$EXIT" -eq 0 ] || fail "go build"

export COMPOUND_HOME
COMPOUND_HOME="$(mktemp -d)/home"
echo "COMPOUND_HOME=$COMPOUND_HOME"

section "1. import — five relational lenses into a fresh home (additive upsert; verified above)"
# `panel import-lenses` takes exactly one positional arg and prints plain text (no --json here).
IMPORT_OUT="$("$BIN" panel import-lenses "$MANIFEST")"
EXIT=$?
echo "$IMPORT_OUT"
echo "import-lenses exit=$EXIT"
[ "$EXIT" -eq 0 ] || fail "panel import-lenses"

IMPORTED_COUNT="$(printf '%s\n' "$IMPORT_OUT" | grep -E '^- imported: ' | awk '{print $3}')"
[ "$IMPORTED_COUNT" = "5" ] || fail "expected imported: 5, got '$IMPORTED_COUNT'"

echo "(route plan needs a model-capabilities authority; importing the existing panel fixture — not part of the relational slice itself)"
CAPS_OUT="$("$BIN" route import-model-capabilities "$MODEL_CAPS")"
EXIT=$?
echo "$CAPS_OUT"
echo "route import-model-capabilities exit=$EXIT"
[ "$EXIT" -eq 0 ] || fail "route import-model-capabilities"

section "2. capture — mint one observed candidate from hard-case transcript (a): mutual performed indifference"
CAPTURE_TEXT="$(cat "$TRANSCRIPT")"
CAPTURE_OUT="$("$BIN" capture --source relational-proof --text "$CAPTURE_TEXT")"
EXIT=$?
echo "$CAPTURE_OUT"
echo "capture exit=$EXIT"
[ "$EXIT" -eq 0 ] || fail "compound capture"

CAND_ID="$(printf '%s' "$CAPTURE_OUT" | python3 -c 'import sys,re; m=re.search(r"candidate=(\S+)", sys.stdin.read()); print(m.group(1) if m else "")')"
[ -n "$CAND_ID" ] || fail "could not parse candidate id from capture output"
echo "captured candidate: $CAND_ID"

section "3. plan — target the captured candidate; select lenses by the relational_reading trigger"
PLAN_JSON="$("$BIN" panel plan \
  --target-kind memory_candidate \
  --target "$CAND_ID" \
  --state proposed \
  --domain relational_reading \
  --task "relational reading of a transcript: interaction, conversation, dialogue, relationship" \
  --not-privacy-sensitive "synthetic fixture transcript, not real personal data" \
  --json)"
EXIT=$?
echo "$PLAN_JSON"
echo "panel plan exit=$EXIT"
[ "$EXIT" -eq 0 ] || fail "panel plan"

PLAN_JSON_FILE="$(mktemp)"
printf '%s' "$PLAN_JSON" >"$PLAN_JSON_FILE"
RUN_ID="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$PLAN_JSON_FILE")"
[ -n "$RUN_ID" ] || fail "no run id in panel plan output"
python3 - "$PLAN_JSON_FILE" <<'PY'
import json, sys
run = json.load(open(sys.argv[1]))
included = sorted(r["ref"] for r in run["lens_set"]["included"])
want = [
    "lens:relational-literal", "lens:relational-pragmatic", "lens:relational-protective",
    "lens:relational-received", "lens:relational-systemic",
]
assert included == want, f"included lenses = {included}, want {want}"
print("OK: plan", run["id"], "selected exactly the five relational lenses")
PY
EXIT=$?
[ "$EXIT" -eq 0 ] || fail "plan lens-selection assertion"

section "4. stage a fixtures dir keyed to the live (day-salted) captured candidate id"
# The candidate id CaptureExhaust mints is content+day-salted (internal/mem/capture.go
# CaptureCandidateID), so it cannot be a fixed, pre-committed fixture filename. The digest
# FixtureCaller looks up is sha256(target_ref)[:12] (internal/panel/caller.go targetDigest);
# compute it here and reuse the SAME canonical lens/judge response content already committed
# for the static mutual-indifference fixture (fixtures/panel/targets/
# mcand_relational_mutual_indifference.json) — the captured text is byte-identical, so the
# responses are equally valid keyed to the live digest.
FIXDIR="$(mktemp -d)"
DIGEST="$(python3 -c "import hashlib,sys; print(hashlib.sha256(sys.argv[1].encode()).hexdigest()[:12])" "$CAND_ID")"
mkdir -p "$FIXDIR/judge-responses"
STAGE_FAIL=0
for LENS in relational-literal relational-pragmatic relational-protective relational-received relational-systemic; do
  mkdir -p "$FIXDIR/lens-responses/lens:$LENS"
  cp "fixtures/panel/lens-responses/lens:$LENS/$STATIC_DIGEST.json" "$FIXDIR/lens-responses/lens:$LENS/$DIGEST.json" || STAGE_FAIL=1
done
cp "fixtures/panel/judge-responses/$STATIC_DIGEST.json" "$FIXDIR/judge-responses/$DIGEST.json" || STAGE_FAIL=1
echo "fixtures staged at $FIXDIR keyed to digest $DIGEST (candidate=$CAND_ID)"
[ "$STAGE_FAIL" -eq 0 ] || fail "could not stage fixtures dir"

section "5. run — execute the panel over the captured candidate in --fixtures mode"
RUN_JSON="$("$BIN" panel run --plan "$RUN_ID" --fixtures "$FIXDIR" --json)"
EXIT=$?
echo "$RUN_JSON"
echo "panel run exit=$EXIT"
[ "$EXIT" -eq 0 ] || fail "panel run"

section "6. assert — 5/5 strict-JSON lens findings, synthesis, non-empty dissent, a probe finding"
RUN_JSON_FILE="$(mktemp)"
printf '%s' "$RUN_JSON" >"$RUN_JSON_FILE"
python3 - "$RUN_JSON_FILE" <<'PY'
import json, sys

run = json.load(open(sys.argv[1]))
finding = run["findings"][0]
lens_findings = finding["lens_findings"]

want = {
    "lens:relational-literal", "lens:relational-pragmatic", "lens:relational-protective",
    "lens:relational-received", "lens:relational-systemic",
}
completed = {lf["lens_ref"] for lf in lens_findings if lf.get("status") == "completed"}
assert completed == want, f"completed lenses = {completed}, want {want}"

for lf in lens_findings:
    assert lf["findings"], f"{lf['lens_ref']} produced no findings"
    for f in lf["findings"]:
        assert f.get("claim"), f"{lf['lens_ref']} has a finding with no claim"
        assert f.get("severity") in ("low", "medium", "high"), f"{lf['lens_ref']} bad severity: {f}"
        assert isinstance(f.get("evidence_refs"), list) and f["evidence_refs"], f"{lf['lens_ref']} missing evidence_refs: {f}"

synthesis = finding["synthesis"]
assert synthesis.get("decision"), "synthesis.decision missing"
assert synthesis.get("dissent"), "synthesis.dissent is empty — competing readings were collapsed into one story"

probes = [
    f["claim"]
    for lf in lens_findings
    for f in lf["findings"]
    if f["claim"].startswith("probe:")
]
assert probes, "no probe: finding present anywhere in the lens findings"

print(
    "OK: 5/5 relational lenses completed with strict-JSON findings; "
    "synthesis.decision=%s; dissent=%d entr%s; probe findings=%d"
    % (synthesis["decision"], len(synthesis["dissent"]), "y" if len(synthesis["dissent"]) == 1 else "ies", len(probes))
)
for d in synthesis["dissent"]:
    print("  dissent:", d["lens_ref"], "position=" + d["position"])
for p in probes:
    print("  probe:", p)
PY
EXIT=$?
[ "$EXIT" -eq 0 ] || fail "final assertions"

printf '\n\033[1mALIVE:\033[0m relational-dynamics lens set -> compound capture -> panel plan (trigger-matched) -> panel run --fixtures -> 5/5 strict-JSON lens findings, synthesis with preserved dissent, and a discriminating probe.\n'
