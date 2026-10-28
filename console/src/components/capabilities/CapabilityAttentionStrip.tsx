import Link from "next/link";

/*
 * A thin attention strip on the Capabilities screen: how many changes to what
 * the system can do are waiting on the operator (capability-lifecycle-proposals,
 * evaluation intake). Counts come from the ONE attention projection, filtered by
 * area — no new projection. It links to /reviews (Decide), where the governed
 * acts live; it never mutates here.
 */

export type CapabilityAttentionItem = { area: string; label: string; count: number };

export function CapabilityAttentionStrip({ items }: { items: CapabilityAttentionItem[] }) {
  const total = items.reduce((sum, i) => sum + i.count, 0);

  if (total === 0) {
    return (
      <div className="rounded-lg border border-dashed border-hair bg-page px-4 py-2.5 text-sm text-muted">
        No capability changes proposed. The graph is settled — propose-only holds the line.
      </div>
    );
  }

  return (
    <div className="admission-seam flex flex-wrap items-center justify-between gap-3 rounded-lg border border-hair bg-panel px-4 py-2.5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-semibold text-ink">
          <span className="mono tabular-nums text-seam">{total}</span> proposed change
          {total === 1 ? "" : "s"} to what the system can do
        </span>
        <span className="flex flex-wrap items-center gap-1.5">
          {items
            .filter((i) => i.count > 0)
            .map((i) => (
              <span
                key={i.area}
                title={i.area}
                className="inline-flex items-center gap-1 rounded-pill border border-hair bg-soft-2 px-2 py-0.5 text-2xs font-semibold text-muted"
              >
                {i.label}
                <span className="mono tabular-nums text-faint">{i.count}</span>
              </span>
            ))}
        </span>
      </div>
      <Link
        href="/reviews"
        className="inline-flex items-center gap-1.5 rounded-md border border-hair bg-panel px-3 py-1.5 text-xs font-semibold text-ink transition-colors hover:bg-soft focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
      >
        Review in Decide →
      </Link>
    </div>
  );
}
