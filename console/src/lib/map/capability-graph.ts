import { readFile, stat } from "node:fs/promises";
import path from "node:path";

import { arr, COMPOUND_HOME, privateFallbacksEnabled, runJson } from "@/lib/compound";

/*
 * The capability graph is the richest "how the parts connect" surface.
 * Graph truth = the raw jsonl (736 nodes / 171 edges). The deduped
 * `compound capability authority --json` view reports fewer (550/133) — both
 * are cited in the lens `sources`; we render the raw graph.
 *
 * CRITICAL: edges reference endpoints absent from nodes.jsonl (e.g.
 * `harness:codex`). The registry builder MUST synthesize placeholder nodes
 * from the `id` prefix, or React Flow silently drops those edges.
 */

export type CapNode = {
  id: string;
  kind: string;
  name: string;
  triggers?: string[];
  weight?: number;
};

export type CapEdge = {
  kind: string;
  from: string;
  to: string;
  confidence?: number;
  created_at?: string;
};

export type DecisionAuthority = {
  id?: string;
  decision_id?: string;
  authority_ref?: string;
  source_path?: string;
  retires?: string;
};

async function readJsonl(
  file: string,
): Promise<{ rows: Record<string, unknown>[]; mtime: string | null }> {
  if (!privateFallbacksEnabled()) return { rows: [], mtime: null };

  try {
    const text = await readFile(file, "utf8");
    let mtime: string | null = null;
    try {
      mtime = (await stat(file)).mtime.toISOString();
    } catch {
      /* optional */
    }
    const rows: Record<string, unknown>[] = [];
    for (const line of text.split(/\r?\n/)) {
      if (!line.trim()) continue;
      try {
        rows.push(JSON.parse(line));
      } catch {
        /* skip */
      }
    }
    return { rows, mtime };
  } catch {
    return { rows: [], mtime: null };
  }
}

const reg = (f: string) => path.join(COMPOUND_HOME, "registries", f);

export async function readCapabilityGraph(): Promise<{
  nodes: CapNode[];
  edges: CapEdge[];
  decisions: DecisionAuthority[];
  mtime: string | null;
}> {
  const projected = await runJson(["capability", "graph", "--json"]);
  if (projected) {
    return {
      nodes: arr(projected.nodes) as CapNode[],
      edges: arr(projected.edges) as CapEdge[],
      decisions: arr(projected.decisions) as DecisionAuthority[],
      mtime: null,
    };
  }

  const [{ rows: nodeRows, mtime: nodeMtime }, { rows: edgeRows, mtime: edgeMtime }, { rows: decisionRows, mtime: decisionMtime }] =
    await Promise.all([
      readJsonl(reg("capability-nodes.jsonl")),
      readJsonl(reg("capability-edges.jsonl")),
      readJsonl(reg("decision-source-authority.jsonl")),
    ]);
  return {
    nodes: nodeRows as unknown as CapNode[],
    edges: edgeRows as unknown as CapEdge[],
    decisions: decisionRows as unknown as DecisionAuthority[],
    mtime: [nodeMtime, edgeMtime, decisionMtime].filter(Boolean).sort().pop() ?? null,
  };
}

export async function readCapabilityNodes(): Promise<{
  nodes: CapNode[];
  mtime: string | null;
}> {
  const { nodes, mtime } = await readCapabilityGraph();
  return { nodes, mtime };
}

export async function readCapabilityEdges(): Promise<{
  edges: CapEdge[];
  mtime: string | null;
}> {
  const { edges, mtime } = await readCapabilityGraph();
  return { edges, mtime };
}

export async function readDecisionSourceAuthority(): Promise<{
  decisions: DecisionAuthority[];
  mtime: string | null;
}> {
  const { decisions, mtime } = await readCapabilityGraph();
  return { decisions, mtime };
}
