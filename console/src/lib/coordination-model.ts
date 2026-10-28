/*
 * Client-safe coordination model: the types + pure predicates the
 * CoordinationBoard renders. NO node: builtins, NO lib/compound import — this
 * module lands in the client bundle, so it must stay leaf-pure (the
 * client-bundle-guard smoke enforces the boundary). The server data plane
 * (lib/coordination.ts) maps `compound coordination --json` onto these types.
 */

export type CoordinationLane = {
  state: string;
  remaining: number;
  resetsAt: string;
  refused: boolean;
  confirmationRequired: boolean;
  message: string;
};

export type CoordinationDoing = {
  workId: string;
  title: string;
  state: string;
  nextAction: string;
  claimedBy: string;
  owner: string;
};

export type CoordinationActor = {
  actor: string;
  doing: CoordinationDoing | null;
  nextAction: string;
  canContinue: boolean;
  waitReason: string;
  blockedOn: string[];
  lane: CoordinationLane | null;
  open: number;
  inProgress: number;
  needsReview: number;
};

export type CoordinationWait = {
  work: string;
  actor: string;
  reason: string;
  blockedOn: string[];
  lane: CoordinationLane | null;
};

export type OperatorCoordinationDelta = {
  kind: string;
  subjectRef: string;
  before: string;
  after: string;
};

export type OperatorCoordinationItem = {
  eventRef: string;
  workRef: string;
  deliveryClass: string;
  delta: OperatorCoordinationDelta;
  artifactRefs: string[];
  semanticRevision: string;
};

export type OperatorCoordinationProjection = {
  schemaVersion: string;
  revision: string;
  total: number;
  truncated: boolean;
  items: OperatorCoordinationItem[];
};

const OPERATOR_COORDINATION_MAX_ITEMS = 8;
const OPERATOR_DELIVERY_CLASSES = new Set([
  "silent_state",
  "next_boundary",
  "interrupt",
  "human_attention",
]);
const COLLABORATION_TOKEN = /^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$/;
const SEMANTIC_REVISION = /^sha256:[0-9a-f]{64}$/;
const ARTIFACT_REF = /^artifact:sha256:[0-9a-f]{64}$/;

function projectionString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function projectionNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function projectionArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function validRequiredRef(value: unknown, prefix: string): value is string {
  return typeof value === "string" &&
    value.startsWith(prefix) &&
    value.length > prefix.length &&
    COLLABORATION_TOKEN.test(value);
}

function validStateToken(value: unknown): value is string | undefined {
  return value === undefined ||
    (typeof value === "string" && (value === "" || COLLABORATION_TOKEN.test(value)));
}

// normalizeOperatorCoordination is a pure client-side whitelist. Unknown and
// hostile fields are discarded instead of crossing into rendered props.
export function normalizeOperatorCoordination(
  value: unknown,
  humanOnly = false,
): OperatorCoordinationProjection {
  const raw = value && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {};
  const eligible = projectionArray(raw.items)
    .map((candidate): OperatorCoordinationItem | null => {
      const item = candidate && typeof candidate === "object" && !Array.isArray(candidate)
        ? candidate as Record<string, unknown>
        : null;
      if (!item) return null;
      const rawDelta = item.delta && typeof item.delta === "object" && !Array.isArray(item.delta)
        ? item.delta as Record<string, unknown>
        : null;
      if (!rawDelta) return null;
      const eventRef = item.event_ref;
      const workRef = item.work_ref;
      const deliveryClass = item.delivery_class;
      const kind = rawDelta.kind;
      const subjectRef = rawDelta.subject_ref;
      const semanticRevision = item.semantic_revision;
      const rawArtifactRefs = projectionArray(item.artifact_refs);
      if (
        !validRequiredRef(eventRef, "wco_") ||
        !validRequiredRef(workRef, "work_") ||
        typeof deliveryClass !== "string" ||
        !OPERATOR_DELIVERY_CLASSES.has(deliveryClass) ||
        typeof kind !== "string" ||
        !COLLABORATION_TOKEN.test(kind) ||
        typeof subjectRef !== "string" ||
        !COLLABORATION_TOKEN.test(subjectRef) ||
        typeof semanticRevision !== "string" ||
        !SEMANTIC_REVISION.test(semanticRevision) ||
        rawArtifactRefs.length === 0 ||
        !rawArtifactRefs.every((ref) => typeof ref === "string" && ARTIFACT_REF.test(ref)) ||
        !validStateToken(rawDelta.before) ||
        !validStateToken(rawDelta.after)
      ) {
        return null;
      }
      return {
        eventRef,
        workRef,
        deliveryClass,
        delta: {
          kind,
          subjectRef,
          before: projectionString(rawDelta.before),
          after: projectionString(rawDelta.after),
        },
        artifactRefs: rawArtifactRefs as string[],
        semanticRevision,
      };
    })
    .filter((item): item is OperatorCoordinationItem => item !== null)
    .filter((item) => !humanOnly || item.deliveryClass === "human_attention");
  const items = eligible.slice(0, OPERATOR_COORDINATION_MAX_ITEMS);
  const rawTotal = Math.max(0, projectionNumber(raw.total));
  const schemaVersion = projectionString(raw.schema_version);
  const total = humanOnly && !schemaVersion.startsWith("compound.operator_attention_projection.")
    ? eligible.length
    : Math.max(rawTotal, eligible.length);
  return {
    schemaVersion,
    revision: projectionString(raw.revision),
    total,
    truncated: raw.truncated === true || eligible.length > items.length || total > items.length,
    items,
  };
}

export type CoordinationView = {
  /** false => the coordination verb never answered (old binary / door down). */
  present: boolean;
  actor: string;
  actorExplicit: boolean;
  /** every actor the view can be re-centered on (generic; me first). */
  actorOptions: string[];
  me: CoordinationActor;
  others: CoordinationActor[];
  waits: CoordinationWait[];
  operatorCoordination: OperatorCoordinationProjection;
  integrity: string[];
  generatedAt: string;
};

/** A lane is a gate (a wait) when the guard refused, holds for confirmation,
 *  or reports a closed state — mirrors the verb's coordinationLaneGated. */
export function laneGated(lane: CoordinationLane | null): boolean {
  if (!lane) return false;
  if (lane.refused || lane.confirmationRequired) return true;
  return lane.state === "walled" || lane.state === "dead" || lane.state === "refused";
}
