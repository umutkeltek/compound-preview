import { readFile, readdir } from "node:fs/promises";
import path from "node:path";

import { arr, COMPOUND_HOME, num, privateFallbacksEnabled, runJson, str, type Json } from "@/lib/compound";

/*
 * Real projections the System Map hydrates from. Every number on the map comes
 * from here — never hardcoded. Mirrors the memory-data.ts pattern for direct
 * JSONL reads; uses runJson for compound verbs.
 */

async function countLines(file: string): Promise<number> {
  if (!privateFallbacksEnabled()) return 0;

  try {
    const source = await readFile(file, "utf8");
    return source.split(/\r?\n/).filter((l) => l.trim().length > 0).length;
  } catch {
    return 0;
  }
}

export async function getCapabilityAuthority(): Promise<Json | null> {
  return runJson(["capability", "authority", "--json"]);
}

export async function getProductAuthority(): Promise<Json | null> {
  return runJson(["product", "authority", "--json"]);
}

export async function getRouteAuthority(): Promise<Json | null> {
  return runJson(["route", "evidence-authority", "--json"]);
}

export type LedgerStats = {
  entities: number;
  claims: number;
  edges: number;
  evidence: number;
  routeEvidence: number;
  noPromote: number;
  knowledgeImports: number;
};

/**
 * Fetches `system stats --json` - the ledger/knowledge/intake fallback
 * readers below all derive from this ONE projection. Callers with several of
 * these needed at once (see loadTopology) should fetch it once and pass it
 * in, rather than let each reader call this independently.
 */
export async function getSystemStatsProjection(): Promise<Json | null> {
  return runJson(["system", "stats", "--json"]);
}

export async function getLedgerStats(stats?: Json | null): Promise<LedgerStats> {
  const projected = stats === undefined ? await getSystemStatsProjection() : stats;
  if (projected) {
    return {
      entities: num(projected.entities),
      claims: num(projected.claims),
      edges: num(projected.edges),
      evidence: num(projected.evidence),
      routeEvidence: num(projected.route_evidence),
      noPromote: num(projected.no_promote),
      knowledgeImports: num(projected.knowledge_imports),
    };
  }

  const ledger = path.resolve(COMPOUND_HOME, "ledger");
  const [
    entities,
    claims,
    edges,
    evidence,
    routeEvidence,
    noPromote,
    knowledgeImports,
  ] = await Promise.all([
    countLines(path.join(ledger, "entities.jsonl")),
    countLines(path.join(ledger, "claims.jsonl")),
    countLines(path.join(ledger, "edges.jsonl")),
    countLines(path.join(ledger, "evidence.jsonl")),
    countLines(path.join(ledger, "execution-route-evidence.jsonl")),
    countLines(path.join(ledger, "no-promote-decisions.jsonl")),
    countLines(path.join(ledger, "knowledge-imports.jsonl")),
  ]);
  return { entities, claims, edges, evidence, routeEvidence, noPromote, knowledgeImports };
}

export async function getKnowledgeSourceCount(stats?: Json | null): Promise<number> {
  const projected = stats === undefined ? await getSystemStatsProjection() : stats;
  if (projected) return num(projected.knowledge_sources);

  return countLines(path.resolve(COMPOUND_HOME, "registries", "knowledge-sources.jsonl"));
}

/** Names of the active capture sources (for the capture node's evidence list). */
export async function getKnowledgeSourceKinds(stats?: Json | null): Promise<string[]> {
  const projected = stats === undefined ? await getSystemStatsProjection() : stats;
  if (projected) return arr(projected.knowledge_source_kinds).map(str).filter(Boolean);

  if (!privateFallbacksEnabled()) return [];

	try {
    const source = await readFile(
      path.resolve(COMPOUND_HOME, "registries", "knowledge-sources.jsonl"),
      "utf8",
    );
    const kinds = new Set<string>();
    for (const line of source.split(/\r?\n/)) {
      if (!line.trim()) continue;
      try {
        const row = JSON.parse(line) as Json;
        const k = str(row.source_kind) || str(row.kind);
        if (k) kinds.add(k);
      } catch {
        /* skip */
      }
    }
    return [...kinds];
  } catch {
    return [];
  }
}

export async function getIntakeRepoCount(stats?: Json | null): Promise<number> {
  const projected = stats === undefined ? await getSystemStatsProjection() : stats;
  if (projected) return num(projected.intake_repos);

  if (!privateFallbacksEnabled()) return 0;

	try {
    const dir = path.resolve(COMPOUND_HOME, "inbox", "external-repo-intake");
    const entries = await readdir(dir, { withFileTypes: true });
    return entries.filter((e) => e.isDirectory()).length;
  } catch {
    return 0;
  }
}

export { num };
