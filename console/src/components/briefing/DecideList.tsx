import Link from "next/link";

import { DomainKicker, EmptyDoctrine, Panel } from "@/components/briefing/primitives";
import { FeedDegraded, feedRenderModel, type FeedProjectionStatus } from "@/components/kit";
import type { AttentionGroup } from "@/lib/attention";
import { humanAge, isOld } from "@/lib/briefing";
import { str } from "@/lib/compound";

function clamp(text: string, max: number): string {
  const clean = str(text).replace(/\s+/g, " ").trim();
  if (clean.length <= max) return clean;
  const cut = clean.slice(0, max);
  const at = cut.lastIndexOf(" ");
  return `${(at > 24 ? cut.slice(0, at) : cut).replace(/[\s.,;:—–-]+$/, "")}…`;
}

/**
 * The Decide list (redesign plan Part 3): the next few decisions after the
 * headline, in the attention projection's own leverage order (never re-sorted
 * client-side), each deep-linked to the Decide queue. Rows carry the domain
 * accent + the amber age when stale — the same seam grammar as the hero.
 */
export function DecideList({
  groups,
  total,
  read,
}: {
  groups: AttentionGroup[];
  total: number;
  read: { status: FeedProjectionStatus; lastGoodAt: string | null };
}) {
  if (feedRenderModel(read.status, groups.length > 0) === "degraded") {
    return (
      <Panel className="px-6 py-8">
        <FeedDegraded label="Decide" source="compound attention --json" lastGoodAt={read.lastGoodAt} />
      </Panel>
    );
  }
  if (!groups.length) {
    return (
      <Panel className="px-6 py-8">
        <EmptyDoctrine>Clean seam. Nothing waits on you.</EmptyDoctrine>
      </Panel>
    );
  }
  const overflow = total - groups.length;

  return (
    <Panel className="overflow-hidden">
      {groups.map((group, index) => {
        const key = str(group.key);
        const created = group.oldest_created_at;
        const warn = isOld(created);
        const age = humanAge(created) || "new";
        const unlock = str(group.operator_unlock);
        return (
          <Link
            key={key || index}
            href={key ? `/reviews?decision=${encodeURIComponent(key)}` : "/reviews"}
            title={key}
            className={`grid grid-cols-1 gap-2 px-5 py-4 transition-colors hover:bg-soft focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus sm:grid-cols-[1fr_auto] sm:items-center sm:gap-5 ${
              index > 0 ? "border-t border-hair" : ""
            }`}
          >
            <div className="min-w-0">
              <DomainKicker area={str(group.area)} />
              <h3 className="mt-1.5 text-md font-semibold leading-snug text-ink">
                {clamp(str(group.operator_summary) || str(group.area) || "A decision is waiting on you.", 96)}
              </h3>
              {unlock ? <div className="mt-1 text-sm text-good">→ {unlock}</div> : null}
            </div>
            <div className="flex items-center justify-between gap-4 sm:justify-end">
              {group.count > 1 ? (
                <span className="shrink-0 text-xs tabular-nums text-faint">×{group.count.toLocaleString("en-US")}</span>
              ) : null}
              <span className={`shrink-0 text-xs tabular-nums ${warn ? "font-semibold text-seam" : "text-faint"}`}>{age}</span>
              <span className="shrink-0 text-sm font-semibold text-work">Review →</span>
            </div>
          </Link>
        );
      })}
      {overflow > 0 ? (
        <Link
          href="/reviews"
          className="flex items-center justify-between border-t border-hair px-5 py-3 text-sm font-medium text-muted transition-colors hover:bg-soft focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus"
        >
          <span>+{overflow.toLocaleString("en-US")} more decisions</span>
          <span className="text-work">All in Decide →</span>
        </Link>
      ) : null}
    </Panel>
  );
}
