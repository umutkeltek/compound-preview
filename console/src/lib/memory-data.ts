import { readdir, readFile } from "node:fs/promises";
import path from "node:path";

import { arr, COMPOUND_HOME, HEAVY_PROJECTION_TIMEOUT_MS, privateFallbacksEnabled, runProjection, type Json, type ProjectionRead, str } from "@/lib/compound";

const MEMORY_INBOX = path.resolve(COMPOUND_HOME, "inbox", "memory-candidates");
const KNOWLEDGE_EXTRACTION_INBOX = path.resolve(COMPOUND_HOME, "inbox", "knowledge-extractions");

export type MemoryCandidate = Json & {
  id: string;
  json_path?: string;
  console_source?: Json;
  /**
   * READ-ONLY DERIVED signal projected by `compound memory candidates --json`.
   * Distinct evidence refs across candidates citing the same normalized
   * claim (entity|predicate|object, lowercased/trimmed). A lone candidate
   * projects 1. The standing-law-candidate chip surfaces this as the
   * "seen Nx" cue when the candidate qualifies — it is a readiness
   * signal, not a promotion criterion (promotion stays the governed
   * memory-adjudicate act).
   */
  recurrence_count?: number;
  /**
   * READ-ONLY DERIVED trend label: "new" | "rising" | "flat" | "falling"
   * against the max confidence of any earlier observation for the same
   * normalized claim. Same trust model as recurrence_count.
   */
  confidence_trend?: string;
};

function asObject(value: unknown): Json {
  return value && typeof value === "object" && !Array.isArray(value) ? (value as Json) : {};
}

function evidenceRef(value: unknown): string {
  const s = str(value).trim();
  return /^ev_[a-z0-9]+$/.test(s) ? s : "";
}

function candidateWithId(item: Json): MemoryCandidate | null {
  const id = str(item.id);
  return id ? ({ ...item, id } as MemoryCandidate) : null;
}

export function isKnowledgeClassCandidate(candidate: Json): boolean {
  const qualifiers = asObject(candidate.qualifiers);
  const entity = str(candidate.entity).trim();
  const source = str(qualifiers.source).trim();
  return (
    entity.startsWith("knowledge:") ||
    source === "knowledge_import" ||
    source === "knowledge_extraction"
  );
}

export function isActionableMemoryCandidate(candidate: Json): boolean {
  return str(candidate.state) === "proposed" && !isKnowledgeClassCandidate(candidate);
}

/**
 * A parked STANDING-LAW candidate: an accepted decision (mined from brain pages
 * or the decision ledger) that is sitting parked, waiting for the operator to
 * rule "make it law". The exact predicate the operator complaint names:
 *   state == "parked" AND (claim_type == "decision" OR entity starts "decision:")
 * This is the authority filter — it also honors the entity-prefix branch, so a
 * decision-shaped row whose claim_type drifted still enters the queue. Knowledge
 * corpus rows (x_likes, imports) are parked in the tens of thousands and MUST
 * NOT flood in: they are neither decision-typed nor decision:-entity.
 *
 * A candidate the operator has already ruled on (keep-aside-drain lane,
 * work_4033b98c5837) is EXCLUDED even though it stays parked: `compound memory
 * adjudicate` (the CLI door the console's "Keep aside" card calls) stamps
 * `adjudicated_by` on the resulting candidate — see mem.Candidate.AdjudicatedBy.
 * Its presence means a human already looked at this exact card and chose "keep
 * as evidence, not yet ambient" — re-showing it as a needs-your-call queue item
 * forever would re-litigate a decision already made. A raw harvested park (never
 * touched by the ratification door) carries no `adjudicated_by` and still renders.
 */
export function isStandingLawCandidate(candidate: Json): boolean {
  if (str(candidate.state) !== "parked") return false;
  if (str(candidate.adjudicated_by).trim()) return false;
  const entity = str(candidate.entity).trim();
  return str(candidate.claim_type).trim() === "decision" || entity.startsWith("decision:");
}

async function consoleSourceFor(candidate: Json): Promise<Json | null> {
  const qualifiers = asObject(candidate.qualifiers);
  const traceRef = str(candidate.source_trace_ref);
  let extractionId = str(qualifiers.extraction_id);
  let evidence = evidenceRef(traceRef) || evidenceRef(qualifiers.evidence_ref);

  if (!extractionId && traceRef.startsWith("knowledge_extraction:")) {
    extractionId = traceRef.slice("knowledge_extraction:".length);
  }

  const out: Json = {};
  if (!privateFallbacksEnabled()) {
    if (evidence) out.evidence_ref = evidence;
    return Object.keys(out).length ? out : null;
  }

  if (extractionId && /^kx_[a-z0-9]+$/i.test(extractionId)) {
    const extractionPath = path.resolve(KNOWLEDGE_EXTRACTION_INBOX, `${extractionId}.json`);
    out.extraction_id = extractionId;
    out.extraction_path = extractionPath;
    try {
      const extraction = JSON.parse(await readFile(extractionPath, "utf8")) as Json;
      evidence = evidence || evidenceRef(extraction.evidence_ref);
      if (str(extraction.item_ref)) out.item_ref = str(extraction.item_ref);
      if (str(extraction.title)) out.title = str(extraction.title);
      if (str(extraction.evidence_ref)) out.evidence_ref = str(extraction.evidence_ref);
    } catch {
      /* details panel can still show the candidate without extraction source */
    }
  }

  if (evidence) out.evidence_ref = evidence;
  return Object.keys(out).length ? out : null;
}

/*
 * Perf fix (measured 2026-07-08): `memory candidates --json --include-evidence`
 * does a per-candidate evidence-ledger lookup. Over the full unfiltered candidate
 * set (~1,100 rows locally) that took ~50s wall clock — well past the 15s
 * execRunJson timeout, so every /memory page load hung and failed. The plain
 * `memory candidates --json` call (no --include-evidence) returns the same
 * items/counts/generated_at shape without those extra lookups; it only omits
 * `source_trace_ref`, `evidence_refs`, the nested `evidence` quote object,
 * `annotations`, and `believed_before` per item. No page-load render path
 * reads those fields (the memory-adjudicate DecisionCard/Dialog fall back to
 * `qualifiers.*`, which is always present), so they are dropped entirely from
 * this loader rather than fetched-then-ignored. If a future component needs
 * per-item evidence/annotations, fetch it on demand for that one item rather
 * than reintroducing this flag on the list loader.
 */
export type MemoryCandidateRead = { candidates: MemoryCandidate[]; read: ProjectionRead };

async function loadFromProjection(filterState?: string): Promise<MemoryCandidateRead> {
  const args = ["memory", "candidates", "--json"];
  if (filterState) args.push("--state", filterState);

  const read = await runProjection(args, { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS });

  const items = arr(read.value?.items)
    .map((item) => candidateWithId(asObject(item)))
    .filter((item): item is MemoryCandidate => Boolean(item));
  return { candidates: items, read };
}

async function loadFromInbox(filterState?: string): Promise<MemoryCandidate[]> {
  if (!privateFallbacksEnabled()) return [];

  let entries: string[];
  try {
    entries = await readdir(MEMORY_INBOX);
  } catch {
    return [];
  }

  const out: MemoryCandidate[] = [];
  for (const entry of entries) {
    if (!entry.endsWith(".json")) continue;
    const candidatePath = path.resolve(MEMORY_INBOX, entry);
    if (!candidatePath.startsWith(MEMORY_INBOX + path.sep)) continue;

    try {
      const data = JSON.parse(await readFile(candidatePath, "utf8")) as Json;
      const id = str(data.id) || entry.replace(/\.json$/, "");
      if (filterState && str(data.state) !== filterState) continue;

      const candidate: MemoryCandidate = { ...data, id, json_path: candidatePath };
      const consoleSource = await consoleSourceFor(candidate);
      if (consoleSource) candidate.console_source = consoleSource;
      out.push(candidate);
    } catch {
      /* skip unreadable candidate files */
    }
  }

  return out;
}

export async function loadMemoryCandidatesRead(filterState?: string): Promise<MemoryCandidateRead> {
  const projected = await loadFromProjection(filterState);
  const out = projected.read.value ? projected.candidates : (await loadFromInbox(filterState));
  out.sort((a, b) => str(b.created_at).localeCompare(str(a.created_at)));
  return { candidates: out, read: projected.read };
}

export async function loadMemoryCandidates(filterState?: string): Promise<MemoryCandidate[]> {
  return (await loadMemoryCandidatesRead(filterState)).candidates;
}

export async function loadActionableMemoryCandidates(): Promise<MemoryCandidate[]> {
  const proposed = await loadMemoryCandidates("proposed");
  return proposed.filter(isActionableMemoryCandidate);
}

export async function loadSourceEvidenceCandidates(state = "proposed"): Promise<MemoryCandidate[]> {
  const candidates = await loadMemoryCandidates(state);
  return candidates.filter(isKnowledgeClassCandidate);
}

/**
 * The /memory page needs both the actionable split and the source-evidence
 * split of the SAME "proposed" set. Calling loadActionableMemoryCandidates()
 * and loadSourceEvidenceCandidates("proposed") separately execs
 * `memory candidates --state proposed` twice; this fetches it once and
 * filters in memory for both.
 */
export async function loadProposedMemorySplit(): Promise<{
  actionable: MemoryCandidate[];
  sourceEvidence: MemoryCandidate[];
  read: ProjectionRead;
}> {
  const { candidates: proposed, read } = await loadMemoryCandidatesRead("proposed");
  return {
    actionable: proposed.filter(isActionableMemoryCandidate),
    sourceEvidence: proposed.filter(isKnowledgeClassCandidate),
    read,
  };
}

/**
 * Parked standing-law candidates for the durable adjudication queue.
 *
 * Server-prefiltered with `--kind decision` (mem CandidateListOptions keys it on
 * ClaimType) so the loader never pulls the full parked set — that set is ~28k
 * rows / ~24MB locally (mostly knowledge corpus) and blows the exec timeout to
 * find ~18. The authoritative filter is still isStandingLawCandidate applied
 * here (it also admits the entity:decision: prefix branch). On the real home the
 * prefilter returns exactly the decision-typed parked rows, so 18 in ⇒ 18 out.
 */
export async function loadStandingLawCandidatesRead(): Promise<MemoryCandidateRead> {
  const read = await runProjection([
    "memory", "candidates", "--json", "--state", "parked", "--kind", "decision",
  ], { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS });
  const rows = read.value
    ? arr(read.value.items)
        .map((item) => candidateWithId(asObject(item)))
        .filter((item): item is MemoryCandidate => Boolean(item))
    : [];
  const out = rows.filter(isStandingLawCandidate);
  out.sort((a, b) => str(b.created_at).localeCompare(str(a.created_at)));
  return { candidates: out, read };
}

export async function loadStandingLawCandidates(): Promise<MemoryCandidate[]> {
  return (await loadStandingLawCandidatesRead()).candidates;
}

export async function loadMemoryCountsRead(): Promise<{ counts: Record<string, number>; read: ProjectionRead }> {
  const { candidates: all, read } = await loadMemoryCandidatesRead();
  const counts: Record<string, number> = {};
  for (const c of all) {
    const s = str(c.state) || "unknown";
    counts[s] = (counts[s] ?? 0) + 1;
  }
  return { counts, read };
}

export async function loadMemoryCounts(): Promise<Record<string, number>> {
  return (await loadMemoryCountsRead()).counts;
}

/**
 * Readiness chip — derived-only signal projected by `compound memory
 * candidates --json`. The chip renders on the standing-law-candidate card
 * when the candidate is a parked decision candidate with at least two
 * distinct evidence refs (recurrence_count >= 2) AND a confidence >= 0.8.
 *
 * The threshold is the packet's specificity: recurrence alone does not
 * qualify (one operator repeatedly re-stating the same rule does not
 * make it ready); high confidence alone does not qualify (a confident
 * single observation may still need corroboration). Both must hold.
 *
 * Returns null when the candidate does not qualify — callers render
 * nothing rather than a "not ready" chip (a negative chip would compete
 * with the existing chrome and add no operator value).
 */
export function standingLawReadinessChip(candidate: Json): { label: string; tone: "seam" | "good" | "muted" } | null {
  const recurrence = numberOrNull(candidate.recurrence_count);
  const confidence = numberOrZero(candidate.confidence);
  const trend = str(candidate.confidence_trend).trim();
  if (recurrence === null || recurrence < 2) return null;
  if (confidence < 0.8) return null;
  const trendLabel = trend === "rising" ? "rising" : trend === "flat" ? "flat" : trend === "falling" ? "falling" : "new";
  // Tone: "rising" is the operator-cue (memory is gaining corroboration
  // momentum); "flat" is informational (already-seen, no momentum);
  // "new"/"falling" is muted (the chip still surfaces recurrence, but
  // the trend is not a positive signal).
  const tone: "seam" | "good" | "muted" =
    trendLabel === "rising" ? "good" : trendLabel === "flat" ? "seam" : "muted";
  return { label: `seen ${recurrence}× · ${trendLabel}`, tone };
}

function numberOrNull(value: unknown): number | null {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  return null;
}

function numberOrZero(value: unknown): number {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  return 0;
}
