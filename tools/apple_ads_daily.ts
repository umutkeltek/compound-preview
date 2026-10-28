#!/usr/bin/env bun
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/*
 * Apple Search Ads → ads ledger bridge.
 *
 * Compound never calls Apple Ads itself. This tool takes a campaign report the
 * `asc` CLI already produced (or runs the command you hand it), maps every
 * campaign's app (adamId) to a Compound product through the SAME private
 * config the ASC daily signals use (`asc_app_id`), writes one canonical CSV
 * per product, and — only with --apply — invokes the existing
 * `compound product ads import` door. No fetcher, no second store.
 *
 * Report shapes understood:
 *  - Campaign Management API v5 `POST v5/reports/campaigns`
 *    (`asc ads v5 reports campaigns --file payload.json --org <org>`):
 *    data.reportingDataResponse.row[].{metadata{campaignId,campaignName,app.adamId},
 *    granularity[]{date,impressions,taps,totalInstalls,localSpend{amount,currency}}}
 *  - Apple Ads Platform API v1 `POST v1/reports/apps/campaigns/query`
 *    (`asc ads reports apps campaigns --file report.json --ad-account <id>`)
 *    when it returns the same row/granularity grammar.
 */

type AppConfig = { product_id: string; asc_app_id?: string };
type Config = { apps: AppConfig[] };

export type CanonicalAdRow = {
  date: string;
  campaign_id: string;
  campaign_name: string;
  spend: string;
  impressions: number;
  clicks: number;
  conversions: number;
  currency: string;
};

export type BridgeRows = {
  byProduct: Record<string, CanonicalAdRow[]>;
  unmapped: { adamId: string; campaignName: string }[];
  campaigns: number;
  dateStart: string;
  dateEnd: string;
};

export const CANONICAL_HEADERS = ["date", "campaign_id", "campaign_name", "spend", "impressions", "clicks", "conversions", "currency"] as const;

export function parseConfig(path: string): Config {
  const raw = JSON.parse(readFileSync(path, "utf8")) as Partial<Config>;
  if (!raw || !Array.isArray(raw.apps)) throw new Error(`config ${path}: expected { apps: [...] }`);
  return { apps: raw.apps.filter((a): a is AppConfig => !!a && typeof a.product_id === "string") };
}

function num(v: unknown): number {
  const n = typeof v === "number" ? v : Number(String(v ?? "").replace(/[^0-9.-]/g, ""));
  return Number.isFinite(n) ? n : 0;
}

function reportRows(report: unknown): Record<string, unknown>[] {
  const r = report as Record<string, unknown>;
  const data = (r?.data ?? r) as Record<string, unknown>;
  const resp = (data?.reportingDataResponse ?? data) as Record<string, unknown>;
  const rows = resp?.row ?? (r as Record<string, unknown>)?.row ?? (r as Record<string, unknown>)?.rows;
  return Array.isArray(rows) ? (rows as Record<string, unknown>[]) : [];
}

/** Pure: report JSON + product config → canonical rows per product. */
export function rowsForReport(config: Config, report: unknown): BridgeRows {
  const app2product = new Map<string, string>();
  for (const app of config.apps) if (app.asc_app_id) app2product.set(String(app.asc_app_id), app.product_id);

  const out: BridgeRows = { byProduct: {}, unmapped: [], campaigns: 0, dateStart: "", dateEnd: "" };
  for (const row of reportRows(report)) {
    const md = (row.metadata ?? {}) as Record<string, unknown>;
    const app = (md.app ?? {}) as Record<string, unknown>;
    const adamId = String(app.adamId ?? md.adamId ?? md.appId ?? "");
    const campaignId = String(md.campaignId ?? "");
    const campaignName = String(md.campaignName ?? campaignId);
    const product = app2product.get(adamId);
    out.campaigns++;
    if (!product) {
      out.unmapped.push({ adamId, campaignName });
      continue;
    }
    const days = Array.isArray(row.granularity) ? (row.granularity as Record<string, unknown>[]) : [];
    for (const d of days) {
      const date = String(d.date ?? "").slice(0, 10);
      if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) continue;
      const spend = (d.localSpend ?? d.spend ?? {}) as Record<string, unknown>;
      const rec: CanonicalAdRow = {
        date,
        campaign_id: campaignId,
        campaign_name: campaignName,
        spend: String(spend.amount ?? "0"),
        impressions: num(d.impressions),
        clicks: num(d.taps ?? d.clicks),
        conversions: num(d.totalInstalls ?? d.installs ?? d.tapInstalls),
        currency: String(spend.currency ?? "USD"),
      };
      (out.byProduct[product] ??= []).push(rec);
      if (!out.dateStart || date < out.dateStart) out.dateStart = date;
      if (date > out.dateEnd) out.dateEnd = date;
    }
  }
  return out;
}

function csvCell(v: string | number): string {
  const s = String(v);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

export function writeCsvs(rows: BridgeRows, outDir: string): Record<string, string> {
  mkdirSync(outDir, { recursive: true });
  const paths: Record<string, string> = {};
  for (const [product, recs] of Object.entries(rows.byProduct)) {
    const path = join(outDir, `${product.replace(/^product:/, "").replace(/[^A-Za-z0-9._-]/g, "_")}.csv`);
    const lines = [CANONICAL_HEADERS.join(",")];
    for (const r of recs) lines.push(CANONICAL_HEADERS.map((h) => csvCell(r[h])).join(","));
    writeFileSync(path, lines.join("\n") + "\n");
    paths[product] = path;
  }
  return paths;
}

type Args = {
  config: string;
  reportJson?: string;
  ascCommand?: string;
  /** --org + --days: the tool builds the v5 report request itself and runs `asc`. */
  org?: string;
  days?: number;
  account?: string;
  outDir?: string;
  compoundBin: string;
  apply: boolean;
  json: boolean;
};

export const USAGE =
  "usage: bun tools/apple_ads_daily.ts --config <asc-products.json> (--report-json <report.json>|--asc-command <cmd>|--org <org-id> --days <N>) [--account <ad-account-id>] [--out-dir <dir>] [--compound-bin compound] [--apply] [--json]";

/** The v5 campaign report request for the last `days` days ending today (UTC). */
export function reportPayload(days: number, now: Date = new Date()): Record<string, unknown> {
  if (!Number.isInteger(days) || days < 1 || days > 366) throw new UsageError("--days must be an integer between 1 and 366");
  const end = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  const start = new Date(end.getTime() - (days - 1) * 86_400_000);
  const day = (d: Date) => d.toISOString().slice(0, 10);
  return {
    startTime: day(start),
    endTime: day(end),
    granularity: "DAILY",
    timeZone: "UTC",
    returnRowTotals: false,
    returnRecordsWithNoMetrics: true,
    selector: { orderBy: [{ field: "campaignId", sortOrder: "ASCENDING" }], pagination: { offset: 0, limit: 1000 } },
  };
}

/** Proxy variables are dropped: the Apple Ads endpoint is reached directly, and an
 *  inherited loopback proxy has made the CLI fail silently before. */
export function ascEnv(base: NodeJS.ProcessEnv = process.env): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...base };
  for (const k of Object.keys(env)) if (/^(https?_proxy|all_proxy)$/i.test(k)) delete env[k];
  return env;
}

/** A usage error is thrown, never exited, so parseArgs stays testable. */
export class UsageError extends Error {}

function usage(): never {
  throw new UsageError(USAGE);
}

export function parseArgs(argv: string[]): Args {
  const args: Args = { config: "", compoundBin: "compound", apply: false, json: false };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--apply" || arg === "--json") {
      args[arg === "--apply" ? "apply" : "json"] = true;
      continue;
    }
    const value = argv[i + 1];
    if (value === undefined) usage();
    switch (arg) {
      case "--config":
        args.config = value;
        break;
      case "--report-json":
        args.reportJson = value;
        break;
      case "--asc-command":
        args.ascCommand = value;
        break;
      case "--org":
        args.org = value;
        break;
      case "--days":
        args.days = Number(value);
        break;
      case "--account":
        args.account = value;
        break;
      case "--out-dir":
        args.outDir = value;
        break;
      case "--compound-bin":
        args.compoundBin = value;
        break;
      default:
        usage();
    }
    i++;
  }
  const sources = [args.reportJson, args.ascCommand, args.org].filter(Boolean).length;
  if (!args.config || sources !== 1) usage();
  if (args.org && (args.days === undefined || !Number.isInteger(args.days))) usage();
  return args;
}

function loadReport(args: Args): unknown {
  if (args.reportJson) return JSON.parse(readFileSync(args.reportJson, "utf8"));
  if (args.org) {
    // Self-built request: the only asc invocation this tool knows by itself.
    const payload = reportPayload(args.days!);
    const dir = mkdtempSync(join(tmpdir(), "apple-ads-daily-"));
    const payloadPath = join(dir, "payload.json");
    writeFileSync(payloadPath, JSON.stringify(payload));
    const result = spawnSync("asc", ["ads", "v5", "reports", "campaigns", "--file", payloadPath, "--org", args.org, "--output", "json"], {
      encoding: "utf8",
      maxBuffer: 64 * 1024 * 1024,
      env: ascEnv(),
    });
    if (result.status !== 0) throw new Error(`asc ads report failed (${result.status}): ${(result.stderr || "").trim().slice(0, 400)}`);
    return JSON.parse(result.stdout.slice(result.stdout.indexOf("{")));
  }
  // The command is the operator's; this tool only runs what it was handed.
  const result = spawnSync("/bin/zsh", ["-lc", args.ascCommand!], { encoding: "utf8", maxBuffer: 64 * 1024 * 1024, env: ascEnv() });
  if (result.status !== 0) throw new Error(`asc command failed (${result.status}): ${(result.stderr || "").trim().slice(0, 400)}`);
  const text = result.stdout;
  return JSON.parse(text.slice(text.indexOf("{")));
}

export function main(argv: string[]): number {
  const args = parseArgs(argv);
  const config = parseConfig(args.config);
  const rows = rowsForReport(config, loadReport(args));
  const outDir = args.outDir ?? `${process.env.COMPOUND_HOME ?? `${process.env.HOME}/.compound`}/state/apple-ads-daily`;
  const csvs = writeCsvs(rows, outDir);
  const report: Record<string, unknown> = {
    schema_version: "compound.tools.apple_ads_daily.v0.1",
    platform: "apple_search_ads",
    campaigns: rows.campaigns,
    date_start: rows.dateStart,
    date_end: rows.dateEnd,
    products: Object.fromEntries(Object.entries(rows.byProduct).map(([p, r]) => [p, { rows: r.length, csv: csvs[p] }])),
    unmapped: rows.unmapped,
    applied: args.apply,
  };
  if (args.apply) {
    const applied: Record<string, unknown>[] = [];
    for (const [product, csv] of Object.entries(csvs)) {
      const cmd = ["product", "ads", "import", "--platform", "apple_search_ads", "--product", product, "--report", csv, "--json"];
      if (args.account) cmd.push("--account", args.account);
      const result = spawnSync(args.compoundBin, cmd, { encoding: "utf8" });
      let summary: unknown = null;
      try {
        summary = JSON.parse(result.stdout.slice(result.stdout.indexOf("{")))?.import ?? null;
      } catch {
        summary = null;
      }
      applied.push({ product, status: result.status, import: summary, stderr: (result.stderr || "").trim().slice(0, 300) });
      if (result.status !== 0) report.status = "failed";
    }
    report.apply_results = applied;
  }
  report.status ??= rows.unmapped.length > 0 ? "partial" : "ok";
  if (args.json) console.log(JSON.stringify(report, null, 2));
  else {
    console.log(`# apple ads daily (${rows.dateStart}..${rows.dateEnd})`);
    console.log(`- campaigns: ${rows.campaigns} · unmapped: ${rows.unmapped.length} · applied: ${args.apply}`);
    for (const [p, csv] of Object.entries(csvs)) console.log(`- ${p}: ${rows.byProduct[p].length} rows → ${csv}`);
    for (const u of rows.unmapped) console.log(`- unmapped adamId ${u.adamId} (${u.campaignName}): add it to the private asc-products config`);
  }
  return report.status === "failed" ? 1 : 0;
}

if (import.meta.main) {
  try {
    process.exit(main(process.argv.slice(2)));
  } catch (err) {
    if (err instanceof UsageError) {
      console.error(err.message);
      process.exit(2);
    }
    console.error(`apple_ads_daily: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(1);
  }
}
