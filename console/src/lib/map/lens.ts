import type { Accent } from "@/components/ui";

/*
 * One canvas, N lenses. Every lens builder emits this normalized shape; the
 * SystemMapCanvas renders any LensGraph. Pure types + metadata only — safe to
 * import on server (builders) and client (shell/hook). No node:fs here.
 */

export type LensId = "runtime" | "registry" | "work-spine" | "memory";

export type LensStatus =
  | "running"
  | "held"
  | "blocked"
  | "needs-review"
  | "idle";

export type LensNode = {
  id: string;
  kind: string; // data-driven: "executor" | "work" | "model" | node-kind, etc.
  label: string; // a NAME, never a bare UUID
  sublabel?: string;
  status?: LensStatus; // drives the live dot (runtime)
  accent: Accent;
  position: { x: number; y: number }; // builder computes deterministically
  group?: string; // machine/project/kind — drives group/collapse
  raw?: unknown; // the real object, shown verbatim in the inspector
  sources: string[]; // e.g. ["compound workbench --json → open[]"]  ← name your projection
  cli?: string; // copyable `compound …`
  proofRefs?: string[]; // receipt/transition/evidence ids
};

export type LensEdge = {
  id: string;
  source: string;
  target: string;
  kind: string;
  label?: string;
  dashed?: boolean;
  accent?: Accent;
};

export type LensGraph = {
  lens: LensId;
  nodes: LensNode[];
  edges: LensEdge[];
  generatedAt: string;
  sources: string[];
  truncated?: boolean;
  note?: string; // honesty line (e.g. count discrepancy)
};

export const LENSES: {
  id: LensId;
  label: string;
  sourceLabel: string;
  soon?: boolean;
}[] = [
  {
    id: "runtime",
    label: "Runtime",
    sourceLabel: "compound workbench --json + leases + route-evidence",
  },
  {
    id: "registry",
    label: "Registry",
    sourceLabel: "capability-{nodes,edges}.jsonl + decisions",
  },
  {
    id: "work-spine",
    label: "Work spine",
    sourceLabel: "compound work show <id>",
    soon: true,
  },
  { id: "memory", label: "Memory", sourceLabel: "ledger claims + edges", soon: true },
];

export const LIVE_LENSES: LensId[] = ["runtime", "registry"];

export function isLensId(v: string): v is LensId {
  return LENSES.some((l) => l.id === v);
}

export function isLiveLens(v: string): v is LensId {
  return (LIVE_LENSES as string[]).includes(v);
}

/* ---------- shared accent maps (data-driven; gray fallback, never a superset) ---------- */

const NODE_KIND_ACCENT: Record<string, Accent> = {
  tool: "blue",
  skill: "violet",
  procedure: "orange",
  mcp: "pink",
  eval: "green",
  adapter: "gray",
  lens: "violet",
  doctrine: "gray",
  harness: "pink",
  model_profile: "blue",
  model: "blue",
  script: "orange",
  workflow: "green",
  taste_standard: "violet",
  decision: "blue",
  executor: "violet",
  work: "blue",
  gateway: "gray",
  dispatch: "orange",
};

export function nodeKindAccent(kind: string): Accent {
  return NODE_KIND_ACCENT[kind] ?? "gray";
}

const EDGE_KIND_ACCENT: Record<string, Accent> = {
  surfaces_with: "green",
  requires: "blue",
  conflicts_with: "red",
  specializes: "violet",
  fallback_for: "orange",
  validated_by: "green",
  evaluated_by: "blue",
  missed_when: "orange",
  false_triggered_by: "red",
  retired_by: "gray",
  // runtime edge kinds
  owns: "green",
  claimed: "blue",
  route: "violet",
  routes: "gray",
  blocked: "red",
};

export function edgeKindAccent(kind: string): Accent {
  return EDGE_KIND_ACCENT[kind] ?? "gray";
}
