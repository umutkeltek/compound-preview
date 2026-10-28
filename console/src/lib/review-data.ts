import { readFile } from "node:fs/promises";
import path from "node:path";

import type { AttentionGroup, AttentionReport } from "@/lib/attention";
import { arr, COMPOUND_HOME, Json, num, privateFallbacksEnabled, reviewCommand, str } from "@/lib/compound";
import { parseReviewDraftYaml, ReviewDraftFields } from "@/lib/review-yaml";

export type ExternalReviewRow = Json & {
  yamlPath: string;
  draft: ReviewDraftFields | null;
};

/** Which band of the operator's attention a review row/group is in (§1/§3/§5 of the redesign plan). */
export type Band = "needs_you" | "watching" | "filed";

const EXTERNAL_AREA = "external-repo-intake";

/* ---------- band + grouping (shared by the Reviews page and the nav badge) ----------
 * Moved out of app/reviews/page.tsx so the nav "Decide" badge counts the exact same
 * needs_you groups the Reviews page renders, instead of a second drifting heuristic.
 * Retires in Phase 2 once `compound attention --json` replaces every client heuristic
 * (Part 1 of the redesign plan).
 */

export function areaOf(row: Json): string {
  return str(row.area) || str(row.review_area);
}

export function bandOf(row: Json): Band {
  const b = str(row.band);
  return b === "watching" || b === "filed" ? b : "needs_you";
}

function uniq(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))].sort();
}

/**
 * Deterministic local re-grouping key for items the Go projection left as
 * singletons (`summary_source: "fallback"` writer debt — see design spec §5
 * and §10). Prefers the item's own group_key so items that genuinely share
 * one (e.g. every contradiction from one synthesis sweep) still collapse
 * into a single card client-side instead of vanishing from Needs You.
 */
function fallbackGroupKey(row: Json): string {
  const area = areaOf(row) || "review";
  const basis =
    str(row.group_key) ||
    str(row.context_label) ||
    str(row.operator_summary) ||
    str(row.next_action) ||
    str(row.id) ||
    "decision";
  return `${area}:${basis.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 80)}`;
}

function fallbackGroups(rows: Json[]): Json[] {
  const byKey = new Map<string, Json>();
  for (const row of rows) {
    const key = fallbackGroupKey(row);
    const previous = byKey.get(key);
    const evidence = uniq([...(arr(previous?.evidence_refs).map(String) ?? []), ...arr(row.evidence_refs).map(String)]);
    const itemIds = uniq([...(arr(previous?.item_ids).map(String) ?? []), str(row.id) || str(row.review_id)]);
    byKey.set(key, {
      ...(previous ?? {}),
      key,
      area: areaOf(row),
      band: bandOf(row),
      count: num(previous?.count) + 1,
      status: str(previous?.status) || str(row.status),
      readiness: str(previous?.readiness) || str(row.readiness),
      operator_summary:
        str(previous?.operator_summary) ||
        str(row.operator_summary) ||
        str(row.next_action) ||
        str(row.id) ||
        "Review this decision.",
      operator_unlock: str(previous?.operator_unlock) || str(row.operator_unlock),
      context_label: str(previous?.context_label) || str(row.context_label) || areaOf(row),
      item_ids: itemIds,
      evidence_refs: evidence,
      terminal_reason: str(previous?.terminal_reason) || "owner has not exposed a safe action for this decision group",
      sample: previous?.sample ?? row,
    });
  }
  return [...byKey.values()];
}

/**
 * Backend groups for one band, plus a client-side fallback grouping of any
 * band-matching items NO projection grouped — so nothing that needs a decision
 * silently drops off the page just because its area's grouping isn't wired up
 * on the backend yet.
 *
 * writer-debt safety net — retire when attention.counts.fallback_groups == 0.
 * The fallback only fires for items neither `compound reviews --json` groups
 * NOR the `compound attention --json` projection groups: every item attention
 * covers is authoritative and left to the one projection. Pass `attention` so
 * that as the projection's per-area grouping grows, this client net shrinks in
 * lockstep instead of double-emitting a group the projection already owns.
 */
export function groupsForBand(
  reviews: Json | null,
  band: "needs_you" | "watching",
  attention?: AttentionReport | null,
): Json[] {
  const backend = arr(reviews?.groups).filter((g: Json) => bandOf(g) === band && areaOf(g) !== EXTERNAL_AREA);
  const covered = new Set<string>([
    ...backend.flatMap((g) => arr(g.item_ids).map(String)),
    ...arr(attention?.groups).flatMap((g) => arr((g as Json).item_ids).map(String)),
  ]);
  const leftover = arr(reviews?.items).filter((it: Json) => {
    if (bandOf(it) !== band) return false;
    if (areaOf(it) === EXTERNAL_AREA) return false;
    const id = str(it.id) || str(it.review_id);
    return Boolean(id) && !covered.has(id);
  });
  return [...backend, ...fallbackGroups(leftover)];
}

function sameMembers(left: unknown, right: unknown): boolean {
  const a = [...new Set(arr(left).map(String).filter(Boolean))].sort();
  const b = [...new Set(arr(right).map(String).filter(Boolean))].sort();
  return a.length > 0 && a.length === b.length && a.every((value, index) => value === b[index]);
}

/**
 * Preserve Attention as the identity, membership, and order owner while
 * restoring richer Reviews metadata for the exact same key. Evidence may
 * enrich a key match. A group-wide action crosses only when both owners name
 * the exact same member set, so stale Reviews membership cannot execute a
 * wider batch than the card represents.
 */
export function enrichAttentionGroups(
  attentionGroups: ReadonlyArray<AttentionGroup>,
  reviewGroups: ReadonlyArray<Json>,
): Json[] {
  const reviewByKey = new Map<string, Json>();
  for (const group of reviewGroups) {
    const key = str(group.key);
    if (key) reviewByKey.set(key, group);
  }
  return attentionGroups.map((group) => {
    const review = reviewByKey.get(group.key);
    const enriched = {
      ...(review ?? {}),
      ...group,
    } as unknown as Json;
    if (review && !sameMembers(group.item_ids, review.item_ids)) {
      // Attention remains the membership/action owner. Keep the Reviews
      // member ids under a separate evidence-only key so a matching Panel
      // run/disposition can retain its claim detail while the two projections
      // refresh at slightly different moments. Consumers must never execute
      // against this fallback membership.
      enriched.review_item_ids = arr(review.item_ids).map(String).filter(Boolean);
      delete enriched.safe_batch_action;
    }
    const sample = enriched.sample as Json | undefined;
    const sampleID = str(sample?.id) || str(sample?.review_id);
    if (sampleID && !group.item_ids.includes(sampleID)) {
      delete enriched.sample;
    }
    return enriched;
  });
}

export function externalReviewYamlPath(id: string): string {
  const reviewsRoot = path.resolve(COMPOUND_HOME, "reviews");
  const resolved = path.resolve(reviewsRoot, EXTERNAL_AREA, `${id}.review.yaml`);
  if (!resolved.startsWith(reviewsRoot + path.sep)) {
    throw new Error("review YAML path escaped reviews root");
  }
  return resolved;
}

function rowKey(item: Json): string {
  return `${str(item.area) || str(item.review_area)}:${str(item.id) || str(item.review_id)}`;
}

export function externalReviewRows(reviews: Json | null, workbench: Json | null = null): Json[] {
  const sources = [
    ...arr(reviews?.items),
    ...arr(reviews?.actionable),
    ...arr(reviews?.review_queue?.actionable),
    ...arr(workbench?.review_actions),
    ...arr(workbench?.reviews),
    ...arr(workbench?.review_queue?.actionable),
  ];

  const merged = new Map<string, Json>();
  for (const item of sources) {
    const key = rowKey(item);
    if (!key || key === ":") continue;
    const prev = merged.get(key) ?? {};
    merged.set(key, {
      ...prev,
      ...item,
      actions: arr(prev.actions).length ? arr(prev.actions) : arr(item.actions),
      boundaries: arr(prev.boundaries).length ? arr(prev.boundaries) : arr(item.boundaries),
      evidence_refs: arr(prev.evidence_refs).length ? arr(prev.evidence_refs) : arr(item.evidence_refs),
    });
  }

  return [...merged.values()].filter((item) => {
    const area = str(item.area) || str(item.review_area);
    if (area !== EXTERNAL_AREA) return false;
    // Band-driven projections (compound reviews --json) already say whether
    // this row is still awaiting the operator (needs_you/watching) or done
    // (filed) — trust that over guessing from status/readiness/next_action
    // prose, which drifted out of sync with the live writer's wording.
    const band = str(item.band);
    if (band) return band !== "filed";
    const status = `${str(item.status)} ${str(item.readiness)} ${str(item.next_action)}`.toLowerCase();
    return status.includes("review") && status.includes("required");
  });
}

export async function loadExternalReviewRow(item: Json): Promise<ExternalReviewRow> {
  const id = str(item.id) || str(item.review_id);
  if (!privateFallbacksEnabled()) {
    return { ...item, yamlPath: "", draft: null };
  }
  const yamlPath = externalReviewYamlPath(id);
  try {
    const source = await readFile(yamlPath, "utf8");
    return { ...item, yamlPath, draft: parseReviewDraftYaml(source) };
  } catch {
    return { ...item, yamlPath, draft: null };
  }
}

export async function loadExternalReviewRows(
  reviews: Json | null,
  workbench: Json | null = null,
): Promise<ExternalReviewRow[]> {
  return Promise.all(externalReviewRows(reviews, workbench).map(loadExternalReviewRow));
}

export function externalReviewCommand(row: ExternalReviewRow): string {
  if (row.yamlPath) return `compound capability external-intake-review ${row.yamlPath}`;
  return reviewCommand(row);
}
