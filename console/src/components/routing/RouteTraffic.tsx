import { CopyCmd } from "@/components/CopyCmd";
import { BarChartIcon, Bars, CheckIcon, FreshnessChip, RouteIcon, SectionHeader } from "@/components/kit";
import type { RouteTrafficReport } from "@/lib/routing";

/**
 * Route traffic & cost — "at what cost is routing sending work?" Reads
 * `compound route evidence-authority --json`. Same honesty contract as Agents'
 * CostLanes (src/components/agents/CostLanes.tsx): route-evidence rows per
 * provider are the CHEAP, LIVE traffic signal — dollar spend is only known for
 * the few rows the provider ledger covers, so we chart traffic, state the token
 * total, and name the spend gap instead of inventing a cost. Bars use the kit
 * <Bars> primitive (the shared version of CostLanes' local bars), route tone.
 */

function StatTile({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="rounded-lg border border-hair bg-panel px-3 py-2.5 shadow-sm">
      <div className="text-2xs font-bold uppercase tracking-wide text-faint">{label}</div>
      <div className="mono mt-1 text-xl tabular-nums text-ink">{value}</div>
      {sub ? <div className="mt-0.5 text-2xs text-faint">{sub}</div> : null}
    </div>
  );
}

/** micros → "$0.0058" (or "$0.00" when nothing is known). */
function formatMicros(micros: number): string {
  return `$${(micros / 1_000_000).toFixed(4)}`;
}

export function RouteTraffic({ report }: { report: RouteTrafficReport | null }) {
  if (!report) {
    return (
      <section className="rounded-lg border border-dashed border-hair bg-page px-4 py-6 text-center">
        <p className="text-sm text-muted">
          No route-evidence authority yet. Traffic reads{" "}
          <span className="mono text-muted">compound route evidence-authority --json</span>.
        </p>
      </section>
    );
  }

  const providerBars = report.providerCounts.slice(0, 8).map((row) => ({
    label: row.label,
    value: row.value,
    tone: "route" as const,
  }));
  const statusBars = report.statusCounts.map((row) => ({
    label: row.label,
    value: row.value,
    tone: (row.label === "failed" ? "crit" : row.label === "completed" || row.label === "ok" ? "good" : "muted") as
      | "crit"
      | "good"
      | "muted",
  }));
  const costCoverage =
    report.tokenUsageRows > 0 ? Math.round((report.costKnownRows / report.tokenUsageRows) * 100) : 0;

  return (
    <section className="flex flex-col gap-4">
      <SectionHeader
        tone="route"
        icon={<RouteIcon />}
        label="Route traffic & cost"
        sub="rows per provider — traffic, not dollar spend"
        right={
          <FreshnessChip
            generatedAt={report.latestEvidenceAt}
            source="compound route evidence-authority --json · latest route-evidence row"
          />
        }
      />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatTile label="Route evidence" value={report.routeEvidenceCount.toLocaleString("en-US")} sub="rows recorded" />
        <StatTile label="Token rows" value={report.tokenUsageRows.toLocaleString("en-US")} sub="with token usage" />
        <StatTile label="Total tokens" value={report.totalTokens.toLocaleString("en-US")} sub="across all lanes" />
        <StatTile
          label="Cost known"
          value={formatMicros(report.totalCostMicros)}
          sub={`${report.costKnownRows} of ${report.tokenUsageRows} rows (${costCoverage}%)`}
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <div className="rounded-xl bg-panel p-4 shadow-sm">
          <SectionHeader tone="route" icon={<BarChartIcon />} label="Traffic by provider" />
          {providerBars.length ? (
            <div className="mt-3">
              <Bars items={providerBars.map((b) => ({ ...b, unit: "route-evidence rows" }))} withTooltip />
            </div>
          ) : (
            <p className="mt-3 text-sm text-muted">No lane traffic yet.</p>
          )}
        </div>
        <div className="rounded-xl bg-panel p-4 shadow-sm">
          <SectionHeader tone="good" icon={<CheckIcon />} label="Outcomes" />
          {statusBars.length ? (
            <div className="mt-3">
              <Bars items={statusBars.map((b) => ({ ...b, unit: "rows" }))} withTooltip />
            </div>
          ) : (
            <p className="mt-3 text-sm text-muted">No recorded outcomes yet.</p>
          )}
          {report.sourceRole || report.fallbackRole ? (
            <p className="mt-3 border-t border-hair pt-3 text-2xs leading-relaxed text-faint">
              Roles: source <span className="mono text-muted">{report.sourceRole || "—"}</span> · fallback{" "}
              <span className="mono text-muted">{report.fallbackRole || "—"}</span>
            </p>
          ) : null}
        </div>
      </div>

      {/* Doctrine gap: per-lane DOLLAR spend needs the provider-calls ledger the
          cost-doctor reads. Only a handful of rows carry known cost, so we name
          the gap rather than inventing a number (same as Agents' CostLanes). */}
      <div className="rounded-lg border border-hair bg-soft px-4 py-3">
        <p className="text-2xs leading-relaxed text-faint">
          Per-lane dollar spend is only partially wired: cost is known for{" "}
          <span className="mono text-muted">
            {report.costKnownRows} of {report.tokenUsageRows}
          </span>{" "}
          token rows. The rest is token traffic only — point{" "}
          <span className="mono text-muted">compound route cost-doctor</span> at a provider-calls ledger to reconstruct
          the rest.
        </p>
        <div className="mt-2">
          <CopyCmd cmd="compound route cost-doctor path/to/provider-calls.ndjson --tail 200" />
        </div>
      </div>
    </section>
  );
}
