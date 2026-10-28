import Link from "next/link";

import { FreshnessChip, StateBadge } from "@/components/kit";
import type { AttentionGroup } from "@/lib/attention";

export function CausalFocus({ top, openWork, activeLanes }: { top: AttentionGroup | null; openWork: number; activeLanes: number }) {
  return (
    <section className="overflow-hidden rounded-xl border border-hair bg-panel shadow-sm">
      <header className="flex flex-wrap items-start justify-between gap-3 border-b border-hair px-4 py-3">
        <div><div className="section-label">Focus path</div><h2 className="mt-1 text-lg font-semibold text-ink">{top ? "Where attention has the most leverage now" : "No ranked bottleneck is projected"}</h2><p className="mt-1 text-xs text-muted">Activity → highest-ranked operator seam → projected unlock</p></div>
        {top ? <FreshnessChip generatedAt={top.freshness.generated_at} source={top.freshness.source || "compound attention --json"} /> : null}
      </header>
      <div className="grid gap-3 p-4 lg:grid-cols-[1fr_auto_1.35fr_auto_1fr] lg:items-stretch">
        <div className="rounded-lg border border-hair bg-soft px-3 py-3">
          <div className="text-2xs font-bold uppercase tracking-wide text-faint">System activity</div>
          <div className="mt-3 grid grid-cols-2 gap-3"><div><div className="mono text-xl text-ink">{openWork}</div><div className="text-2xs text-faint">open work</div></div><div><div className="mono text-xl text-good">{activeLanes}</div><div className="text-2xs text-faint">active lanes</div></div></div>
          <p className="mt-3 text-xs leading-relaxed text-muted">Capacity is evidence of activity, not proof of outward progress.</p>
        </div>
        <div className="hidden items-center text-faint lg:flex" aria-hidden>→</div>
        <div className={`rounded-lg border px-4 py-3 ${top ? "border-seam-line bg-seam-soft/45" : "border-hair bg-soft"}`}>
          <div className="flex items-center justify-between gap-3"><div className="text-2xs font-bold uppercase tracking-wide text-seam">Highest leverage seam</div>{top ? <StateBadge state={top.action ? "ready" : "waiting"} label={top.action ? "action ready" : "owner action unavailable"} /> : null}</div>
          <p className="mt-3 text-md font-semibold leading-snug text-ink">{top?.operator_summary || "Compound attention has not selected a top group."}</p>
          {top ? <div className="mt-3 flex flex-wrap gap-2 text-2xs text-muted"><span className="rounded-pill border border-hair bg-panel px-2 py-1">{top.count} {top.count === 1 ? "item" : "items"} in group</span><span className="rounded-pill border border-hair bg-panel px-2 py-1">{top.blast_radius.replace(/_/g, " ") || "blast radius unknown"}</span><span className="rounded-pill border border-hair bg-panel px-2 py-1">{top.reversibility || "reversibility unknown"}</span></div> : null}
        </div>
        <div className="hidden items-center text-faint lg:flex" aria-hidden>→</div>
        <div className="rounded-lg border border-hair bg-soft px-3 py-3">
          <div className="text-2xs font-bold uppercase tracking-wide text-faint">Projected unlock</div>
          <p className="mt-3 text-sm font-semibold leading-relaxed text-ink">{top?.operator_unlock || "No owner-projected unlock is available."}</p>
          {top ? <Link href={`/reviews?decision=${encodeURIComponent(top.key)}`} className="mt-4 inline-flex text-xs font-semibold text-work hover:underline">Open the decision evidence →</Link> : null}
        </div>
      </div>
      <details className="group border-t border-hair px-4 py-3">
        <summary className="flex cursor-pointer list-none items-center justify-between gap-3 text-xs font-semibold text-muted marker:hidden"><span>Why this shape?</span><span aria-hidden className="text-sm text-faint transition-transform group-open:rotate-90">›</span></summary>
        <p className="mt-2 max-w-read text-xs leading-relaxed text-muted">The middle card is Compound attention&apos;s deterministic top-ranked group. The arrows explain the operator reading order; they do not claim an unprojected dependency graph.</p>
      </details>
    </section>
  );
}
