import { MemoryGraphCanvas } from "@/components/map/MemoryGraphCanvas";
import type { EgoGraph } from "@/lib/map/memory-graph";
import { GraphSeedSearch } from "./GraphSeedSearch";

/*
 * The memory ego-graph, embedded in the Memory screen (the /map/memory route is
 * retired — the graph lives here now). A collapsed-by-default section that opens
 * the moment a seed is present: any candidate card's "Show in graph" or the seed
 * box below sets ?graph=<seed>, the server re-projects, and this panel opens.
 * The heavy `system memory-graph --json` read only runs when a seed is set, so a
 * plain /memory load stays fast.
 */

const EDGE_LEGEND: { kind: string; cls: string }[] = [
  { kind: "supports", cls: "border-good-line bg-good-soft text-good" },
  { kind: "contradicts", cls: "border-crit-line bg-crit-soft text-crit" },
  { kind: "supersedes", cls: "border-hair bg-soft-2 text-muted" },
  { kind: "depends_on", cls: "border-work-line bg-work-soft text-work" },
  { kind: "part_of", cls: "border-memory-line bg-memory-soft text-memory" },
];

export function MemoryGraphPanel({
  seed,
  graph,
  open,
}: {
  seed: string;
  graph: EgoGraph | null;
  open: boolean;
}) {
  return (
    <details
      id="memory-graph"
      open={open}
      className="scroll-mt-20 rounded-lg border border-memory-line bg-panel"
    >
      <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3">
        <span className="flex items-center gap-2">
          <span
            aria-hidden
            className="inline-flex h-6 w-6 items-center justify-center rounded-sm border border-memory-line bg-memory-soft text-xs font-bold text-memory"
          >
            ✦
          </span>
          <span className="text-md font-semibold text-ink">Memory graph</span>
          <span className="text-xs text-faint">the dense web — claims linked by supports / contradicts / supersedes</span>
        </span>
        <span className="mono shrink-0 text-2xs text-faint">
          {seed ? (graph?.found ? "live" : "no match") : "seed to load"}
        </span>
      </summary>

      <div className="flex flex-col gap-4 border-t border-hair px-4 py-4">
        <GraphSeedSearch initial={seed} />

        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-2xs font-bold uppercase tracking-wide text-faint">edges</span>
          {EDGE_LEGEND.map((e) => (
            <span
              key={e.kind}
              className={`inline-flex items-center rounded-pill border px-2 py-0.5 text-2xs font-semibold ${e.cls}`}
            >
              {e.kind}
            </span>
          ))}
          {graph?.truncated ? (
            <span className="inline-flex items-center rounded-pill border border-seam-line bg-seam-soft px-2 py-0.5 text-2xs font-semibold text-seam">
              truncated — showing {graph.nodes.length}
            </span>
          ) : null}
        </div>

        {!seed ? (
          <div className="rounded-md border border-dashed border-hair bg-page px-4 py-10 text-center text-sm text-muted">
            Seed a claim or entity to see its web. Every belief is a node; typed edges show what
            supports, contradicts, or supersedes it.
          </div>
        ) : graph?.found ? (
          <div className="flex flex-col gap-2">
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm font-semibold text-ink">{graph.resolvedAs}</span>
              <span className="mono text-2xs tabular-nums text-faint">
                {graph.nodes.length} claims · {graph.links.length} links
              </span>
            </div>
            <MemoryGraphCanvas graph={graph} />
          </div>
        ) : (
          <div className="rounded-md border border-dashed border-hair bg-page px-4 py-10 text-center text-sm text-muted">
            No entity or claim matches <code className="mono text-ink">{seed}</code>. Try a slug like{" "}
            <code className="mono text-ink">example-os</code> or a claim id.
          </div>
        )}

        <p className="mono text-2xs leading-snug text-faint">source: compound system memory-graph --json</p>
      </div>
    </details>
  );
}
