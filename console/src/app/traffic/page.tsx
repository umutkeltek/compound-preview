import { FreshnessChip, PageHeader } from "@/components/kit";
import { CoordinationBoard, type ExecDetail } from "@/components/coordination/CoordinationBoard";
import { getCoordination } from "@/lib/coordination";
import { getTraffic } from "@/lib/traffic";

export const dynamic = "force-dynamic";

/**
 * Coordination — "I'm doing X, that agent is doing Y, and I'm waiting on it to
 * start Z." The reframe of the old traffic fleet monitor around the
 * load-bearing waiting-on / blocked-on / handoff relationship: Me / Others /
 * Waiting-on, over the generic read-only `compound coordination` verb (actors
 * are generic — fleet rows, lease holders, claimers; never hardcoded agents),
 * live via `compound stream --follow` (events carry `by`). The run-attempt
 * join from the traffic data plane supplies the per-work execution detail
 * (harness/status/retry) under each actor's doing.
 *
 * (?actor= recenters "me"; empty = the verb's most-present actor.)
 */
export default async function CoordinationPage({ searchParams }: { searchParams: Promise<{ actor?: string }> }) {
  const { actor } = await searchParams;
  const actorParam = typeof actor === "string" ? actor.trim() : "";
  const [coordination, traffic] = await Promise.all([getCoordination(actorParam || undefined), getTraffic()]);

  const execDetail: Record<string, ExecDetail> = {};
  for (const lane of traffic.lanes) {
    for (const work of lane.inFlight) {
      if (execDetail[work.workId]) continue;
      execDetail[work.workId] = {
        harness: work.harness,
        runStatus: work.runStatus,
        actor: work.actor,
        retryDepth: work.retryDepth,
      };
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        breadcrumb="operate / coordination"
        title="Coordination"
        right={<FreshnessChip generatedAt={coordination.view.generatedAt || traffic.generatedAt} source="compound coordination / stream" />}
      />
      <p className="text-sm text-muted" style={{ maxWidth: "var(--read-max)" }}>
        Who is doing what — and who is waiting on whom to start. Each actor's face is the read-only planning half of
        `work continue` (workbench, candidate prep, lane-capacity gate) without executing any of it; the Waiting-on
        section is the global dependency graph. State streams in live; the projection reconciles on a short poll
        whether or not you are at the wheel.
      </p>
      <CoordinationBoard
        view={coordination.view}
        doorStatus={coordination.status}
        doorError={coordination.error}
        execDetail={execDetail}
        runAttemptsPresent={traffic.runAttemptsPresent}
      />
    </div>
  );
}
