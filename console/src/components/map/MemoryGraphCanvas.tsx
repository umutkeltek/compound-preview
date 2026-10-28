"use client";

import { Background, Controls, ReactFlow } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { memo, useMemo } from "react";

import type { EgoGraph, MemoryNode } from "@/lib/map/memory-graph";

const ENTITY_COLORS = ["#3b82f6", "#8b5cf6", "#10b981", "#f59e0b", "#ec4899", "#06b6d4"];

function entityColor(entity: string): string {
  let h = 0;
  for (let i = 0; i < entity.length; i++) h = (h * 31 + entity.charCodeAt(i)) | 0;
  return ENTITY_COLORS[Math.abs(h) % ENTITY_COLORS.length];
}

const EDGE_STYLE: Record<string, { stroke: string; dashed?: boolean }> = {
  supports: { stroke: "#10b981" },
  contradicts: { stroke: "#ef4444", dashed: true },
  supersedes: { stroke: "#9aa4b2", dashed: true },
  depends_on: { stroke: "#3b82f6" },
  part_of: { stroke: "#8b5cf6" },
};

const ClaimNode = memo(function ClaimNode({ data }: { data: { entity: string; predicate: string; object: string; color: string } }) {
  return (
    <div
      className="rounded-lg border bg-white px-2 py-1.5 shadow-sm"
      style={{ borderColor: data.color, width: 168 }}
    >
      <div className="mono truncate text-[9px] font-semibold uppercase" style={{ color: data.color }}>
        {data.entity}
      </div>
      <div className="truncate text-[10.5px] font-semibold text-ink">{data.predicate}</div>
      <div className="line-clamp-2 text-[9px] leading-tight text-ink-2">{data.object}</div>
    </div>
  );
});

const nodeTypes = { claim: ClaimNode };

export function MemoryGraphCanvas({ graph }: { graph: EgoGraph }) {
  const nodes = useMemo(
    () =>
      graph.nodes.map((n: MemoryNode) => ({
        id: n.id,
        type: "claim",
        position: n.position,
        data: {
          entity: n.entity,
          predicate: n.predicate,
          object: n.object,
          color: entityColor(n.entity),
        },
      })),
    [graph],
  );

  const edges = useMemo(
    () =>
      graph.links.map((l) => {
        const s = EDGE_STYLE[l.kind] ?? { stroke: "#c3cad4" };
        return {
          id: l.id,
          source: l.source,
          target: l.target,
          label: l.kind,
          type: "default",
          style: {
            stroke: s.stroke,
            strokeWidth: 1.5,
            strokeDasharray: s.dashed ? "4 3" : undefined,
          },
          labelStyle: { fontSize: 9, fill: s.stroke },
        };
      }),
    [graph],
  );

  if (!graph.nodes.length) {
    return (
      <div className="rounded-xl border border-dashed border-line bg-canvas/40 px-4 py-10 text-center text-[12.5px] text-ink-3">
        No claims or connections for this seed.
      </div>
    );
  }

  return (
    <div className="h-[560px] overflow-hidden rounded-xl border border-line bg-canvas/30">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        fitView
        minZoom={0.2}
        maxZoom={2}
        proOptions={{ hideAttribution: true }}
      >
        <Background color="#e5e9ef" gap={20} />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
