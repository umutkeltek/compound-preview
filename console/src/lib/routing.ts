import { arr, num, runJson, str, type Json, type ProjectionStatus } from "@/lib/compound";
import type { AttentionReport } from "@/lib/attention";
import { buildDecideGroups, type DecideGroup } from "@/lib/decide-model";
import { areaOf, groupsForBand } from "@/lib/review-data";

/**
 * Routing & cost server model. The screen answers ONE question — "is routing
 * sending the right mind at the right cost, and should a lane change?" — from
 * three read-only projections:
 *
 *   - `compound route model-capabilities --json`  → the lane roster with
 *      tier + cost_class (the "which mind" table; compare → table doctrine).
 *   - `compound route evidence-authority --json`   → route-evidence traffic +
 *      token/cost aggregates (the "at what cost" signal; honest about the
 *      dollar-spend gap, same as Agents' CostLanes).
 *   - `compound attention --json` + `compound reviews --json` → the route
 *      promotion/remorse decisions (the "should a lane change" gate), built
 *      through the SAME decide-model resolver the Decide queue uses so the
 *      bespoke `route-promotions` Record-hold act is governed identically.
 *
 * `route plan` is deliberately NOT fetched: it requires a `--candidate` or
 * `--capability-class` argument the console cannot supply from a projection
 * (verified: `route plan --json` with no args prints usage), so the page
 * renders an honest copy-command instead of faking a plan.
 */

/* ---------- route areas ---------- */

export const ROUTE_AREAS = ["route-promotions", "route-remorse"] as const;

export function isRouteArea(area: string): boolean {
  return area === "route-promotions" || area === "route-remorse";
}

/** Blast radii that force Class-A (irreversible-gate) framing. */
const CLASS_A_BLAST = new Set(["public", "destructive", "financial", "security"]);

/* ---------- model lanes (route model-capabilities --json) ---------- */

export type ModelLane = {
  modelId: string;
  tier: string; // free | cheap | premium | local-batch | ...
  costClass: string; // free | api-marginal | subscription-fixed | local
  harness: string;
  providerRoute: string;
  capabilityCount: number;
};

export type ModelLanesReport = {
  generatedAt: string;
  authorityRef: string;
  sourceAuthorityId: string;
  modelCount: number;
  capabilityClasses: string[];
  routingDefaultCount: number;
  lanes: ModelLane[];
};

function toLane(raw: Json): ModelLane {
  return {
    modelId: str(raw.model_id) || str(raw.modelID) || str(raw.model),
    tier: str(raw.tier),
    costClass: str(raw.cost_class),
    harness: str(raw.harness),
    providerRoute: str(raw.provider_route),
    capabilityCount: arr(raw.capabilities).length,
  };
}

export async function getModelLanes(): Promise<ModelLanesReport | null> {
  const raw = await runJson(["route", "model-capabilities", "--json"]);
  if (!raw) return null;
  return {
    generatedAt: str(raw.generated_at),
    authorityRef: str(raw.authority_ref),
    sourceAuthorityId: str(raw.source_authority_id),
    modelCount: num(raw.model_count),
    capabilityClasses: arr(raw.capability_classes).map(String),
    routingDefaultCount: num(raw.routing_default_count),
    lanes: arr(raw.lanes).map((l) => toLane(l as Json)),
  };
}

/** Tier ordering for the lane table: premium first, local-batch last. */
export const TIER_ORDER = ["premium", "cheap", "free", "local-batch"] as const;

export function tierRank(tier: string): number {
  const i = (TIER_ORDER as readonly string[]).indexOf(tier);
  return i < 0 ? TIER_ORDER.length : i;
}

/* ---------- route traffic (route evidence-authority --json) ---------- */

export type CountRow = { label: string; value: number };

export type RouteTrafficReport = {
  installed: boolean;
  authorityRef: string;
  authorityPath: string;
  routeEvidenceCount: number;
  providerLedgerEvidenceCount: number;
  tokenUsageRows: number;
  costKnownRows: number;
  totalTokens: number;
  totalCostMicros: number;
  statusCounts: CountRow[];
  providerCounts: CountRow[];
  protocolCounts: CountRow[];
  sourceRole: string;
  fallbackRole: string;
  /** Timestamp of the most recent route-evidence row — the true freshness of
   *  the traffic signal (the projection itself stamps no generated_at). */
  latestEvidenceAt: string;
};

function toCountRows(raw: unknown): CountRow[] {
  if (!raw || typeof raw !== "object") return [];
  return Object.entries(raw as Record<string, unknown>)
    .map(([label, value]) => ({ label, value: num(value) }))
    .sort((a, b) => b.value - a.value);
}

export async function getRouteTraffic(): Promise<RouteTrafficReport | null> {
  const raw = await runJson(["route", "evidence-authority", "--json"]);
  if (!raw) return null;
  const recent = arr(raw.recent);
  return {
    installed: raw.installed !== false,
    authorityRef: str(raw.authority_ref),
    authorityPath: str(raw.authority_path),
    routeEvidenceCount: num(raw.route_evidence_count),
    providerLedgerEvidenceCount: num(raw.provider_ledger_evidence_count),
    tokenUsageRows: num(raw.token_usage_rows),
    costKnownRows: num(raw.cost_known_rows),
    totalTokens: num(raw.total_tokens),
    totalCostMicros: num(raw.total_cost_micros),
    statusCounts: toCountRows(raw.status_counts),
    providerCounts: toCountRows(raw.provider_counts),
    protocolCounts: toCountRows(raw.protocol_counts),
    sourceRole: str(raw.source_role),
    fallbackRole: str(raw.fallback_role),
    latestEvidenceAt: recent.length ? str((recent[0] as Json).created_at) : "",
  };
}

/* ---------- promotion band (attention + reviews → decide-model) ----------
 * Ports the minimal glue from app/reviews/page.tsx (buildItemIndex /
 * sampleForGroup / orderByAttention) so route promotion/remorse groups build
 * into the exact same DecideGroup shape the Decide queue renders — the
 * judgment (which act, in what mode) stays in decide-model, never here.
 */

function buildItemIndex(reviews: Json | null): Map<string, Json> {
  const map = new Map<string, Json>();
  for (const item of arr(reviews?.items)) {
    const id = str(item.id) || str(item.review_id);
    if (id) map.set(id, item);
  }
  return map;
}

function groupCreatedAt(group: Json, itemByID: Map<string, Json>): string {
  const sample = group.sample;
  const sampleCreated = sample && typeof sample === "object" ? str((sample as Json).created_at) : "";
  if (sampleCreated) return sampleCreated;
  let min = "";
  for (const id of arr(group.item_ids).map(String)) {
    const created = str(itemByID.get(id)?.created_at);
    if (created && (!min || created < min)) min = created;
  }
  return min || str(group.created_at);
}

function sampleForGroup(group: Json, itemByID: Map<string, Json>): Json {
  const sample = group.sample;
  if (sample && typeof sample === "object") return sample as Json;
  for (const id of arr(group.item_ids).map(String)) {
    const item = itemByID.get(id);
    if (item) return item;
  }
  return {
    id: str(group.key),
    area: areaOf(group),
    readiness: str(group.readiness),
    status: str(group.status),
    next_action: str(group.operator_summary),
    operator_summary: str(group.operator_summary),
    operator_unlock: str(group.operator_unlock),
    group_key: str(group.key),
    evidence_refs: arr(group.evidence_refs),
  };
}

function orderByAttention(groups: Json[], attention: AttentionReport | null, itemByID: Map<string, Json>): Json[] {
  const rankByKey = new Map<string, number>();
  const rankByArea = new Map<string, number>();
  arr(attention?.groups).forEach((g: Json, index: number) => {
    const key = str(g.key);
    if (key && !rankByKey.has(key)) rankByKey.set(key, index);
    const area = str(g.area);
    if (area && !rankByArea.has(area)) rankByArea.set(area, index);
  });
  const UNRANKED = Number.MAX_SAFE_INTEGER;
  const rankOf = (group: Json): number => {
    const byKey = rankByKey.get(str(group.key));
    if (byKey != null) return byKey;
    return rankByArea.get(areaOf(group)) ?? UNRANKED;
  };
  return groups
    .map((group, index) => ({ group, index, rank: rankOf(group) }))
    .sort((a, b) => {
      if (a.rank !== b.rank) return a.rank - b.rank;
      const ageA = groupCreatedAt(a.group, itemByID) || "9999";
      const ageB = groupCreatedAt(b.group, itemByID) || "9999";
      if (ageA !== ageB) return ageA.localeCompare(ageB);
      return a.index - b.index;
    })
    .map((entry) => entry.group);
}

export type RoutePromotionModel = {
  /** needs-you route groups (the promotion/remorse gate). */
  needsYou: DecideGroup[];
  /** watching route groups — hold-insufficient-evidence carries the Record-hold act. */
  watching: DecideGroup[];
  /** true when any needs-you route group is irreversible or high-blast (Class-A gate). */
  classA: boolean;
  needsYouItems: number;
  watchingItems: number;
  generatedAt: string;
};

function buildBand(
  band: "needs_you" | "watching",
  reviews: Json | null,
  attention: AttentionReport | null,
  itemByID: Map<string, Json>,
  projectionStatus?: ProjectionStatus,
): DecideGroup[] {
  const routeGroups = groupsForBand(reviews, band, attention).filter((g) => isRouteArea(areaOf(g)));
  const ordered = orderByAttention(routeGroups, attention, itemByID);
  return buildDecideGroups(ordered, itemByID, attention, (g) => sampleForGroup(g, itemByID), projectionStatus).filter((g) =>
    isRouteArea(g.area),
  );
}

export function buildRoutePromotionModel(
  reviews: Json | null,
  attention: AttentionReport | null,
  projectionStatus?: ProjectionStatus,
): RoutePromotionModel {
  const itemByID = buildItemIndex(reviews);
  const needsYou = buildBand("needs_you", reviews, attention, itemByID, projectionStatus);
  const watching = buildBand("watching", reviews, attention, itemByID, projectionStatus);

  const classA = arr(attention?.groups).some(
    (g: Json) =>
      isRouteArea(str(g.area)) &&
      str(g.band) === "needs_you" &&
      (str(g.reversibility) === "irreversible" || CLASS_A_BLAST.has(str(g.blast_radius))),
  );

  return {
    needsYou,
    watching,
    classA,
    needsYouItems: needsYou.reduce((sum, g) => sum + g.count, 0),
    watchingItems: watching.reduce((sum, g) => sum + g.count, 0),
    generatedAt: str(attention?.generated_at),
  };
}
