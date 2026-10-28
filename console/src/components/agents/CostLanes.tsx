import { CopyCmd } from "@/components/CopyCmd";
import { CoinsIcon, FreshnessChip, SectionHeader, TooltipCard } from "@/components/kit";

import type { ExecutorRow } from "./executor-model";

/**
 * Cost / lane-traffic bars — hand-built div bars (DESIGN.md §8, no chart lib).
 *
 * HONEST DATA. There is no cheap per-lane DOLLAR-cost projection: `compound
 * route cost-doctor` needs a `<provider-calls.ndjson>` ledger positional
 * (verified 2026-07-09: `route cost-doctor --json` errors — the ledger isn't
 * wired), so spend numbers are NOT invented here. What IS cheap and live is
 * `route_evidence_count` per executor from the executors projection — the
 * lane-traffic signal (route-evidence rows, not spend). We chart that,
 * labelled for exactly what it is, and name the missing spend projection so
 * the gap is legible rather than faked.
 */

interface Lane {
  label: string;
  value: number;
}

function Bars({ lanes }: { lanes: Lane[] }) {
  const max = Math.max(1, ...lanes.map((lane) => lane.value));
  return (
    <div className="flex flex-col gap-2">
      {lanes.map((lane) => {
        const pct = Math.round((lane.value / max) * 100);
        return (
          <div key={lane.label} className="flex items-center gap-3">
            <div className="w-28 shrink-0 truncate text-sm text-muted" title={lane.label}>
              {lane.label}
            </div>
            {/* Hover rewards curiosity (§9.4): the bar answers with its exact
                traffic + share of the busiest lane. */}
            <TooltipCard
              side="top"
              title={lane.label}
              rows={[{ label: "route-evidence rows", value: lane.value.toLocaleString(), tone: "route" }]}
              derived={{ label: "of busiest lane", value: `${pct}%` }}
              trigger={
                <button
                  type="button"
                  aria-label={`${lane.label}: ${lane.value} route-evidence rows`}
                  className="h-2 flex-1 cursor-default overflow-hidden rounded-pill bg-soft-2 transition-shadow hover:shadow"
                >
                  <span
                    className="block h-2 rounded-pill bg-route"
                    style={{ width: `${Math.max(pct, lane.value > 0 ? 4 : 0)}%` }}
                    aria-hidden
                  />
                </button>
              }
            />
            <div className="mono w-16 shrink-0 text-right text-2xs tabular-nums text-muted">
              {lane.value.toLocaleString()}
            </div>
          </div>
        );
      })}
    </div>
  );
}

export function CostLanes({
  executors,
  generatedAt,
}: {
  executors: ExecutorRow[];
  generatedAt: string;
}) {
  const lanes: Lane[] = executors
    .filter((executor) => executor.routeEvidencePresent || executor.routeEvidenceCount > 0)
    .map((executor) => ({ label: executor.label, value: executor.routeEvidenceCount }))
    .sort((a, b) => b.value - a.value);

  const hasTraffic = lanes.some((lane) => lane.value > 0);

  return (
    <section className="rounded-xl bg-panel p-4 shadow-sm">
      <SectionHeader
        tone="route"
        icon={<CoinsIcon />}
        label="Cost & lane traffic"
        right={<FreshnessChip generatedAt={generatedAt} source="compound executors --json" />}
      />
      <p className="mt-1 text-2xs text-muted">Route-evidence rows per lane — traffic, not dollar spend.</p>

      {hasTraffic ? (
        <div className="mt-4">
          <Bars lanes={lanes} />
        </div>
      ) : (
        <div className="mt-4 rounded-md border border-dashed border-hair bg-page px-4 py-6 text-center">
          <p className="text-sm text-muted">No lane traffic yet. Route evidence is how spend will be reconstructed.</p>
        </div>
      )}

      {/* Doctrine gap: per-lane spend needs the provider ledger cost-doctor reads.
          It isn't wired, so we say so instead of inventing a cost number. */}
      <div className="mt-4 border-t border-hair pt-3">
        <p className="text-2xs leading-relaxed text-faint">
          Per-lane spend is not wired: <span className="mono text-muted">compound route cost-doctor</span> reads a
          provider-calls ledger this home does not keep yet. Point it at one to reconstruct cost.
        </p>
        <div className="mt-2">
          <CopyCmd cmd="compound route cost-doctor path/to/provider-calls.ndjson --tail 200" />
        </div>
      </div>
    </section>
  );
}
