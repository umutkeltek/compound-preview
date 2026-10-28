import { arr, HEAVY_PROJECTION_TIMEOUT_MS, num, runProjection, str, type Json, type ProjectionStatus } from "@/lib/compound";
import {
  normalizeOperatorCoordination,
  type CoordinationActor,
  type CoordinationLane,
  type CoordinationDoing,
  type CoordinationView,
  type CoordinationWait,
  type OperatorCoordinationProjection,
} from "@/lib/coordination-model";

/*
 * Coordination data plane (SERVER) — the read model for the Me / Others /
 * Waiting-on view. Wraps `compound coordination --json`
 * (compound.coordination.v0.1): the generic, READ-ONLY verb projecting the
 * planning half `work continue` runs for every actor — doing / can_continue /
 * wait_reason / blocked_on / lane gate — plus the global wait edges (who is
 * waiting on what to start). Actors are generic (fleet rows, lease holders,
 * claimers): "Codex"/"Claude" are just executors bound in, never hardcoded.
 *
 * Honesty (same law as lib/traffic): an unreachable verb (older binary)
 * surfaces present=false — the board shows "coordination door unreachable",
 * never an invented empty fleet. buildCoordinationView is PURE (synthetic
 * inputs in coordination.selftest.ts); getCoordination is the server I/O
 * wrapper reading through the shared runProjection memo.
 *
 * Client components import the leaf-pure types/predicates from
 * ./coordination-model (client-bundle-guard boundary); this module pulls in
 * node: builtins via lib/compound and must never enter the client bundle.
 */

export type {
  CoordinationActor,
  CoordinationDoing,
  CoordinationLane,
  CoordinationView,
  CoordinationWait,
  OperatorCoordinationProjection,
} from "@/lib/coordination-model";
export { laneGated, normalizeOperatorCoordination } from "@/lib/coordination-model";

export type CoordinationRead = {
  view: CoordinationView;
  status: ProjectionStatus;
  lastGoodAt: string | null;
  error: string | null;
};

function buildLane(value: any): CoordinationLane | null {
  if (!value || typeof value !== "object") return null;
  return {
    state: str(value.state),
    remaining: num(value.remaining),
    resetsAt: str(value.resets_at),
    refused: value.refused === true,
    confirmationRequired: value.confirmation_required === true,
    message: str(value.message),
  };
}

function buildDoing(value: any): CoordinationDoing | null {
  if (!value || typeof value !== "object") return null;
  const workId = str(value.work_id);
  if (!workId) return null;
  return {
    workId,
    title: str(value.title) || workId,
    state: str(value.state),
    nextAction: str(value.next_action),
    claimedBy: str(value.claimed_by),
    owner: str(value.owner),
  };
}

export function buildActor(value: any): CoordinationActor {
  const raw = value && typeof value === "object" ? value : {};
  return {
    actor: str(raw.actor),
    doing: buildDoing(raw.doing),
    nextAction: str(raw.next_action),
    canContinue: raw.can_continue === true,
    waitReason: str(raw.wait_reason),
    blockedOn: arr(raw.blocked_on).map(str).filter((s: string) => s !== ""),
    lane: buildLane(raw.lane),
    open: num(raw.open),
    inProgress: num(raw.in_progress),
    needsReview: num(raw.needs_review),
  };
}

export function buildWait(value: any): CoordinationWait {
  const raw = value && typeof value === "object" ? value : {};
  return {
    work: str(raw.work),
    actor: str(raw.actor),
    reason: str(raw.reason),
    blockedOn: arr(raw.blocked_on).map(str).filter((s: string) => s !== ""),
    lane: buildLane(raw.lane),
  };
}

const EMPTY_ACTOR: CoordinationActor = {
  actor: "",
  doing: null,
  nextAction: "",
  canContinue: false,
  waitReason: "",
  blockedOn: [],
  lane: null,
  open: 0,
  inProgress: 0,
  needsReview: 0,
};

/** Pure: map a coordination verb payload to the view. `value === null` (door
 *  unreachable or unparseable) yields present=false with nothing invented. */
export function buildCoordinationView(value: Json | null): CoordinationView {
  if (!value || typeof value !== "object" || !str(value.schema_version).startsWith("compound.coordination.")) {
    return {
      present: false,
      actor: "",
      actorExplicit: false,
      actorOptions: [],
      me: EMPTY_ACTOR,
      others: [],
      waits: [],
      operatorCoordination: normalizeOperatorCoordination(null),
      integrity: [],
      generatedAt: "",
    };
  }
  const me = buildActor(value.me);
  const others = arr(value.others).map(buildActor);
  const waits = arr(value.waits).map(buildWait);
  const actor = str(value.actor) || me.actor;
  const options = [actor, ...others.map((o) => o.actor)].filter((a) => a !== "");
  return {
    present: true,
    actor,
    actorExplicit: value.actor_explicit === true,
    actorOptions: Array.from(new Set(options)),
    me,
    others,
    waits,
    operatorCoordination: normalizeOperatorCoordination(value.operator_coordination),
    integrity: arr(value.integrity).map(str).filter((s: string) => s !== ""),
    generatedAt: str(value.generated_at),
  };
}

/** Server I/O: read the coordination verb through the honest-degradation memo.
 *  `actor` recenters "me" (generic actor id; empty = the verb's default). */
export async function getCoordination(actor?: string): Promise<CoordinationRead> {
  const args = ["coordination", "--json"];
  const trimmed = (actor ?? "").trim();
  if (trimmed) args.push("--actor", trimmed);
  const read = await runProjection(args, { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS });
  return {
    view: buildCoordinationView(read.value),
    status: read.status,
    lastGoodAt: read.lastGoodAt,
    error: read.error,
  };
}
