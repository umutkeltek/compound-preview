import { NextResponse } from "next/server";

import type { ViewProposal } from "@/ai-console";
import { getAttentionProjection } from "@/lib/attention";
import { arr, HEAVY_PROJECTION_TIMEOUT_MS, runProjection, str, type Json } from "@/lib/compound";

type AskRequest = { query?: unknown; route?: unknown };

function workTitle(item: Json): string {
  const surface = str(item.canonical_surface);
  if (surface.startsWith("packet:")) return surface.slice("packet:".length).replace(/-/g, " ");
  return str(item.success_predicate) || str(item.desired_outcome) || str(item.work_id) || "work item";
}

/**
 * Read-only composition endpoint. It returns registered-view references, never
 * arbitrary UI or mutation calls. This is the replaceable Tambo seam: Tambo
 * may choose the composition later, while Compound projections and the view
 * registry continue to own truth and rendering authority.
 */
export async function POST(request: Request) {
  const raw = await request.json().catch(() => ({})) as AskRequest;
  const query = typeof raw.query === "string" ? raw.query.trim() : "";
  const route = typeof raw.route === "string" ? raw.route : "/";
  if (!query) return NextResponse.json({ error: "query is required" }, { status: 400 });

  const lower = query.toLowerCase();
  const asksCompare = /compare|option|if i|if we|wait/.test(lower);
  const asksFresh = /fresh|evidence|source|recent/.test(lower);
  const asksWork = /work|blocked|moving|running|owner|harness/.test(lower) || route.startsWith("/work") || route.startsWith("/live");
  const asksBlocked = /block|needs me|waiting/.test(lower);
  const [attentionProjection, workbenchRead] = await Promise.all([
    getAttentionProjection({ timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
    asksWork || asksFresh
      ? runProjection(["workbench", "--json", "--recent", "100"], { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS })
      : Promise.resolve(null),
  ]);
  const attention = attentionProjection.report;
  const top = attention?.top ?? attention?.groups.find((group) => group.band === "needs_you") ?? null;
  const open = arr(workbenchRead?.value?.open);

  const proposal: ViewProposal = { components: [], actionProposals: [] };
  if (top && (asksBlocked || !asksCompare)) {
    proposal.components.push({
      view: "causal-brief",
      props: {
        title: "Highest-leverage seam",
        summary: top.operator_summary,
        unlock: top.operator_unlock || undefined,
        count: top.count,
        blastRadius: top.blast_radius || undefined,
        reversibility: top.reversibility || undefined,
        generatedAt: attentionProjection.read.generatedAt || top.freshness.generated_at || undefined,
        source: top.freshness.source || "compound attention --json",
      },
    });
  }
  if (top && asksCompare) {
    proposal.components.push({
      view: "comparison-brief",
      props: {
        title: top.operator_summary,
        actLabel: top.action?.label || "Review in Decide",
        actConsequence: top.operator_unlock || undefined,
        generatedAt: attentionProjection.read.generatedAt || top.freshness.generated_at || undefined,
        source: top.freshness.source || "compound attention --json",
      },
    });
  }
  if (asksWork) {
    const matching = open.filter((item) => !asksBlocked || ["blocked", "held"].includes(str(item.state))).slice(0, 5);
    proposal.components.push({
      view: "work-list",
      props: {
        label: asksBlocked ? "Work at a seam" : "Open work",
        items: matching.map((item) => ({
          id: str(item.work_id) || str(item.id),
          title: workTitle(item),
          state: str(item.state) || "unknown",
          owner: str(item.claimed_by) || str(item.owner) || undefined,
        })).filter((item) => item.id),
      },
    });
  }
  if (asksFresh) {
    proposal.components.push(
      { view: "freshness-chip", props: { generatedAt: attentionProjection.read.generatedAt || undefined, source: "compound attention --json" } },
      { view: "freshness-chip", props: { generatedAt: workbenchRead?.generatedAt || undefined, source: "compound workbench --json --recent 100" } },
    );
  }
  if (!proposal.components.length) {
    proposal.components.push({
      view: "stat-tile",
      props: {
        label: "Current projections",
        value: open.length,
        sub: `${attention?.counts.needs_you_groups ?? 0} attention groups · ask about work, evidence, or a comparison`,
      },
    });
  }
  return NextResponse.json(proposal);
}
