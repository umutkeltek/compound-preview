import { arr, str, type Json } from "@/lib/compound";
import type { MemoryCandidate } from "@/lib/memory-data";

/*
 * Pure presentation helper for source-evidence cards. No fetching, scoring,
 * or governance — the page that owns the queue owns those. Rows use the real
 * MemoryCandidate wire shape: the object text leads, provenance stays
 * separate from record identity, and every raw value stays reachable in
 * labeled technical details.
 */

export type EvidenceKindLabel =
  | "promoted source evidence"
  | "parked source evidence"
  | "rejected source evidence"
  | "source evidence";

export type EvidenceTechnicalDetail = { label: string; value: string };

export type EvidenceCardObservation = {
  /** Lead text — the object's observation, or an honest gap line. */
  observation: string;
  /** Human label for the row's recorded state; "source evidence" for unknown. */
  kindLabel: EvidenceKindLabel;
  /** Human labels for known source kinds; "Source evidence" for unknown. */
  sourceLabels: string[];
  /** Optional metadata_field pill. Null if absent. */
  metadataField: string | null;
  /** Candidate id, exposed for drill-down. */
  id: string;
  /** Optional observation_date in the original wire shape. */
  observedAt: string | null;
  /** Provenance refs only — source trace, evidence refs, source refs. */
  refs: string[];
  /** True when refs is empty; the card must explain the gap, not hide it. */
  provenanceMissing: boolean;
  /** Record identity and raw qualifiers — technical details, not evidence. */
  technicalDetails: EvidenceTechnicalDetail[];
  /** Evidence-not-fact notice plus the recorded status. Never a decision claim. */
  note: string;
};

const OBSERVATION_UNAVAILABLE = "Observation text is unavailable for this row.";
const PROVENANCE_GAP = "Source link unavailable for this row — no provenance refs recorded.";

export const evidenceProvenanceGapNotice = PROVENANCE_GAP;

function buildNote(state: string): string {
  const s = str(state).trim();
  const recorded = s ? `Recorded status: ${s}.` : "No recorded status on this row.";
  return `Source record, not verified fact. ${recorded}`;
}

function buildKindLabel(state: string): EvidenceKindLabel {
  const s = str(state).toLowerCase();
  if (s === "promoted") return "promoted source evidence";
  if (s === "parked") return "parked source evidence";
  if (s === "rejected" || s === "rejected_duplicate") return "rejected source evidence";
  return "source evidence";
}

/* Known kinds get a human label; anything else is honestly "Source evidence".
 * The raw value always survives in technicalDetails. */
const sourceKindLabels: Record<string, string> = {
  knowledge_import: "knowledge import",
  knowledge_extraction: "knowledge extraction",
  knowledge: "knowledge corpus",
  scenario: "scenario",
};

function humanSourceLabel(raw: string): string {
  const s = str(raw).trim();
  return sourceKindLabels[s] ?? "Source evidence";
}

function buildSourceLabels(q: Json, candidate: MemoryCandidate): string[] {
  const labels: string[] = [];
  const seen = new Set<string>();
  for (const slot of [str(q.source), str(candidate.source), str(q.source_kind), str(candidate.source_kind)]) {
    const s = slot.trim();
    if (!s) continue;
    const human = humanSourceLabel(s);
    if (!seen.has(human)) {
      seen.add(human);
      labels.push(human);
    }
    if (labels.length >= 3) break;
  }
  return labels;
}

/* The object is the observation. Nothing else may stand in for it — an item
 * ref, entity, or id is a pointer, not a human observation. */
function buildObservationText(candidate: MemoryCandidate): string {
  const obj = str(candidate.object).trim();
  return obj || OBSERVATION_UNAVAILABLE;
}

function buildMetadataField(candidate: MemoryCandidate): string | null {
  return str(objectValue(candidate.qualifiers).metadata_field).trim() || null;
}

function buildObservedAt(candidate: MemoryCandidate): string | null {
  return str(candidate.observation_date).trim() || null;
}

/* Provenance only: the source trace, evidence refs, and source-side refs the
 * row actually carries. The record id is identity, not provenance — a row
 * with an id but no source links must still surface as a gap. */
function buildRefs(candidate: MemoryCandidate): string[] {
  const q = objectValue(candidate.qualifiers);
  return unique([
    str(candidate.source_trace_ref),
    str(q.item_ref),
    str(q.extraction_id),
    str(q.source_ref),
    str(q.canonical),
    ...arr(candidate.evidence_refs).map(String),
  ]);
}

function buildTechnicalDetails(candidate: MemoryCandidate, q: Json): EvidenceTechnicalDetail[] {
  const details: EvidenceTechnicalDetail[] = [];
  const add = (label: string, value: unknown) => {
    const v = str(value);
    if (v) details.push({ label, value: v });
  };
  add("id", candidate.id);
  add("entity", candidate.entity);
  add("predicate", candidate.predicate);
  add("object", candidate.object);
  add("source", q.source);
  add("source_kind", q.source_kind);
  add("raw record", JSON.stringify(candidate, null, 2));
  return details;
}

function objectValue(value: unknown): Json {
  return value && typeof value === "object" && !Array.isArray(value) ? (value as Json) : {};
}

function unique(values: string[]): string[] {
  return [...new Set(values.filter(Boolean))];
}

export function buildEvidenceCardObservation(candidate: MemoryCandidate): EvidenceCardObservation {
  const q = objectValue(candidate.qualifiers);
  const refs = buildRefs(candidate);
  return {
    observation: buildObservationText(candidate),
    kindLabel: buildKindLabel(str(candidate.state)),
    sourceLabels: buildSourceLabels(q, candidate),
    metadataField: buildMetadataField(candidate),
    id: str(candidate.id) || "(no id)",
    observedAt: buildObservedAt(candidate),
    refs,
    provenanceMissing: refs.length === 0,
    technicalDetails: buildTechnicalDetails(candidate, q),
    note: buildNote(str(candidate.state)),
  };
}
