import { LineageSearch } from "@/components/map/LineageSearch";
import { TimelineStep } from "@/components/map/TimelineStep";
import { Card, EmptyState, FreshnessChip } from "@/components/ui";
import { traceLineage } from "@/lib/map/lineage";

export const dynamic = "force-dynamic";

export default async function LineagePage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const queryId = decodeURIComponent(id);
  const lineage = await traceLineage(queryId);

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-end justify-between gap-4">
        <div>
          <h1 className="text-[22px] font-semibold tracking-tight">Lineage</h1>
          <p className="mt-0.5 text-[12.5px] text-ink-3">
            How a datum arrived and what it feeds — source → evidence → candidate → adjudication → claim.
          </p>
        </div>
        <FreshnessChip source="compound system lineage --json" />
      </div>

      <LineageSearch initial={queryId} />

      <Card>
        <div className="mb-4">
          <span className="section-label">Resolved</span>
          <div className="text-[13px] font-semibold text-ink">{lineage.resolvedAs}</div>
        </div>
        {lineage.found && lineage.steps.length ? (
          <ol className="flex flex-col">
            {lineage.steps.map((step, i) => (
              <TimelineStep key={i} step={step} last={i === lineage.steps.length - 1} />
            ))}
          </ol>
        ) : (
          <EmptyState>
            Not found — no claim, candidate, evidence, work, or entity matched{" "}
            <code className="mono">{queryId}</code>.
          </EmptyState>
        )}
      </Card>
    </div>
  );
}
