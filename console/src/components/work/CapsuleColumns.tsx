import { CopyCmd } from "@/components/CopyCmd";
import { CapsuleColumn, EmptyState, FreshnessChip, RouteIcon, SectionHeader } from "@/components/kit";
import type { Tone, TooltipRow } from "@/components/kit";
import type { LaneTrafficReport, LaneTrafficRow } from "@/lib/lane-traffic";
import { WORK_COPY } from "@/lib/work-copy";

/**
 * CapsuleColumns (DESIGN.md §9.6) — lane traffic and account capacity in one
 * glance. Capsule HEIGHT is traffic (route-evidence rows in the last ~200,
 * scaled to the busiest lane — same "traffic, not spend" honesty contract as
 * agents/CostLanes.tsx and routing/RouteTraffic.tsx). Capsule TONE is account
 * HEALTH (healthy vs walled accounts, from projections/account-projection.json)
 * — a provider with traffic but no account projection renders neutral; a
 * health verdict is never fabricated from absent data.
 */
function toneFor(row: LaneTrafficRow): Tone {
  if (row.totalAccountCount === 0) return "neutral";
  return row.healthyAccountCount > 0 ? "good" : "crit";
}

function rowsFor(row: LaneTrafficRow): TooltipRow[] {
  const tone = toneFor(row);
  const rows: TooltipRow[] = [
    {
      label: "healthy accounts",
      value: row.totalAccountCount > 0 ? `${row.healthyAccountCount}/${row.totalAccountCount}` : "unknown",
      tone,
    },
  ];
  if (row.bestRemainingSummary) rows.push({ label: "best remaining", value: row.bestRemainingSummary, tone: "neutral" });
  rows.push({ label: "errors", value: String(row.errorCount), tone: row.errorCount > 0 ? "crit" : "neutral" });
  rows.push({ label: "avg latency", value: `${row.avgLatencyMs}ms`, tone: "neutral" });
  if (row.totalTokens > 0) rows.push({ label: "tokens", value: row.totalTokens.toLocaleString("en-US"), tone: "neutral" });
  return rows;
}

export function CapsuleColumns({ report }: { report: LaneTrafficReport }) {
  const { rows, accountProjection } = report;
  const max = Math.max(1, ...rows.map((r) => r.requestCount));

  return (
    <section className="rounded-xl bg-panel p-4 shadow-sm">
      <SectionHeader
        tone="route"
        icon={<RouteIcon />}
        label="Lane traffic"
        sub="requests per provider, last 200 — capsule tone is account health"
        right={
          <FreshnessChip
            generatedAt={accountProjection?.importedAt}
            source="projections/account-projection.json · compound route evidence-authority --json --recent 200"
            staleAfterMs={6 * 60 * 60 * 1000}
          />
        }
      />

      {rows.length ? (
        <div className="mt-4 flex flex-wrap items-end gap-4">
          {rows.map((row) => (
            <CapsuleColumn
              key={row.provider}
              value={row.requestCount}
              max={max}
              tone={toneFor(row)}
              label={row.provider}
              caption={row.provider}
              unit="requests"
              extraRows={rowsFor(row)}
            />
          ))}
        </div>
      ) : (
        <div className="mt-4">
          <EmptyState>{WORK_COPY.laneTraffic.noTraffic}</EmptyState>
        </div>
      )}

      {!accountProjection ? (
        <div className="mt-4 border-t border-hair pt-3">
          <p className="text-2xs leading-relaxed text-faint">
            {WORK_COPY.laneTraffic.noAccountProjection.prose}
          </p>
          <div className="mt-2">
            <CopyCmd cmd={WORK_COPY.laneTraffic.noAccountProjection.command} />
          </div>
        </div>
      ) : null}
    </section>
  );
}
