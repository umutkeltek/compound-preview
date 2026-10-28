"use client";

import { ReactFlowProvider } from "@xyflow/react";
import { useRouter } from "next/navigation";
import { useMemo, useState } from "react";

import { FilterChipRow } from "@/components/FilterChipRow";
import { LiveStamp } from "@/components/map/LiveStamp";
import { MapSearch } from "@/components/map/MapSearch";
import { NodeInspector } from "@/components/map/NodeInspector";
import { SystemMapCanvas } from "@/components/map/SystemMapCanvas";
import { Button, Tabs, TabsList, TabsTrigger } from "@/components/kit";
import type { LensGraph, LensId, LensStatus } from "@/lib/map/lens";
import { useLensData } from "@/lib/map/useLensData";

/*
 * The Capabilities canvas shell. Owns lens/filter/selection state, polls the
 * lens route, and renders the dark graph canvas + inspector. Reads only
 * projections; every mutation surfaces as a copyable command (the inspector),
 * never auto-run (DEC-0021).
 *
 * The Registry lens (the capability graph — what the system can do) gets top
 * billing; Runtime (what is live right now) is the second tab. The page owns
 * the screen title, so this renders a compact lens toolbar, not an h1.
 */

const RUNTIME_STATUSES: LensStatus[] = ["running", "held", "blocked", "needs-review", "idle"];

export function SystemMap({
  initial,
  lens: initialLens,
}: {
  initial: LensGraph | null;
  lens: LensId;
}) {
  const router = useRouter();
  const [lens, setLens] = useState<LensId>(initialLens);
  const [group, setGroup] = useState<"kind" | "none">("kind");
  const [status, setStatus] = useState<string>("all");
  const [selected, setSelected] = useState<string | null>(null);

  const params = lens === "registry" ? { group } : undefined;
  const { graph, error, loading, refetch } = useLensData(lens, initial, params);

  function switchLens(id: string) {
    setLens(id as LensId);
    setSelected(null);
    setStatus("all");
    router.replace(`/map?lens=${id}`, { scroll: false });
  }

  const view = useMemo<LensGraph | null>(() => {
    if (!graph) return null;
    if (lens !== "runtime" || status === "all") return graph;
    const nodes = graph.nodes.filter((n) => !n.status || n.status === status);
    const ids = new Set(nodes.map((n) => n.id));
    const edges = graph.edges.filter((e) => ids.has(e.source) && ids.has(e.target));
    return { ...graph, nodes, edges };
  }, [graph, lens, status]);

  const statusCounts = useMemo(() => {
    const c: Record<string, number> = { all: 0 };
    if (graph) {
      for (const n of graph.nodes) {
        if (!n.status) continue;
        c.all += 1;
        c[n.status] = (c[n.status] ?? 0) + 1;
      }
    }
    return c;
  }, [graph]);

  const selectedNode = graph?.nodes.find((n) => n.id === selected) ?? null;

  return (
    <ReactFlowProvider>
      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <Tabs value={lens} onValueChange={switchLens}>
            <TabsList label="Graph lens">
              <TabsTrigger value="registry" count={lens === "registry" ? graph?.nodes.length : undefined}>
                Registry
              </TabsTrigger>
              <TabsTrigger value="runtime" count={lens === "runtime" ? graph?.nodes.length : undefined}>
                Runtime
              </TabsTrigger>
            </TabsList>
          </Tabs>
          <div className="flex items-center gap-2">
            <LiveStamp generatedAt={graph?.generatedAt} sources={graph?.sources} />
            <Button variant="secondary" onClick={refetch} disabled={loading}>
              {loading ? "…" : "Refresh"}
            </Button>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-3">
          {lens === "runtime" ? (
            <FilterChipRow
              options={[
                { value: "all", label: "All" },
                ...RUNTIME_STATUSES.map((s) => ({ value: s, label: s })),
              ]}
              active={status}
              counts={statusCounts}
              onChange={setStatus}
            />
          ) : (
            <FilterChipRow
              options={[
                { value: "kind", label: "Clusters" },
                { value: "none", label: "Expanded" },
              ]}
              active={group}
              onChange={(v) => setGroup(v as "kind" | "none")}
            />
          )}
          <div className="ml-auto">{view ? <MapSearch nodes={view.nodes} onSelect={setSelected} /> : null}</div>
        </div>

        {error ? (
          <div className="rounded-md border border-crit-line bg-crit-soft px-3 py-2 text-xs text-crit">
            Lens read failed: {error}. Showing last good data if any.
          </div>
        ) : null}

        {view ? (
          <SystemMapCanvas graph={view} selectedId={selected} onSelect={setSelected} />
        ) : (
          <div
            className="flex items-center justify-center rounded-lg border border-hair bg-soft-2 text-sm text-muted"
            style={{ height: 600 }}
          >
            loading lens…
          </div>
        )}

        {graph?.note ? <p className="text-xs text-muted">{graph.note}</p> : null}
        {graph?.sources?.length ? (
          <p className="mono text-2xs leading-snug text-faint">sources: {graph.sources.join("  ·  ")}</p>
        ) : null}
      </div>

      <NodeInspector node={selectedNode} onClose={() => setSelected(null)} />
    </ReactFlowProvider>
  );
}
