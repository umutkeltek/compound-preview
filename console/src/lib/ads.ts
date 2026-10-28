import { arr, num, runProjection, str, type Json, type ProjectionStatus } from "@/lib/compound";
import type { Tone } from "@/components/kit/tone";

/** Strict boolean read: only a JSON `true` is true (never "true", 1, or a truthy object). */
const bool = (value: unknown): boolean => value === true;

/*
 * Ads panel data plane — a typed view over `compound product ads --json`
 * (read-only projection of the ads ledger). The console never touches the
 * ledger or an ad platform: every number here was imported from a platform's
 * own report export and folded by the projection. This module is pure
 * mapping + derivation so the selftest can prove the panel is honest about
 * undefined ratios, mixed currencies, and not-connected platforms.
 */

export type AdsWindowDays = 7 | 14 | 30 | 90;
export const ADS_WINDOWS: AdsWindowDays[] = [7, 14, 30, 90];
export const ADS_DEFAULT_WINDOW: AdsWindowDays = 30;

export type AdsTotals = {
  spend: number;
  impressions: number;
  clicks: number;
  conversions: number;
  revenue: number;
  hasRevenue: boolean;
  revenueCoverage: number | null;
  spendUnknownCurrency: number;
  rowsUnknownCurrency: number;
  ctr: number;
  cpc: number;
  cpa: number;
  cpm: number;
  roas: number;
  currency: string;
  mixedCurrency: boolean;
  spendByCurrency: Record<string, number>;
  days: number;
  rows: number;
};

export type AdsDailyPoint = {
  date: string;
  currency: string;
  spend: number | null;
  impressions: number;
  clicks: number;
  conversions: number;
  revenue: number | null;
};

export type AdsPlatformStatus = "connected" | "stale" | "not_connected";

export type AdsPlatformView = {
  platform: string;
  label: string;
  status: AdsPlatformStatus;
  exportHint: string;
  firstDate: string;
  lastDate: string;
  lastImportAt: string;
  daysSinceLastDate: number;
  totals: AdsTotals;
  previous: AdsTotals;
  daily: AdsDailyPoint[];
  products: string[];
  campaignsActive: number;
  campaignsTotal: number;
  nextMove: { text: string; command: string };
};

export type AdsPlatformShare = { platform: string; label: string; spend: number; share: number };

export type AdsProductView = {
  productId: string;
  totals: AdsTotals;
  previous: AdsTotals;
  byPlatform: AdsPlatformShare[];
  daily: AdsDailyPoint[];
};

export type AdsCampaignView = {
  platform: string;
  label: string;
  productId: string;
  account: string;
  campaignId: string;
  campaignName: string;
  status: "active" | "quiet";
  firstDate: string;
  lastDate: string;
  totals: AdsTotals;
  previous: AdsTotals;
  daily: AdsDailyPoint[];
  sourceRef: string;
};

export type AdsImport = {
  id: string;
  platform: string;
  productId: string;
  account: string;
  sourceFile: string;
  sourceSha256: string;
  rowsParsed: number;
  rowsNew: number;
  rowsUpdated: number;
  rowsUnchanged: number;
  rowsSkipped: number;
  dateStart: string;
  dateEnd: string;
  currency: string;
  importedAt: string;
};

export type AdsGap = { kind: string; platform: string; productId: string; text: string; command: string };

export type AdsProjection = {
  schemaVersion: string;
  authorityRef: string;
  generatedAt: string;
  status: "ok" | "quiet" | "empty" | string;
  asOf: string;
  window: { days: number; start: string; end: string; previousStart: string; previousEnd: string };
  filters: { productId: string; platform: string };
  counts: {
    rows: number;
    rowsInWindow: number;
    campaigns: number;
    campaignsActive: number;
    products: number;
    platformsConnected: number;
    platformsStale: number;
    platformsNotConnected: number;
    imports: number;
  };
  totals: AdsTotals;
  previous: AdsTotals;
  platforms: AdsPlatformView[];
  products: AdsProductView[];
  campaigns: AdsCampaignView[];
  imports: AdsImport[];
  gaps: AdsGap[];
};

export type AdsRead = {
  status: ProjectionStatus;
  projection: AdsProjection | null;
  lastGoodAt: string | null;
  source: string;
};

/* ---------- mapping (untrusted JSON → typed) ---------- */

function rec(v: unknown): Record<string, unknown> {
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}

export function mapTotals(v: unknown): AdsTotals {
  const o = rec(v);
  const byCurrency: Record<string, number> = {};
  for (const [k, val] of Object.entries(rec(o.spend_by_currency))) {
    if (typeof val === "number" && Number.isFinite(val)) byCurrency[k] = val;
  }
  return {
    spend: num(o.spend),
    impressions: num(o.impressions),
    clicks: num(o.clicks),
    conversions: num(o.conversions),
    revenue: num(o.revenue),
    hasRevenue: bool(o.has_revenue),
    revenueCoverage: typeof o.revenue_coverage === "number" && Number.isFinite(o.revenue_coverage) && o.revenue_coverage >= 0 && o.revenue_coverage <= 1 ? o.revenue_coverage : null,
    spendUnknownCurrency: num(o.spend_unknown_currency),
    rowsUnknownCurrency: num(o.rows_unknown_currency),
    ctr: num(o.ctr),
    cpc: num(o.cpc),
    cpa: num(o.cpa),
    cpm: num(o.cpm),
    roas: num(o.roas),
    currency: str(o.currency),
    mixedCurrency: bool(o.mixed_currency),
    spendByCurrency: byCurrency,
    days: num(o.days),
    rows: num(o.rows),
  };
}

function mapDaily(v: unknown): AdsDailyPoint[] {
  return arr(v).map((p) => {
    const o = rec(p);
    return {
      date: str(o.date),
      currency: str(o.currency),
      spend: typeof o.spend === "number" && Number.isFinite(o.spend) && knownCurrency(str(o.currency)) ? o.spend : null,
      impressions: num(o.impressions),
      clicks: num(o.clicks),
      conversions: num(o.conversions),
      revenue: typeof o.revenue === "number" && Number.isFinite(o.revenue) && knownCurrency(str(o.currency)) ? o.revenue : null,
    };
  });
}

function mapPlatformStatus(v: unknown): AdsPlatformStatus {
  const s = str(v);
  return s === "connected" || s === "stale" ? s : "not_connected";
}

export function mapAdsProjection(j: Json | null): AdsProjection | null {
  if (!j) return null;
  const o = rec(j);
  const window = rec(o.window);
  const counts = rec(o.counts);
  const filters = rec(o.filters);
  return {
    schemaVersion: str(o.schema_version),
    authorityRef: str(o.authority_ref),
    generatedAt: str(o.generated_at),
    status: str(o.status) || "empty",
    asOf: str(o.as_of),
    window: {
      days: num(window.days),
      start: str(window.start),
      end: str(window.end),
      previousStart: str(window.previous_start),
      previousEnd: str(window.previous_end),
    },
    filters: { productId: str(filters.product_id), platform: str(filters.platform) },
    counts: {
      rows: num(counts.rows),
      rowsInWindow: num(counts.rows_in_window),
      campaigns: num(counts.campaigns),
      campaignsActive: num(counts.campaigns_active),
      products: num(counts.products),
      platformsConnected: num(counts.platforms_connected),
      platformsStale: num(counts.platforms_stale),
      platformsNotConnected: num(counts.platforms_not_connected),
      imports: num(counts.imports),
    },
    totals: mapTotals(o.totals),
    previous: mapTotals(o.previous),
    platforms: arr(o.platforms).map((p) => {
      const x = rec(p);
      const move = rec(x.next_move);
      return {
        platform: str(x.platform),
        label: str(x.label) || str(x.platform),
        status: mapPlatformStatus(x.status),
        exportHint: str(x.export_hint),
        firstDate: str(x.first_date),
        lastDate: str(x.last_date),
        lastImportAt: str(x.last_import_at),
        daysSinceLastDate: num(x.days_since_last_date),
        totals: mapTotals(x.totals),
        previous: mapTotals(x.previous),
        daily: mapDaily(x.daily),
        products: arr(x.products).map(str).filter(Boolean),
        campaignsActive: num(x.campaigns_active),
        campaignsTotal: num(x.campaigns_total),
        nextMove: { text: str(move.text), command: str(move.command) },
      };
    }),
    products: arr(o.products).map((p) => {
      const x = rec(p);
      return {
        productId: str(x.product_id),
        totals: mapTotals(x.totals),
        previous: mapTotals(x.previous),
        byPlatform: arr(x.by_platform).map((s) => {
          const y = rec(s);
          return { platform: str(y.platform), label: str(y.label) || str(y.platform), spend: num(y.spend), share: num(y.share) };
        }),
        daily: mapDaily(x.daily),
      };
    }),
    campaigns: arr(o.campaigns).map((c) => {
      const x = rec(c);
      return {
        platform: str(x.platform),
        label: str(x.label) || str(x.platform),
        productId: str(x.product_id),
        account: str(x.account),
        campaignId: str(x.campaign_id),
        campaignName: str(x.campaign_name) || str(x.campaign_id),
        status: str(x.status) === "active" ? "active" : "quiet",
        firstDate: str(x.first_date),
        lastDate: str(x.last_date),
        totals: mapTotals(x.totals),
        previous: mapTotals(x.previous),
        daily: mapDaily(x.daily),
        sourceRef: str(x.source_ref),
      };
    }),
    imports: arr(o.imports).map((i) => {
      const x = rec(i);
      return {
        id: str(x.id),
        platform: str(x.platform),
        productId: str(x.product_id),
        account: str(x.account),
        sourceFile: str(x.source_file),
        sourceSha256: str(x.source_sha256),
        rowsParsed: num(x.rows_parsed),
        rowsNew: num(x.rows_new),
        rowsUpdated: num(x.rows_updated),
        rowsUnchanged: num(x.rows_unchanged),
        rowsSkipped: num(x.rows_skipped),
        dateStart: str(x.date_start),
        dateEnd: str(x.date_end),
        currency: str(x.currency),
        importedAt: str(x.imported_at),
      };
    }),
    gaps: arr(o.gaps).map((g) => {
      const x = rec(g);
      return { kind: str(x.kind), platform: str(x.platform), productId: str(x.product_id), text: str(x.text), command: str(x.command) };
    }),
  };
}

/* ---------- the door ---------- */

export function adsProjectionArgs(opts: { window?: number; product?: string; platform?: string }): string[] {
  const args = ["product", "ads", "--json", "--window", String(opts.window && opts.window > 0 ? opts.window : ADS_DEFAULT_WINDOW)];
  if (opts.product) args.push("--product", opts.product);
  if (opts.platform) args.push("--platform", opts.platform);
  return args;
}

export async function getAdsProjection(opts: { window?: number; product?: string; platform?: string } = {}): Promise<AdsRead> {
  const args = adsProjectionArgs(opts);
  const read = await runProjection(args);
  return {
    status: read.status,
    projection: mapAdsProjection(read.value),
    lastGoodAt: read.lastGoodAt,
    source: `compound ${args.join(" ")}`,
  };
}

export function parseAdsWindowParam(raw: unknown): AdsWindowDays {
  const n = Number(typeof raw === "string" ? raw.replace(/d$/i, "") : raw);
  return (ADS_WINDOWS as number[]).includes(n) ? (n as AdsWindowDays) : ADS_DEFAULT_WINDOW;
}

/* ---------- display helpers (plain language, never system slugs) ---------- */

const PLATFORM_TONE: Record<string, Tone> = {
  apple_search_ads: "product",
  google_ads: "route",
  meta_ads: "work",
  tiktok_ads: "memory",
  x_ads: "neutral",
  reddit_ads: "seam",
  linkedin_ads: "capability",
  other: "neutral",
};

export function platformTone(platform: string): Tone {
  return PLATFORM_TONE[platform] ?? "neutral";
}

export function platformStatusLabel(status: AdsPlatformStatus): { label: string; tone: "good" | "seam" | "muted" } {
  switch (status) {
    case "connected":
      return { label: "connected", tone: "good" };
    case "stale":
      return { label: "stale", tone: "seam" };
    default:
      return { label: "not connected", tone: "muted" };
  }
}

const CURRENCY_SYMBOL: Record<string, string> = { USD: "$", EUR: "€", GBP: "£", TRY: "₺" };

/** Money with the row currency; "mixed" when currencies disagree; a bare
 *  number when the report carried none (never a fabricated symbol). */
export function formatMoney(value: number, currency: string, opts: { compact?: boolean } = {}): string {
  if (currency === "MIXED") return "mixed";
  const abs = Math.abs(value);
  const digits = opts.compact && abs >= 1000 ? 0 : 2;
  const body = abs.toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits });
  const sign = value < 0 ? "−" : "";
  if (!currency) return `${sign}${body}`;
  const symbol = CURRENCY_SYMBOL[currency];
  return symbol ? `${sign}${symbol}${body}` : `${sign}${body} ${currency}`;
}

export function formatMixedSpend(t: AdsTotals): string {
  return Object.entries(t.spendByCurrency)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([c, v]) => c === "UNKNOWN" ? `${formatMoney(v, "")} (currency unknown)` : formatMoney(v, c, { compact: true }))
    .join(" + ");
}

function knownCurrency(currency: string): boolean {
  return currency !== "" && currency !== "MIXED" && currency !== "UNKNOWN";
}

export function comparableSpend(t: AdsTotals): boolean {
  return knownCurrency(t.currency) && !t.mixedCurrency && t.rowsUnknownCurrency === 0;
}

export function formatSpend(t: AdsTotals, opts: { compact?: boolean } = {}): string {
  if (t.mixedCurrency) return formatMixedSpend(t);
  if (!comparableSpend(t)) return `${formatMoney(t.spend, "")} (currency unknown)`;
  return formatMoney(t.spend, t.currency, opts);
}

export function roasExplanation(t: AdsTotals): string {
  if (!comparableSpend(t)) return "ROAS unavailable: spend currencies are mixed or unknown";
  if (!t.hasRevenue) return "ROAS unavailable: no conversion value imported";
  if (t.revenueCoverage === null) return "ROAS unavailable: revenue completeness unknown";
  if (t.revenueCoverage < 1) return `ROAS unavailable: partial revenue (${Math.round(t.revenueCoverage * 100)}% coverage)`;
  if (t.spend <= 0) return "ROAS unavailable: no positive spend";
  return "revenue ÷ spend · complete revenue coverage";
}

export function formatCount(value: number): string {
  if (!Number.isFinite(value)) return "—";
  if (Math.abs(value) >= 10_000) return `${(value / 1000).toFixed(value >= 100_000 ? 0 : 1)}k`;
  return Number.isInteger(value) ? value.toLocaleString("en-US") : value.toFixed(1);
}

/** Ratios are only rendered when their denominator exists; "—" otherwise. */
export function formatCtr(t: AdsTotals): string {
  return t.impressions > 0 ? `${(t.ctr * 100).toFixed(2)}%` : "—";
}
export function formatCpc(t: AdsTotals): string {
  return t.clicks > 0 && comparableSpend(t) ? formatMoney(t.cpc, t.currency) : "—";
}
export function formatCpa(t: AdsTotals): string {
  return t.conversions > 0 && comparableSpend(t) ? formatMoney(t.cpa, t.currency) : "—";
}

export function formatCpm(t: AdsTotals): string {
  return t.impressions > 0 && comparableSpend(t) ? formatMoney(t.cpm, t.currency) : "—";
}
export function formatRoas(t: AdsTotals): string {
  return t.hasRevenue && t.revenueCoverage === 1 && t.spend > 0 && comparableSpend(t) ? `${t.roas.toFixed(2)}×` : "—";
}

/** Percent change vs the previous window; null when the previous window is
 *  empty or currencies cannot be compared (a delta would be a lie). */
export function windowDelta(now: AdsTotals, previous: AdsTotals, key: "spend" | "conversions" | "clicks" | "impressions"): number | null {
  if (previous.rows === 0) return null;
  if (key === "spend" && (!comparableSpend(now) || !comparableSpend(previous) || now.currency !== previous.currency)) return null;
  const before = previous[key];
  const after = now[key];
  if (before === 0) return after === 0 ? 0 : null;
  return Math.round(((after - before) / before) * 100);
}

/* ---------- derivations for the page ---------- */

const STATUS_RANK: Record<AdsPlatformStatus, number> = { connected: 0, stale: 1, not_connected: 2 };

/** Connected first (by spend), then stale, then not connected in registry
 *  order — "what is running" before "what could be". */
export function orderPlatforms(platforms: AdsPlatformView[]): AdsPlatformView[] {
  return platforms
    .map((p, index) => ({ p, index }))
    .sort((a, b) => {
      const rank = STATUS_RANK[a.p.status] - STATUS_RANK[b.p.status];
      if (rank !== 0) return rank;
      if (a.p.status !== "not_connected" && a.p.totals.spend !== b.p.totals.spend) return b.p.totals.spend - a.p.totals.spend;
      return a.index - b.index;
    })
    .map((x) => x.p);
}

export type AdsHeadline = {
  state: "empty" | "quiet" | "live";
  text: string;
};

/** One sentence the header can carry — never a number the data cannot back. */
export function adsHeadline(p: AdsProjection): AdsHeadline {
  if (p.status === "empty" || p.counts.rows === 0) {
    return { state: "empty", text: "No ad reports imported yet. Every platform below is waiting for its first export." };
  }
  if (p.status === "quiet" || p.counts.rowsInWindow === 0) {
    return {
      state: "quiet",
      text: `No rows fall inside the last ${p.window.days} days — the newest import stops earlier. Import a fresh report or widen the window.`,
    };
  }
  const live = p.platforms.filter((x) => x.status === "connected").length;
  const stale = p.platforms.filter((x) => x.status === "stale").length;
  const parts = [`${live} platform${live === 1 ? "" : "s"} current`];
  if (stale > 0) parts.push(`${stale} stale`);
  parts.push(`${p.counts.campaignsActive} campaign${p.counts.campaignsActive === 1 ? "" : "s"} active`);
  return { state: "live", text: parts.join(" · ") };
}

export function productDisplay(productId: string): string {
  return productId.replace(/^product:/, "");
}

export function sparkValues(daily: AdsDailyPoint[], key: "spend" | "conversions" | "clicks"): number[] {
  if (key !== "spend") return daily.map((d) => d[key]);
  if (daily.some((d) => d.spend === null || !knownCurrency(d.currency)) || new Set(daily.map((d) => d.currency)).size !== 1) return [];
  return daily.map((d) => d.spend!);
}
