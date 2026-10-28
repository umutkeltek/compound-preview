#!/usr/bin/env bash
set -euo pipefail

# expertise-pack-e2e.sh — prove an Expertise Pack flows through compound end to end,
# as EVIDENCE that earns promotion, never as authority written straight in.
#
# Chain: import (evidence + parked no-promote) → extract (grounded) → show the
# authority gate refuse a basis-less claim → curate → promote → recall + orient.
#
# Doctrine + design: docs/architecture/knowledge/expertise-packs.md
# CI falsifiers:      internal/knowledge/expertise_pack_test.go
#
# Usage: scripts/proof/expertise-pack-e2e.sh

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

PACK="fixtures/knowledge/expertise-ios-performance-export.json"
# The pack item we drive through the whole chain, and a quote copied verbatim from
# its content (the grounding gate requires the quote to appear in the pack text).
ITEM_REF="expertise:ios-perf:mental-model:frame-loop"
GROUNDED_QUOTE="The modern unit of failure is the hitch: a frame that misses its target and is shown late"

section() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

BIN="$(mktemp -d)/compound"
section "build"
go build -o "$BIN" ./cmd/compound
export COMPOUND_HOME="$(mktemp -d)/home"
echo "COMPOUND_HOME=$COMPOUND_HOME"

section "1. import — evidence + PARKED no-promote candidates (nothing to authority)"
IMPORT_JSON="$("$BIN" knowledge import "$PACK" --json)"
echo "$IMPORT_JSON" | python3 -c 'import sys,json; d=json.load(sys.stdin); print("items=%d imported=%d no_promote_decisions=%d" % (d["items"], d["imported"], len(d.get("no_promote_ids") or [])))'
echo "source trust posterior (low — model synthesis is not trusted):"
cat "$COMPOUND_HOME/registries/knowledge-sources.jsonl"
echo "promoted claims ledger present? $([ -s "$COMPOUND_HOME/ledger/claims.jsonl" ] && echo yes || echo 'no — correct')"

# Find the extraction request for our item by reading its item_ref (order-independent).
KX="$(python3 - "$COMPOUND_HOME" "$ITEM_REF" <<'PY'
import sys, os, json, glob
home, item_ref = sys.argv[1], sys.argv[2]
for p in glob.glob(os.path.join(home, "inbox", "knowledge-extractions", "*.json")):
    r = json.load(open(p))
    if r.get("item_ref") == item_ref:
        print(r["id"]); break
PY
)"
echo "extraction request for $ITEM_REF -> $KX"

section "2. extract — GROUNDED memory candidate (quote must appear in pack text)"
FIX="$(mktemp -d)"
python3 - "$FIX/$KX.json" "$GROUNDED_QUOTE" <<'PY'
import sys, json
path, quote = sys.argv[1], sys.argv[2]
json.dump({"essence_lines": [{
    "predicate_hint": "heuristic",
    "statement": "Treat iOS performance failures as hitches measured in milliseconds per frame, not binary dropped frames.",
    "quote": quote,
}]}, open(path, "w"))
PY
"$BIN" knowledge extract --item "$KX" --fixtures "$FIX" --json
CAND="$("$BIN" memory candidates --json | python3 -c 'import sys,json; d=json.load(sys.stdin); print([c["id"] for c in (d.get("items") or []) if c.get("qualifiers",{}).get("source")=="knowledge_extraction"][0])')"
echo "grounded extraction candidate: $CAND (state=parked, promotion_basis empty)"

section "3. authority gate — a basis-less model claim CANNOT self-promote"
if "$BIN" memory adjudicate "$CAND" --verdict promote 2>err.txt; then
  echo "UNEXPECTED: basis-less claim promoted — authority gate bypassed"; cat err.txt; rm -f err.txt; exit 1
else
  echo "refused (correct): $(cat err.txt)"; rm -f err.txt
fi

section "4. governed promotion — review → curate → promote (basis=curated_page)"
PAGE="$(mktemp -d)/ios-perf-hitch-model.md"
cat > "$PAGE" <<'MD'
---
schema_version: compound.knowledge_page.v0.1
id: knowledge_ios_perf_hitch_model
title: iOS performance — the hitch mental model
entity: ios-performance
owner: expertise-review
visibility: contextual
claims:
  - entity: ios-performance
    predicate: hitch_mental_model
    object: "On iOS treat performance failures as hitches measured in milliseconds per frame at the device refresh rate, not binary dropped frames."
    confidence: 0.8
    derivation: inferred
    claim_type: architecture
    promotion_basis: curated_page
    topic_refs: ["ios", "performance"]
    evidence_quote: "The modern unit of failure is the hitch"
    qualifiers:
      source_pack: "expertise:ios-performance-engineering:v1"
      reviewed_by: operator
---
# iOS performance — the hitch mental model
Curated from the iOS-performance Expertise Pack after review.
MD
"$BIN" memory import-page "$PAGE" | head -3
PCAND="$("$BIN" memory candidates --json | python3 -c 'import sys,json; d=json.load(sys.stdin); print([c["id"] for c in (d.get("items") or []) if c.get("qualifiers",{}).get("source")=="knowledge_page" and c.get("predicate")=="hitch_mental_model"][0])')"
"$BIN" memory adjudicate "$PCAND" --verdict promote

section "5. consume — recall (human) AND orient --task (agent-facing ambient recall)"
echo "--- recall ios-performance ---"
"$BIN" memory recall "ios-performance"
echo "--- orient --task surfaces the promoted heuristic ---"
"$BIN" orient --harness claude --task "reduce iOS scrolling frame hitches and improve rendering performance" --json \
  | python3 -c 'import sys,json; blob=json.dumps(json.load(sys.stdin)); print("promoted heuristic in orient pack:", "hitches measured in milliseconds" in blob)'

printf '\n\033[1mALIVE:\033[0m pack → evidence → grounded extraction → governed promotion → real executor.\n'
