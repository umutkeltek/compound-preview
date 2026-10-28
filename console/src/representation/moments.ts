/**
 * Normalize the existing workbench `recent` projection into the one moment the
 * Briefing can show as a proof-backed change. Receipt and transition references
 * establish no later reuse, downstream outcome, or measured improvement.
 */

export type ChangeMove = "outward_delta" | "memory_delta" | "rule_delta" | "control_plane_repair" | string;

export interface ProofBackedChange {
  kind: "proof_backed_change";
  evidenceLevel: "proof_backed_change";
  workId: string;
  headline: string;
  moveType: ChangeMove;
  moveLabel: string;
  producer: string;
  receiptRef: string;
  transitionRef: string;
  evidenceRefs: string[];
  occurredAt: string;
}

type ProjectionRow = Record<string, unknown>;

function row(value: unknown): ProjectionRow | null {
  return value != null && typeof value === "object" && !Array.isArray(value) ? (value as ProjectionRow) : null;
}

function text(value: unknown): string {
  return typeof value === "string" ? value.trim().replace(/\s+/g, " ") : "";
}

function strings(value: unknown): string[] {
  return Array.isArray(value) ? value.map(text).filter(Boolean) : [];
}

function moveLabel(move: string): string {
  switch (move) {
    case "outward_delta":
      return "Product changed";
    case "memory_delta":
      return "Memory changed";
    case "rule_delta":
      return "Rule changed";
    case "control_plane_repair":
      return "System repaired";
    default:
      return "Proof-backed change";
  }
}

function humanHeadline(item: ProjectionRow): string {
  const direct = text(item.desired_outcome) || text(item.title) || text(item.subject);
  if (direct) return direct;

  const surface = text(item.canonical_surface);
  if (surface.startsWith("packet:")) return surface.slice("packet:".length).replace(/-/g, " ");

  return text(item.success_predicate) || "A proof-backed change was recorded";
}

function rank(item: ProjectionRow): number {
  const value = item.updated_at ?? item.completed_at ?? item.claimed_at ?? item.created_at;
  if (typeof value === "number" && Number.isFinite(value)) return value;
  const raw = text(value);
  if (!raw) return 0;
  const numeric = Number(raw);
  if (Number.isFinite(numeric)) return numeric;
  const parsed = Date.parse(raw);
  return Number.isFinite(parsed) ? parsed : 0;
}

function normalize(value: unknown): ProofBackedChange | null {
  const item = row(value);
  if (!item) return null;

  const state = text(item.state);
  const changed = item.changed === true || text(item.changed).toLowerCase() === "true";
  const moveType = text(item.move_type) || text(item.move);
  const receiptRef = text(item.receipt_id) || text(item.receipt);
  const transitionRef = text(item.transition_id) || text(item.transition);
  if (state !== "completed" || !changed || !moveType || moveType === "audit_only" || !receiptRef || !transitionRef) {
    return null;
  }

  return {
    kind: "proof_backed_change",
    evidenceLevel: "proof_backed_change",
    workId: text(item.work_id) || text(item.id),
    headline: humanHeadline(item),
    moveType,
    moveLabel: moveLabel(moveType),
    producer: text(item.claimed_by) || text(item.owner) || "unknown producer",
    receiptRef,
    transitionRef,
    evidenceRefs: strings(item.evidence_refs),
    occurredAt: text(item.updated_at) || text(item.completed_at) || text(item.claimed_at) || text(item.created_at),
  };
}

export function latestProofBackedChange(values: readonly unknown[]): ProofBackedChange | null {
  let latest: { moment: ProofBackedChange; rank: number } | null = null;
  for (const value of values) {
    const item = row(value);
    const moment = normalize(value);
    if (!item || !moment) continue;
    const itemRank = rank(item);
    if (!latest || itemRank > latest.rank) latest = { moment, rank: itemRank };
  }
  return latest?.moment ?? null;
}
