/*
 * Depth falsifiers for the delegated-session console slice: the pure normalize
 * (whitelist) + render model that drive the Agents supervised-session board.
 * Green-with / red-without a single rule — privacy whitelist, empty state,
 * status→tone rendering, and the running/exited/refused rows the board draws.
 * Run via `bun scripts/delegated-sessions-smoke.mjs`; exits non-zero on FAIL so
 * the battery can gate on it. NO real projection, NO eventstore — pure TS.
 */
import { delegatedSessionView, delegatedSessionViews } from "./executor-model";
import { normalizeExecutorProjection } from "./executor-normalize";

type Check = { name: string; ok: boolean; detail: string };

function finalize(checks: Check[]): { ok: boolean; details: string[] } {
  return {
    ok: checks.every((c) => c.ok),
    details: checks.map((c) => `${c.ok ? "ok" : "FAIL"}: ${c.name} — ${c.detail}`),
  };
}

// A raw `compound executors --json`-shaped payload. The delegated rows carry
// ONLY the safe fields the server emits; the two adversarial keys (prompt /
// argv / message) simulate a server that accidentally leaked and MUST be
// dropped by the whitelist normalize.
function rawProjection() {
  return {
    generated_at: "2026-07-22T12:00:00Z",
    authority_ref: "compound://executors/projection",
    source_refs: ["compound.workbench", "compound.events.work_delegate"],
    next_command: ["compound", "executors", "--json"],
    counts: { total: 0 },
    executors: [],
    delegated_sessions: [
      {
        work_id: "work_run",
        lane: "claude",
        session: "compound-work_run",
        status: "running",
        holder: "codex",
        run_attempt_id: "ratt_20260722T120000Z_aaaa",
        run_attempt_status: "running",
        progress: { done: 3, empty: 1, failed: 0, remaining: 6 },
        last_steer_at: "2026-07-22T12:05:00Z",
        // adversarial leak fields — must never survive the whitelist:
        prompt: "TOP-SECRET-PROMPT",
        argv: ["claude", "--system", "TOP-SECRET-ARGV"],
        message: "steered len=42 enter=true",
      },
      {
        work_id: "work_exit",
        lane: "codex",
        session: "compound-work_exit",
        status: "exited",
        run_attempt_id: "ratt_20260722T110000Z_bbbb",
        run_attempt_status: "succeeded",
        observed_exit: "succeeded",
        exited_at: "2026-07-22T11:30:00Z",
      },
      {
        work_id: "work_refused",
        lane: "claude",
        status: "refused",
        refused_reason: "walled",
        message: "walled: lane at 0% for 4d",
      },
      { work_id: "work_weird", lane: "claude", status: "banana" },
    ],
    delegated_counts: { total: 4, running: 1, exited: 1, refused: 1, unknown: 1 },
  };
}

export function runDelegatedSessionsSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  const proj = normalizeExecutorProjection(rawProjection());
  if (!proj) {
    return { ok: false, details: ["FAIL: normalize returned null for a valid projection"] };
  }

  // --- lifecycle reduction: rows survive with the reduced status set ---
  const byId = new Map(proj.delegatedSessions.map((r) => [r.workId, r]));
  push(
    "running-row",
    byId.get("work_run")?.status === "running" &&
      byId.get("work_run")?.runAttemptId === "ratt_20260722T120000Z_aaaa" &&
      byId.get("work_run")?.runAttemptStatus === "running",
    `run=${JSON.stringify(byId.get("work_run"))}`,
  );
  push(
    "running-progress-whitelist",
    JSON.stringify(byId.get("work_run")).includes('"progress":{"done":3,"empty":1,"failed":0,"remaining":6}'),
    `run=${JSON.stringify(byId.get("work_run"))}`,
  );
  push(
    "exited-row",
    byId.get("work_exit")?.status === "exited" && byId.get("work_exit")?.observedExit === "succeeded",
    `exit=${JSON.stringify(byId.get("work_exit"))}`,
  );
  push(
    "refused-row-reason-only",
    byId.get("work_refused")?.status === "refused" &&
      byId.get("work_refused")?.refusedReason === "walled" &&
      byId.get("work_refused")?.session === "" &&
      byId.get("work_refused")?.runAttemptId === "",
    `refused=${JSON.stringify(byId.get("work_refused"))}`,
  );
  push(
    "unknown-status-coerced",
    byId.get("work_weird")?.status === "unknown",
    `weird=${byId.get("work_weird")?.status}`,
  );

  // --- privacy: no leaked prompt/argv/message reaches ANY parsed row ---
  const blob = JSON.stringify(proj.delegatedSessions);
  push(
    "privacy-no-prompt-argv",
    !blob.includes("TOP-SECRET") && !blob.includes("steered len="),
    `serialized rows must not carry leaked content: ${blob}`,
  );
  // the whitelist drops unknown keys entirely (structural, not just string):
  const runKeys = Object.keys(byId.get("work_run") ?? {}).sort().join(",");
  push(
    "privacy-whitelist-keys",
    !runKeys.includes("prompt") && !runKeys.includes("argv") && !runKeys.includes("message"),
    `row keys = ${runKeys}`,
  );

  // --- rendered running/exited/refused rows: status→tone/label + detail ---
  const views = delegatedSessionViews(proj.delegatedSessions);
  const vById = new Map(views.map((v) => [v.workId, v]));
  push(
    "render-running-live",
    vById.get("work_run")?.tone === "live" && vById.get("work_run")?.statusLabel === "Running",
    `view=${JSON.stringify(vById.get("work_run"))}`,
  );
  push(
    "render-running-progress",
    JSON.stringify(vById.get("work_run")).includes("3 done · 1 empty · 0 failed · 6 left"),
    `view=${JSON.stringify(vById.get("work_run"))}`,
  );
  push(
    "render-exited-done",
    vById.get("work_exit")?.tone === "done" && vById.get("work_exit")?.detail === "succeeded",
    `view=${JSON.stringify(vById.get("work_exit"))}`,
  );
  push(
    "render-refused-seam-detail",
    vById.get("work_refused")?.tone === "seam" && vById.get("work_refused")?.detail === "walled",
    `view=${JSON.stringify(vById.get("work_refused"))}`,
  );
  const runView = delegatedSessionView(byId.get("work_run")!);
  push(
    "render-run-attempt-label",
    runView.runAttemptLabel === "ratt_20260722T120000Z_aaaa · running",
    `label=${runView.runAttemptLabel}`,
  );

  // --- empty state: a null projection and an empty board both stay clean ---
  const empty = normalizeExecutorProjection({
    generated_at: "2026-07-22T12:00:00Z",
    counts: {},
    executors: [],
    delegated_sessions: [],
  });
  push(
    "empty-board",
    !!empty && empty.delegatedSessions.length === 0 && empty.delegatedCounts.total === 0 && delegatedSessionViews(empty.delegatedSessions).length === 0,
    `empty=${JSON.stringify(empty?.delegatedCounts)}`,
  );
  push("null-projection", normalizeExecutorProjection(null) === null, "null in → null out");

  return finalize(checks);
}
