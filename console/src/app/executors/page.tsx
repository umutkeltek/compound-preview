import { CostLanes } from "@/components/agents/CostLanes";
import { DelegatedSessions } from "@/components/agents/DelegatedSessions";
import { summarizeHygiene, type ExecutorProjection } from "@/components/agents/executor-model";
import { normalizeExecutorProjection } from "@/components/agents/executor-normalize";
import { ExecutorTable } from "@/components/agents/ExecutorTable";
import { HygieneBand } from "@/components/agents/HygieneBand";
import { LeasePressure } from "@/components/agents/LeasePressure";
import { LocalModelsTile } from "@/components/agents/LocalModelsTile";
import { FreshnessChip, PageHeader } from "@/components/kit";
import { getExecutors, getWorktreeInventory } from "@/lib/compound";
import { WorktreeInventory } from "@/components/agents/WorktreeInventory";

export const dynamic = "force-dynamic";

/**
 * Agents — "who is working, on what, at what cost, is anything stuck?"
 * (plan Part 3). One projection (`compound executors --json`) drives the whole
 * screen: a lease-hygiene band on top (the needs-you win), then the executor
 * table (compare → table, not cards), then cost/lane traffic + the local-model
 * lane. Every data section carries a freshness chip; the fleet reads true or
 * says why it can't.
 */

function FleetSummary({ counts }: { counts: ExecutorProjection["counts"] }) {
  const items: { label: string; value: number; seam?: boolean }[] = [
    { label: "Executors", value: counts.total },
    { label: "Running", value: counts.running },
    { label: "Ready", value: counts.ready },
    { label: "Assigned", value: counts.assigned },
    { label: "Needs you", value: counts.needsMe, seam: true },
    { label: "Leases", value: counts.leases },
  ];
  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
      {items.map((item) => {
        const emphasise = item.seam && item.value > 0;
        return (
          <div key={item.label} className="rounded-lg border border-hair bg-panel px-3 py-2.5 shadow-sm">
            <div className={`text-2xs font-bold uppercase tracking-wide ${emphasise ? "text-seam" : "text-faint"}`}>
              {item.label}
            </div>
            <div className={`mono mt-1 text-xl tabular-nums ${emphasise ? "text-seam" : "text-ink"}`}>{item.value}</div>
          </div>
        );
      })}
    </div>
  );
}

function Provenance({ projection }: { projection: ExecutorProjection }) {
  return (
    <div className="rounded-lg border border-hair bg-soft px-4 py-3 text-2xs leading-relaxed text-faint">
      <span className="font-bold uppercase tracking-wide">Authority</span>{" "}
      <span className="mono text-muted">{projection.authorityRef || "—"}</span>
      {projection.sourceRefs.length ? (
        <>
          {" · "}
          <span className="font-bold uppercase tracking-wide">Sources</span>{" "}
          <span className="mono text-muted">{projection.sourceRefs.join(", ")}</span>
        </>
      ) : null}
      {projection.nextCommand.length ? (
        <>
          {" · "}
          <span className="mono text-muted">{projection.nextCommand.join(" ")}</span>
        </>
      ) : null}
    </div>
  );
}

export default async function AgentsPage() {
  const [executors, worktrees] = await Promise.all([getExecutors(), getWorktreeInventory()]);
  const projection = normalizeExecutorProjection(executors);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        breadcrumb="operate / agents"
        title="Agents"
        right={<FreshnessChip generatedAt={projection?.generatedAt} source="compound executors --json" />}
      />
      <p className="text-sm text-muted">Who is working, on what, at what cost — and is anything stuck?</p>
      <WorktreeInventory {...worktrees} />

      {projection ? (
        <>
          <HygieneBand
            summary={summarizeHygiene(projection)}
            generatedAt={projection.generatedAt}
            staleAfterSeconds={projection.staleAfterSeconds}
            staleHistory={projection.counts.staleHistory}
          />

          <FleetSummary counts={projection.counts} />

          {projection.executors.length ? (
            <ExecutorTable executors={projection.executors} staleAfterSeconds={projection.staleAfterSeconds} />
          ) : (
            <div className="rounded-lg border border-dashed border-hair bg-page px-6 py-12 text-center">
              <p className="text-sm text-muted">No executors in the projection yet.</p>
            </div>
          )}

          <LeasePressure executors={projection.executors} staleAfterSeconds={projection.staleAfterSeconds} />

          <DelegatedSessions rows={projection.delegatedSessions} counts={projection.delegatedCounts} />

          <div className="grid gap-4 lg:grid-cols-2">
            <CostLanes executors={projection.executors} generatedAt={projection.generatedAt} />
            <LocalModelsTile />
          </div>

          <Provenance projection={projection} />
        </>
      ) : (
        <div className="rounded-lg border border-dashed border-hair bg-page px-6 py-12 text-center">
          <p className="text-sm text-muted">
            No executor projection available. The fleet reads from{" "}
            <span className="mono text-muted">compound executors --json</span>.
          </p>
        </div>
      )}
    </div>
  );
}
