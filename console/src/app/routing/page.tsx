import { CopyCmd } from "@/components/CopyCmd";
import { BenchPanel } from "@/components/routing/BenchPanel";
import { LanesTable } from "@/components/routing/LanesTable";
import { PromotionBand } from "@/components/routing/PromotionBand";
import { RouteTraffic } from "@/components/routing/RouteTraffic";
import { RoutingTabs } from "@/components/routing/RoutingTabs";
import { FreshnessChip, PageHeader, RouteIcon, SectionHeader } from "@/components/kit";
import { getAttentionProjection } from "@/lib/attention";
import { getReviews, HEAVY_PROJECTION_TIMEOUT_MS, str } from "@/lib/compound";
import { consoleMutationsEnabled } from "@/lib/console-mutations";
import { computeConsoleMutationReadiness } from "@/lib/instance-pin";
import { buildRoutePromotionModel, getModelLanes, getRouteTraffic } from "@/lib/routing";

export const dynamic = "force-dynamic";

/**
 * Routing & cost — "is routing sending the right mind at the right cost, and
 * should a lane change?" (redesign plan Part 3). One screen, three answers:
 *   1. the promotion/remorse gate (should a lane change) — governed decisions;
 *   2. the lane roster by cost class (which mind) — compare → table;
 *   3. route-evidence traffic + honest cost (at what cost) + bench doctrine.
 * Every section reads a live projection and stamps its own freshness.
 */

/** Route plan needs a class/candidate arg the console can't supply — so we hand
 *  over the honest copyable command instead of faking a plan. */
function RoutePlanCard({ classes }: { classes: string[] }) {
  const exampleClass = classes[0] || "<capability-class>";
  return (
    <section className="rounded-xl bg-panel p-4 shadow-sm">
      <SectionHeader
        tone="route"
        icon={<RouteIcon />}
        label="Plan a route"
        right={
          classes.length ? (
            <span className="text-2xs text-faint">
              classes: <span className="mono text-muted">{classes.join(", ")}</span>
            </span>
          ) : null
        }
      />
      <p className="mt-1 max-w-prose text-2xs leading-relaxed text-muted">
        A route plan is per-request: it needs a capability class or a candidate id the console can&apos;t infer, so it
        isn&apos;t projected here. Ask for one by class — the plan prefers already-paid lanes first.
      </p>
      <div className="mt-3 flex flex-col gap-3">
        <div>
          <div className="text-2xs font-bold uppercase tracking-wide text-faint">By capability class</div>
          <div className="mt-1">
            <CopyCmd cmd={`compound route plan --capability-class ${exampleClass} --json`} />
          </div>
        </div>
        <div>
          <div className="text-2xs font-bold uppercase tracking-wide text-faint">By candidate</div>
          <div className="mt-1">
            <CopyCmd cmd="compound route plan --candidate <candidate_id> --json" />
          </div>
        </div>
      </div>
    </section>
  );
}

export default async function RoutingPage() {
  const [attentionProjection, reviews, lanes, traffic] = await Promise.all([
    getAttentionProjection({ timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
    getReviews({ timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
    getModelLanes(),
    getRouteTraffic(),
  ]);

  const { read: attentionRead, report: attention } = attentionProjection;
  const promotion = buildRoutePromotionModel(reviews, attention, attentionRead.status);
  const mutationsEnabled = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;

  return (
    <div className="flex flex-col gap-8">
      <PageHeader
        breadcrumb="control / routing & cost"
        title="Routing & cost"
        right={
          <>
            <FreshnessChip generatedAt={attentionRead.generatedAt ?? str(attention?.generated_at)} source="compound attention --json" />
            {lanes?.generatedAt ? (
              <FreshnessChip generatedAt={lanes.generatedAt} source="compound route model-capabilities --json" />
            ) : null}
          </>
        }
      />
      <p className="max-w-prose text-sm text-muted">
        Is routing sending the right mind at the right cost — and should a lane change?
      </p>

      <PromotionBand
        needsYou={promotion.needsYou}
        holdRecord={promotion.watching}
        classA={promotion.classA}
        mutationsEnabled={mutationsEnabled}
      />

      <RoutePlanCard classes={lanes?.capabilityClasses ?? []} />

      <RoutingTabs
        lanes={<LanesTable report={lanes} />}
        traffic={<RouteTraffic report={traffic} />}
        bench={<BenchPanel />}
      />
    </div>
  );
}
