import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

import { buildPlaytestBatch, emitPlaytestObservations } from "./playtest_findings_observe.ts";
import { createTestTempScope, initializeCompoundHome } from "./test_support.ts";

const temporary = createTestTempScope();
afterAll(() => temporary.cleanup());

// Built once per test process rather than via `go run` per invocation: `go run` puts a compile and
// link inside the per-test timeout, so the budget ends up measuring machine load instead of the
// observe-batch behaviour under test. See the same helper in chatgpt_conversation_edge.test.ts.
function buildCompound(): string {
  const bin = join(temporary.dir("compound-playtest-observe-bin-"), "compound");
  const build = spawnSync("go", ["build", "-o", bin, "./cmd/compound"], { encoding: "utf8", timeout: 120_000 });
  if (build.status !== 0) throw new Error(build.stderr || build.stdout);
  return bin;
}

// Build in setup, not module loading: Bun runs teardown when setup fails.
let COMPOUND_BIN: string;
beforeAll(() => {
  COMPOUND_BIN = buildCompound();
}, 120_000);

const SAMPLE_RUN = "fixtures/playtest/run-sample";
const UNKNOWN_TYPE_RUN = "fixtures/playtest/run-unknown-type";
const CAPTURED_AT = "2026-07-13T00:00:00Z";

function tempHome(): string {
  return temporary.dir("compound-playtest-observe-");
}

// Falsifier 1: a run dir with a paired finding (pf1 + rec-pf1) AND an orphan finding
// (pf2, no recorded wrapper) emits one valid batch of 2 source observations with exactly
// one provenance warning, and `observe-batch --dry-run` routes both to source observations
// with 0 errors.
describe("playtest_findings_observe — falsifier 1: run dir to routed source observations", () => {
  test("emits a 2-observation batch, 1 provenance warning, dry-run routes both to source observations", () => {
    const homePath = tempHome();
    initializeCompoundHome(COMPOUND_BIN, homePath);
    const report = emitPlaytestObservations(SAMPLE_RUN, {
      product: "sample-app",
      capturedAt: CAPTURED_AT,
      homePath,
    });

    expect(report.newCount).toBe(2);
    expect(report.batchPath).toBeTruthy();
    expect(existsSync(report.batchPath!)).toBe(true);
    // Exactly one provenance warning: pf2 has no recorded wrapper.
    const provenanceWarnings = report.warnings.filter((w) => w.includes("provenance"));
    expect(provenanceWarnings).toHaveLength(1);
    expect(provenanceWarnings[0]).toContain("pf2");

    const batch = JSON.parse(readFileSync(report.batchPath!, "utf8"));
    expect(batch.schema_version).toBe("compound.observation_batch.v0.1");
    expect(batch.source).toMatchObject({
      kind: "playtest_findings",
      ref: "run-sample",
      collector: "tools/playtest_findings_observe.ts",
      privacy: "private",
    });
    expect(batch.subject).toMatchObject({ kind: "product", id: "sample-app", surface: "playtest" });
    expect(batch.observations).toHaveLength(2);
    // Every finding observation must route to source observations: kind is system.
    expect(batch.observations.every((o: { kind: string }) => o.kind === "system")).toBe(true);

    // pf1 carries severity/type/provenance + recorded work_id + artifact hash provenance.
    const pf1 = batch.observations.find((o: { dimensions?: { finding_id?: string } }) => o.dimensions?.finding_id === "pf1");
    expect(pf1.dimensions.severity).toBe("P0");
    expect(pf1.dimensions.finding_type).toBe("false_completion_feedback");
    expect(pf1.dimensions.provenance).toBe("recorded");
    expect(pf1.dimensions.work_id).toBe("work_sample00000001");
    expect(pf1.evidence_refs).toContain("shots/s1-save-banner.png");
    expect(pf1.evidence_refs.some((r: string) => r.startsWith("artifact_sha256:"))).toBe(true);
    expect(pf1.title).toContain("Phantom save");
    expect(pf1.content).toContain("Ledger truth");

    // pf2 is emitted with partial provenance (orphan raw finding).
    const pf2 = batch.observations.find((o: { dimensions?: { finding_id?: string } }) => o.dimensions?.finding_id === "pf2");
    expect(pf2.dimensions.provenance).toBe("partial");
    expect(pf2.dimensions.work_id).toBeUndefined();

    // Real intake door: dry-run must route both observations to source observations, 0 errors.
    const dry = spawnSync(
      COMPOUND_BIN,
      ["source", "observe-batch", report.batchPath!, "--dry-run", "--json"],
      { cwd: process.cwd(), env: { ...process.env, COMPOUND_HOME: homePath }, encoding: "utf8" },
    );
    expect(dry.stderr).toBe("");
    expect(dry.status).toBe(0);
    const plan = JSON.parse(dry.stdout);
    expect(plan.status).toBe("ok");
    expect(plan.route_counts.source_observation).toBe(2);
    expect(plan.route_counts.unsupported_kind ?? 0).toBe(0);
    expect(plan.route_counts.blocked_private_input ?? 0).toBe(0);
    expect(plan.decisions.every((d: { route: string }) => d.route === "source_observation")).toBe(true);
  });
});

// Falsifier 2: re-running the same run dir against the same home emits 0 new observations
// (the seen-ledger already recorded every finding id).
describe("playtest_findings_observe — falsifier 2: seen-ledger idempotency", () => {
  test("re-run of the same run dir emits 0 new observations", () => {
    const homePath = tempHome();
    const first = emitPlaytestObservations(SAMPLE_RUN, { product: "sample-app", capturedAt: CAPTURED_AT, homePath });
    expect(first.newCount).toBe(2);

    const second = emitPlaytestObservations(SAMPLE_RUN, { product: "sample-app", capturedAt: CAPTURED_AT, homePath });
    expect(second.newCount).toBe(0);
    expect(second.batchPath).toBeNull();
    expect(second.seenCount).toBe(2);
  });
});

// Falsifier 3: a finding whose `type` is outside the descriptive enum is still emitted with
// the type carried verbatim; the tool logs a warning but does not gate the finding.
describe("playtest_findings_observe — falsifier 3: unknown type is descriptive, not a gate", () => {
  test("unknown finding type is emitted verbatim with a warning", () => {
    const { batch, warnings } = buildPlaytestBatch(UNKNOWN_TYPE_RUN, {
      product: "sample-app",
      capturedAt: CAPTURED_AT,
    });

    expect(batch.observations).toHaveLength(1);
    expect(batch.observations[0].kind).toBe("system");
    expect(batch.observations[0].dimensions.finding_type).toBe("totally_made_up_signal");
    const typeWarnings = warnings.filter((w) => w.includes("unknown finding type"));
    expect(typeWarnings).toHaveLength(1);
    expect(typeWarnings[0]).toContain("totally_made_up_signal");
  });
});
