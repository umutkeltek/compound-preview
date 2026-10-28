import { num, runJson } from "@/lib/compound";
import type { LensGraph, LensNode, LensEdge } from "@/lib/map/lens";
import { edgeKindAccent, nodeKindAccent } from "@/lib/map/lens";
import {
  readCapabilityGraph,
  type CapNode,
} from "@/lib/map/capability-graph";

/*
 * Registry lens — "how the parts connect."
 * Nodes = capability graph projection + a decisions cluster.
 * Default group="kind" collapses 736 nodes to per-kind clusters; group="none"
 * expands the full graph in a deterministic column-by-kind grid (no layout dep).
 *
 * Dangling endpoints (edge targets absent from nodes.jsonl, e.g. harness:codex)
 * are synthesized from the id prefix so React Flow never drops the edge.
 */

const kindOf = (id: string, byId: Map<string, CapNode>): string =>
  byId.get(id)?.kind || (id.includes(":") ? id.split(":")[0] : "unknown");

async function authorityCounts(): Promise<{ n: number; e: number } | null> {
  const j = await runJson(["capability", "authority", "--json"]);
  if (!j) return null;
  return { n: num(j.node_count), e: num(j.edge_count) };
}

export async function buildRegistryLens(
  opts: { group?: "kind" | "none" } = {},
): Promise<LensGraph> {
  const group = opts.group ?? "kind";
  const [{ nodes: capNodes, edges: capEdges, decisions, mtime }, auth] = await Promise.all([
    readCapabilityGraph(),
    authorityCounts(),
  ]);

  const byId = new Map<string, CapNode>();
  for (const n of capNodes) byId.set(n.id, n);

  const uniqueNodes = byId.size;
  const generatedAt = mtime || new Date().toISOString();
  const sources = [
    `compound capability graph --json → nodes (${capNodes.length} rows, ${uniqueNodes} unique)`,
    `compound capability graph --json → edges (${capEdges.length} rows)`,
    auth
      ? `compound capability authority --json (deduped view: ${auth.n} nodes / ${auth.e} edges)`
      : "compound capability authority --json (deduped view)",
    `compound capability graph --json → decisions (${decisions.length})`,
  ];
  const note =
    `Graph truth = compound capability graph --json: ${capNodes.length} nodes / ${uniqueNodes} unique nodes, ${capEdges.length} edges.` +
    (auth ? ` Deduped authority view: ${auth.n} nodes / ${auth.e} edges — both cited.` : "");

  /* decisions that retire a capability → retired_by edges (rare, honest) */
  const decisionEdges: LensEdge[] = [];
  for (const d of decisions) {
    if (d.retires && d.decision_id) {
      decisionEdges.push({
        id: `retired:${d.decision_id}:${d.retires}`,
        source: `decision:${d.decision_id}`,
        target: d.retires,
        kind: "retired_by",
        dashed: true,
        label: "retires",
      });
    }
  }

  if (group === "kind") {
    // ----- collapse to per-kind clusters (unique nodes, not lines) -----
    const kindCount = new Map<string, number>();
    for (const n of byId.values()) kindCount.set(n.kind, (kindCount.get(n.kind) ?? 0) + 1);

    const clusterEdge = new Map<string, { count: number; kinds: Set<string> }>();
    for (const e of capEdges) {
      const fk = kindOf(e.from, byId);
      const tk = kindOf(e.to, byId);
      const key = `${fk}→${tk}`;
      const agg = clusterEdge.get(key) ?? { count: 0, kinds: new Set<string>() };
      agg.count += 1;
      agg.kinds.add(e.kind);
      clusterEdge.set(key, agg);
    }

    const kinds = [...kindCount.keys()].sort((a, b) => (kindCount.get(b) ?? 0) - (kindCount.get(a) ?? 0));
    const perRow = 4;
    const nodes: LensNode[] = kinds.map((k, i) => ({
      id: `kind:${k}`,
      kind: k,
      label: k,
      sublabel: `${kindCount.get(k)} nodes`,
      accent: nodeKindAccent(k),
      position: { x: (i % perRow) * 260, y: Math.floor(i / perRow) * 200 },
      group: "kind",
      sources: [`compound capability graph --json → nodes kind=${k}`],
      cli: "compound capability authority --json",
      raw: { kind: k, count: kindCount.get(k) },
    }));

    // decisions as one cluster node (registry completeness without 156 floaters)
    if (decisions.length) {
      const i = kinds.length;
      nodes.push({
        id: "kind:decision",
        kind: "decision",
        label: "decisions",
        sublabel: `${decisions.length} records`,
        accent: nodeKindAccent("decision"),
        position: { x: (i % perRow) * 260, y: Math.floor(i / perRow) * 200 },
        group: "kind",
        sources: ["compound capability graph --json → decisions"],
        cli: "compound decisions authority --json",
        raw: { decisions: decisions.length },
      });
    }

    const nodeIds = new Set(nodes.map((n) => n.id));
    const edges: LensEdge[] = [];
    for (const [key, agg] of clusterEdge) {
      const [fk, tk] = key.split("→");
      const s = `kind:${fk}`;
      const t = `kind:${tk}`;
      if (!nodeIds.has(s) || !nodeIds.has(t)) continue;
      const dominant = [...agg.kinds][0] ?? "surfaces_with";
      edges.push({
        id: `ce:${key}`,
        source: s,
        target: t,
        kind: dominant,
        label: `${agg.count}`,
        accent: edgeKindAccent(dominant),
      });
    }

    return { lens: "registry", nodes, edges, generatedAt, sources, note };
  }

  // ----- expanded: full graph, synthesize dangling endpoints -----
  const referenced = new Set<string>();
  for (const e of capEdges) {
    referenced.add(e.from);
    referenced.add(e.to);
  }
  const allIds = new Set<string>([...byId.keys(), ...referenced]);

  // deterministic column-by-kind grid; wrap tall kinds into sub-columns
  const idsByKind = new Map<string, string[]>();
  for (const id of allIds) {
    const k = kindOf(id, byId);
    (idsByKind.get(k) ?? idsByKind.set(k, []).get(k)!).push(id);
  }
  const kindOrder = [...idsByKind.keys()].sort(
    (a, b) => (idsByKind.get(b)?.length ?? 0) - (idsByKind.get(a)?.length ?? 0),
  );

  const KIND_GAP = 1400;
  const SUBCOL = 170;
  const ROWH = 46;
  const WRAP = 22;
  const pos = new Map<string, { x: number; y: number }>();
  kindOrder.forEach((k, ki) => {
    const ids = (idsByKind.get(k) ?? []).sort();
    ids.forEach((id, i) => {
      const sub = Math.floor(i / WRAP);
      const row = i % WRAP;
      pos.set(id, { x: ki * KIND_GAP + sub * SUBCOL, y: row * ROWH });
    });
  });

  const nodes: LensNode[] = [...allIds].map((id) => {
    const real = byId.get(id);
    const k = kindOf(id, byId);
    const name = real?.name || (id.includes(":") ? id.split(":").slice(1).join(":") : id);
    return {
      id,
      kind: k,
      label: name,
      sublabel: real ? k : `${k} (referenced)`,
      accent: nodeKindAccent(k),
      position: pos.get(id) ?? { x: 0, y: 0 },
      group: k,
      sources: real
        ? ["compound capability graph --json → nodes"]
        : ["synthesized from edge endpoint (absent from nodes.jsonl)"],
      cli: "compound capability authority --json",
      raw: real ?? { id, synthesized: true },
    };
  });

  const edges: LensEdge[] = capEdges.map((e, i) => ({
    id: `e${i}:${e.from}->${e.to}:${e.kind}`,
    source: e.from,
    target: e.to,
    kind: e.kind,
    label: e.kind,
    accent: edgeKindAccent(e.kind),
    dashed: e.kind === "conflicts_with" || e.kind === "false_triggered_by",
  }));

  return {
    lens: "registry",
    nodes,
    edges,
    generatedAt,
    sources,
    note: `${note} Expanded: ${nodes.length} nodes (incl. synthesized endpoints), ${edges.length} edges preserved.`,
    truncated: false,
  };
}
