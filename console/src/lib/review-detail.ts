import { readFile } from "node:fs/promises";
import { resolve, sep } from "node:path";

import { arr, COMPOUND_HOME, privateFallbacksEnabled, str, type Json } from "./compound";

/**
 * The reviews projection carries pointers (json_path) — the human content
 * (statement, detail, confidence, warnings) lives in the artifact file under
 * COMPOUND_HOME. Read it server-side, defensively, read-only — and ONLY when
 * the operator opted into private runtime reads (privateFallbacksEnabled);
 * a public/demo deployment renders from projection fields alone.
 */
export async function readReviewBody(jsonPath: string): Promise<Json | null> {
  if (!privateFallbacksEnabled()) return null;
  const path = resolve(str(jsonPath));
  const home = resolve(COMPOUND_HOME);
  if (!path.startsWith(home + sep)) return null; // never read outside the data home
  try {
    const parsed = JSON.parse(await readFile(path, "utf8"));
    return typeof parsed === "object" && parsed !== null ? (parsed as Json) : null;
  } catch {
    return null;
  }
}

/** Best plain headline for a review artifact, whatever its schema. */
export function reviewHeadline(row: Json, body: Json | null): string {
  const proposal = (body?.proposal ?? {}) as Json;
  return (
    str(body?.statement) ||
    str(body?.subject) ||
    str(body?.title) ||
    str(body?.summary) ||
    str(proposal.seed_text) ||
    str(row.next_action) ||
    str(row.id)
  );
}

/** Best supporting body text. */
export function reviewBodyText(row: Json, body: Json | null): string {
  const headline = reviewHeadline(row, body);
  const candidates = [
    str(body?.detail),
    str(body?.description),
    str((body?.draft ?? {}).proposed_change),
    str(body?.next_action),
    str(row.next_action),
  ].filter((t) => t && t !== headline);
  return candidates[0] ?? "";
}

export type ReviewCardData = {
  id: string;
  area: string;
  headline: string;
  body: string;
  confidence: string;
  warnings: string[];
  evidenceCount: number;
  createdAt: string;
  readiness: string;
  nextAction: string;
};

export async function loadReviewCard(row: Json): Promise<ReviewCardData> {
  const body = str(row.json_path) ? await readReviewBody(str(row.json_path)) : null;
  const confidence =
    body?.confidence != null && body?.confidence !== "" ? String(body.confidence) : "";
  return {
    id: str(row.id),
    area: str(row.area) || str(row.review_area),
    headline: reviewHeadline(row, body),
    body: reviewBodyText(row, body),
    confidence,
    warnings: arr(body?.warnings).map(String).slice(0, 4),
    evidenceCount: arr(row.evidence_refs).length || arr(body?.evidence_refs).length,
    createdAt: str(row.created_at) || str(body?.created_at),
    readiness: str(row.readiness) || str(row.status) || str(body?.status),
    nextAction: str(row.next_action) || str(body?.next_action),
  };
}
