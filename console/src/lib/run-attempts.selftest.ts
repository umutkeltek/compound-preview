/*
 * Depth tests for the run-attempt door's pure mapper + retry-depth (packet
 * 2026-07-20-realtime-multiagent-traffic-board). Would FAIL if the mapper
 * fabricated attempts from no-id/garbage rows, or if retryDepth miscounted the
 * predecessor chain / spun on a cycle. Run via scripts/traffic-board-smoke.mjs.
 */

import { normalizeRunAttempts, retryDepth, type RunAttempt } from "./run-attempts";

type Check = { name: string; ok: boolean; detail: string };

function finalize(checks: Check[]): { ok: boolean; details: string[] } {
  return {
    ok: checks.every((c) => c.ok),
    details: checks.map((c) => `${c.ok ? "ok" : "FAIL"}: ${c.name} — ${c.detail}`),
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

export function runRunAttemptsSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  const norm = normalizeRunAttempts([
    {
      id: "ratt_a",
      work_id: "work_1",
      actor: "codex",
      harness: "os-harness-entrypoints",
      status: "running",
      command: "echo hi",
      transcript_ref: "trace/a.json",
      predecessor_id: "",
    },
    { work_id: "work_2" }, // no id -> dropped, never fabricated
    "garbage", // not an object -> dropped
    null,
  ]);
  push(
    "maps-fields",
    norm.length === 1 &&
      norm[0].workId === "work_1" &&
      norm[0].harness === "os-harness-entrypoints" &&
      norm[0].actor === "codex" &&
      norm[0].transcriptRef === "trace/a.json",
    `len=${norm.length}`,
  );
  push("drops-no-id-and-garbage", norm.every((a) => a.id) && norm.length === 1, `len=${norm.length}`);
  push(
    "non-array-empty",
    normalizeRunAttempts({ nope: 1 }).length === 0 &&
      normalizeRunAttempts(null).length === 0 &&
      normalizeRunAttempts(undefined).length === 0,
    "ok",
  );

  // retryDepth follows the predecessor chain: a 3-attempt chain has depth 2 at the tip.
  const a1 = mkRA({ id: "ratt_1", predecessorId: "" });
  const a2 = mkRA({ id: "ratt_2", predecessorId: "ratt_1" });
  const a3 = mkRA({ id: "ratt_3", predecessorId: "ratt_2" });
  const byId = new Map<string, RunAttempt>([
    [a1.id, a1],
    [a2.id, a2],
    [a3.id, a3],
  ]);
  push(
    "retry-depth-chain",
    retryDepth(a3, byId) === 2 && retryDepth(a2, byId) === 1 && retryDepth(a1, byId) === 0,
    `a3=${retryDepth(a3, byId)} a2=${retryDepth(a2, byId)} a1=${retryDepth(a1, byId)}`,
  );
  // Cycle guard: a self-referencing predecessor never spins (bounded depth).
  const cyc = mkRA({ id: "ratt_c", predecessorId: "ratt_c" });
  push("retry-depth-cycle-guard", retryDepth(cyc, new Map([[cyc.id, cyc]])) <= 1, `depth=${retryDepth(cyc, new Map([[cyc.id, cyc]]))}`);

  return finalize(checks);
}
