"use client";

import { EmptyState, IdRef, StateBadge } from "@/components/kit";
import type { OfferedPacketModel } from "@/lib/work-pipeline";

/**
 * Offered-packets lens — the standing packet menu (packets whose state reads
 * `offered`), kept from the old surface and restyled onto kit components.
 * Packets are pre-work: they carry a canonical surface, not a work_id, so they
 * link to their surface rather than opening a Trace drawer.
 */
export function OfferedLens({ packets }: { packets: OfferedPacketModel[] }) {
  if (!packets.length) {
    return (
      <EmptyState>
        No packets are on offer right now. Offered packets are read from the plan directory the live console
        serves; a fresh worktree has none.
      </EmptyState>
    );
  }
  return (
    <div className="grid gap-3 md:grid-cols-2">
      {packets.map((packet) => (
        <div key={packet.key} className="flex flex-col gap-2 rounded-lg border border-hair bg-panel px-3 py-3 shadow-sm">
          <div className="flex items-start justify-between gap-2">
            <h3 className="text-sm font-semibold leading-snug text-ink">{packet.title}</h3>
            <StateBadge state="draft" label="offered" />
          </div>
          {packet.surface ? <IdRef id={packet.surface} kind="generic" plain truncate /> : null}
          {packet.path ? <span className="mono truncate text-2xs text-faint" title={packet.path}>{packet.path}</span> : null}
        </div>
      ))}
    </div>
  );
}
