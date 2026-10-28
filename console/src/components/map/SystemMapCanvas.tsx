"use client";

import {
  Background,
  Controls,
  Handle,
  MiniMap,
  Position,
  ReactFlow,
  useEdgesState,
  useNodesState,
  useReactFlow,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { memo, useEffect } from "react";

import { accentColor } from "@/components/ui";
import type { LensGraph, LensNode } from "@/lib/map/lens";

/*
 * One generic canvas that renders ANY LensGraph. Dark (WrongStack feel) while
 * the surrounding cockpit stays light. Data updates on poll via setNodes/
 * setEdges — the canvas never remounts, so pan/zoom/selection survive. Only
 * refits when the lens changes.
 */

const STATUS_COLOR: Record<string, string> = {
  running: "#10b981",
  held: "#f59e0b",
  blocked: "#ef4444",
  "needs-review": "#8b5cf6",
  idle: "#64748b",
};

type NodeData = {
  label: string;
  sublabel?: string;
  kind: string;
  accentColor: string;
  status?: string;
  selected?: boolean;
};

const LensNodeCard = memo(function LensNodeCard({ data }: { data: NodeData }) {
  return (
    <div
      className="rounded-lg border px-2.5 py-1.5 shadow-md"
      style={{
        width: 176,
        background: "#131a2b",
        borderColor: data.selected ? data.accentColor : "#26324a",
        borderWidth: data.selected ? 2 : 1,
        boxShadow: data.selected ? `0 0 0 3px ${data.accentColor}33` : undefined,
      }}
    >
      <Handle type="target" position={Position.Left} style={{ opacity: 0 }} />
      <div className="flex items-center gap-1.5">
        {data.status ? (
          <span
            className="h-2 w-2 shrink-0 rounded-full"
            style={{ background: STATUS_COLOR[data.status] ?? "#64748b" }}
          />
        ) : null}
        <span
          className="mono truncate text-[8.5px] font-semibold uppercase tracking-wide"
          style={{ color: data.accentColor }}
        >
          {data.kind}
        </span>
      </div>
      <div className="truncate text-[11px] font-semibold text-slate-100">{data.label}</div>
      {data.sublabel ? (
        <div className="mono truncate text-[9px] leading-tight text-slate-400">{data.sublabel}</div>
      ) : null}
      <Handle type="source" position={Position.Right} style={{ opacity: 0 }} />
    </div>
  );
});

const nodeTypes = { lens: LensNodeCard };

function toFlowNodes(graph: LensGraph, selectedId: string | null) {
  return graph.nodes.map((n: LensNode) => ({
    id: n.id,
    type: "lens",
    position: n.position,
    data: {
      label: n.label,
      sublabel: n.sublabel,
      kind: n.kind,
      accentColor: accentColor(n.accent),
      status: n.status,
      selected: n.id === selectedId,
    } satisfies NodeData,
  }));
}

function toFlowEdges(graph: LensGraph) {
  return graph.edges.map((e) => {
    const stroke = e.accent ? accentColor(e.accent) : "#475569";
    return {
      id: e.id,
      source: e.source,
      target: e.target,
      label: e.label,
      type: "default" as const,
      animated: e.kind === "route",
      style: {
        stroke,
        strokeWidth: 1.4,
        strokeDasharray: e.dashed ? "4 3" : undefined,
      },
      labelStyle: { fontSize: 8.5, fill: "#94a3b8" },
      labelBgStyle: { fill: "#0b0f19", fillOpacity: 0.8 },
    };
  });
}

export function SystemMapCanvas({
  graph,
  selectedId,
  onSelect,
}: {
  graph: LensGraph;
  selectedId: string | null;
  onSelect: (id: string | null) => void;
}) {
  const [nodes, setNodes, onNodesChange] = useNodesState(toFlowNodes(graph, selectedId));
  const [edges, setEdges, onEdgesChange] = useEdgesState(toFlowEdges(graph));
  const rf = useReactFlow();

  // sync data on poll / selection without remounting (preserves viewport)
  useEffect(() => {
    setNodes(toFlowNodes(graph, selectedId));
    setEdges(toFlowEdges(graph));
  }, [graph, selectedId, setNodes, setEdges]);

  // refit only when the lens changes (new node set), not on every poll
  useEffect(() => {
    const t = setTimeout(() => rf.fitView({ duration: 300, padding: 0.2 }), 80);
    return () => clearTimeout(t);
  }, [graph.lens, rf]);

  if (!graph.nodes.length) {
    return (
      <div className="flex h-[600px] items-center justify-center rounded-xl border border-dashed border-line bg-[#0b0f19] text-center text-[12.5px] text-slate-400">
        Nothing to show for this lens right now — that&apos;s live truth, not an error.
      </div>
    );
  }

  return (
    <div className="h-[600px] overflow-hidden rounded-xl border border-line" style={{ background: "#0b0f19" }}>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onNodeClick={(_, node) => onSelect(node.id)}
        onPaneClick={() => onSelect(null)}
        fitView
        minZoom={0.1}
        maxZoom={2.5}
        proOptions={{ hideAttribution: true }}
      >
        <Background color="#1e293b" gap={22} />
        <Controls showInteractive={false} />
        <MiniMap
          pannable
          zoomable
          nodeColor={(n) => (n.data as NodeData)?.accentColor ?? "#64748b"}
          maskColor="rgba(2,6,23,0.6)"
          style={{ background: "#0b0f19" }}
        />
      </ReactFlow>
    </div>
  );
}
