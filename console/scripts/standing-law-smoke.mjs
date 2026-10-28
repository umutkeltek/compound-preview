/*
 * Standing-law view smoke (ruling-harvest slice B). Two layers:
 *
 *   1) PURE: runs the mapper depth test (standing-law.selftest.ts) over a
 *      synthetic `decisions standing-law --json` payload — always, no binary.
 *      Proves: superseded record never live, conflict strip present + linked,
 *      search narrows to the right record.
 *
 *   2) E2E (when a `compound` binary is resolvable): seeds a TEMP home with two
 *      accepted rulings (one superseding the other) + one plain grant + one
 *      parked conflict candidate, runs the REAL `decisions standing-law --json`,
 *      maps it, and asserts the same four falsifier properties on live data.
 *      Red-first: an old binary without the subcommand emits no standing-law
 *      schema, so the e2e reds here.
 *
 * Run:  bun scripts/standing-law-smoke.mjs
 *       COMPOUND_BIN=../compound-sl bun scripts/standing-law-smoke.mjs
 */

import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

import { buildStandingLaw } from "../src/lib/standing-law.ts";
import { filterStandingLaw } from "../src/lib/standing-law.ts";
import { runStandingLawSelfTest } from "../src/lib/standing-law.selftest.ts";

/* ---------- layer 1: pure mapper depth test ---------- */
const pure = runStandingLawSelfTest();
for (const line of pure.details) console.log(line);
if (!pure.ok) {
  console.error("standing-law smoke FAILED (pure mapper depth test)");
  process.exit(1);
}
console.log("standing-law pure ok: mapper hides superseded law, keeps the conflict strip, narrows on search");

/* ---------- layer 2: end-to-end over a real seeded home ----------
 * Gated on COMPOUND_BIN being set EXPLICITLY (not just `compound` on PATH): the
 * e2e exercises the NEW `decisions standing-law` subcommand, so a STALE installed
 * binary on PATH would red here for the wrong reason (installed-runtime drift,
 * not a real defect). A host-bound proof-check therefore runs the pure layer
 * only; the lane (and anyone) runs the e2e with a freshly-built binary:
 *   COMPOUND_BIN=../compound-sl bun scripts/standing-law-smoke.mjs
 * CI has no stale-binary problem — it builds the binary in the same job — so
 * .github/workflows/proof.yml sets COMPOUND_BIN plus CONSOLE_SMOKE_REQUIRE_CLI=1,
 * which turns a skipped e2e there into a red gate instead of a silent pass.
 * The real-seed e2e of the DATA layer is additionally covered, always, by the Go
 * test TestStandingLawSupersededNeverLive under `go test ./...`. */
const BIN = process.env.COMPOUND_BIN;
// CI wires COMPOUND_BIN to a binary it just built, so a skip THERE means the
// wiring regressed and the e2e layer quietly stopped proving anything. The
// switch is set by the workflow only; a local run keeps the skip above.
const REQUIRE_CLI = process.env.CONSOLE_SMOKE_REQUIRE_CLI === "1";

function binaryAvailable() {
  if (!BIN) return false;
  try {
    execFileSync(BIN, ["version"], { stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}

if (!binaryAvailable()) {
  if (REQUIRE_CLI) {
    console.error(
      `standing-law e2e FAIL: CONSOLE_SMOKE_REQUIRE_CLI=1 but COMPOUND_BIN does not resolve to a runnable binary (COMPOUND_BIN=${BIN ?? "<unset>"})`,
    );
    process.exit(1);
  }
  console.log(
    "standing-law e2e skipped: set COMPOUND_BIN to a freshly-built binary to run the seed→CLI→map e2e (pure layer already green)",
  );
  process.exit(0);
}

const HOME = mkdtempSync(path.join(tmpdir(), "standing-law-smoke-"));

function run(args) {
  return execFileSync(BIN, args, {
    env: { ...process.env, COMPOUND_HOME: HOME },
    encoding: "utf8",
    maxBuffer: 64 * 1024 * 1024,
  });
}
function runJson(args) {
  const out = run(args);
  const start = out.indexOf("{");
  assert.ok(start >= 0, `no JSON object in \`${BIN} ${args.join(" ")}\``);
  return JSON.parse(out.slice(start));
}

run(["home", "init"]);
run([
  "decisions", "record", "--kind", "ruling", "--id", "old-exec-lane",
  "--title", "Execution routes through Sonnet subagents",
  "--reviewed-by", "operator:test", "--json",
]);
run([
  "decisions", "record", "--kind", "ruling", "--id", "new-exec-lane",
  "--title", "Execution routes through Codex only, never Opus subagents",
  "--reviewed-by", "operator:test", "--undo-of", "old-exec-lane", "--json",
]);
run([
  "decisions", "record", "--kind", "grant", "--id", "director-apply-grant",
  "--title", "Let the director apply job-health work on its own",
  "--reviewed-by", "operator:test", "--json",
]);

// Resolve the live successor's claim id, then file a parked conflict candidate
// that reverses it — exactly the shape internal/harvest writes for a conflict.
const seeded = buildStandingLaw(runJson(["decisions", "standing-law", "--json"]));
const successor = seeded.records.find((r) => r.decisionId === "new-exec-lane");
assert.ok(successor, "seeded successor 'new-exec-lane' missing before conflict seed");

const inbox = path.join(HOME, "inbox", "decision-candidates");
mkdirSync(inbox, { recursive: true });
writeFileSync(
  path.join(inbox, "harvest-ruling-smoke01.json"),
  JSON.stringify(
    {
      id: "harvest-ruling-smoke01",
      kind: "decision",
      entity: "decision:session-ruling-smoke01",
      predicate: "decided",
      object: "Execution should route through Opus subagents",
      subject: "Ruling: Execution should route through Opus subagents",
      statement: "Ruling: Execution should route through Opus subagents",
      verdict: "park",
      qualifiers: {
        kind: "ruling",
        source: "sessions_harvest",
        lifecycle_status: "proposed",
        harvest_outcome: "conflict",
        session: "sess-conflict",
        transcript: "/tmp/sess-conflict.jsonl",
        line: "42",
        verbatim: "actually execution should route through Opus subagents for everything i talk",
        conflicts_with_claim: successor.claimId,
        conflicts_with_decision: "new-exec-lane",
        needs_operator: "true",
      },
    },
    null,
    2,
  ) + "\n",
);

const model = buildStandingLaw(runJson(["decisions", "standing-law", "--json"]));
const live = new Map(model.records.map((r) => [r.decisionId, r]));

// Both live records render; the superseded old ruling does NOT.
assert.ok(live.has("new-exec-lane"), "live successor missing from e2e records");
assert.ok(live.has("director-apply-grant"), "live grant missing from e2e records");
assert.ok(!live.has("old-exec-lane"), "superseded 'old-exec-lane' leaked into live records");

// Supersession chain hangs the old ruling under its successor, titled.
const chain = live.get("new-exec-lane").supersedes;
assert.ok(
  chain.length === 1 && chain[0].decisionId === "old-exec-lane" && chain[0].title.length > 0,
  `supersession chain wrong: ${JSON.stringify(chain)}`,
);

// The contradiction strip is present and links the conflicting live claim.
assert.equal(model.conflicts.length, 1, "expected exactly one conflict candidate in the strip");
assert.equal(
  model.conflicts[0].conflictsWithClaim,
  successor.claimId,
  "conflict must link the live claim it reverses",
);
assert.ok(model.conflicts[0].conflictingTitle.length > 0, "conflict must resolve the conflicting record title");

// Search narrows to the right record; a superseded-only term matches no live row.
const codex = filterStandingLaw(model.records, "codex");
assert.ok(
  codex.length === 1 && codex[0].decisionId === "new-exec-lane",
  `search 'codex' must narrow to the exec-lane ruling, got ${codex.map((r) => r.decisionId).join(",")}`,
);
const sonnet = filterStandingLaw(model.records, "sonnet");
assert.equal(sonnet.length, 0, "'sonnet' lives only in the superseded (dimmed) record — must not match a live row");

console.log("standing-law e2e ok:");
console.log(`  home                 ${HOME}`);
console.log(`  live records         ${model.records.map((r) => r.decisionId).join(", ")}`);
console.log(`  superseded (nested)  ${chain.map((c) => c.decisionId).join(", ")}`);
console.log(`  conflict strip       ${model.conflicts.length} (links ${model.conflicts[0].conflictsWithClaim})`);
console.log(`  search 'codex'       -> ${codex.map((r) => r.decisionId).join(", ")}`);
console.log("standing-law smoke ok");
