import { DeltaPill, Sparkline, StatusChip } from "@/components/kit";
import { ago } from "@/lib/time";
import {
  formatCount,
  formatCpa,
  formatCtr,
  comparableSpend,
  formatSpend,
  roasExplanation,
  type AdsTotals,
  formatMoney,
  formatRoas,
  productDisplay,
  sparkValues,
  windowDelta,
  type AdsCampaignView,
  type AdsImport,
  type AdsProductView,
} from "@/lib/ads";

/*
 * The three tables under the platform cards: spend by product (with the
 * platform split), the campaign ledger (top by spend), and the import trail
 * (the evidence every number above came from). Dense, tabular, mono numbers.
 */

const TH = "px-2 py-1.5 text-left text-2xs font-bold uppercase tracking-wide text-faint";
const TD = "px-2 py-1.5 align-top text-xs";
const NUM = `${TD} mono tabular-nums text-right`;

function Spend({ totals }: { totals: AdsTotals }) {
  return <span title={comparableSpend(totals) ? undefined : "Spend currencies are mixed or unknown"}>{formatSpend(totals)}</span>;
}

export function AdsProductTable({ products, windowDays }: { products: AdsProductView[]; windowDays: number }) {
  if (products.length === 0) return null;
  return (
    <div className="overflow-x-auto rounded-lg border border-hair bg-panel shadow-sm">
      <table className="w-full border-collapse">
        <thead className="border-b border-hair bg-soft">
          <tr>
            <th className={TH}>Product</th>
            <th className={`${TH} text-right`}>Spend</th>
            <th className={TH}>vs prev.</th>
            <th className={`${TH} text-right`}>Conv.</th>
            <th className={`${TH} text-right`}>CPA</th>
            <th className={`${TH} text-right`}>ROAS</th>
            <th className={TH}>Split by platform</th>
            <th className={TH}>Trend</th>
          </tr>
        </thead>
        <tbody>
          {products.map((p) => {
            const delta = windowDelta(p.totals, p.previous, "spend");
            return (
              <tr key={p.productId} className="border-b border-hair last:border-0">
                <td className={`${TD} font-medium text-ink`}>{productDisplay(p.productId)}</td>
                <td className={NUM}>
                  <Spend totals={p.totals} />
                </td>
                <td className={TD}>
                  {delta != null ? <DeltaPill value={delta} suffix="%" goodDirection="down" title={`vs previous ${windowDays} days`} showZero /> : <span className="text-faint">—</span>}
                </td>
                <td className={NUM}>{formatCount(p.totals.conversions)}</td>
                <td className={NUM}>{formatCpa(p.totals)}</td>
                <td className={NUM} title={roasExplanation(p.totals)}>{formatRoas(p.totals)}</td>
                <td className={TD}>
                  <div className="flex flex-wrap gap-1">
                    {p.byPlatform.map((s) => (
                      <span
                        key={s.platform}
                        className="inline-flex items-center gap-1 rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs font-semibold text-muted"
                        title={comparableSpend(p.totals) ? `${formatMoney(s.spend, p.totals.currency)} on ${s.label}` : "Spend share unavailable across mixed or unknown currencies"}
                      >
                        {s.label}
                        {comparableSpend(p.totals) ? <span className="mono tabular-nums text-faint">{Math.round(s.share * 100)}%</span> : null}
                      </span>
                    ))}
                  </div>
                </td>
                <td className={`${TD} text-work`}>
                  <Sparkline values={sparkValues(p.daily, "spend")} width={80} height={20} />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function AdsCampaignTable({ campaigns, windowDays }: { campaigns: AdsCampaignView[]; windowDays: number }) {
  if (campaigns.length === 0) return null;
  return (
    <div className="overflow-x-auto rounded-lg border border-hair bg-panel shadow-sm">
      <table className="w-full border-collapse">
        <thead className="border-b border-hair bg-soft">
          <tr>
            <th className={TH}>Campaign</th>
            <th className={TH}>Platform</th>
            <th className={TH}>Product</th>
            <th className={TH}>State</th>
            <th className={`${TH} text-right`}>Spend</th>
            <th className={TH}>vs prev.</th>
            <th className={`${TH} text-right`}>Impr.</th>
            <th className={`${TH} text-right`}>CTR</th>
            <th className={`${TH} text-right`}>Conv.</th>
            <th className={`${TH} text-right`}>CPA</th>
            <th className={TH}>Last row</th>
            <th className={TH}>Trend</th>
          </tr>
        </thead>
        <tbody>
          {campaigns.map((c) => {
            const delta = windowDelta(c.totals, c.previous, "spend");
            const key = `${c.platform}:${c.productId}:${c.account}:${c.campaignId}`;
            return (
              <tr key={key} className="border-b border-hair last:border-0">
                <td className={`${TD} max-w-[22ch]`}>
                  <div className="truncate font-medium text-ink" title={c.campaignName}>
                    {c.campaignName}
                  </div>
                  <div className="mono truncate text-2xs text-faint" title={c.sourceRef}>
                    {c.campaignId}
                    {c.account ? ` · ${c.account}` : ""}
                  </div>
                </td>
                <td className={`${TD} text-muted`}>{c.label}</td>
                <td className={`${TD} text-muted`}>{productDisplay(c.productId)}</td>
                <td className={TD}>
                  <StatusChip tone={c.status === "active" ? "good" : "muted"} label={c.status} />
                </td>
                <td className={NUM}>
                  <Spend totals={c.totals} />
                </td>
                <td className={TD}>
                  {delta != null ? <DeltaPill value={delta} suffix="%" goodDirection="down" title={`vs previous ${windowDays} days`} showZero /> : <span className="text-faint">—</span>}
                </td>
                <td className={NUM}>{formatCount(c.totals.impressions)}</td>
                <td className={NUM}>{formatCtr(c.totals)}</td>
                <td className={NUM}>{formatCount(c.totals.conversions)}</td>
                <td className={NUM}>{formatCpa(c.totals)}</td>
                <td className={`${TD} mono text-faint`}>{c.lastDate}</td>
                <td className={`${TD} text-work`}>
                  <Sparkline values={sparkValues(c.daily, "spend")} width={72} height={18} />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function AdsImportList({ imports }: { imports: AdsImport[] }) {
  if (imports.length === 0) return null;
  return (
    <ul className="flex flex-col divide-y divide-hair rounded-lg border border-hair bg-panel shadow-sm">
      {imports.map((i) => (
        <li key={i.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-xs">
          <span className="font-medium text-ink">{i.sourceFile}</span>
          <span className="text-muted">
            {i.platform.replace(/_/g, " ")} · {productDisplay(i.productId)}
            {i.account ? ` · ${i.account}` : ""}
          </span>
          <span className="mono tabular-nums text-faint">
            {i.rowsNew} new · {i.rowsUpdated} updated · {i.rowsUnchanged} unchanged
            {i.rowsSkipped > 0 ? ` · ${i.rowsSkipped} skipped` : ""}
          </span>
          <span className="mono text-faint">
            {i.dateStart}..{i.dateEnd}
            {i.currency ? ` · ${i.currency}` : ""}
          </span>
          <span className="ml-auto mono text-2xs text-faint" title={`sha256:${i.sourceSha256}`}>
            {ago(i.importedAt)} · sha {i.sourceSha256.slice(0, 8)}
          </span>
        </li>
      ))}
    </ul>
  );
}
