#!/usr/bin/env bun
// app_store_reviews.ts — a rented source edge.
//
// Normalizes App Store customer-review pulls (iTunes RSS `customerreviews`
// JSON) into `compound.observation_batch.v0.1` files that
// `compound source observe-batch <file>` routes into the SourceObservation
// owner as PARKED candidates. This tool observes; it never promotes, never
// fetches, and never holds credentials.
//
// - Input is a JSON file (`--reviews-json`) or the stdout of a shell-out
//   template (`--reviews-command`, placeholders {app_id} {country} {page}).
//   Auth/network live OUTSIDE the tool, in that command.
// - Config REUSES the ASC products file ({apps:[{product_id, asc_app_id,
//   surface, ...}]}) via asc_daily_signals' parseConfig — no second config.
// - Output files land under ${COMPOUND_HOME}/state/app-store-reviews/.
// - Idempotency: a per-product seen-review-id ledger excludes already-exported
//   reviews on re-runs.
//
// Deviations from the packet premise (documented, additive):
//  * Source.ref uses the `app-store:` scheme (an ALLOWED observation ref
//    scheme; see internal/observation/observation.go allowedObservationRefScheme)
//    instead of the packet's `itunes-rss:` prefix, which ValidateBatch rejects
//    as an unsupported scheme. The itunes-rss provenance is preserved in the
//    ref path (`app-store:itunes-rss:<app_id>`).
//  * Observation.kind is `system` — the router keys RouteSourceObservation off
//    kind ∈ {system, tool_health, generated_view}. A review is not a metric,
//    so `system` (generic observed datum) is the honest lever to reach the
//    SourceObservation owner the packet requires.

import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { parseConfig } from "./asc_daily_signals.ts";

type AppConfig = {
  product_id: string;
  asc_app_id: string;
  surface?: string;
  vendor?: string;
  metrics?: string[];
};

type RssLabel = { label?: string; attributes?: Record<string, string> };

type RssEntry = {
  id?: RssLabel;
  title?: RssLabel;
  content?: RssLabel;
  updated?: RssLabel;
  author?: { name?: RssLabel; uri?: RssLabel; label?: string };
  link?: { attributes?: Record<string, string> } | Array<{ attributes?: Record<string, string> }>;
  "im:name"?: RssLabel;
  "im:rating"?: RssLabel;
  "im:version"?: RssLabel;
  "im:voteCount"?: RssLabel;
  "im:voteSum"?: RssLabel;
  [key: string]: unknown;
};

type RssFeed = {
  feed?: {
    id?: RssLabel;
    entry?: RssEntry | RssEntry[];
    link?: Array<{ attributes?: Record<string, string> }>;
    [key: string]: unknown;
  };
};

type ReviewRecord = {
  review_id: string;
  title: string;
  content: string;
  rating?: string;
  version?: string;
  author?: string;
  voteCount?: string;
  voteSum?: string;
  url?: string;
  updated?: string;
};

type Observation = {
  id?: string;
  observed_at: string;
  kind: string;
  metric?: string;
  dimensions?: Record<string, string>;
  raw_ref?: string;
  confidence?: number;
  title?: string;
  content?: string;
  url?: string;
  evidence_refs?: string[];
};

type Batch = {
  schema_version: "compound.observation_batch.v0.1";
  source: { kind: string; ref: string; captured_at: string; collector?: string; privacy?: string };
  subject: { kind: string; id: string; surface?: string; channel?: string };
  observations: Observation[];
};

type ExtractOutcome = { ok: true; review: ReviewRecord } | { ok: false; reason: string };

type SkippedEntry = { index: number; reason: string; review_id?: string };

type SeenLedger = {
  product_id: string;
  app_id: string;
  seen_review_ids: string[];
  updated_at: string;
};

const OBSERVATION_KIND = "system";
const SOURCE_KIND = "app_store_reviews";
const COLLECTOR = "tools/app_store_reviews.ts";

function label(node: RssLabel | undefined): string {
  return (node?.label ?? "").trim();
}

function normalizeEntries(feed: RssFeed): RssEntry[] {
  const entry = feed.feed?.entry;
  if (!entry) return [];
  return Array.isArray(entry) ? entry : [entry];
}

function feedAppId(feed: RssFeed): string {
  const entries = normalizeEntries(feed);
  // Ordered by authority: feed-level id, then the app-header entry's im:id.
  // Review-entry ids are deliberately excluded — those are review ids, not the
  // app id, and must never be mistaken for it. Leading zeros are preserved so
  // the resolved id matches the configured asc_app_id verbatim.
  const candidates = [
    feed.feed?.id?.attributes?.["im:id"],
    feed.feed?.id?.label,
    ...(feed.feed?.link ?? []).map((link) => link.attributes?.href),
    ...entries.map((entry) => (entry.id as RssLabel | undefined)?.attributes?.["im:id"]),
  ];
  for (const candidate of candidates) {
    if (!candidate) continue;
    const trimmed = candidate.trim();
    if (/^\d{3,}$/.test(trimmed)) return trimmed;
    const match = trimmed.match(/id=?(\d{3,})/);
    if (match) return match[1];
  }
  return "";
}

function parseReviewsFeed(input: string | RssFeed): { appId: string; entries: RssEntry[] } {
  const feed: RssFeed = typeof input === "string" ? (JSON.parse(input) as RssFeed) : input;
  return { appId: feedAppId(feed), entries: normalizeEntries(feed) };
}

// extractReview validates one RSS entry as a customer review. App-header
// entries (URL id, no rating) and truncated/malformed entries return a reason
// instead of throwing — partial data must never fail the whole pull.
function extractReview(entry: RssEntry): ExtractOutcome {
  const reviewId = label(entry.id);
  if (!reviewId || reviewId.includes("://")) {
    return { ok: false, reason: "entry has no numeric review id (app header or malformed)" };
  }
  const content = label(entry.content);
  if (!content) {
    return { ok: false, reason: `review ${reviewId} missing content` };
  }
  const rating = label(entry["im:rating"]);
  if (!rating) {
    return { ok: false, reason: `review ${reviewId} missing rating` };
  }
  const linkHref = Array.isArray(entry.link)
    ? entry.link[0]?.attributes?.href
    : entry.link?.attributes?.href;
  return {
    ok: true,
    review: {
      review_id: reviewId,
      title: label(entry.title),
      content,
      rating,
      version: label(entry["im:version"]) || undefined,
      author: label(entry.author?.name) || undefined,
      voteCount: label(entry["im:voteCount"]) || undefined,
      voteSum: label(entry["im:voteSum"]) || undefined,
      url: (linkHref ?? "").trim() || undefined,
      updated: label(entry.updated) || undefined,
    },
  };
}

function isRfc3339(value: string | undefined): value is string {
  if (!value) return false;
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/.test(value)) return false;
  return !Number.isNaN(Date.parse(value));
}

function reviewObservation(review: ReviewRecord, capturedAt: string, appId: string): Observation {
  const dimensions: Record<string, string> = { review_id: review.review_id };
  if (review.rating) dimensions.rating = review.rating;
  if (review.version) dimensions.app_version = review.version;
  if (review.author) dimensions.author = review.author;
  if (review.voteCount) dimensions.vote_count = review.voteCount;
  if (review.voteSum) dimensions.vote_sum = review.voteSum;
  // iTunes RSS review entries carry no reliable per-review timestamp; fall back
  // to the batch capture time so observed_at is always a real RFC3339 instant.
  const observedAt = isRfc3339(review.updated) ? review.updated : capturedAt;
  const observation: Observation = {
    id: `app_store_review:${appId}:${review.review_id}`,
    observed_at: observedAt,
    kind: OBSERVATION_KIND,
    title: review.title || `App Store review ${review.review_id}`,
    content: review.content,
    raw_ref: `app-store:review:${appId}:${review.review_id}`,
    dimensions,
    confidence: 0.5,
    evidence_refs: [`app-store-review:${appId}:${review.review_id}`],
  };
  if (review.url && /^https?:\/\//.test(review.url)) {
    observation.url = review.url;
  }
  return observation;
}

function buildReviewBatch(params: {
  app: AppConfig;
  entries: RssEntry[];
  seenIds: string[] | Set<string>;
  capturedAt: string;
  appId?: string;
}): { batch: Batch; newReviewIds: string[]; skipped: SkippedEntry[]; seenExcluded: number } {
  const appId = (params.appId ?? params.app.asc_app_id).trim();
  const seen = new Set(params.seenIds);
  const observations: Observation[] = [];
  const newReviewIds: string[] = [];
  const skipped: SkippedEntry[] = [];
  let seenExcluded = 0;

  params.entries.forEach((entry, index) => {
    const outcome = extractReview(entry);
    if (!outcome.ok) {
      skipped.push({ index, reason: outcome.reason });
      return;
    }
    if (seen.has(outcome.review.review_id)) {
      seenExcluded++;
      return;
    }
    observations.push(reviewObservation(outcome.review, params.capturedAt, appId));
    newReviewIds.push(outcome.review.review_id);
  });

  const batch: Batch = {
    schema_version: "compound.observation_batch.v0.1",
    source: {
      kind: SOURCE_KIND,
      ref: `app-store:itunes-rss:${appId}`,
      captured_at: params.capturedAt,
      collector: COLLECTOR,
      privacy: "private",
    },
    subject: {
      kind: "product",
      id: params.app.product_id,
      surface: "app_store",
    },
    observations,
  };
  return { batch, newReviewIds, skipped, seenExcluded };
}

function loadSeenReviewIds(path: string): string[] {
  if (!existsSync(path)) return [];
  try {
    const parsed = JSON.parse(readFileSync(path, "utf8")) as SeenLedger | string[];
    const ids = Array.isArray(parsed) ? parsed : parsed.seen_review_ids;
    return Array.isArray(ids) ? ids.map(String) : [];
  } catch {
    return [];
  }
}

function writeSeenReviewIds(path: string, app: AppConfig, existing: string[], added: string[]): string[] {
  const union = [...new Set([...existing.map(String), ...added.map(String)])].sort();
  const ledger: SeenLedger = {
    product_id: app.product_id,
    app_id: app.asc_app_id,
    seen_review_ids: union,
    updated_at: new Date().toISOString(),
  };
  writeFileSync(path, `${JSON.stringify(ledger, null, 2)}\n`);
  return union;
}

function safeName(value: string): string {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

function usage(): never {
  console.error(`usage: bun tools/app_store_reviews.ts (--reviews-json <file>|--reviews-command <template>) [--config <asc-products.json>] [--product-id <id>] [--country us] [--page 1] [--out-dir <dir>] [--compound-bin compound] [--captured-at <RFC3339>] [--apply] [--apply-for-real] [--json]

Rented edge: normalizes an App Store customer-review pull into a
compound.observation_batch.v0.1 file. It never fetches and never promotes.
--reviews-command placeholders: {app_id} {country} {page}. Auth/network stay
inside that command, outside this tool.`);
  process.exit(2);
}

function parseArgs(argv: string[]): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  const flags = new Set(["apply", "apply-for-real", "json"]);
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith("--")) usage();
    const key = arg.slice(2);
    if (flags.has(key)) {
      out[key] = true;
      continue;
    }
    if (i + 1 >= argv.length) usage();
    out[key] = argv[++i];
  }
  return out;
}

function resolveConfigPath(args: Record<string, string | boolean>): string {
  if (typeof args.config === "string") return args.config;
  const home = process.env.COMPOUND_HOME ?? `${process.env.HOME}/.compound`;
  return join(home, "private", "asc-products.json");
}

function loadFeedForApp(args: Record<string, string | boolean>, app: AppConfig): RssFeed {
  if (typeof args["reviews-json"] === "string") {
    return JSON.parse(readFileSync(args["reviews-json"], "utf8")) as RssFeed;
  }
  if (typeof args["reviews-command"] !== "string") usage();
  const cmd = args["reviews-command"]
    .replaceAll("{app_id}", shellQuote(app.asc_app_id))
    .replaceAll("{country}", shellQuote(String(args.country ?? "us")))
    .replaceAll("{page}", shellQuote(String(args.page ?? "1")));
  const result = spawnSync("/bin/zsh", ["-lc", cmd], { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(`reviews command failed for ${app.asc_app_id}: ${result.stderr || result.stdout}`);
  }
  return JSON.parse(result.stdout) as RssFeed;
}

function selectApps(args: Record<string, string | boolean>, apps: AppConfig[]): AppConfig[] {
  if (typeof args["product-id"] === "string") {
    const match = apps.find((app) => app.product_id === args["product-id"]);
    if (!match) throw new Error(`config has no app with product_id ${args["product-id"]}`);
    return [match];
  }
  // A single reviews JSON file is one app's feed; resolve it from the feed's app
  // id when the config holds more than one product.
  if (typeof args["reviews-json"] === "string" && apps.length > 1) {
    const { appId } = parseReviewsFeed(readFileSync(args["reviews-json"], "utf8"));
    const match = apps.find((app) => app.asc_app_id === appId);
    if (!match) {
      throw new Error(`reviews feed app id ${appId || "<none>"} matches no configured asc_app_id`);
    }
    return [match];
  }
  return apps;
}

function applyBatch(path: string, mode: "dry-run" | "source-observations", compoundBin: string): string {
  const flag = mode === "dry-run" ? "--dry-run" : "--apply-source-observations";
  const result = spawnSync(compoundBin, ["source", "observe-batch", path, flag], { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(`compound source observe-batch ${flag} failed: ${result.stderr || result.stdout}`);
  }
  return result.stdout.trim();
}

export {
  buildReviewBatch,
  extractReview,
  feedAppId,
  loadSeenReviewIds,
  parseReviewsFeed,
  reviewObservation,
  writeSeenReviewIds,
};

if (import.meta.main) {
  try {
    const args = parseArgs(process.argv.slice(2));
    if (typeof args["reviews-json"] !== "string" && typeof args["reviews-command"] !== "string") usage();

    const config = parseConfig(resolveConfigPath(args));
    const apps = selectApps(args, config.apps);
    const capturedAt = typeof args["captured-at"] === "string" ? args["captured-at"] : new Date().toISOString();
    const home = process.env.COMPOUND_HOME ?? `${process.env.HOME}/.compound`;
    const outDir = typeof args["out-dir"] === "string" ? args["out-dir"] : join(home, "state", "app-store-reviews");
    const compoundBin = typeof args["compound-bin"] === "string" ? args["compound-bin"] : "compound";
    mkdirSync(outDir, { recursive: true });

    const emitted: Array<{ product_id: string; path: string; observations: number; applied?: string }> = [];
    const empties: Array<{ product_id: string; seen_excluded: number }> = [];
    let skippedTotal = 0;

    for (const app of apps) {
      const feed = loadFeedForApp(args, app);
      const { appId, entries } = parseReviewsFeed(feed);
      const seenPath = join(outDir, `seen-${safeName(app.product_id)}.json`);
      const existing = loadSeenReviewIds(seenPath);
      const { batch, newReviewIds, skipped, seenExcluded } = buildReviewBatch({
        app,
        entries,
        seenIds: existing,
        capturedAt,
        appId: appId || app.asc_app_id,
      });

      for (const entry of skipped) {
        skippedTotal++;
        console.warn(`app_store_reviews: skipped entry #${entry.index} (${app.product_id}): ${entry.reason}`);
      }

      if (batch.observations.length === 0) {
        empties.push({ product_id: app.product_id, seen_excluded: seenExcluded });
        continue;
      }

      const datePart = capturedAt.slice(0, 10);
      const outPath = join(outDir, `${datePart}.${safeName(app.product_id)}.${appId || app.asc_app_id}.json`);
      writeFileSync(outPath, `${JSON.stringify(batch, null, 2)}\n`);
      writeSeenReviewIds(seenPath, app, existing, newReviewIds);

      let applied: string | undefined;
      if (args.apply) {
        applied = applyBatch(outPath, args["apply-for-real"] ? "source-observations" : "dry-run", compoundBin);
      }
      emitted.push({ product_id: app.product_id, path: outPath, observations: batch.observations.length, applied });
    }

    const report = {
      status: "ok",
      emitted: emitted.length,
      empty: empties.length,
      skipped: skippedTotal,
      batches: emitted,
      empties,
    };
    if (args.json) {
      console.log(JSON.stringify(report, null, 2));
    } else {
      console.log(`app-store-reviews ok emitted=${emitted.length} empty=${empties.length} skipped=${skippedTotal}`);
      for (const batch of emitted) {
        console.log(`- ${batch.product_id} observations=${batch.observations} -> ${batch.path}`);
      }
      for (const empty of empties) {
        console.log(`- ${empty.product_id} no new reviews (seen_excluded=${empty.seen_excluded})`);
      }
    }
  } catch (error) {
    console.error(`app-store-reviews: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
