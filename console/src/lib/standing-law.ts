/*
 * Standing-law view mapper (ruling-harvest slice B) — the operator-facing twin
 * of what orient serves agents. Turns the `compound decisions standing-law
 * --json` projection into the model the /standing-law page and its client
 * search render.
 *
 * PURE by design (no node/server imports, only `import type`): the client
 * StandingLawBrowser and the selftest both import these shapes + the
 * buildStandingLaw/filterStandingLaw functions without pulling a runtime
 * dependency. The server loader (loadStandingLaw) lives in the page, which owns
 * the runJson call.
 */

import type { Json } from "@/lib/compound";

/* local pure accessors so this module imports NO runtime value (mirrors
 * work-trace.ts) — @/lib/compound's str/arr pull node builtins at import. */
const s = (v: unknown): string => (typeof v === "string" ? v : "");
const a = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const n = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) ? v : 0);

export type StandingLawKind = "ruling" | "grant" | "freeze" | "policy" | string;

export type SupersededRecord = {
  decisionId: string;
  claimId: string;
  kind: string;
  title: string;
  date: string;
};

export type StandingLawRecord = {
  claimId: string;
  decisionId: string;
  kind: StandingLawKind;
  title: string;
  scope: string;
  owner: string;
  reviewedBy: string;
  date: string;
  falsifier: string;
  reviewTrigger: string;
  domains: string[];
  keywords: string[];
  sourcePath: string;
  corroborationCount: number;
  corroborationEvidence: string[];
  supersedes: SupersededRecord[];
  /** Precomputed lowercased haystack for client-side search (title+scope+kind+keywords+domains+id). */
  searchText: string;
};

export type StandingLawConflict = {
  candidateId: string;
  kind: string;
  restatement: string;
  verbatim: string;
  session: string;
  transcript: string;
  line: string;
  conflictsWithClaim: string;
  conflictsWithDecision: string;
  conflictingTitle: string;
};

export type StandingLawCounts = {
  records: number;
  rulings: number;
  grants: number;
  freezes: number;
  superseded: number;
  conflicts: number;
};

export type StandingLawModel = {
  records: StandingLawRecord[];
  conflicts: StandingLawConflict[];
  counts: StandingLawCounts;
};

function mapSuperseded(raw: unknown): SupersededRecord {
  const o = (raw ?? {}) as Json;
  return {
    decisionId: s(o.decision_id),
    claimId: s(o.claim_id),
    kind: s(o.kind),
    title: s(o.title),
    date: s(o.date),
  };
}

function mapRecord(raw: unknown): StandingLawRecord {
  const o = (raw ?? {}) as Json;
  const domains = a(o.domains).map(s).filter(Boolean);
  const keywords = a(o.keywords).map(s).filter(Boolean);
  const scope = s(o.scope);
  const kind = s(o.kind);
  const title = s(o.title);
  const decisionId = s(o.decision_id);
  const searchText = [title, scope, kind, decisionId, ...keywords, ...domains]
    .join(" ")
    .toLowerCase();
  return {
    claimId: s(o.claim_id),
    decisionId,
    kind,
    title,
    scope,
    owner: s(o.owner),
    reviewedBy: s(o.reviewed_by),
    date: s(o.date),
    falsifier: s(o.falsifier),
    reviewTrigger: s(o.review_trigger),
    domains,
    keywords,
    sourcePath: s(o.source_path),
    corroborationCount: n(o.corroboration_count),
    corroborationEvidence: a(o.corroboration_evidence).map(s).filter(Boolean),
    supersedes: a(o.supersedes).map(mapSuperseded),
    searchText,
  };
}

function mapConflict(raw: unknown): StandingLawConflict {
  const o = (raw ?? {}) as Json;
  return {
    candidateId: s(o.candidate_id),
    kind: s(o.kind),
    restatement: s(o.restatement),
    verbatim: s(o.verbatim),
    session: s(o.session),
    transcript: s(o.transcript),
    line: s(o.line),
    conflictsWithClaim: s(o.conflicts_with_claim),
    conflictsWithDecision: s(o.conflicts_with_decision),
    conflictingTitle: s(o.conflicting_title),
  };
}

/** Build the render model from the standing-law JSON projection. Defensive:
 *  a null/short payload yields an empty (never-crashing) model. */
export function buildStandingLaw(data: Json | null): StandingLawModel {
  const records = a(data?.records)
    .map(mapRecord)
    // Newest-first (the CLI already sorts; re-assert so the view never trusts
    // an unsorted upstream). ValidFrom is the record date; ties break on id.
    .sort((x, y) => (x.date !== y.date ? (x.date > y.date ? -1 : 1) : x.claimId > y.claimId ? -1 : 1));
  const conflicts = a(data?.conflicts).map(mapConflict);
  const countsRaw = (data?.counts ?? {}) as Json;
  const counts: StandingLawCounts = {
    records: n(countsRaw.records) || records.length,
    rulings: n(countsRaw.rulings),
    grants: n(countsRaw.grants),
    freezes: n(countsRaw.freezes),
    superseded: n(countsRaw.superseded),
    conflicts: n(countsRaw.conflicts) || conflicts.length,
  };
  return { records, conflicts, counts };
}

/** Client-side filter over title+scope+keywords (the <4-interaction journey:
 *  land → type → see the ruling). Empty query returns all records unchanged. */
export function filterStandingLaw(records: StandingLawRecord[], query: string): StandingLawRecord[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return records;
  // Every whitespace-separated term must match (AND) so "opus execution" narrows.
  const terms = needle.split(/\s+/).filter(Boolean);
  return records.filter((r) => terms.every((t) => r.searchText.includes(t)));
}
