import Link from "next/link";

import { EmptyDoctrine, Panel } from "@/components/briefing/primitives";
import { FeedDegraded, feedRenderModel, type FeedProjectionStatus, StateBadge } from "@/components/kit";
import { arr, num, str, type Json } from "@/lib/compound";
import { ago } from "@/lib/time";

/**
 * Live activity feed (redesign plan Part 3 below-the-fold): the most recent
 * transitions off the workbench projection, restyled onto the one pill grammar
 * (DESIGN.md §2). Each row is a closed move with its proof ids in mono — the
 * ledger, made legible. Deep-links to the work item's Trace.
 */

function moveLabel(move: string): string {
  switch (move) {
    case "outward_delta":
      return "Product change";
    case "memory_delta":
      return "Memory change";
    case "rule_delta":
      return "Rule change";
    case "control_plane_repair":
      return "System repair";
    case "audit_only":
      return "Audit";
    default:
      return move ? move.replace(/_/g, " ") : "Change";
  }
}

function title(item: Json): string {
  const surface = str(item.canonical_surface);
  if (surface.startsWith("packet:")) return surface.slice("packet:".length).replace(/-/g, " ");
  return str(item.success_predicate) || str(item.desired_outcome) || str(item.work_id) || str(item.id);
}

function clamp(text: string, max: number): string {
  const clean = text.replace(/\s+/g, " ").trim();
  return clean.length <= max ? clean : `${clean.slice(0, max).replace(/[\s.,;:—–-]+$/, "")}…`;
}

export function ActivityFeed({
  recent,
  read,
}: {
  recent: Json[];
  read: { status: FeedProjectionStatus; lastGoodAt: string | null };
}) {
  if (feedRenderModel(read.status, recent.length > 0) === "degraded") {
    return (
      <Panel className="px-6 py-6">
        <FeedDegraded label="Live activity" source="compound workbench --json --recent 500" lastGoodAt={read.lastGoodAt} />
      </Panel>
    );
  }

  const rows = recent
    .slice()
    .sort((a, b) => num(b.claimed_at) - num(a.claimed_at))
    .slice(0, 8);

  if (!rows.length) {
    return (
      <Panel className="px-6 py-6">
        <EmptyDoctrine>No moves recorded yet. audit_only counts for nothing.</EmptyDoctrine>
      </Panel>
    );
  }

  return (
    <Panel className="overflow-hidden">
      {rows.map((item, index) => {
        const workId = str(item.work_id) || str(item.id);
        const receipt = str(item.receipt_id) || str(item.receipt);
        const transition = str(item.transition_id) || str(item.transition);
        const actor = str(item.claimed_by) || str(item.owner);
        const move = str(item.move_type) || str(item.move);
        const proof = [receipt, transition].filter(Boolean).join(" · ");
        const body = (
          <div className="flex min-w-0 flex-col gap-1">
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-semibold text-ink">{moveLabel(move)}</span>
              <StateBadge state={str(item.state)} />
              {actor ? <span className="text-2xs text-muted">{actor}</span> : null}
            </div>
            <div className="truncate text-sm text-muted">{clamp(title(item), 120)}</div>
            {proof ? (
              <div className="mono truncate text-2xs text-faint" title={proof}>
                {proof}
              </div>
            ) : null}
          </div>
        );
        return (
          <div key={workId || index} className={`flex items-start justify-between gap-3 px-5 py-3.5 ${index > 0 ? "border-t border-hair" : ""}`}>
            {workId ? (
              <Link href={`/work/${workId}`} className="min-w-0 hover:underline focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus">
                {body}
              </Link>
            ) : (
              body
            )}
            <span className="shrink-0 text-2xs tabular-nums text-faint">{ago(str(item.claimed_at))}</span>
          </div>
        );
      })}
    </Panel>
  );
}
