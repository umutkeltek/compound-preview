import { Panel, domainForArea, type DomainTone } from "@/components/briefing/primitives";
import {
  DotMatrix,
  FeedDegraded,
  feedRenderModel,
  type FeedProjectionStatus,
  GridIcon,
  type MatrixRow,
  SectionHeader,
} from "@/components/kit";
import type { AttentionGroup } from "@/lib/attention";
import { toDate } from "@/lib/time";

/**
 * Attention-age DotMatrix (DESIGN.md §9.6) — the SHAPE of the needs-you queue,
 * not just its count: rows are domain classes, columns are age buckets, each
 * cell's intensity ramps with how many items sit there and answers on hover with
 * a TooltipCard. A compact below-fold panel — the fold stays the hero + KPIs.
 *
 * Pure aggregation over the ONE attention projection (already server-ranked); no
 * client re-ranking (attention.ts law).
 */

const AGE_BUCKETS = [
  { label: "<1d", maxDays: 1 },
  { label: "1–3d", maxDays: 3 },
  { label: "3–7d", maxDays: 7 },
  { label: "7d+", maxDays: Number.POSITIVE_INFINITY },
] as const;

const DOMAIN_ORDER: DomainTone[] = ["work", "memory", "capability", "route", "product"];
const DOMAIN_LABEL: Record<DomainTone, string> = {
  work: "Work",
  memory: "Memory",
  capability: "Capability",
  route: "Routing",
  product: "Product",
};

function bucketIndex(ageDays: number): number {
  for (let i = 0; i < AGE_BUCKETS.length; i++) if (ageDays < AGE_BUCKETS[i].maxDays) return i;
  return AGE_BUCKETS.length - 1;
}

/** count → 0..3 intensity: 0 empty · 1–2 low · 3–5 mid · 6+ hot. */
function level(count: number): number {
  if (count <= 0) return 0;
  if (count <= 2) return 1;
  if (count <= 5) return 2;
  return 3;
}

export function AttentionAgeMatrix({
  groups,
  read,
}: {
  groups: AttentionGroup[];
  read: { status: FeedProjectionStatus; lastGoodAt: string | null };
}) {
  if (feedRenderModel(read.status, groups.length > 0) === "degraded") {
    return (
      <Panel className="p-4">
        <SectionHeader tone="seam" icon={<GridIcon />} label="Attention age" sub="needs-you by domain × age" />
        <div className="mt-4">
          <FeedDegraded label="Attention age" source="compound attention --json" lastGoodAt={read.lastGoodAt} />
        </div>
      </Panel>
    );
  }

  // domain → [bucket counts]
  const tally = new Map<DomainTone, number[]>();
  const now = Date.now();
  let total = 0;

  for (const group of groups) {
    const domain = domainForArea(group.area);
    const created = toDate(group.oldest_created_at);
    const ageDays = created ? (now - created.getTime()) / 86_400_000 : 0;
    const bucket = bucketIndex(ageDays);
    const count = Math.max(1, group.count);
    if (!tally.has(domain)) tally.set(domain, [0, 0, 0, 0]);
    tally.get(domain)![bucket] += count;
    total += count;
  }

  const rows: MatrixRow[] = DOMAIN_ORDER.filter((d) => tally.has(d)).map((domain) => {
    const counts = tally.get(domain)!;
    return {
      label: DOMAIN_LABEL[domain],
      tone: domain,
      cells: AGE_BUCKETS.map((bucket, i) => ({
        level: level(counts[i]),
        count: counts[i],
        colLabel: bucket.label,
        sub: counts[i] > 0 ? "waiting on you" : undefined,
      })),
    };
  });

  return (
    <Panel className="p-4">
      <SectionHeader
        tone="seam"
        icon={<GridIcon />}
        label="Attention age"
        sub="needs-you by domain × age"
        right={<span className="mono text-2xs tabular-nums text-faint">{total} waiting</span>}
      />
      <div className="mt-4">
        {rows.length ? (
          <DotMatrix columns={AGE_BUCKETS.map((b) => b.label)} rows={rows} />
        ) : (
          <p className="text-sm text-muted">Clean seam. Nothing waits on you.</p>
        )}
      </div>
    </Panel>
  );
}
