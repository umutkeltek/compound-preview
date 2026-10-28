/**
 * Representation cluster v1 — the type layer of the volatility cluster
 * (packet 2026-07-11-representation-cluster-v1; ruling volatility-clustering-2026-07-11).
 *
 * These types ARE the frozen interface. Dev lanes may loop on shape/instrument
 * internals freely; what the rest of the console depends on is only this
 * vocabulary + the functions re-exported from `./index`. Nothing here reaches
 * the governed act path — an act leaves the cluster ONLY as an `ActRequest`
 * (settled decision #1), which a consumer outside the cluster maps onto
 * `/api/decision/execute`.
 */
import type { ReactElement } from "react";

/** The v1 shape vocabulary. `story` is the default — text wins unless a shape beats it. */
export type ShapeName =
  | "split-timeline" // expected <-> observed pair
  | "heartbeat" // a claim with no receipts to back it
  | "collision" // two statements that cancel
  | "ascii-pipe" // a flow gaining or losing an edge
  | "sparkline" // a quantity over time
  | "story"; // default — plain text

export type ActEffect = "record" | "apply" | "track";

/**
 * Shared object-state vocabulary for every operator-facing entity. Individual
 * projections may use richer lifecycle enums internally; the face translates
 * them into this small human grammar before rendering.
 */
export type ObjectFaceState =
  | "watching"
  | "needs_attention"
  | "ready"
  | "running"
  | "waiting"
  | "blocked"
  | "completed"
  | "failed";

export interface ObjectFaceAction {
  label: string;
  effect: ActEffect;
  /** Plain-language boundary for what this step does and does not do. */
  description: string;
}

/** Public-safe normalized target carried by the Go-owned AuthorityFace. */
export interface AuthorityFaceTarget {
  kind: string;
  id: string;
  path: string;
}

/** Public-safe normalized scope carried by the Go-owned AuthorityFace. */
export interface AuthorityFaceScope {
  entityId: string;
  projectId: string;
  productId: string;
  surface: string;
}

/**
 * The exact current policy representation for one action. This is display
 * authority, not a client-side policy evaluator; its revision stays opaque.
 */
export interface AuthorityFace {
  actionIntentId: string;
  policyDecisionId: string;
  authorityRevision: string;
  current: boolean;
  allowed: boolean;
  policyOutcome: "allow" | "allow_with_obligations" | "hold" | "deny";
  standingGrantId: string;
  wording: string;
  target: AuthorityFaceTarget;
  targetScope: string;
  scope: AuthorityFaceScope;
  blastRadius: string;
  reversibility: string;
  rollbackPlan: string;
  verifiedRecoverability: "verified" | "not_verified" | "unknown";
  operatorControlContinuity: "continuous" | "impaired" | "unknown";
  policySource: string;
  exactGate: string;
  expectedReceipt: string;
  expectedProof: string[];
}

/**
 * The stable human face of a Compound object. IDs and evidence stay available
 * to the owning surface, but this contract makes identity, state, why-now,
 * ownership, freshness, and the one next move scan consistently first.
 */
export interface ObjectFace {
  id: string;
  kind: string;
  label: string;
  state: ObjectFaceState;
  stateLabel: string;
  whyNow: string;
  owner: string;
  /** When the represented proposal was created. */
  proposedAt: string;
  /** Server-formatted local date/time for stable hydration and scanning. */
  proposedAtLabel: string;
  freshness: { generatedAt: string; source: string };
  confidence?: { value: number; label: string };
  /** Exact owner-issued authority for this face's action, or null when unavailable. */
  authority: AuthorityFace | null;
  primaryAction: ObjectFaceAction | null;
  unavailableReason: string;
  unblockPath: string;
}

export interface EvidenceFlow {
  nodes: string[];
  /** the one edge that changed this cycle; `gained:false` means an edge was lost. */
  changed?: { edge: [string, string]; gained: boolean };
}

/**
 * A self-explaining act affordance. pf4's law: the button states its
 * consequence AND its reversibility — a two-word button with neither is a bug.
 */
export interface EvidenceAction {
  area: string;
  itemId: string;
  actionId: string;
  label: string;
  requiresConfirmation: boolean;
  effect: ActEffect;
  consequence: string;
  reversibility: "reversible" | "irreversible";
  expectedRevision?: string;
  rationaleRequired?: boolean;
}

/**
 * Normalized evidence — the composer's input. Every field except `id` is
 * optional: projections are lossy and a shape must degrade honestly (pf3's
 * law — never fabricate a field a projection did not supply).
 */
export interface Evidence {
  id: string;
  /** human subject, rendered first (pf4 fix: no raw work_ ids as titles). */
  subject?: string;
  state?: string;
  kind?: string;
  expected_state?: string;
  observed_state?: string;
  /** two statements that cancel each other (a contradiction). */
  statements?: [string, string];
  flow?: EvidenceFlow;
  series?: number[];
  series_label?: string;
  /** the goal text — ALWAYS labelled as a goal, never rendered as a fact (pf3). */
  success_predicate?: string;
  receipt_refs?: string[];
  last_receipt_at?: string;
  summary?: string;
  action?: EvidenceAction | null;
}

/**
 * The ONLY way an act leaves the cluster (settled decision #1). A consumer
 * outside `src/representation/**` maps this onto the governed act path; the
 * cluster never imports the act client, so it can never mutate on its own.
 */
export interface ActRequest {
  area: string;
  itemId: string;
  actionId: string;
  effect: ActEffect;
  requiresConfirmation: boolean;
  expectedRevision?: string;
  rationale?: string;
  /** the prior decision record this one supersedes (instruments mint these). */
  supersedes?: string;
}

/**
 * The governed path's response: a receipt or a visible error. There is no
 * third "optimistic" branch — that absence is pf1's structural fix.
 */
export type ActResult =
  | { ok: true; receipt_ref: string; new_state?: string }
  | { ok: false; error: string };

/** What a chip is allowed to show, computed purely from an `ActResult`. */
export interface StateChip {
  state: string;
  error: string | null;
  receiptRef: string | null;
}

/** The minimal card shape the home number is derived from (FR2). */
export interface RepresentationCard {
  id: string;
  band: string;
  actionable: boolean;
}

/** Home accounting: the number is DERIVED from the cards, so they cannot diverge. */
export interface HomeAccount {
  number: number;
  actionable: RepresentationCard[];
  all: RepresentationCard[];
}

/** A board headline that refuses to render an unclosed goal as an accomplished fact (FR3). */
export interface BoardHeadline {
  text: string;
  predicateLabeled: boolean;
  rendersAsFact: boolean;
  claimStyled: boolean;
  lastReceiptAge: string | null;
}

export type ShapeChoice = { shape: ShapeName; reason: string };

export type ShapeRenderer = (input: { evidence: Evidence; reason: string }) => ReactElement;

export type RenderResult =
  | { ok: true; element: ReactElement; shape: ShapeName; reason: string; actRequests: ActRequest[] }
  | { ok: false; error: string };
