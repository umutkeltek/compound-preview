import { arr, num, runJson, str } from "@/lib/compound";
import type { Accent } from "@/components/ui";

/*
 * Product loop board — the product as convergence point. A product is one
 * identity; this joins its signals (evidence), hypotheses, reviews (verdicts),
 * freshness, and gaps into one loop. Reads `compound product loops --json` +
 * `compound product authority --json` (read-only projections).
 */

export type ProductSignal = {
  signalId: string;
  provider: string;
  metric: string;
  before: number | null;
  after: number | null;
  observedAt: string;
  changeEvent: string;
  transitionRef: string;
};

export type ProductReview = {
  id: string;
  hypothesisId: string;
  signalId: string;
  verdict: string;
  reviewedAt: string;
};

export type ProductLoop = {
  productId: string;
  name: string;
  signals: ProductSignal[];
  metricNames: string[];
  hypothesisCount: number;
  reviews: ProductReview[];
  sourceIssueCount: number;
  freshness: { latestSignalAt: string; latestReviewAt: string; stale: boolean };
  loopGaps: string[];
  signalCount: number;
};

export type ProductPortfolioRow = {
  productId: string;
  status: string;
  signalCount: number;
  hypothesisCount: number;
  pendingReviewCount: number;
  reviewReadyCount: number;
  sourceIssueCount: number;
};

function numOrNull(v: unknown): number | null {
  return typeof v === "number" ? v : null;
}

function mapSignal(s: Record<string, unknown>): ProductSignal {
  return {
    signalId: str(s.signal_id),
    provider: str(s.provider),
    metric: str(s.metric),
    before: numOrNull(s.before),
    after: numOrNull(s.after),
    observedAt: str(s.observed_at),
    changeEvent: str(s.change_event),
    transitionRef: str(s.transition_ref),
  };
}

function mapLoop(p: Record<string, unknown>): ProductLoop {
  const byMetric = (p.signals_by_metric ?? {}) as Record<string, unknown>;
  const signals: ProductSignal[] = [];
  const metricNames: string[] = [];
  for (const [metric, rows] of Object.entries(byMetric)) {
    metricNames.push(metric);
    for (const r of arr(rows)) signals.push(mapSignal(r as Record<string, unknown>));
  }
  const fresh = (p.freshness ?? {}) as Record<string, unknown>;
  return {
    productId: str(p.product_id),
    name: str(p.name) || str(p.product_id),
    signals,
    metricNames: metricNames.sort(),
    hypothesisCount: arr(p.open_hypotheses).length,
    reviews: arr(p.reviews).map((r) => {
      const o = r as Record<string, unknown>;
      return {
        id: str(o.id),
        hypothesisId: str(o.hypothesis_id),
        signalId: str(o.signal_id),
        verdict: str(o.verdict),
        reviewedAt: str(o.reviewed_at),
      };
    }),
    sourceIssueCount:
      num((p.open_source_issues as Record<string, unknown>)?.count) || 0,
    freshness: {
      latestSignalAt: str(fresh.latest_signal_at),
      latestReviewAt: str(fresh.latest_review_at),
      stale: fresh.stale === true,
    },
    loopGaps: arr(p.loop_gaps).map(str).filter(Boolean),
    signalCount: signals.length,
  };
}

export type ProductLoopsReport = { generatedAt: string; loops: ProductLoop[] };

/** Loops plus the projection's own generated_at, so the page can stamp a
 *  FreshnessChip against the real read time (not a fabricated one). */
export async function getProductLoopsReport(): Promise<ProductLoopsReport> {
  const j = await runJson(["product", "loops", "--json"]);
  return {
    generatedAt: str(j?.generated_at),
    loops: arr(j?.products)
      .map((p) => mapLoop(p as Record<string, unknown>))
      .sort((a, b) => b.signalCount - a.signalCount),
  };
}

export async function getProductLoops(): Promise<ProductLoop[]> {
  return (await getProductLoopsReport()).loops;
}

export async function getProductPortfolio(): Promise<ProductPortfolioRow[]> {
  const j = await runJson(["product", "authority", "--json"]);
  return arr(j?.products).map((p) => {
    const o = p as Record<string, unknown>;
    return {
      productId: str(o.product_id),
      status: str(o.status),
      signalCount: num(o.signal_count),
      hypothesisCount: num(o.hypothesis_count),
      pendingReviewCount: num(o.pending_review_count),
      reviewReadyCount: num(o.review_ready_count),
      sourceIssueCount: num(o.source_issue_count),
    };
  });
}

/* ---------- display helpers (plain language, never system slugs) ---------- */

export function productDisplayName(productId: string): string {
  return productId.replace(/^product:/, "");
}

const METRIC_LABEL: Record<string, string> = {
  "app.conversion_rate": "Conversion rate",
  "app.downloads_first_time": "First-time downloads",
  "app.impressions": "Impressions",
  app_store_connect_review_count: "App Store reviews",
  app_store_connect_average_rating: "Average rating",
  gsc_clicks: "Search clicks",
  gsc_impressions: "Search impressions",
  gsc_ctr: "Search CTR",
  gsc_avg_position: "Search position",
  ga4_active_users: "Active users",
  ga4_sessions: "Sessions",
  ga4_conversions: "Conversions",
  revenuecat_mrr: "MRR",
  revenuecat_active_subscriptions: "Active subscriptions",
  ads_spend: "Ad spend",
  ads_impressions: "Ad impressions",
  ads_clicks: "Ad clicks",
  ads_conversions: "Ad conversions",
  ads_revenue: "Ad revenue",
  ads_ctr: "Ad CTR",
  ads_cpc: "Cost per click",
  ads_cpa: "Cost per conversion",
  ads_roas: "ROAS",
};

const AD_PLATFORM_LABEL: Record<string, string> = {
  apple_search_ads: "Apple Search Ads",
  google_ads: "Google Ads",
  meta_ads: "Meta Ads",
  tiktok_ads: "TikTok Ads",
  x_ads: "X Ads",
  reddit_ads: "Reddit Ads",
  linkedin_ads: "LinkedIn Ads",
  other: "Ads",
};

export function metricLabel(metric: string): string {
  return METRIC_LABEL[metric] ?? metric.replace(/_/g, " ");
}

export function metricSurface(metric: string, provider: string): { label: string; accent: Accent } {
  if (provider === "app_store_connect" || metric.startsWith("app.") || metric.startsWith("app_store"))
    return { label: "App Store", accent: "blue" };
  if (provider === "google_search_console" || metric.startsWith("gsc_"))
    return { label: "Search Console", accent: "green" };
  if (provider === "google_analytics" || metric.startsWith("ga4_"))
    return { label: "Analytics", accent: "orange" };
  if (provider === "revenuecat" || metric.startsWith("revenuecat"))
    return { label: "RevenueCat", accent: "violet" };
  if (provider === "posthog" || metric.startsWith("posthog"))
    return { label: "PostHog", accent: "pink" };
  if (AD_PLATFORM_LABEL[provider] || metric.startsWith("ads_"))
    return { label: AD_PLATFORM_LABEL[provider] ?? "Ads", accent: "red" };
  return { label: "Canonical", accent: "gray" };
}

export function formatMetricValue(metric: string, value: number | null): string {
  if (value == null) return "—";
  if (metric === "ads_conversions") return Number.isInteger(value) ? value.toLocaleString() : value.toFixed(1);
  if (metric === "ads_roas") return `${value.toFixed(2)}×`;
  if (metric.includes("conversion") || metric.includes("ctr"))
    return `${(value * 100).toFixed(2)}%`;
  if (metric.includes("position")) return value.toFixed(1);
  if (Number.isInteger(value)) return value.toLocaleString();
  return value.toFixed(2);
}

export function verdictTone(verdict: string): Accent {
  switch (verdict) {
    case "supported":
      return "green";
    case "refuted":
      return "red";
    case "inconclusive":
      return "orange";
    default:
      return "gray";
  }
}

/* ---------- domain lenses (a VIEW of the same signals, not new claims) ----------
 * The loop adapts to its domain: a paid-app loop populates monetization/reach/
 * quality; a search loop populates reach/quality; a subscription loop populates
 * monetization. A lens only appears if the loop has a metric that feeds it, so
 * the perspectives are domain-driven, never silently generic. Anything that maps
 * to no lens surfaces under "Unlensed" — shown, not hidden. */

export type LoopLens = "monetization" | "reach" | "quality" | "engagement" | "other";

export const LENS_META: Record<LoopLens, { label: string; question: string; accent: Accent }> = {
  monetization: { label: "Monetization", question: "converting & earning?", accent: "green" },
  reach: { label: "Reach", question: "are people finding it?", accent: "blue" },
  quality: { label: "Quality", question: "well-received & ranking?", accent: "violet" },
  engagement: { label: "Engagement", question: "do they come back?", accent: "orange" },
  other: { label: "Unlensed", question: "no domain lens matched yet", accent: "gray" },
};

const LENS_ORDER: LoopLens[] = ["monetization", "reach", "quality", "engagement", "other"];

export function lensForMetric(metric: string): LoopLens {
  const m = metric.toLowerCase();
  if (/^ads_(spend|cpc|cpa|cpm)$/.test(m)) return "reach";
  if (/(conversion|proceeds|revenue|mrr|subscription|earnings|purchase|arpu|ltv|rpm|roas)/.test(m)) return "monetization";
  if (/(activation|retention|\bdau\b|\bmau\b|active_users|returning|session)/.test(m)) return "engagement";
  if (/(rating|review_count|position|ctr|rank|crash|stars)/.test(m)) return "quality";
  if (/(impression|click|download|install|first_time|page_view|reach|new_users|visits)/.test(m)) return "reach";
  return "other";
}

export function groupSignalsByLens(
  signals: ProductSignal[],
): { lens: LoopLens; meta: (typeof LENS_META)[LoopLens]; signals: ProductSignal[] }[] {
  const buckets = new Map<LoopLens, ProductSignal[]>();
  for (const s of signals) {
    const lens = lensForMetric(s.metric);
    (buckets.get(lens) ?? buckets.set(lens, []).get(lens)!).push(s);
  }
  return LENS_ORDER.filter((l) => buckets.has(l)).map((lens) => ({
    lens,
    meta: LENS_META[lens],
    signals: (buckets.get(lens) ?? []).sort((a, b) => metricLabel(a.metric).localeCompare(metricLabel(b.metric))),
  }));
}

/** The doctor is the "what's my next move" engine — turn loop gaps into a plain
 *  sentence + the exact command that reveals/executes the next step. */
export function nextMove(loop: ProductLoop): { text: string; cmd: string } {
  const id = loop.productId;
  const doctor = `compound product doctor ${id}`;
  if (loop.signalCount === 0) {
    return {
      text: "Connected, but no signal yet — feed it a metric to start the loop.",
      cmd: `compound product signal --asc-datapoints <daily.json> --product ${id} --app-id <asc_id>`,
    };
  }
  if (loop.loopGaps.includes("review_ready")) {
    return {
      text: "A signal and hypothesis are matched — record the verdict to close the loop.",
      cmd: doctor,
    };
  }
  if (loop.loopGaps.includes("signals_without_hypothesis")) {
    return {
      text: `${loop.signalCount} signals with no hypothesis yet — form one to turn evidence into a loop.`,
      cmd: doctor,
    };
  }
  if (loop.loopGaps.includes("hypothesis_without_signal")) {
    return {
      text: "A hypothesis is waiting for a matching signal.",
      cmd: doctor,
    };
  }
  return { text: "Loop is current — no open gap.", cmd: doctor };
}
