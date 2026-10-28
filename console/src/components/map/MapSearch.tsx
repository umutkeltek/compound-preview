"use client";

import { useReactFlow } from "@xyflow/react";
import { useState } from "react";

import type { LensNode } from "@/lib/map/lens";

/*
 * Search that pans-to-node. Must live inside <ReactFlowProvider> (the shell
 * provides it) so useReactFlow().setCenter can move the viewport.
 */
export function MapSearch({
  nodes,
  onSelect,
}: {
  nodes: LensNode[];
  onSelect: (id: string) => void;
}) {
  const [q, setQ] = useState("");
  const rf = useReactFlow();

  function go(e: React.FormEvent) {
    e.preventDefault();
    const needle = q.trim().toLowerCase();
    if (!needle) return;
    const hit =
      nodes.find((n) => n.label.toLowerCase().includes(needle)) ??
      nodes.find((n) => n.id.toLowerCase().includes(needle));
    if (hit) {
      rf.setCenter(hit.position.x + 88, hit.position.y + 20, {
        zoom: 1.2,
        duration: 400,
      });
      onSelect(hit.id);
    }
  }

  return (
    <form onSubmit={go} className="flex items-center gap-1.5">
      <input
        value={q}
        onChange={(e) => setQ(e.target.value)}
        placeholder="find a node…"
        className="w-[160px] rounded-lg border border-line bg-white px-2.5 py-1 text-[11.5px] text-ink-2 outline-none focus:border-ink/40"
      />
      <button
        type="submit"
        className="rounded-lg border border-line bg-white px-2.5 py-1 text-[11.5px] font-medium text-ink-2 hover:bg-canvas/60"
      >
        pan
      </button>
    </form>
  );
}
