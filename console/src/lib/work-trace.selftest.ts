/*
 * Depth test for the envelope-spine mapper (the Trace drawer's data layer).
 * These assertions would FAIL if the mapping were shallow — i.e. if it
 * fabricated stages the projection has no record for, lost the honesty note,
 * or lit more than one beating node. Pure (synthetic payloads); run via
 * scripts/work-trace-smoke.mjs.
 */

import { mapEnvelopeSpine, parseWorkShow, type TraceStep } from "./work-trace";

type Check = { name: string; ok: boolean; detail: string };

function stepByKey(steps: TraceStep[], key: string): TraceStep {
  const step = steps.find((s) => s.key === key);
  if (!step) throw new Error(`missing spine step: ${key}`);
  return step;
}

export function runWorkTraceSelfTest(): { ok: boolean; details: string[] } {
  const checks: Check[] = [];
  const push = (name: string, ok: boolean, detail: string) => checks.push({ name, ok, detail });

  // 1. A genuinely running work: exactly ONE node beats, and it is the envelope.
  const running = mapEnvelopeSpine({
    id: "work_run00",
    state: "running",
    success_predicate: "do the thing",
    run_ids: ["act_1"],
    policy_decision_id: "pol_1",
    ids: {},
  });
  const liveNodes = running.steps.filter((s) => s.status === "live");
  push(
    "exactly-one-beating-node",
    liveNodes.length === 1 && liveNodes[0].key === "envelope",
    `live nodes: ${liveNodes.map((s) => s.key).join(",") || "none"}`,
  );

  // 2. Honesty law: empty scope + empty evidence => ContextPack pending + noRecord,
  //    never a fabricated "done".
  const ctx = stepByKey(running.steps, "context");
  push(
    "context-pack-honest-no-record",
    ctx.status === "pending" && ctx.noRecord === true,
    `context status=${ctx.status} noRecord=${ctx.noRecord}`,
  );

  // 3. run_ids present => ActionIntent done; policy id present => PolicyDecision done.
  push(
    "action-and-policy-present-are-done",
    stepByKey(running.steps, "action").status === "done" &&
      stepByKey(running.steps, "policy").status === "done",
    `action=${stepByKey(running.steps, "action").status} policy=${stepByKey(running.steps, "policy").status}`,
  );

  // 4. A non-live work never beats (only genuinely live data animates).
  const proposed = mapEnvelopeSpine({ id: "work_prop0", state: "proposed", ids: {} });
  push(
    "non-live-work-has-no-beat",
    proposed.steps.every((s) => s.status !== "live"),
    `statuses: ${proposed.steps.map((s) => s.status).join(",")}`,
  );

  // 5. run_ids absent => ActionIntent pending + noRecord (not fabricated).
  const action = stepByKey(proposed.steps, "action");
  push(
    "action-absent-is-pending-no-record",
    action.status === "pending" && action.noRecord === true,
    `action status=${action.status} noRecord=${action.noRecord}`,
  );

  // 6. A completed work with a transition id => Transition done; receipt via workbench.
  const completed = mapEnvelopeSpine(
    {
      id: "work_done0",
      state: "completed",
      run_ids: ["act_1", "act_2"],
      policy_decision_id: "pol_9",
      move_type: "rule_delta",
      ids: { transition_id: "tr_9" },
    },
    ["(new) -> ready created", "ready -> completed closed"],
    { receiptId: "rec_9" },
  );
  push(
    "completed-has-transition-and-receipt-done",
    stepByKey(completed.steps, "transition").status === "done" &&
      stepByKey(completed.steps, "receipt").status === "done",
    `transition=${stepByKey(completed.steps, "transition").status} receipt=${stepByKey(completed.steps, "receipt").status}`,
  );

  // 7. audit_only is observation, not completion: it must not render as done.
  const auditOnly = mapEnvelopeSpine({
    id: "work_audit0",
    state: "completed",
    move_type: "audit_only",
    ids: { transition_id: "tr_audit" },
  });
  push(
    "audit-only-is-not-done",
    stepByKey(auditOnly.steps, "transition").status !== "done",
    `transition=${stepByKey(auditOnly.steps, "transition").status}`,
  );

  // 8. The spine is always the seven envelope steps in runtime-grammar order.
  const order = running.steps.map((s) => s.key).join(">");
  push(
    "seven-step-envelope-order",
    order === "intent>context>envelope>action>policy>receipt>transition",
    order,
  );

  // 8. parseWorkShow extracts the JSON object and strips the lifecycle trailer.
  const raw = `{"id":"work_x","state":"claimed","ids":{}}\n--- lifecycle ---\n(new) -> ready   created\nready -> claimed  x claimed`;
  const parsed = parseWorkShow(raw);
  push(
    "parse-splits-json-and-lifecycle",
    parsed.projection?.id === "work_x" && parsed.lifecycle.length === 2,
    `projection=${parsed.projection?.id} lifecycle=${parsed.lifecycle.length}`,
  );

  const details = checks.map((c) => `${c.ok ? "ok  " : "FAIL"} ${c.name} — ${c.detail}`);
  return { ok: checks.every((c) => c.ok), details };
}
