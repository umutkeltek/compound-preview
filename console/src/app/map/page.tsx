import Link from "next/link";

import {
  CapabilityAttentionStrip,
  type CapabilityAttentionItem,
} from "@/components/capabilities/CapabilityAttentionStrip";
import { SystemMap } from "@/components/map/SystemMap";
import { SystemNodeCard } from "@/components/map/SystemNodeCard";
import { FreshnessChip, IconTile, PageHeader } from "@/components/kit";
import { getAttention } from "@/lib/attention";
import { str } from "@/lib/compound";
import type { LensGraph, LensId } from "@/lib/map/lens";
import { isLiveLens } from "@/lib/map/lens";
import { buildRegistryLens } from "@/lib/map/lenses/registry";
import { buildRuntimeLens } from "@/lib/map/lenses/runtime";
import { loadTopology } from "@/lib/map/topology";
import { LANE_META, LANE_ORDER, type Lane } from "@/lib/map/types";

export const dynamic = "force-dynamic";

/*
 * Capabilities — "What can the system do, what's proposed to change?" The
 * capability graph is the hero (Registry lens, top billing — not collapsed);
 * Runtime is a tab on the same canvas. A thin attention strip surfaces
 * proposed changes (they act in Decide). The static substrate topology — a
 * different question, "how is the substrate wired end-to-end" — is kept as a
 * collapsed, orientation-only section below the live graph (see report on why
 * it survives rather than being dropped as redundant).
 */

const CAP_LABELS: Record<string, string> = {
  "capability-lifecycle-proposals": "Lifecycle proposals",
  "capability-evaluation-intake": "Evaluation intake",
  "external-repo-intake": "Repo intake",
};

async function buildInitial(lens: LensId): Promise<LensGraph> {
  return lens === "registry" ? buildRegistryLens({ group: "kind" }) : buildRuntimeLens();
}

function capabilityAttention(groups: { area: string; band: string; count: number }[]): CapabilityAttentionItem[] {
  const byArea = new Map<string, number>();
  for (const g of groups) {
    if (g.band !== "needs_you") continue;
    if (!g.area.startsWith("capability-")) continue;
    byArea.set(g.area, (byArea.get(g.area) ?? 0) + (g.count || 0));
  }
  return [...byArea.entries()]
    .map(([area, count]) => ({ area, label: CAP_LABELS[area] ?? area.replace(/-/g, " "), count }))
    .sort((a, b) => b.count - a.count);
}

export default async function CapabilitiesPage({
  searchParams,
}: {
  searchParams: Promise<{ lens?: string }>;
}) {
  const sp = await searchParams;
  const lens: LensId = sp.lens && isLiveLens(sp.lens) ? sp.lens : "registry";

  const [initial, topo, attention] = await Promise.all([
    buildInitial(lens),
    loadTopology(),
    getAttention(),
  ]);

  const capItems = capabilityAttention(
    (attention?.groups ?? []).map((g) => ({ area: str(g.area), band: str(g.band), count: g.count })),
  );
  const byLane = (lane: Lane) => topo.nodes.filter((n) => n.lane === lane);
  const liveEdges = topo.edges.filter((e) => e.status === "live").length;
  const plannedEdges = topo.edges.filter((e) => e.status === "planned").length;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        breadcrumb="know / capabilities"
        title="Capabilities"
        right={<FreshnessChip generatedAt={initial.generatedAt} source="compound capability graph --json" />}
      />

      <CapabilityAttentionStrip items={capItems} />

      <SystemMap initial={initial} lens={lens} />

      {/* Substrate topology — a DIFFERENT question than the capability graph:
          how data flows capture → storage → meaning → outward. Collapsed by
          default; orientation, not the hero. */}
      <details className="rounded-lg border border-hair bg-panel">
        <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3">
          <span className="text-md font-semibold text-ink">
            Substrate topology
            <span className="ml-2 text-xs font-normal text-faint">capture → storage → meaning → outward</span>
          </span>
          <span className="flex items-center gap-1.5">
            <span className="inline-flex items-center rounded-pill border border-good-line bg-good-soft px-2 py-0.5 text-2xs font-semibold tabular-nums text-good">
              {liveEdges} live
            </span>
            {plannedEdges ? (
              <span className="inline-flex items-center rounded-pill border border-seam-line bg-seam-soft px-2 py-0.5 text-2xs font-semibold tabular-nums text-seam">
                {plannedEdges} planned
              </span>
            ) : null}
          </span>
        </summary>

        <div className="flex flex-col gap-5 border-t border-hair px-4 py-4">
          <div className="flex flex-wrap items-center gap-2">
            <Link
              href="/map/lineage"
              className="inline-flex items-center gap-1.5 rounded-md border border-hair bg-panel px-3 py-1.5 text-xs font-semibold text-ink transition-colors hover:bg-soft focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus"
            >
              Lineage trace →
            </Link>
            <span className="text-2xs text-faint">follow any work id backward through its envelope spine</span>
          </div>

          <div className="grid gap-4 lg:grid-cols-4">
            {LANE_ORDER.map((lane) => {
              const meta = LANE_META[lane];
              const nodes = byLane(lane);
              return (
                <section key={lane} className="flex flex-col gap-3">
                  <div className="rounded-md bg-soft px-3 py-2.5">
                    <div className="flex items-center gap-2">
                      <IconTile tone="neutral">{meta.glyph}</IconTile>
                      <span className="text-sm font-semibold text-ink">{meta.label}</span>
                    </div>
                    <p className="mt-1 text-2xs leading-snug text-faint">{meta.blurb}</p>
                  </div>
                  <div className="flex flex-col gap-2.5">
                    {nodes.map((node) => (
                      <SystemNodeCard key={node.id} node={node} />
                    ))}
                  </div>
                </section>
              );
            })}
          </div>
        </div>
      </details>
    </div>
  );
}
