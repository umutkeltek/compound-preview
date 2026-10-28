import { CopyCmd } from "@/components/CopyCmd";
import { DeltaPill, Sparkline, StatusChip } from "@/components/kit";
import { ago } from "@/lib/time";
import {
  formatCount,
  formatCpa,
  formatCpc,
  formatCpm,
  formatCtr,
  formatSpend,
  formatRoas,
  roasExplanation,
  platformStatusLabel,
  productDisplay,
  sparkValues,
  windowDelta,
  type AdsPlatformView,
} from "@/lib/ads";

/*
 * One ad platform = one card. The registry is rendered whole, so a platform
 * with no rows is a card that says "not connected" and carries the exact
 * import command — the absence is a state, not a missing tile. A connected
 * card is spend + a 30-day whisper + the four unit-economics numbers, each
 * of which reads "—" when its denominator is missing.
 */

function Metric({ label, value, title }: { label: string; value: string; title?: string }) {
  return (
    <div className="min-w-0" title={title}>
      <div className="text-2xs font-bold uppercase tracking-wide text-faint">{label}</div>
      <div className="mono mt-0.5 truncate text-sm tabular-nums text-ink">{value}</div>
    </div>
  );
}

export function AdsPlatformCard({ view, windowDays }: { view: AdsPlatformView; windowDays: number }) {
  const status = platformStatusLabel(view.status);
  const t = view.totals;
  const connected = view.status !== "not_connected";
  const spendDelta = windowDelta(t, view.previous, "spend");
  const spendLabel = formatSpend(t);
  const seam = view.status === "stale";

  return (
    <article className={`flex flex-col gap-3 rounded-lg border border-hair bg-panel p-4 shadow-sm ${seam ? "admission-seam" : ""}`}>
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h3 className="text-md font-semibold text-ink">{view.label}</h3>
          <p className="mt-0.5 text-2xs text-faint">
            {connected
              ? `${view.campaignsActive} of ${view.campaignsTotal} campaign${view.campaignsTotal === 1 ? "" : "s"} active · rows to ${view.lastDate}`
              : "no report imported"}
          </p>
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1.5">
          <StatusChip tone={status.tone} label={status.label} pulse={view.status === "connected"} />
          {view.lastImportAt ? <span className="mono text-2xs text-faint">imported {ago(view.lastImportAt)}</span> : null}
        </div>
      </div>

      {connected ? (
        <>
          <div className="flex items-end justify-between gap-3">
            <div className="min-w-0">
              <div className="text-2xs font-bold uppercase tracking-wide text-faint">Spend · last {windowDays} days</div>
              <div className="mono mt-0.5 flex items-baseline gap-2 text-xl tabular-nums text-ink">
                <span className="truncate">{spendLabel}</span>
                {spendDelta != null ? (
                  <DeltaPill value={spendDelta} suffix="%" goodDirection="down" title={`vs previous ${windowDays} days`} showZero />
                ) : null}
              </div>
            </div>
            <span className="shrink-0 text-work">
              <Sparkline values={sparkValues(view.daily, "spend")} width={96} height={28} />
            </span>
          </div>

          <div className="grid grid-cols-4 gap-2 rounded-md border border-hair bg-soft px-3 py-2">
            <Metric label="Impr." value={formatCount(t.impressions)} />
            <Metric label="Clicks" value={formatCount(t.clicks)} title="taps / link clicks as the platform reports them" />
            <Metric label="Conv." value={formatCount(t.conversions)} title="installs / results / conversions as the platform reports them" />
            <Metric label="CTR" value={formatCtr(t)} />
            <Metric label="CPC" value={formatCpc(t)} />
            <Metric label="CPA" value={formatCpa(t)} title="spend ÷ conversions — reads — with no conversions" />
            <Metric label="ROAS" value={formatRoas(t)} title={roasExplanation(t)} />
            <Metric label="CPM" value={formatCpm(t)} />
          </div>
          {formatRoas(t) === "—" ? <p className="text-2xs text-faint">{roasExplanation(t)}</p> : null}

          {view.products.length > 0 ? (
            <div className="flex flex-wrap items-center gap-1.5 text-2xs text-faint">
              <span>products</span>
              {view.products.map((p) => (
                <span key={p} className="inline-flex items-center rounded-pill border border-hair bg-soft px-2 py-0.5 font-semibold text-muted">
                  {productDisplay(p)}
                </span>
              ))}
            </div>
          ) : null}
        </>
      ) : (
        <p className="text-xs leading-relaxed text-muted">{view.exportHint}</p>
      )}

      <div className="rounded-md border border-hair bg-soft px-3 py-2.5">
        <div className="section-label mb-1.5">Next move</div>
        <p className="mb-2 text-xs text-ink">{view.nextMove.text}</p>
        {view.nextMove.command ? <CopyCmd cmd={view.nextMove.command} /> : null}
      </div>
    </article>
  );
}
