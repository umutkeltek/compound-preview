import { readFile } from "node:fs/promises";
import path from "node:path";

import { COMPOUND_HOME, privateFallbacksEnabled, runJson, type Json } from "@/lib/compound";

/*
 * The dense web: entities hold claims; claims connect to claims via typed edges
 * (supports / contradicts / supersedes / depends_on / part_of). This loads an
 * ego-graph around a seed (entity slug or claim id) — seed's claims in the
 * inner ring, their edge-neighbors in the outer ring. The CLI projection owns
 * the runtime schema; private ledger reads are only the opt-in fallback.
 */

export type MemoryNode = {
  id: string;
  entity: string;
  predicate: string;
  object: string;
  confidence?: number;
  position: { x: number; y: number };
};

export type MemoryLink = {
  id: string;
  source: string;
  target: string;
  kind: string;
};

export type EgoGraph = {
  seed: string;
  resolvedAs: string;
  nodes: MemoryNode[];
  links: MemoryLink[];
  truncated: boolean;
  found: boolean;
};

const LEDGER = path.resolve(COMPOUND_HOME, "ledger");
const str = (v: unknown): string => (typeof v === "string" ? v : "");

async function readJsonl(file: string): Promise<Json[]> {
  try {
    const source = await readFile(file, "utf8");
    const out: Json[] = [];
    for (const line of source.split(/\r?\n/)) {
      if (!line.trim()) continue;
      try {
        out.push(JSON.parse(line) as Json);
      } catch {
        /* skip */
      }
    }
    return out;
  } catch {
    return [];
  }
}

function ring(positions: Map<string, { x: number; y: number }>, ids: string[], radius: number, offset: number) {
  const n = ids.length;
  ids.forEach((id, i) => {
    if (positions.has(id)) return;
    const angle = offset + (i / Math.max(n, 1)) * 2 * Math.PI;
    positions.set(id, { x: Math.round(radius * Math.cos(angle)), y: Math.round(radius * Math.sin(angle)) });
  });
}

export async function loadMemoryEgoGraph(
  seed: string,
  opts: { maxNodes?: number } = {},
): Promise<EgoGraph> {
  const maxNodes = opts.maxNodes ?? 60;
  const projected = await runJson(["system", "memory-graph", seed, "--max-nodes", String(maxNodes), "--json"]);
  if (projected) return projected as unknown as EgoGraph;

  if (!privateFallbacksEnabled()) {
    return {
      seed,
      resolvedAs: "private fallback disabled",
      nodes: [],
      links: [],
      truncated: false,
      found: false,
    };
  }

  const [claims, edges] = await Promise.all([
    readJsonl(path.join(LEDGER, "claims.jsonl")),
    readJsonl(path.join(LEDGER, "edges.jsonl")),
  ]);

  const claimById = new Map<string, Json>();
  for (const c of claims) claimById.set(str(c.id), c);

  // seed claims: entity match, or the claim id itself
  const seedClaims = claims.filter(
    (c) => str(c.entity) === seed || str(c.id) === seed,
  );
  if (!seedClaims.length) {
    return { seed, resolvedAs: `unknown ${seed}`, nodes: [], links: [], truncated: false, found: false };
  }

  const resolvedAs =
    str(seedClaims[0].id) === seed ? `claim ${seed}` : `entity ${seed}`;

  const seedIds = new Set(seedClaims.map((c) => str(c.id)));
  const seedIdList = [...seedIds];

  // edges touching a seed claim → neighbor claims
  const links: MemoryLink[] = [];
  const neighborIds = new Set<string>();
  for (const e of edges) {
    const from = str(e.from);
    const to = str(e.to);
    if (seedIds.has(from) || seedIds.has(to)) {
      links.push({ id: `e_${str(e.seq) ?? from + to}`, source: from, target: to, kind: str(e.kind) });
      if (!seedIds.has(from)) neighborIds.add(from);
      if (!seedIds.has(to)) neighborIds.add(to);
    }
  }

  // bound the graph
  let neighborIdList = [...neighborIds];
  let truncated = false;
  const total = seedIdList.length + neighborIdList.length;
  if (total > maxNodes) {
    neighborIdList = neighborIdList.slice(0, maxNodes - seedIdList.length);
    truncated = true;
  }

  const positions = new Map<string, { x: number; y: number }>();
  ring(positions, seedIdList, 140, 0);
  ring(positions, neighborIdList, 360, 0.4);

  const nodeIds = new Set([...seedIdList, ...neighborIdList]);
  const nodes: MemoryNode[] = [];
  for (const c of claims) {
    const id = str(c.id);
    if (!nodeIds.has(id)) continue;
    const p = positions.get(id) ?? { x: 0, y: 0 };
    nodes.push({
      id,
      entity: str(c.entity),
      predicate: str(c.predicate),
      object: str(c.object).slice(0, 80),
      confidence: typeof c.confidence === "number" ? c.confidence : undefined,
      position: p,
    });
  }

  // drop links whose endpoints got truncated out
  const visible = new Set(nodes.map((n) => n.id));
  const visibleLinks = links.filter((l) => visible.has(l.source) && visible.has(l.target));

  return { seed, resolvedAs, nodes, links: visibleLinks, truncated, found: true };
}
