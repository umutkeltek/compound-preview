/*
 * Envelope-spine mapper — the data layer behind the Trace drawer (the
 * signature element of the redesign). Pure: no node/server imports, so both
 * the API route (server) and a depth self-test can drive it, and the client
 * drawer can `import type` its shapes without pulling a runtime dependency.
 *
 * It maps the live `compound work show <id> --json` projection
 * (`compound.work.v0.1`) onto the runtime grammar's envelope spine
 * (docs/architecture/runtime.md):
 *
 *   Intent → ContextPack → WorkEnvelope → ActionIntent → PolicyDecision
 *          → Receipt → Transition
 *
 * HONESTY LAW (redesign plan Part 3): render only what the projection carries.
 * A stage the projection has no record for is shown as `pending` with a muted
 * "no record" note — never fabricated. Verified against the live payload:
 *  - Intent        — always present (desired_outcome / success_predicate)
 *  - ContextPack   — NO dedicated ref in v0.1; inferred from scope + evidence
 *                    refs, so it is usually `pending` ("no record") today
 *  - WorkEnvelope  — always present (state + lifecycle trailer); the ONE node
 *                    that beats, and only while state is genuinely live
 *  - ActionIntent  — run_ids[] (act_…)
 *  - PolicyDecision— policy_decision_id (pol_…)
 *  - Receipt       — receipt_id only rides the workbench item, not work show;
 *                    fall back to artifact_ids / verification_ids
 *  - Transition    — ids.transition_id (tr_…), present once the work closes
 */

type WorkJson = Record<string, unknown>;

export type TraceNodeStatus = "done" | "live" | "pending";

export type TraceStep = {
  key: string;
  name: string;
  /** Short phase pill text (e.g. the lifecycle state, "scoped", "proof"). */
  phase: string;
  status: TraceNodeStatus;
  description: string;
  /** Mono refs to render under the step (ids, timestamps). */
  meta: string[];
  /** The projection has no record for this stage — render the muted note. */
  noRecord: boolean;
};

export type TraceModel = {
  workId: string;
  title: string;
  state: string;
  createdAt: string;
  owner: string;
  source: string;
  steps: TraceStep[];
  /** Parsed `--- lifecycle ---` trailer lines (state history + policy). */
  lifecycle: string[];
};

const LIVE_STATES = new Set(["running", "verifying", "in_progress"]);

function s(value: unknown): string {
  return typeof value === "string" ? value : "";
}
function list(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}
function strings(value: unknown): string[] {
  return list(value).map(String).filter(Boolean);
}
function obj(value: unknown): WorkJson {
  return value && typeof value === "object" && !Array.isArray(value) ? (value as WorkJson) : {};
}

/**
 * Extract the first balanced JSON object from `work show --json` output and
 * the human `--- lifecycle ---` trailer that follows it. Mirrors the console's
 * own `firstJsonObject` scanner so the mapper stays dependency-free.
 */
export function parseWorkShow(raw: string): { projection: WorkJson | null; lifecycle: string[] } {
  const start = raw.indexOf("{");
  if (start < 0) return { projection: null, lifecycle: [] };

  let depth = 0;
  let inString = false;
  let escaped = false;
  let end = -1;
  for (let i = start; i < raw.length; i++) {
    const ch = raw[i];
    if (inString) {
      if (escaped) escaped = false;
      else if (ch === "\\") escaped = true;
      else if (ch === '"') inString = false;
      continue;
    }
    if (ch === '"') inString = true;
    else if (ch === "{") depth++;
    else if (ch === "}") {
      depth--;
      if (depth === 0) {
        end = i + 1;
        break;
      }
    }
  }
  if (end < 0) return { projection: null, lifecycle: [] };

  let projection: WorkJson | null = null;
  try {
    projection = JSON.parse(raw.slice(start, end)) as WorkJson;
  } catch {
    projection = null;
  }

  const trailer = raw.slice(end);
  const marker = trailer.indexOf("--- lifecycle ---");
  const lifecycle =
    marker < 0
      ? []
      : trailer
          .slice(marker + "--- lifecycle ---".length)
          .split(/\r?\n/)
          .map((line) => line.replace(/\s+/g, " ").trim())
          .filter(Boolean);

  return { projection, lifecycle };
}

/** Best-effort human title from the projection (mirrors the board's rule). */
function projectionTitle(p: WorkJson): string {
  const surface = s(p.canonical_surface);
  if (surface.startsWith("packet:")) return surface.slice("packet:".length).replace(/-/g, " ");
  return s(p.success_predicate) || s(p.desired_outcome) || s(p.id) || "work item";
}

/**
 * Map a work-show projection (+ its lifecycle trailer + the receipt_id that
 * only the workbench item carries) onto the seven envelope-spine steps.
 * `receiptId` is optional: the trace API passes it through from the workbench
 * item so the Receipt node reflects reality instead of always pending.
 */
export function mapEnvelopeSpine(
  projection: WorkJson,
  lifecycle: string[] = [],
  extra: { receiptId?: string } = {},
): TraceModel {
  const p = projection;
  const state = s(p.state);
  const ids = obj(p.ids);

  const createdAt = s(p.created_at);
  const owner = s(p.owner) || s(obj(p.authority).owner);
  const authority = obj(p.authority);

  const evidenceRefs = strings(p.evidence_refs);
  const scope = obj(p.scope);
  const scopeKeys = Object.keys(scope);
  const runIds = strings(p.run_ids);
  const policyId = s(p.policy_decision_id);
  const artifactIds = strings(p.artifact_ids);
  const verificationIds = strings(p.verification_ids);
  const receiptId = s(extra.receiptId) || s(p.receipt_id);
  const transitionId = s(ids.transition_id) || s(p.transition_id);
  const moveType = s(p.move_type);

  const isLive = LIVE_STATES.has(state);

  const contextHasRecord = evidenceRefs.length > 0 || scopeKeys.length > 0;
  const actionHasRecord = runIds.length > 0;
  const policyHasRecord = policyId.length > 0;
  const receiptHasRecord = receiptId.length > 0 || artifactIds.length > 0 || verificationIds.length > 0;
  const transitionHasRecord = transitionId.length > 0;

  const steps: TraceStep[] = [
    {
      key: "intent",
      name: "Intent",
      phase: "intent",
      status: "done",
      description: `Predicate — ${s(p.success_predicate) || s(p.desired_outcome) || "work intent recorded"}`,
      meta: [createdAt, owner ? `owner ${owner}` : ""].filter(Boolean),
      noRecord: false,
    },
    {
      key: "context",
      name: "Context pack",
      phase: contextHasRecord ? "scoped" : "—",
      status: contextHasRecord ? "done" : "pending",
      description: contextHasRecord
        ? `${evidenceRefs.length} evidence ref${evidenceRefs.length === 1 ? "" : "s"} scoped${
            authority.source_class ? ` · authority ${s(authority.source_class)}` : ""
          }.`
        : "No context pack recorded in this projection (work.v0.1 carries no ContextPack ref).",
      meta: contextHasRecord
        ? [...evidenceRefs.slice(0, 4), ...scopeKeys.slice(0, 2).map((k) => `scope.${k}`)]
        : authority.source_class
          ? [`authority ${s(authority.source_class)}/${s(authority.owner)}`]
          : [],
      noRecord: !contextHasRecord,
    },
    {
      key: "envelope",
      name: "Work envelope",
      phase: state || "unknown",
      status: isLive ? "live" : "done",
      description: state
        ? `Lifecycle at ${state.replace(/_/g, " ")}${
            lifecycle.length ? ` · ${lifecycle.length} recorded step${lifecycle.length === 1 ? "" : "s"}` : ""
          }.`
        : "Work envelope exists.",
      meta: lifecycle.slice(-3),
      noRecord: false,
    },
    {
      key: "action",
      name: "Action intent",
      phase: actionHasRecord ? "dispatched" : "—",
      status: actionHasRecord ? "done" : "pending",
      description: actionHasRecord
        ? `${runIds.length} action run${runIds.length === 1 ? "" : "s"} dispatched under this work.`
        : "No action runs dispatched yet.",
      meta: runIds.slice(0, 4),
      noRecord: !actionHasRecord,
    },
    {
      key: "policy",
      name: "Policy decision",
      phase: policyHasRecord ? "governed" : "—",
      status: policyHasRecord ? "done" : "pending",
      description: policyHasRecord
        ? "A PolicyDecision governs this work — every mutation is gated at the adapter, not asked for in a prompt."
        : "No PolicyDecision recorded yet.",
      meta: policyHasRecord ? [policyId] : [],
      noRecord: !policyHasRecord,
    },
    {
      key: "receipt",
      name: "Receipt",
      phase: receiptHasRecord ? "proof" : "—",
      status: receiptHasRecord ? "done" : "pending",
      description: receiptHasRecord
        ? "Proof recorded: what ran, what it touched, what came out."
        : "No receipt yet — work is still open.",
      meta: [receiptId, ...artifactIds.slice(0, 2), ...verificationIds.slice(0, 2)].filter(Boolean),
      noRecord: !receiptHasRecord,
    },
    {
      key: "transition",
      name: "Transition",
      phase: transitionHasRecord ? moveType || "recorded" : "—",
      status: transitionHasRecord && moveType !== "audit_only" ? "done" : "pending",
      description: transitionHasRecord
        ? `Apex transition recorded${moveType ? ` (${moveType.replace(/_/g, " ")})` : ""}. audit_only counts for nothing.`
        : "No transition recorded — audit_only or still open.",
      meta: transitionHasRecord ? [transitionId] : [],
      noRecord: !transitionHasRecord,
    },
  ];

  return {
    workId: s(p.id),
    title: projectionTitle(p),
    state,
    createdAt,
    owner,
    source: `compound work show ${s(p.id)} --json`,
    steps,
    lifecycle,
  };
}
