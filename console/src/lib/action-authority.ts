/**
 * Defensive client for the Go-owned ActionIntent -> PolicyDecision ->
 * AuthorityFace bundle. This module validates identity/linkage and preserves
 * the policy-owned display contract. It does not evaluate policy and it never
 * reproduces the opaque AuthorityFace revision hash.
 */

import type { AuthorityFace, AuthorityFaceScope, AuthorityFaceTarget } from "@/representation";

type UnknownRecord = Record<string, unknown>;

export type AuthorityDisplay = {
  wording: string;
  rows: Array<{ label: string; value: string }>;
  expectedProof: string[];
};

/**
 * One client-safe display projection for the exact Go-owned AuthorityFace.
 * Renderers may change layout, but they must not reinterpret policy or select
 * a subset of the owner's action context.
 */
export function authorityDisplay(authority: AuthorityFace): AuthorityDisplay {
  const target = [authority.target.kind, authority.target.id, authority.target.path].filter(Boolean).join(" · ");
  return {
    wording: authority.wording,
    rows: [
      { label: "Target", value: target || "Not specified by owner" },
      { label: "Target scope", value: authority.targetScope },
      { label: "Blast radius", value: authority.blastRadius },
      { label: "Reversibility", value: authority.reversibility },
      { label: "Rollback plan", value: authority.rollbackPlan || "Not provided by owner" },
      { label: "Verified recoverability", value: authority.verifiedRecoverability },
      { label: "Operator-control continuity", value: authority.operatorControlContinuity },
      { label: "Policy source", value: authority.policySource },
      { label: "Standing grant", value: authority.standingGrantId || "No standing grant" },
      { label: "Exact gate", value: authority.exactGate },
      { label: "Expected receipt", value: authority.expectedReceipt || "Not specified by owner" },
    ],
    expectedProof: authority.expectedProof,
  };
}

/** Exact AuthorityFace facts used by compact confirmation surfaces. */
export function authoritySummarySegments(authority: AuthorityFace): string[] {
  const display = authorityDisplay(authority);
  return [
    display.wording,
    ...display.rows.map((row) => `${row.label}: ${row.value}`),
    `Expected proof: ${display.expectedProof.join(", ") || "Not specified by owner"}`,
  ];
}

const AUTHORITY_REVISION = /^policy-authority-v1:sha256:[0-9a-f]{64}$/;
const ACTION_ID = /^act_\S+$/;
const POLICY_ID = /^pol_\S+$/;
const WORK_ID = /^work_\S+$/;
const MUTATION_CLASSES = new Set([
  "read",
  "local_reversible",
  "external_side_effect",
  "public",
  "destructive",
  "financial",
  "security",
]);
const REVERSIBILITIES = new Set(["none", "easy", "manual", "hard", "irreversible"]);
const POLICY_OUTCOMES = new Set(["allow", "allow_with_obligations", "hold", "deny"]);
const RECOVERABILITY = new Set(["verified", "not_verified", "unknown"]);
const OPERATOR_CONTROL = new Set(["continuous", "impaired", "unknown"]);

function record(value: unknown): UnknownRecord | null {
  return value != null && typeof value === "object" && !Array.isArray(value)
    ? value as UnknownRecord
    : null;
}

function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function stringList(value: unknown): string[] | null {
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== "string")) return null;
  return value.map((entry) => entry.trim());
}

function timestamp(value: unknown): boolean {
  const raw = text(value);
  return Boolean(raw && Number.isFinite(Date.parse(raw)));
}

function scopeOf(value: unknown): AuthorityFaceScope | null {
  const raw = record(value);
  if (!raw) return null;
  return {
    entityId: text(raw.entity_id),
    projectId: text(raw.project_id),
    productId: text(raw.product_id),
    surface: text(raw.surface),
  };
}

function targetOf(value: unknown): AuthorityFaceTarget | null {
  const raw = record(value);
  if (!raw) return null;
  return { kind: text(raw.kind), id: text(raw.id), path: text(raw.path) };
}

function sameScope(left: AuthorityFaceScope, right: AuthorityFaceScope): boolean {
  return left.entityId === right.entityId &&
    left.projectId === right.projectId &&
    left.productId === right.productId &&
    left.surface === right.surface;
}

function sameTarget(left: AuthorityFaceTarget, right: AuthorityFaceTarget): boolean {
  return left.kind === right.kind && left.id === right.id && left.path === right.path;
}

/**
 * Parse one action's embedded authority bundle. `null` means the owner did not
 * provide one complete, internally linked bundle. A non-current or held bundle
 * can still parse so the UI may show the exact policy context, but callers must
 * require `current && allowed` before mounting an Act.
 */
export function parseActionAuthorityBundle(rawAction: unknown): AuthorityFace | null {
  const action = record(rawAction);
  const intent = record(action?.action_intent);
  const decision = record(action?.policy_decision);
  const face = record(action?.policy_authority);
  if (!intent || !decision || !face) return null;

  const actionIntentId = text(intent.id);
  const policyDecisionId = text(decision.id);
  const actionWorkId = text(record(intent.ids)?.work_id);
  const decisionWorkId = text(record(decision.ids)?.work_id);
  const actionScope = scopeOf(intent.scope);
  const decisionScope = scopeOf(decision.scope);
  const actionTarget = targetOf(intent.target);
  const faceTarget = targetOf(face.target);
  const mutationClass = text(intent.mutation_class);
  const reversibility = text(intent.reversibility);
  const blastRadius = text(intent.blast_radius);
  const policyOutcome = text(decision.outcome);
  const decisionObligations = stringList(decision.obligations);
  const expectedProof = stringList(face.expected_proof);
  const current = face.current;
  const allowed = face.allowed;

  if (
    text(intent.schema_version) !== "compound.action_intent.v0.1" ||
    !ACTION_ID.test(actionIntentId) ||
    !timestamp(intent.created_at) ||
    !WORK_ID.test(actionWorkId) ||
    !actionScope ||
    !text(intent.capability) ||
    !MUTATION_CLASSES.has(mutationClass) ||
    !REVERSIBILITIES.has(reversibility) ||
    !blastRadius ||
    (mutationClass !== "read" && (!actionTarget?.kind || (!actionTarget.id && !actionTarget.path))) ||
    text(decision.schema_version) !== "compound.policy_decision.v0.1" ||
    !POLICY_ID.test(policyDecisionId) ||
    !timestamp(decision.created_at) ||
    !WORK_ID.test(decisionWorkId) ||
    !decisionScope ||
    text(decision.action_intent_id) !== actionIntentId ||
    !POLICY_OUTCOMES.has(policyOutcome) ||
    !decisionObligations ||
    (policyOutcome === "allow_with_obligations" && !decisionObligations.some(Boolean)) ||
    actionWorkId !== decisionWorkId ||
    !sameScope(actionScope, decisionScope) ||
    text(face.schema_version) !== "compound.policy_authority_face.v0.1" ||
    !AUTHORITY_REVISION.test(text(face.revision)) ||
    typeof current !== "boolean" ||
    typeof allowed !== "boolean" ||
    text(face.action_intent_id) !== actionIntentId ||
    text(face.policy_decision_id) !== policyDecisionId ||
    text(face.policy_outcome) !== policyOutcome ||
    text(face.standing_grant_id) !== text(decision.standing_grant_id) ||
    !actionTarget ||
    !faceTarget ||
    !sameTarget(actionTarget, faceTarget) ||
    text(face.blast_radius) !== blastRadius ||
    text(face.reversibility) !== reversibility ||
    allowed !== (policyOutcome === "allow" || policyOutcome === "allow_with_obligations") ||
    !text(face.wording) ||
    !text(face.target_scope) ||
    !RECOVERABILITY.has(text(face.verified_recoverability)) ||
    !OPERATOR_CONTROL.has(text(face.operator_control_continuity)) ||
    !text(face.policy_source) ||
    !text(face.exact_gate) ||
    !expectedProof ||
    (mutationClass !== "read" && (!text(face.expected_receipt) || !expectedProof.some(Boolean)))
  ) return null;

  return {
    actionIntentId,
    policyDecisionId,
    authorityRevision: text(face.revision),
    current,
    allowed,
    policyOutcome: policyOutcome as AuthorityFace["policyOutcome"],
    standingGrantId: text(face.standing_grant_id),
    wording: text(face.wording),
    target: actionTarget,
    targetScope: text(face.target_scope),
    scope: actionScope,
    blastRadius,
    reversibility,
    rollbackPlan: text(intent.rollback_plan),
    verifiedRecoverability: text(face.verified_recoverability) as AuthorityFace["verifiedRecoverability"],
    operatorControlContinuity: text(face.operator_control_continuity) as AuthorityFace["operatorControlContinuity"],
    policySource: text(face.policy_source),
    exactGate: text(face.exact_gate),
    expectedReceipt: text(face.expected_receipt),
    expectedProof,
  };
}
