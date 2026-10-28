"use client";

import { Background, Controls, Handle, MiniMap, Position, ReactFlow, ReactFlowProvider } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import Link from "next/link";
import type { CSSProperties } from "react";
import { memo, useMemo, useState } from "react";

import { IdRef, StateBadge } from "@/components/kit";
import { harnessDisplayName } from "@/lib/harness-label";

export type LiveWork = {
  workId: string;
  title: string;
  state: string;
  nextAction: string;
  harness: string;
  runStatus: string;
  actor: string;
  retryDepth: number;
};

export type LiveLane = {
  id: string;
  label: string;
  status: string;
  running: number;
  needsMe: number;
  totalWork: number;
  work: LiveWork[];
};

type LaneData = { kind: "lane"; lane: LiveLane; color: string };
type WorkData = { kind: "work"; work: LiveWork; laneLabel: string; laneId: string; active: boolean; color: string };
type LiveData = LaneData | WorkData;
const ACTIVE_RUN = new Set(["running", "pending", "started"]);

function laneColor(lane: Pick<LiveLane, "id" | "label">): string {
  const identity = `${lane.id} ${lane.label}`.toLowerCase();
  if (identity.includes("codex")) return "var(--product-fg)";
  if (identity.includes("claude")) return "var(--cap-fg)";
  if (identity.includes("script")) return "var(--memory-fg)";
  if (identity.includes("switchback") || identity.includes("api")) return "var(--good-fg)";
  return "var(--work-fg)";
}

const LaneNode = memo(function LaneNode({ data }: { data: LaneData }) {
  const live = data.lane.running > 0;
  return (
    <button
      type="button"
      aria-label={`Inspect ${data.lane.label}: ${data.lane.running} running, ${data.lane.needsMe} waiting`}
      className="live-node w-52 rounded-lg border px-3 py-2.5 text-left shadow-lg"
      data-live={live}
      style={{ "--lane-color": data.color } as CSSProperties}
    >
      <Handle type="source" position={Position.Right} className="!border-0" style={{ background: data.color }} />
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="truncate text-sm font-semibold text-[var(--canvas-ink)]">{data.lane.label}</div>
          <div className="mono mt-0.5 text-2xs text-[var(--canvas-muted)]">{data.lane.id || "unbound lane"}</div>
        </div>
        <span className={`live-presence ${live ? "text-good" : "text-[var(--canvas-muted)]"}`}>
          <span className={live ? "live-dot" : "h-1.5 w-1.5 rounded-full bg-current"} aria-hidden />
          {live ? "live" : "idle"}
        </span>
      </div>
      <div className="mt-3 flex items-center gap-2 text-2xs text-[var(--canvas-muted)]">
        <span>{data.lane.running} running</span><span>·</span><span>{data.lane.totalWork} work</span>
        {data.lane.needsMe > 0 ? <><span>·</span><span className="text-seam">{data.lane.needsMe} paused</span></> : null}
      </div>
    </button>
  );
});

const WorkNode = memo(function WorkNode({ data }: { data: WorkData }) {
  return (
    <button
      type="button"
      aria-label={`Inspect ${data.work.title}: ${data.work.runStatus || data.work.state}`}
      className="live-node w-64 rounded-lg border px-3 py-2.5 text-left shadow-lg"
      data-live={data.active}
      style={{ "--lane-color": data.color } as CSSProperties}
    >
      <Handle type="target" position={Position.Left} className="!border-0" style={{ background: data.color }} />
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="line-clamp-2 text-sm font-semibold leading-snug text-[var(--canvas-ink)]">{data.work.title}</div>
          <div className="mono mt-1 truncate text-2xs text-[var(--canvas-muted)]">{data.work.workId}</div>
        </div>
        <StateBadge state={data.work.runStatus || data.work.state} />
      </div>
      <div className="mt-2 flex flex-wrap gap-x-2 gap-y-1 text-2xs text-[var(--canvas-muted)]">
        <span title={data.work.harness ? `Recorded harness: ${data.work.harness}` : undefined}>{data.work.harness ? harnessDisplayName(data.work.harness) : data.laneLabel}</span>
        {data.work.actor ? <span>by {data.work.actor}</span> : null}
        {data.work.retryDepth > 0 ? <span className="text-seam">retry {data.work.retryDepth}</span> : null}
      </div>
    </button>
  );
});

const nodeTypes = { lane: LaneNode, work: WorkNode };

function graphFor(lanes: LiveLane[]) {
  const nodes: any[] = [];
  const edges: any[] = [];
  let cursorY = 28;
  lanes.forEach((lane, laneIndex) => {
    const laneId = `lane-${lane.id || laneIndex}`;
    const color = laneColor(lane);
    const rows = Math.max(1, lane.work.length);
    nodes.push({ id: laneId, type: "lane", position: { x: 28, y: cursorY + Math.max(0, (rows - 1) * 38) }, data: { kind: "lane", lane, color } });
    lane.work.forEach((work, workIndex) => {
      const id = `work-${work.workId}-${laneIndex}-${workIndex}`;
      const active = ACTIVE_RUN.has(work.runStatus) || work.state === "running";
      nodes.push({ id, type: "work", position: { x: 390, y: cursorY + workIndex * 82 }, data: { kind: "work", work, laneLabel: lane.label, laneId: lane.id, active, color } });
      edges.push({ id: `${laneId}-${id}`, source: laneId, target: id, animated: active, style: { stroke: active ? color : "var(--canvas-border)", strokeWidth: active ? 2 : 1.2 } });
    });
    cursorY += Math.max(118, rows * 82 + 24);
  });
  return { nodes, edges };
}

function initialSelection(lanes: LiveLane[]): LiveData | null {
  for (const lane of lanes) {
    const work = lane.work.find((item) => ACTIVE_RUN.has(item.runStatus) || item.state === "running");
    if (work) return { kind: "work", work, laneLabel: lane.label, laneId: lane.id, active: true, color: laneColor(lane) };
  }
  const lane = lanes[0];
  if (!lane) return null;
  const work = lane.work[0];
  return work
    ? { kind: "work", work, laneLabel: lane.label, laneId: lane.id, active: false, color: laneColor(lane) }
    : { kind: "lane", lane, color: laneColor(lane) };
}

function Canvas({ lanes }: { lanes: LiveLane[] }) {
  const graph = useMemo(() => graphFor(lanes), [lanes]);
  const [selected, setSelected] = useState<LiveData | null>(() => initialSelection(lanes));
  if (!lanes.length) {
    return <div className="live-canvas flex h-[520px] items-center justify-center rounded-xl border text-center text-sm text-[var(--canvas-muted)]">No harness projection is available. Nothing is animated without heartbeat or transition evidence.</div>;
  }
  const work = selected?.kind === "work" ? selected.work : null;
  const lane = selected?.kind === "lane" ? selected.lane : null;
  const selectedColor = selected?.color ?? "var(--work-fg)";
  return (
    <div className="grid min-w-0 gap-3 xl:grid-cols-[minmax(0,1fr)_300px]">
      <div className="live-canvas live-canvas-stage min-w-0 overflow-hidden rounded-xl border">
        <ReactFlow
          nodes={graph.nodes}
          edges={graph.edges}
          nodeTypes={nodeTypes}
          onNodeClick={(_, node) => setSelected(node.data as LiveData)}
          onPaneClick={() => setSelected(initialSelection(lanes))}
          defaultViewport={{ x: 36, y: 22, zoom: 0.78 }} minZoom={0.35} maxZoom={1.6} nodesDraggable={false} nodesConnectable={false}
          proOptions={{ hideAttribution: true }}
        >
          <Background color="var(--canvas-border)" gap={24} size={1} />
          <Controls showInteractive={false} />
          <MiniMap pannable zoomable nodeColor={(node) => (node.data as LiveData).color} maskColor="rgba(3,7,13,.72)" style={{ background: "var(--canvas-panel)" }} />
        </ReactFlow>
      </div>
      <aside
        className="live-inspector min-w-0 rounded-xl p-4 text-[var(--canvas-ink)]"
        style={{ "--lane-color": selectedColor } as CSSProperties}
        aria-live="polite"
      >
        <div className="flex items-center justify-between gap-3 border-b border-[var(--canvas-border)] pb-3">
          <div className="text-2xs font-bold uppercase tracking-wide text-[var(--canvas-muted)]">
            {work ? "Selected work" : "Harness stream"}
          </div>
          <span className="live-presence text-good">
            <span className="live-dot" aria-hidden />
            projected
          </span>
        </div>
        {work ? (
          <div className="mt-4 flex flex-col gap-4">
            <div>
              <h3 className="line-clamp-6 text-lg font-semibold leading-snug" title={work.title}>{work.title}</h3>
              <div className="mt-2"><IdRef id={work.workId} kind="work" plain /></div>
            </div>
            <div className="grid grid-cols-2 gap-2">
              <div className="rounded-lg border border-[var(--canvas-border)] bg-soft/30 px-3 py-2"><div className="text-2xs font-bold uppercase text-[var(--canvas-muted)]">Envelope</div><div className="mt-1"><StateBadge state={work.state} /></div></div>
              <div className="rounded-lg border border-[var(--canvas-border)] bg-soft/30 px-3 py-2"><div className="text-2xs font-bold uppercase text-[var(--canvas-muted)]">Run</div><div className="mt-1"><StateBadge state={work.runStatus || "unknown"} /></div></div>
            </div>
            <div><div className="text-2xs font-bold uppercase text-[var(--canvas-muted)]">Owned by</div><p className="mt-1 text-sm font-semibold" title={work.harness ? `Recorded harness: ${work.harness}` : undefined}>{selected?.kind === "work" ? selected.laneLabel : harnessDisplayName(work.harness)}</p><p className="mono mt-1 text-2xs text-[var(--canvas-muted)]">{selected?.kind === "work" ? selected.laneId : ""}{work.actor ? ` · ${work.actor}` : ""}</p></div>
            <div><div className="text-2xs font-bold uppercase text-[var(--canvas-muted)]">What moves next</div><p className="mt-1 text-sm leading-relaxed text-[var(--canvas-muted)]">{work.nextAction || "No next action projected."}</p></div>
            <Link href={`/work/${work.workId}`} className="rounded-md border border-work-line bg-work-soft px-3 py-2 text-center text-xs font-semibold text-work transition-colors hover:bg-work-soft/70">Open full trace →</Link>
          </div>
        ) : lane ? (
          <div className="mt-4 flex flex-col gap-4">
            <div><h3 className="text-lg font-semibold">{lane.label}</h3><p className="mono mt-1 text-2xs text-[var(--canvas-muted)]">{lane.id}</p></div>
            <div className="grid grid-cols-3 gap-2">
              {[['Running', lane.running], ['Work', lane.totalWork], ['Paused', lane.needsMe]].map(([label, value]) => <div key={label} className="rounded-lg border border-[var(--canvas-border)] bg-soft/30 px-2 py-2"><div className="text-2xs font-bold uppercase text-[var(--canvas-muted)]">{label}</div><div className="mono mt-1 text-lg">{value}</div></div>)}
            </div>
            <p className="text-sm leading-relaxed text-[var(--canvas-muted)]">This harness appears live only when its projection reports active work. Configuration alone never counts as activity.</p>
          </div>
        ) : null}
      </aside>
    </div>
  );
}

export function LiveHarnessCanvas({ lanes }: { lanes: LiveLane[] }) {
  return <ReactFlowProvider><Canvas lanes={lanes} /></ReactFlowProvider>;
}
