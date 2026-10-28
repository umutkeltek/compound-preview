#!/usr/bin/env bun
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

import { parseConfig } from "./asc_daily_signals.ts";

type AppConfig = {
  product_id: string;
  asc_app_id: string;
};

type SalesRow = {
  apple_identifier?: string;
  date?: string;
  [key: string]: unknown;
};

type BridgeOptions = {
  seriesPath: string;
  beforeWindow?: string;
  afterWindow?: string;
  afterEnd?: string;
  days: number;
  changeEvent: string;
  metrics: string[];
  compoundBin: string;
};

type PlannedCommand = {
  product_id: string;
  asc_app_id: string;
  metric: string;
  before: string;
  after: string;
  args: string[];
};

type ApplyResult = {
  product_id: string;
  asc_app_id: string;
  metric: string;
  signal_count: number;
  transition_count: number;
};

type SkippedApp = {
  product_id: string;
  asc_app_id: string;
  reason: string;
};

const supportedSalesMetrics = new Set([
  "units",
  "app_units",
  "iap_units",
  "customer_sales_usd_est",
  "developer_proceeds_usd",
]);

function usage(): never {
  console.error(`usage: bun tools/asc_sales_series_bridge.ts --config <json|yaml> --sales-series <daily-app-series.jsonl> --change-event <text> [--before YYYY-MM-DD/YYYY-MM-DD --after YYYY-MM-DD/YYYY-MM-DD | --after-end YYYY-MM-DD --days N] [--metric units,developer_proceeds_usd] [--compound-bin compound] [--apply] [--json]

Bridge an existing App Store Connect sales-series JSONL export into Compound. It does not fetch ASC data; it invokes the existing:
compound product signal --asc-sales-series ...`);
  process.exit(2);
}

function parseArgs(argv: string[]): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--apply" || arg === "--json") {
      out[arg.slice(2)] = true;
      continue;
    }
    if (!arg.startsWith("--") || i + 1 >= argv.length) usage();
    out[arg.slice(2)] = argv[++i];
  }
  return out;
}

function loadSalesRows(path: string): SalesRow[] {
  return readFileSync(path, "utf8")
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line, index) => {
      try {
        return JSON.parse(line) as SalesRow;
      } catch (error) {
        throw new Error(`parse sales-series ${path} line ${index + 1}: ${error instanceof Error ? error.message : String(error)}`);
      }
    });
}

function parseMetricList(raw: string | undefined): string[] {
  const metrics = (raw ?? "units")
    .split(",")
    .map((metric) => metric.trim())
    .filter(Boolean);
  if (metrics.length === 0) throw new Error("at least one metric required");
  for (const metric of metrics) {
    if (!supportedSalesMetrics.has(metric)) {
      throw new Error(`unsupported sales metric ${metric}; supported=${[...supportedSalesMetrics].join(",")}`);
    }
  }
  return metrics;
}

function planSalesSeriesApplications(configApps: AppConfig[], rows: SalesRow[], opts: BridgeOptions): {
  commands: PlannedCommand[];
  skipped: SkippedApp[];
  before: string;
  after: string;
} {
  const windows = resolveWindows(rows, opts);
  const commands: PlannedCommand[] = [];
  const skipped: SkippedApp[] = [];

  for (const app of configApps) {
    const beforeRows = rowsInWindow(rows, app.asc_app_id, windows.before);
    const afterRows = rowsInWindow(rows, app.asc_app_id, windows.after);
    if (beforeRows.length === 0 || afterRows.length === 0) {
      skipped.push({
        product_id: app.product_id,
        asc_app_id: app.asc_app_id,
        reason: beforeRows.length === 0 ? "no_rows_in_before_window" : "no_rows_in_after_window",
      });
      continue;
    }

    for (const metric of opts.metrics) {
      commands.push({
        product_id: app.product_id,
        asc_app_id: app.asc_app_id,
        metric,
        before: windows.before,
        after: windows.after,
        args: [
          "product",
          "signal",
          "--asc-sales-series",
          opts.seriesPath,
          "--product",
          app.product_id,
          "--app-id",
          app.asc_app_id,
          "--before",
          windows.before,
          "--after",
          windows.after,
          "--change-event",
          opts.changeEvent,
          "--metric",
          metric,
        ],
      });
    }
  }

  return { commands, skipped, before: windows.before, after: windows.after };
}

function resolveWindows(rows: SalesRow[], opts: BridgeOptions): { before: string; after: string } {
  if (opts.beforeWindow || opts.afterWindow) {
    if (!opts.beforeWindow || !opts.afterWindow) throw new Error("--before and --after must be provided together");
    parseDateRange(opts.beforeWindow);
    parseDateRange(opts.afterWindow);
    return { before: opts.beforeWindow, after: opts.afterWindow };
  }

  if (opts.days < 1) throw new Error("--days must be >= 1");
  const end = opts.afterEnd ?? latestRowDate(rows);
  const afterEnd = parseISODate(end);
  const afterStart = addDays(afterEnd, -(opts.days - 1));
  const beforeEnd = addDays(afterStart, -1);
  const beforeStart = addDays(beforeEnd, -(opts.days - 1));
  return {
    before: `${formatDate(beforeStart)}/${formatDate(beforeEnd)}`,
    after: `${formatDate(afterStart)}/${formatDate(afterEnd)}`,
  };
}

function latestRowDate(rows: SalesRow[]): string {
  const dates = rows.map((row) => String(row.date ?? "")).filter(Boolean).sort();
  if (dates.length === 0) throw new Error("sales series has no dated rows");
  return dates[dates.length - 1];
}

function rowsInWindow(rows: SalesRow[], appID: string, rawWindow: string): SalesRow[] {
  const range = parseDateRange(rawWindow);
  return rows.filter((row) => {
    if (String(row.apple_identifier ?? "") !== appID) return false;
    const date = parseISODate(String(row.date ?? ""));
    return date >= range.start && date <= range.end;
  });
}

function parseDateRange(raw: string): { start: Date; end: Date } {
  const parts = raw.split("/");
  if (parts.length !== 2) throw new Error(`date range must be YYYY-MM-DD/YYYY-MM-DD, got ${raw}`);
  const start = parseISODate(parts[0]);
  const end = parseISODate(parts[1]);
  if (end < start) throw new Error(`date range end before start: ${raw}`);
  return { start, end };
}

function parseISODate(raw: string): Date {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(raw)) throw new Error(`date must be YYYY-MM-DD, got ${raw}`);
  const date = new Date(`${raw}T00:00:00Z`);
  if (Number.isNaN(date.getTime())) throw new Error(`invalid date ${raw}`);
  return date;
}

function addDays(date: Date, days: number): Date {
  const out = new Date(date.getTime());
  out.setUTCDate(out.getUTCDate() + days);
  return out;
}

function formatDate(date: Date): string {
  return date.toISOString().slice(0, 10);
}

function applyCommands(commands: PlannedCommand[], compoundBin: string): ApplyResult[] {
  return commands.map((command) => {
    const result = spawnSync(compoundBin, command.args, { encoding: "utf8" });
    if (result.status !== 0) {
      throw new Error(`compound ${command.args.join(" ")} failed: ${result.stderr || result.stdout}`);
    }
    const stdout = result.stdout.trim();
    return {
      product_id: command.product_id,
      asc_app_id: command.asc_app_id,
      metric: command.metric,
      signal_count: countMatches(stdout, /^# product signal /gm),
      transition_count: countMatches(stdout, /^- transition: /gm),
    };
  });
}

function countMatches(text: string, pattern: RegExp): number {
  return Array.from(text.matchAll(pattern)).length;
}

export { loadSalesRows, parseMetricList, planSalesSeriesApplications };

if (import.meta.main) {
  try {
    const args = parseArgs(process.argv.slice(2));
    if (typeof args.config !== "string" || typeof args["sales-series"] !== "string") usage();
    if (typeof args["change-event"] !== "string") usage();

    const config = parseConfig(args.config);
    const rows = loadSalesRows(args["sales-series"]);
    const opts: BridgeOptions = {
      seriesPath: args["sales-series"],
      beforeWindow: typeof args.before === "string" ? args.before : undefined,
      afterWindow: typeof args.after === "string" ? args.after : undefined,
      afterEnd: typeof args["after-end"] === "string" ? args["after-end"] : undefined,
      days: typeof args.days === "string" ? Number.parseInt(args.days, 10) : 7,
      changeEvent: args["change-event"],
      metrics: parseMetricList(typeof args.metric === "string" ? args.metric : undefined),
      compoundBin: typeof args["compound-bin"] === "string" ? args["compound-bin"] : "compound",
    };
    const plan = planSalesSeriesApplications(config.apps, rows, opts);
    if (plan.commands.length === 0) throw new Error("no applicable ASC sales-series commands planned");
    const applied = args.apply ? applyCommands(plan.commands, opts.compoundBin) : [];
    const report = {
      status: "ok",
      before: plan.before,
      after: plan.after,
      planned: plan.commands.length,
      applied: applied.length,
      apply_results: args.apply ? applied : undefined,
      skipped: plan.skipped,
      commands: args.apply ? undefined : plan.commands.map((command) => [opts.compoundBin, ...command.args]),
    };
    if (args.json) {
      console.log(JSON.stringify(report, null, 2));
    } else {
      console.log(`asc-sales-series-bridge ok planned=${plan.commands.length} applied=${applied.length}`);
      for (const command of plan.commands) console.log(`- ${[opts.compoundBin, ...command.args].join(" ")}`);
      for (const skipped of plan.skipped) console.log(`- skipped ${skipped.product_id}: ${skipped.reason}`);
    }
  } catch (error) {
    console.error(`asc-sales-series-bridge: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
