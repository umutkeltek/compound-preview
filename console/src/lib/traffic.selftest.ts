/*
 * Depth tests for the traffic projection's pure builder (the real-time board's
 * seed/reconcile data plane; packet 2026-07-20-realtime-multiagent-traffic-board).
 * Would FAIL if the builder picked the claim-mint over the executed run-attempt,
 * lost the retry depth, fabricated a work from a stray run-attempt, or lied about
 * the run-attempt door being reachable. Run via scripts/traffic-board-smoke.mjs.
 */

import { buildTraffic } from "./traffic";
import type { ExecutorProjection, ExecutorRow, ExecutorWorkRef } from "@/components/agents/executor-model";
import type { RunAttempt } from "./run-attempts";
import { harnessDisplayName } from "./harness-label";

type Check = { name: string; ok: boolean; detail: string };

function finalize(checks: Check[]): { ok: boolean; details: string[] } {
  return {
    ok: checks.every((c) => c.ok),
    details: checks.map((c) => `${c.ok ? "ok" : "FAIL"}: ${c.name} — ${c.detail}`),
  };
}

function mkWork(over: Partial<ExecutorWorkRef> = {}): ExecutorWorkRef {
  return { workId: "work_x", title: "do the thing", state: "running", nextAction: "run it", ...over };
}

function mkRow(over: Partial<ExecutorRow> = {}): ExecutorRow {
  return {
    id: "codex",
    label: "Codex",
    status: "available",
    statusSource: "profile_guide",
    waitReason: "",
    bestFor: [],
    canUseTools: [],
    costLimits: [],
    running: 1,
    ready: 0,
    assigned: 1,
    needsMe: 0,
    leaseCount: 1,
    oldestLeaseAgeSeconds: null,
    leases: [],
    routeEvidenceCount: 0,
    routeEvidencePresent: false,
    lastHook: null,
    currentWork: [],
    nextAssigned: null,
    projectionNote: "",
    ...over,
  };
}

function mkProj(rows: ExecutorRow[]): ExecutorProjection {
  return {
    generatedAt: "2026-07-20T22:00:00Z",
    authorityRef: "compound://executors/projection",
    sourceRefs: [],
    nextCommand: [],
    staleAfterSeconds: 172800,
    counts: { total: rows.length, open: 0, running: 0, ready: 0, needsMe: 0, assigned: 0, leases: 0, staleHistory: 0 },
    executors: rows,
    delegatedSessions: [],
    delegatedCounts: { total: 0, running: 0, exited: 0, refused: 0, unknown: 0 },
  };
}

function mkRA(over: Partial<RunAttempt> = {}): RunAttempt {
  return {
    id: "ratt_x",
    workId: "work_x",
    actionIntentId: "",
    actor: "",
    harness: "",
    status: "",
    command: "",
    transcriptRef: "",
    predecessorId: "",
    createdAt: "",
    completedAt: "",
    lastHeartbeatAt: "",
    ...over,
  };
}

export function runTrafficSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  const proj = mkProj([
    mkRow({
      currentWork: [mkWork({ workId: "work_1", state: "running" }), mkWork({ workId: "work_2", state: "claimed" })],
    }),
  ]);
  // work_1 has a bare claim-mint (no harness) AND the executed attempt (harness,
  // running, transcript, predecessor=claim-mint => retry depth 1).
  const claim = mkRA({ id: "ratt_c1", workId: "work_1", status: "pending", createdAt: "2026-07-20T21:00:00Z" });
  const exec1 = mkRA({
    id: "ratt_e1",
    workId: "work_1",
    actor: "codex",
    harness: "os-harness-entrypoints",
    status: "running",
    transcriptRef: "trace/e1.json",
    predecessorId: "ratt_c1",
    createdAt: "2026-07-20T21:30:00Z",
    lastHeartbeatAt: "2026-07-20T21:31:00Z",
  });

  const report = buildTraffic(proj, [claim, exec1]);
  const lane = report.lanes[0];
  const w1 = lane.inFlight.find((w) => w.workId === "work_1");
  const w2 = lane.inFlight.find((w) => w.workId === "work_2");

  push("lane-shape", report.lanes.length === 1 && lane.inFlight.length === 2, `lanes=${report.lanes.length} inFlight=${lane.inFlight.length}`);
  push(
    "representative-picks-executed",
    !!w1 && w1.harness === "os-harness-entrypoints" && w1.runStatus === "running" && w1.actor === "codex" && w1.transcriptRef === "trace/e1.json",
    `harness=${w1?.harness} status=${w1?.runStatus}`,
  );
  push(
    "run-time-evidence-joined",
    w1?.startedAt === "2026-07-20T21:30:00Z" && w1?.lastHeartbeatAt === "2026-07-20T21:31:00Z",
    `started=${w1?.startedAt} heartbeat=${w1?.lastHeartbeatAt}`,
  );
  push("retry-depth-joined", !!w1 && w1.retryDepth === 1, `depth=${w1?.retryDepth}`);
  push(
    "fixture-harness-is-not-presented-as-a-real-product-harness",
    harnessDisplayName("fake") === "Fixture adapter",
    `label=${harnessDisplayName("fake")}`,
  );
  push("no-attempt-empty-exec", !!w2 && w2.harness === "" && w2.runStatus === "" && w2.retryDepth === 0, `harness='${w2?.harness}'`);
  push("counts", report.inFlightCount === 2 && report.runAttemptsPresent === true, `inFlight=${report.inFlightCount} present=${report.runAttemptsPresent}`);

  // F3: run-attempt door unreachable (null) => flag false + exec blanked (board
  //    will render "execution detail unavailable", never an invented harness).
  const noAttempts = buildTraffic(proj, null);
  const nw1 = noAttempts.lanes[0].inFlight.find((w) => w.workId === "work_1");
  push(
    "F3-door-unreachable-flag",
    noAttempts.runAttemptsPresent === false && !!nw1 && nw1.harness === "" && nw1.runStatus === "",
    `present=${noAttempts.runAttemptsPresent}`,
  );

  // F4: a run-attempt for a work in NO lane fabricates nothing.
  const stray = mkRA({ id: "ratt_s", workId: "work_NOTINLANE", harness: "ghost", status: "running" });
  const withStray = buildTraffic(proj, [claim, exec1, stray]);
  const allWorkIds = withStray.lanes.flatMap((l) => l.inFlight.map((w) => w.workId));
  push("F4-no-fabrication", !allWorkIds.includes("work_NOTINLANE") && allWorkIds.length === 2, `ids=${allWorkIds.join(",")}`);

  // null projection => empty report, no throw.
  const empty = buildTraffic(null, [claim, exec1]);
  push("null-projection-empty", empty.lanes.length === 0 && empty.inFlightCount === 0, `lanes=${empty.lanes.length}`);

  return finalize(checks);
}
