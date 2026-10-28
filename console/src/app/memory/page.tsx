import { AdjudicationCard } from "@/components/memory/AdjudicationCard";
import { EvidenceCard } from "@/components/memory/EvidenceCard";
import { MemoryGraphPanel } from "@/components/memory/MemoryGraphPanel";
import { buildAdjudicationModel, type AdjudicationCardModel } from "@/components/memory/model";
import { BrainIcon, DatabaseIcon, DocIcon, EmptyState, FeedDegraded, PageHeader, SectionHeader } from "@/components/kit";
import { consoleMutationsEnabled } from "@/lib/console-mutations";
import { computeConsoleMutationReadiness } from "@/lib/instance-pin";
import { loadMemoryCountsRead, loadProposedMemorySplit, loadStandingLawCandidatesRead } from "@/lib/memory-data";
import type { ProjectionRead } from "@/lib/compound";
import { loadMemoryEgoGraph, type EgoGraph } from "@/lib/map/memory-graph";

export const dynamic = "force-dynamic";

/*
 * Memory — "What does the system believe, what's contested, what needs
 * adjudication?" One column, three honest bands:
 *   1) the adjudication queue (the operator's actual job — governed verdicts);
 *   2) the belief graph, embedded and seedable from any card (the /map/memory
 *      route is retired into this section);
 *   3) what's already canon (a tabular state strip) + the proposed source
 *      evidence that is deliberately kept OUT of the approval queue.
 */

const STAT_STATES: { key: string; label: string }[] = [
  { key: "promoted", label: "Promoted" },
  { key: "parked", label: "Parked" },
  { key: "rejected", label: "Rejected" },
  { key: "no_promote", label: "No-promote" },
  { key: "proposed", label: "Proposed" },
];

function MemoryReadNotice({ label, source, read }: { label: string; source: string; read: ProjectionRead }) {
  if (read.status === "live") return null;
  return (
    <div>
      {read.status === "unreachable" ? (
        <FeedDegraded label={label} source={source} lastGoodAt={read.lastGoodAt} />
      ) : (
        <p className="text-sm text-muted">Showing previously read {label} data. This is not a current confirmed read.</p>
      )}
      <details className="mt-2 text-xs text-muted">
        <summary>Read details</summary>
        <p className="mono">{source}</p>
        <p>Last successful read: {read.lastGoodAt ?? "none"}</p>
        <p>Source generated: {read.generatedAt ?? "unknown"}</p>
        {read.error ? <p className="mono">{read.error}</p> : null}
      </details>
    </div>
  );
}

export default async function Memory({
  searchParams,
}: {
  searchParams: Promise<{ graph?: string }>;
}) {
  const { graph: graphParam } = await searchParams;
  const seed = (graphParam ?? "").trim();

  const [{ actionable, sourceEvidence, read: proposedRead }, law, countRead, ego] = await Promise.all([
    loadProposedMemorySplit(),
    loadStandingLawCandidatesRead(),
    loadMemoryCountsRead(),
    seed ? loadMemoryEgoGraph(seed, { maxNodes: 60 }) : Promise.resolve<EgoGraph | null>(null),
  ]);

  const mutationsEnabled = computeConsoleMutationReadiness(consoleMutationsEnabled()).ready;
  // Parked accepted decisions (standing-law candidates) are durable verdicts the
  // operator owes just like proposed memory — they lead the queue so the clean
  // seam can never lie while any waits. Non-law parked rows (the 28k-row
  // knowledge corpus) are filtered out at the loader, so they never flood in.
  const lawModels = law.candidates
    .map((candidate) => buildAdjudicationModel(candidate, "standing-law-candidate"))
    .filter((m): m is AdjudicationCardModel => Boolean(m));
  const proposedModels = actionable
    .map((candidate) => buildAdjudicationModel(candidate))
    .filter((m): m is AdjudicationCardModel => Boolean(m));
  const models = [...lawModels, ...proposedModels];
  const queueLive = proposedRead.status === "live" && law.read.status === "live";
  const counts = countRead.counts;

  return (
    <div className="flex flex-col gap-8">
      <PageHeader
        breadcrumb="know / memory"
        title="Memory"
        right={
          <>
            {models.length ? (
              <span className="inline-flex items-center gap-1.5 rounded-pill border border-seam-line bg-seam-soft px-2.5 py-0.5 text-xs font-semibold text-seam">
                <span className="mono tabular-nums">{models.length}</span> {queueLive ? "awaiting your verdict" : "known items awaiting your verdict"}
              </span>
            ) : null}
            <span className="mono text-xs text-muted">{countRead.read.status}</span>
          </>
        }
      />
      <p className="text-sm text-muted" style={{ maxWidth: "var(--read-max)" }}>
        Only durable candidates enter your queue. Source evidence stays searchable but out of
        approval debt until a promoted claim cites it.
      </p>

      {/* 1 — adjudication queue (the operator's gate) */}
      <section className="flex flex-col gap-3">
        <SectionHeader
          tone="memory"
          icon={<BrainIcon />}
          label="Adjudication queue"
          sub="promote / park / reject / no-promote — each with its basis"
        />
        <MemoryReadNotice label="proposed memory" source="compound memory candidates --json --state proposed" read={proposedRead} />
        <MemoryReadNotice label="standing decisions" source="compound memory candidates --json --state parked --kind decision" read={law.read} />
        {models.length ? (
          <div className="flex flex-col gap-3">
            {models.map((model) => (
              <AdjudicationCard key={model.id} model={model} mutationsEnabled={mutationsEnabled} />
            ))}
          </div>
        ) : queueLive ? (
          <EmptyState>
            Clean seam. No durable memory candidate needs a verdict — recall stays exactly as you
            left it.
          </EmptyState>
        ) : null}
      </section>

      {/* 2 — the belief graph (retired /map/memory lives here) */}
      <MemoryGraphPanel seed={seed} graph={ego} open={Boolean(seed)} />

      {/* 3 — what's already believed: tabular state strip + proposed evidence */}
      <section className="flex flex-col gap-3">
        <SectionHeader
          tone="memory"
          icon={<DatabaseIcon />}
          label="What's believed"
          sub="candidate lifecycle across the ledger"
        />
        <MemoryReadNotice label="memory counts" source="compound memory candidates --json" read={countRead.read} />
        <div className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border border-hair bg-hair sm:grid-cols-5">
          {STAT_STATES.map((s) => (
            <div key={s.key} className="flex flex-col gap-1 bg-panel px-4 py-3">
              <span className="text-2xs font-bold uppercase tracking-wide text-faint">{s.label}</span>
              <span className="text-xl font-extrabold tabular-nums text-ink">{countRead.read.status === "unreachable" ? "—" : counts[s.key] ?? 0}</span>
            </div>
          ))}
        </div>
      </section>

      <section className="flex flex-col gap-3">
        <SectionHeader
          tone="neutral"
          icon={<DocIcon />}
          label="Source evidence"
          sub="proposed imports & extractions — evidence, not approval debt"
        />
        <MemoryReadNotice label="source evidence" source="compound memory candidates --json --state proposed" read={proposedRead} />
        {sourceEvidence.length ? (
          <div className="grid gap-3 lg:grid-cols-2">
            {sourceEvidence.slice(0, 24).map((candidate) => (
              <EvidenceCard key={candidate.id} candidate={candidate} />
            ))}
          </div>
        ) : proposedRead.status === "live" ? (
          <EmptyState>No proposed source evidence. Nothing waits to be cited into canon.</EmptyState>
        ) : null}
        {sourceEvidence.length > 24 ? (
          <p className="mono text-2xs text-faint">+{sourceEvidence.length - 24} more not shown.</p>
        ) : null}
      </section>
    </div>
  );
}
