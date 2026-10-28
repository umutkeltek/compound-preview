import { readFile } from "node:fs/promises";
import path from "node:path";

import { COMPOUND_HOME, getWork, privateFallbacksEnabled, runJson, type Json } from "@/lib/compound";

/*
 * Lineage trace — for any id (claim / candidate / evidence / work / entity),
 * reconstruct its path through the spine: source → evidence → candidate →
 * adjudication → claim → outward. The CLI projection owns the runtime schema;
 * private ledger/candidate reads are only the opt-in fallback. Honest: if a
 * step can't be found, it's omitted — never fabricated.
 */

export type LineageStep = {
  kind: "source" | "evidence" | "candidate" | "adjudication" | "claim" | "edge" | "work" | "receipt";
  label: string;
  detail?: string;
  timestamp?: string;
  refs: string[];
};

export type Lineage = {
  queryId: string;
  resolvedAs: string;
  found: boolean;
  steps: LineageStep[];
};

const LEDGER = path.resolve(COMPOUND_HOME, "ledger");
const CANDIDATES = path.resolve(COMPOUND_HOME, "inbox", "memory-candidates");

/** Canonical candidate-id grammar compound itself mints: `cand_` + lowercase
 * hex, 12 chars (internal/mem idFor) or 16 (internal/knowledge shortDigest).
 * No separators, no dot segments, no encoded forms — any other shape is
 * untrusted route input, never a minted id, and must not build a path. */
const CANDIDATE_ID = /^cand_(?:[0-9a-f]{12}|[0-9a-f]{16})$/;

async function readJsonl(file: string): Promise<Json[]> {
  try {
    const source = await readFile(file, "utf8");
    const out: Json[] = [];
    for (const line of source.split(/\r?\n/)) {
      if (!line.trim()) continue;
      try {
        out.push(JSON.parse(line) as Json);
      } catch {
        /* skip malformed */
      }
    }
    return out;
  } catch {
    return [];
  }
}

const str = (v: unknown): string => (typeof v === "string" ? v : "");

async function readCandidate(id: string): Promise<Json | null> {
  // The id arrives as a decoded URL segment; it never constructs a path unless
  // it matches the minted grammar. Traversal, separators, and encoded or
  // alternate forms fail soft as not-found with no filesystem read.
  if (!CANDIDATE_ID.test(id)) return null;
  const file = path.resolve(CANDIDATES, `${id}.json`);
  // Containment invariant stated independently of the grammar above: a
  // candidate read is always a DIRECT CHILD of the candidate directory, so a
  // later grammar change cannot silently reopen the traversal door.
  if (path.dirname(file) !== CANDIDATES) return null;
  try {
    return JSON.parse(await readFile(file, "utf8")) as Json;
  } catch {
    return null;
  }
}

function findClaimByEntity(claims: Json[], entity: string): Json | null {
  return claims.find((c) => str(c.entity) === entity) ?? null;
}

export async function traceLineage(queryId: string): Promise<Lineage> {
  const id = queryId.trim();
  const steps: LineageStep[] = [];
  let resolvedAs = id;

  const projected = await runJson(["system", "lineage", id, "--json"]);
  if (projected) return projected as unknown as Lineage;

  if (!privateFallbacksEnabled()) {
    if (id.startsWith("work_")) {
      resolvedAs = `work ${id}`;
      const work = await getWork(id);
      if (!work) return { queryId: id, resolvedAs, found: false, steps };
      steps.push({
        kind: "work",
        label: id,
        detail: str(work.desired_outcome) || str(work.success_predicate),
        timestamp: str(work.created_at),
        refs: [id],
      });
      return { queryId: id, resolvedAs, found: true, steps };
    }
    return { queryId: id, resolvedAs: "private fallback disabled", found: false, steps };
  }

  const [claims, evidence, adjudications] = await Promise.all([
    readJsonl(path.join(LEDGER, "claims.jsonl")),
    readJsonl(path.join(LEDGER, "evidence.jsonl")),
    readJsonl(path.join(LEDGER, "adjudications.jsonl")),
  ]);

  const evidenceFor = (evId: string): Json | null =>
    evidence.find((e) => str(e.id) === evId) ?? null;

  const pushEvidenceChain = (evId: string) => {
    const ev = evidenceFor(evId);
    if (!ev) return;
    steps.push({
      kind: "evidence",
      label: str(ev.id),
      detail: str(ev.quote_text).slice(0, 180) || `${str(ev.source_kind)} evidence`,
      timestamp: str(ev.observed_at),
      refs: [str(ev.id)],
    });
    const sk = str(ev.source_kind);
    const sref = str(ev.source_id) || str(ev.source_ref);
    if (sk || sref) {
      steps.push({
        kind: "source",
        label: sk || "source",
        detail: sref,
        timestamp: str(ev.observed_at),
        refs: sref ? [sref] : [],
      });
    }
  };

  const pushClaim = (claim: Json) => {
    const pred = str(claim.predicate);
    const obj = str(claim.object).slice(0, 180);
    steps.push({
      kind: "claim",
      label: str(claim.id),
      detail: pred ? `${pred}: ${obj}` : obj,
      timestamp: str(claim.valid_from) || str(claim.created_at),
      refs: [str(claim.id)],
    });
    for (const ref of Array.isArray(claim.evidence_refs) ? claim.evidence_refs : []) {
      pushEvidenceChain(String(ref));
    }
  };

  // 1) candidate → evidence + adjudication + (if promoted) claim
  if (id.startsWith("cand_")) {
    resolvedAs = `candidate ${id}`;
    const cand = await readCandidate(id);
    if (!cand) return { queryId: id, resolvedAs, found: false, steps };
    steps.push({
      kind: "candidate",
      label: id,
      detail: `${str(cand.entity)} — ${str(cand.predicate)}: ${str(cand.object).slice(0, 140)}`,
      timestamp: str(cand.created_at),
      refs: [id],
    });
    const trace = str(cand.source_trace_ref);
    if (trace) pushEvidenceChain(trace);
    const adj = adjudications.find((a) => str(a.candidate_id) === id);
    if (adj) {
      steps.push({
        kind: "adjudication",
        label: str(adj.verdict) || "adjudicated",
        detail: str(adj.basis) ? `basis: ${str(adj.basis)}` : undefined,
        timestamp: str(adj.created_at),
        refs: [id],
      });
      if (str(adj.verdict) === "promote") {
        const promoted = findClaimByEntity(claims, str(cand.entity));
        if (promoted) pushClaim(promoted);
      }
    }
    return { queryId: id, resolvedAs, found: true, steps };
  }

  // 2) evidence → claims that cite it
  if (id.startsWith("ev_")) {
    resolvedAs = `evidence ${id}`;
    const ev = evidenceFor(id);
    if (!ev) return { queryId: id, resolvedAs, found: false, steps };
    pushEvidenceChain(id);
    const citing = claims.filter((c) => Array.isArray(c.evidence_refs) && c.evidence_refs.includes(id));
    for (const c of citing.slice(0, 5)) pushClaim(c);
    return { queryId: id, resolvedAs, found: true, steps };
  }

  // 3) work → receipt + transition
  if (id.startsWith("work_")) {
    resolvedAs = `work ${id}`;
    const work = await getWork(id);
    if (!work) return { queryId: id, resolvedAs, found: false, steps };
    steps.push({
      kind: "work",
      label: id,
      detail: str(work.desired_outcome) || str(work.success_predicate),
      timestamp: str(work.created_at),
      refs: [id],
    });
    const rid = str(work.receipt_id) || str(work.ids?.receipt);
    const tid = str(work.transition_id) || str(work.ids?.transition);
    if (rid || tid) {
      steps.push({
        kind: "receipt",
        label: [rid && `receipt ${rid}`, tid && `transition ${tid}`].filter(Boolean).join(" · "),
        refs: [rid, tid].filter(Boolean) as string[],
      });
    }
    return { queryId: id, resolvedAs, found: true, steps };
  }

  // 4) a claim id directly
  const directClaim = claims.find((c) => str(c.id) === id);
  if (directClaim) {
    resolvedAs = `claim ${id}`;
    pushClaim(directClaim);
    return { queryId: id, resolvedAs, found: true, steps };
  }

  // 5) an entity token → its claims
  const entityClaims = claims.filter((c) => str(c.entity) === id);
  if (entityClaims.length) {
    resolvedAs = `entity ${id}`;
    for (const c of entityClaims.slice(0, 6)) pushClaim(c);
    return { queryId: id, resolvedAs, found: true, steps };
  }

  return { queryId: id, resolvedAs: `unknown ${id}`, found: false, steps };
}
