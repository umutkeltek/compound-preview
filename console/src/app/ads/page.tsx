import Link from "next/link";

import { AdsPlatformCard } from "@/components/ads/AdsPlatformCard";
import { AdsCampaignTable, AdsImportList, AdsProductTable } from "@/components/ads/AdsTables";
import { CopyCmd } from "@/components/CopyCmd";
import { BarChartIcon, CoinsIcon, EmptyState, FeedDegraded, FreshnessChip, PageHeader, PlugIcon, SectionHeader, SourceIcon, TableIcon } from "@/components/kit";
import {
  ADS_WINDOWS,
  adsHeadline,
  formatCount,
  formatCpa,
  formatCtr,
  comparableSpend,
  formatSpend,
  roasExplanation,
  formatRoas,
  getAdsProjection,
  orderPlatforms,
  parseAdsWindowParam,
  productDisplay,
  windowDelta,
  type AdsProjection,
} from "@/lib/ads";
import { DeltaPill } from "@/components/kit";

export const dynamic = "force-dynamic";

/*
 * Ads — "what am I paying to acquire, on every platform, and is it working?"
 * Lives under Outcomes because ad spend is product feedback: the same
 * change → signal → hypothesis → review loop, fed by each platform's own
 * report export through `compound product ads import`. The page is a pure
 * projection of `compound product ads --json`: no platform is contacted, no
 * number is computed here that the ledger cannot back, and every platform
 * in the registry is rendered — "not connected" is a state with a command,
 * not a missing card.
 */

function StatTile({ label, value, sub, delta, windowDays }: { label: string; value: string; sub?: string; delta?: number | null; windowDays: number }) {
  return (
    <div className="rounded-lg border border-hair bg-panel px-3 py-2.5 shadow-sm">
      <div className="text-2xs font-bold uppercase tracking-wide text-faint">{label}</div>
      <div className="mono mt-1 flex items-baseline gap-2 text-xl tabular-nums text-ink">
        <span className="truncate">{value}</span>
        {delta != null ? <DeltaPill value={delta} suffix="%" goodDirection="down" title={`vs previous ${windowDays} days`} showZero /> : null}
      </div>
      {sub ? <div className="mt-0.5 truncate text-2xs text-faint">{sub}</div> : null}
    </div>
  );
}

function WindowSwitch({ active, product }: { active: number; product: string }) {
  return (
    <div className="inline-flex items-center rounded-pill border border-hair bg-soft p-0.5" role="group" aria-label="Window">
      {ADS_WINDOWS.map((days) => {
        const isActive = days === active;
        const href = product ? `/ads?window=${days}&product=${encodeURIComponent(product)}` : `/ads?window=${days}`;
        return (
          <Link
            key={days}
            href={href}
            aria-current={isActive ? "true" : undefined}
            className={`mono rounded-pill px-2.5 py-0.5 text-2xs font-semibold tabular-nums transition-colors ${
              isActive ? "bg-panel text-ink shadow-sm" : "text-faint hover:text-muted"
            }`}
          >
            {days}d
          </Link>
        );
      })}
    </div>
  );
}

function ProductFilter({ projection, active, windowDays }: { projection: AdsProjection; active: string; windowDays: number }) {
  const ids = new Set<string>();
  for (const p of projection.platforms) for (const id of p.products) ids.add(id);
  for (const p of projection.products) ids.add(p.productId);
  if (active) ids.add(active);
  const list = [...ids].sort();
  if (list.length === 0) return null;
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <Link
        href={`/ads?window=${windowDays}`}
        className={`rounded-pill border px-2 py-0.5 text-2xs font-semibold ${!active ? "border-work-line bg-work-soft text-work" : "border-hair bg-soft text-faint hover:text-muted"}`}
      >
        all products
      </Link>
      {list.map((id) => (
        <Link
          key={id}
          href={`/ads?window=${windowDays}&product=${encodeURIComponent(id)}`}
          className={`rounded-pill border px-2 py-0.5 text-2xs font-semibold ${active === id ? "border-work-line bg-work-soft text-work" : "border-hair bg-soft text-faint hover:text-muted"}`}
        >
          {productDisplay(id)}
        </Link>
      ))}
    </div>
  );
}

export default async function AdsPage({ searchParams }: { searchParams: Promise<{ window?: string; product?: string }> }) {
  const params = await searchParams;
  const windowDays = parseAdsWindowParam(params.window);
  const product = typeof params.product === "string" ? params.product.trim() : "";
  const read = await getAdsProjection({ window: windowDays, product: product || undefined });
  const projection = read.projection;

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        breadcrumb="outcomes / ads"
        title="Ads"
        right={
          <>
            <WindowSwitch active={windowDays} product={product} />
            <FreshnessChip generatedAt={projection?.generatedAt} source={read.source} staleAfterMs={10 * 60_000} />
          </>
        }
      />
      <p className="text-sm text-muted" style={{ maxWidth: "var(--read-max)" }}>
        Paid acquisition on every platform, read from each platform&apos;s own report export. Compound never calls an ad
        network: you export a daily campaign report, import it, and this panel folds the rows — spend, clicks,
        conversions, CPA, ROAS — per platform, product, and campaign. A connected platform can feed the loop as a
        product signal; a stale one carries the amber seam until a fresh report lands.
      </p>

      {read.status === "unreachable" || !projection ? (
        <FeedDegraded label="Ads" source={read.source} lastGoodAt={read.lastGoodAt} />
      ) : (
        <AdsBody projection={projection} windowDays={windowDays} product={product} stale={read.status === "stale"} />
      )}
    </div>
  );
}

function AdsBody({ projection, windowDays, product, stale }: { projection: AdsProjection; windowDays: number; product: string; stale: boolean }) {
  const headline = adsHeadline(projection);
  const t = projection.totals;
  const platforms = orderPlatforms(projection.platforms);
  const spendValue = formatSpend(t, { compact: t.spend >= 10_000 });
  const importCmd = `compound product ads import --platform <platform> --product ${product || "<product:id>"} --report <export.csv>`;

  return (
    <>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2 text-sm">
          <span
            className={`h-1.5 w-1.5 rounded-full ${headline.state === "live" ? "bg-good" : headline.state === "quiet" ? "bg-seam" : "bg-faint"}`}
            aria-hidden
          />
          <span className="text-ink">{headline.text}</span>
          {stale ? <span className="text-2xs text-seam">projection served from cache</span> : null}
        </div>
        <ProductFilter projection={projection} active={product} windowDays={windowDays} />
      </div>

      {headline.state === "empty" ? (
        <EmptyState cta={<CopyCmd cmd={importCmd} />}>
          Export a daily campaign report from any platform below and import it. Each card carries its own exact
          command; <span className="mono">compound product ads platforms</span> lists the headers every dialect
          understands.
        </EmptyState>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-6">
          <StatTile label="Spend" value={spendValue} sub={t.mixedCurrency ? "mixed currencies — not summed" : `${t.rows} rows · ${t.days} days with data`} delta={windowDelta(t, projection.previous, "spend")} windowDays={windowDays} />
          <StatTile label="Impressions" value={formatCount(t.impressions)} delta={windowDelta(t, projection.previous, "impressions")} windowDays={windowDays} />
          <StatTile label="Clicks" value={formatCount(t.clicks)} sub={formatCtr(t) === "—" ? "no impressions" : `CTR ${formatCtr(t)}`} delta={windowDelta(t, projection.previous, "clicks")} windowDays={windowDays} />
          <StatTile label="Conversions" value={formatCount(t.conversions)} sub="installs / results as reported" delta={windowDelta(t, projection.previous, "conversions")} windowDays={windowDays} />
          <StatTile label="CPA" value={formatCpa(t)} sub={!comparableSpend(t) ? "currency unresolved" : t.conversions > 0 ? "spend ÷ conversions" : "no conversions"} windowDays={windowDays} />
          <StatTile label="ROAS" value={formatRoas(t)} sub={roasExplanation(t)} windowDays={windowDays} />
        </div>
      )}

      <section className="flex flex-col gap-3">
        <SectionHeader
          tone="product"
          icon={<PlugIcon />}
          label="Platforms"
          sub="every platform, connected or not — current first, then stale, then waiting for a report"
          right={
            <span className="mono text-2xs tabular-nums text-faint">
              {projection.counts.platformsConnected} current · {projection.counts.platformsStale} stale · {projection.counts.platformsNotConnected} not connected
            </span>
          }
        />
        <div className="grid gap-4 lg:grid-cols-2 2xl:grid-cols-3">
          {platforms.map((view) => (
            <AdsPlatformCard key={view.platform} view={view} windowDays={windowDays} />
          ))}
        </div>
      </section>

      {projection.products.length > 0 ? (
        <section className="flex flex-col gap-3">
          <SectionHeader tone="product" icon={<CoinsIcon />} label="By product" sub="where the money goes, and what it buys" right={<span className="mono text-2xs tabular-nums text-faint">{projection.products.length}</span>} />
          <AdsProductTable products={projection.products} windowDays={windowDays} />
        </section>
      ) : null}

      {projection.campaigns.length > 0 ? (
        <section className="flex flex-col gap-3">
          <SectionHeader
            tone="product"
            icon={<TableIcon />}
            label="Campaigns"
            sub={`by spend in the last ${windowDays} days · active = a row within the last 3 days`}
            right={
              <span className="mono text-2xs tabular-nums text-faint">
                {projection.counts.campaignsActive} active of {projection.counts.campaigns}
              </span>
            }
          />
          <AdsCampaignTable campaigns={projection.campaigns} windowDays={windowDays} />
        </section>
      ) : null}

      {projection.gaps.length > 0 ? (
        <section className="flex flex-col gap-3">
          <SectionHeader tone="seam" icon={<BarChartIcon />} label="Gaps" sub="what this panel cannot yet say, and the command that closes each gap" right={<span className="mono text-2xs tabular-nums text-faint">{projection.gaps.length}</span>} />
          <ul className="flex flex-col gap-2">
            {projection.gaps.map((g, i) => (
              <li key={`${g.kind}:${g.platform}:${i}`} className="flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-md border border-hair bg-panel px-3 py-2 text-xs">
                <span className="mono rounded-pill border border-hair bg-soft px-2 py-0.5 text-2xs font-semibold text-muted">{g.kind.replace(/_/g, " ")}</span>
                <span className="text-ink">{g.text}</span>
                {g.command ? (
                  <span className="ml-auto">
                    <CopyCmd cmd={g.command} compact />
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
        </section>
      ) : null}

      {projection.imports.length > 0 ? (
        <section className="flex flex-col gap-3">
          <SectionHeader tone="neutral" icon={<SourceIcon />} label="Imports" sub="the evidence trail — each report, hashed, with what it changed" right={<span className="mono text-2xs tabular-nums text-faint">{projection.counts.imports} total</span>} />
          <AdsImportList imports={projection.imports} />
        </section>
      ) : null}

      <p className="mono text-2xs leading-snug text-faint">
        source: compound product ads --json · window {projection.window.start}..{projection.window.end} (previous {projection.window.previousStart}..{projection.window.previousEnd}) · authority {projection.authorityRef} · ratios read — when their denominator is missing; mixed currencies are never summed.
      </p>
    </>
  );
}
