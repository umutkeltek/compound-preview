import { FreshnessChip, PageHeader } from "@/components/kit";
import { WorkLenses } from "@/components/work/WorkLenses";
import { arr, getSourceConnections, getWorkbench, runJson, str } from "@/lib/compound";
import { consoleMutationsEnabled } from "@/lib/console-mutations";
import { computeConsoleMutationReadiness } from "@/lib/instance-pin";
import { buildLaneTraffic } from "@/lib/lane-traffic";
import { getTraffic } from "@/lib/traffic";
import { buildBoard, buildOffered, buildPipeline } from "@/lib/work-pipeline";
import { buildWorkLensItems, groupWorkLensItems } from "@/lib/work-views";
import type { WorkLensKey } from "@/components/work/WorkLenses";

export const dynamic = "force-dynamic";

/**
 * Work — "Where is every unit of work in its lifecycle, and what can I move?"
 * Default Table lens answers who owns each active item and what moves next.
 * Flow, Space, Pipeline, Board, and Offered stay available as progressively
 * deeper views over the same source projection.
 */
export default async function WorkPage({
  searchParams,
}: {
  searchParams: Promise<{ trace?: string; lens?: string }>;
}) {
  const { trace, lens } = await searchParams;

  const [workbench, sources, suggestions, packets, laneTraffic, traffic] = await Promise.all([
    getWorkbench(),
    getSourceConnections(),
    // Read-only: `director propose --dry-run` creates nothing (verified); the
    // suggestions feed the Propose stage. Fail-soft to null.
    runJson(["director", "propose", "--dry-run", "--json"]),
    runJson(["packets", "list", "--json"]),
    buildLaneTraffic(),
    getTraffic(),
  ]);

  const open = arr(workbench?.open);
  const recent = arr(workbench?.recent);
  const proposals = arr(suggestions?.proposals ?? suggestions?.proposed ?? suggestions?.items);

  const stages = buildPipeline(workbench, sources, proposals);
  const columns = buildBoard(open, recent);
  const offered = buildOffered(arr(packets?.items));

  const initialTrace = str(trace).match(/^work_[a-z0-9]+$/) ? str(trace) : null;
  const mutationsReady = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;
  const items = buildWorkLensItems(open, traffic);
  const groups = groupWorkLensItems(items, traffic);
  const allowedLenses = new Set<WorkLensKey>(["pipeline", "table", "flow", "space", "board", "offered"]);
  const defaultLens = allowedLenses.has(lens as WorkLensKey) ? lens as WorkLensKey : "table";

  return (
    <div className="console-stage flex flex-col gap-6">
      <PageHeader
        breadcrumb="operate / work"
        title="Work"
        right={
          <FreshnessChip
            generatedAt={str(workbench?.generated_at)}
            source="compound workbench / sources / director dry-run"
          />
        }
      />
      <p className="text-sm text-muted" style={{ maxWidth: "var(--read-max)" }}>
        See what is moving, who owns it, and what needs intervention. Start with the focused table; switch to Flow
        or Space when relationships matter, and open any item for its complete trace.
      </p>

      <WorkLenses
        stages={stages}
        columns={columns}
        offered={offered}
        openCount={open.length}
        initialTrace={initialTrace}
        laneTraffic={laneTraffic}
        items={items}
        groups={groups}
        defaultLens={defaultLens}
        mutationsReady={mutationsReady}
      />
    </div>
  );
}
