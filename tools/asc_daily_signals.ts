#!/usr/bin/env bun
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";

type AppConfig = {
  product_id: string;
  asc_app_id: string;
  surface?: string;
  vendor?: string;
  metrics?: string[];
};

type Config = {
  apps: AppConfig[];
};

type SnapshotApp = {
  asc_app_id?: string;
  apple_identifier?: string;
  app_id?: string;
  date?: string;
  captured_at?: string;
  metrics?: Record<string, unknown> | ASCMetricPoint[];
  [key: string]: unknown;
};

type ASCMetricPoint = {
  name?: string;
  value?: unknown;
  thisDay?: unknown;
  previousDay?: unknown;
  delta?: unknown;
  status?: string;
};

type Snapshot = {
  date?: string;
  captured_at?: string;
  apps?: SnapshotApp[];
  rows?: SnapshotApp[];
};

type SignalExport = {
  schema_version: "compound.product_signal_export.v0.1";
  source: { kind: "app_store_connect"; ref: string };
  signal_id: string;
  product_id: string;
  surface: string;
  change_event: string;
  metric: {
    name: string;
    before: { value: number; period: string; ref: string };
    after: { value: number; period: string; ref: string };
  };
  window: string;
  confidence: number;
  confounders: string[];
  interpretations: { lens: string; claim: string }[];
  next_action: string;
  evidence_refs: string[];
  observed_at: string;
};

const defaultMetrics = [
  "units",
  "developer_proceeds_usd",
  "impressions",
  "product_page_views",
  "product_page_conversion_rate",
];

function usage(): never {
  console.error(`usage: bun tools/asc_daily_signals.ts --config <json> (--asc-json <snapshot.json>|--asc-command <cmd>) [--out-dir <dir>] [--compound-bin compound] [--apply] [--date YYYY-MM-DD] [--json]

The script is a rented edge: it normalizes ASC daily metrics to ProductSignalExport files and optionally applies them through compound product signal.`);
  process.exit(2);
}

function parseArgs(argv: string[]) {
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

function parseConfig(path: string): Config {
  const text = readFileSync(path, "utf8");
  const config = path.endsWith(".json") ? (JSON.parse(text) as Config) : parseTinyYamlConfig(text);
  return validateConfig(config);
}

function parseTinyYamlConfig(text: string): Config {
  const apps: AppConfig[] = [];
  let current: AppConfig | undefined;
  let collectingMetrics = false;
  for (const raw of text.split(/\r?\n/)) {
    const trimmed = raw.replace(/#.*/, "").trim();
    if (trimmed === "" || trimmed === "apps:") continue;
    if (trimmed.startsWith("- ")) {
      const value = trimmed.slice(2).trim();
      if (collectingMetrics && current && !value.includes(":")) {
        current.metrics = [...(current.metrics ?? []), cleanScalar(value)];
        continue;
      }
      current = { product_id: "", asc_app_id: "" };
      apps.push(current);
      collectingMetrics = false;
      const split = splitPair(value);
      if (split) assignConfigValue(current, split[0], split[1]);
      continue;
    }
    const split = splitPair(trimmed);
    if (!split || !current) continue;
    if (split[0] === "metrics" && split[1] === "") {
      current.metrics = [];
      collectingMetrics = true;
      continue;
    }
    collectingMetrics = false;
    assignConfigValue(current, split[0], split[1]);
  }
  return { apps };
}

function splitPair(value: string): [string, string] | undefined {
  const index = value.indexOf(":");
  if (index < 0) return undefined;
  return [value.slice(0, index).trim(), value.slice(index + 1).trim()];
}

function assignConfigValue(app: AppConfig, key: string, raw: string) {
  if (key === "metrics") {
    app.metrics = raw.startsWith("[")
      ? raw.replace(/^\[|\]$/g, "").split(",").map(cleanScalar).filter(Boolean)
      : [cleanScalar(raw)].filter(Boolean);
    return;
  }
  (app as unknown as Record<string, string>)[key] = cleanScalar(raw);
}

function cleanScalar(raw: string): string {
  return raw.trim().replace(/^['"]|['"]$/g, "");
}

function validateConfig(config: Config): Config {
  if (!Array.isArray(config.apps) || config.apps.length === 0) {
    throw new Error("config apps[] required");
  }
  for (const app of config.apps) {
    if (!app.product_id || !app.asc_app_id) {
      throw new Error("each app requires product_id and asc_app_id");
    }
  }
  return config;
}

function loadSnapshot(args: Record<string, string | boolean>, app?: AppConfig): Snapshot {
  if (typeof args["asc-json"] === "string") {
    return JSON.parse(readFileSync(args["asc-json"], "utf8")) as Snapshot;
  }
  if (typeof args["asc-command"] !== "string" || !app) usage();
  const date = String(args.date ?? todayISO());
  const cmd = args["asc-command"]
    .replaceAll("{app_id}", shellQuote(app.asc_app_id))
    .replaceAll("{product_id}", shellQuote(app.product_id))
    .replaceAll("{vendor}", shellQuote(app.vendor ?? ""))
    .replaceAll("{date}", shellQuote(date));
  const result = spawnSync("/bin/zsh", ["-lc", cmd], { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(`ASC command failed for ${app.asc_app_id}: ${result.stderr || result.stdout}`);
  }
  const parsed = JSON.parse(result.stdout) as Snapshot & SnapshotApp;
  if (!parsed.apps && !parsed.rows) {
    return {
      date: parsed.date,
      captured_at: parsed.captured_at,
      apps: [{ ...parsed, asc_app_id: app.asc_app_id, metrics: parsed.metrics ?? parsed }],
    };
  }
  return parsed;
}

function exportsForSnapshot(config: Config, snapshot: Snapshot, dateOverride = ""): SignalExport[] {
  const rows = snapshot.apps ?? snapshot.rows ?? [];
  const out: SignalExport[] = [];
  for (const app of config.apps) {
    const row = rows.find((candidate) => rowAppID(candidate) === app.asc_app_id);
    if (!row) throw new Error(`snapshot missing asc_app_id ${app.asc_app_id}`);
    const date = dateOverride || row.date || snapshot.date || todayISO();
    const observedAt = row.captured_at || snapshot.captured_at || `${date}T00:00:00Z`;
    for (const metric of app.metrics ?? defaultMetrics) {
      const value = metricValue(row, metric);
      if (value === undefined) continue;
      out.push(signalExport(app, metric, value, date, observedAt));
    }
  }
  return out;
}

function rowAppID(row: SnapshotApp): string {
  return String(row.asc_app_id ?? row.apple_identifier ?? row.app_id ?? "");
}

function metricValue(row: SnapshotApp, metric: string): number | undefined {
  const normalized = normalizeMetric(metric);
  const metrics = row.metrics ?? {};
  if (Array.isArray(metrics)) {
    return metricArrayValue(metrics, normalized, metric);
  }
  const value =
    (metrics as Record<string, unknown>)[normalized] ??
    (metrics as Record<string, unknown>)[metric] ??
    row[normalized] ??
    row[metric];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function metricArrayValue(metrics: ASCMetricPoint[], normalized: string, raw: string): number | undefined {
  for (const candidate of metricLookupNames(normalized, raw)) {
    const found = metrics.find((metric) => metric.name === candidate);
    if (!found || (found.status && found.status !== "ok")) continue;
    const value = found.thisDay ?? found.value;
    if (typeof value === "number" && Number.isFinite(value)) return value;
  }
  return undefined;
}

function metricLookupNames(normalized: string, raw: string): string[] {
  const aliases: Record<string, string[]> = {
    units: ["units", "monetized_units", "subscription_units", "renewal_units"],
    // Subscription/renewal proceeds describe a subset and do not attest USD.
    // Only an explicitly named USD total can populate this metric, including zero.
    developer_proceeds_usd: ["developer_proceeds_usd"],
  };
  return [...new Set([normalized, raw, ...(aliases[normalized] ?? [])])];
}

function normalizeMetric(metric: string): string {
  switch (metric) {
    case "downloads":
      return "units";
    case "conversion_rate":
      return "product_page_conversion_rate";
    default:
      return metric;
  }
}

function signalExport(app: AppConfig, metric: string, value: number, date: string, observedAt: string): SignalExport {
  const normalizedMetric = normalizeMetric(metric);
  const ref = `asc:daily:${app.asc_app_id}:${date}:${normalizedMetric}`;
  return {
    schema_version: "compound.product_signal_export.v0.1",
    source: { kind: "app_store_connect", ref },
    signal_id: stableID("sig_", app.product_id, "app_store_connect", app.asc_app_id, normalizedMetric, date),
    product_id: app.product_id,
    surface: app.surface ?? "app_store",
    change_event: "App Store Connect daily metric snapshot observed.",
    metric: {
      name: normalizedMetric,
      before: { value, period: date, ref },
      after: { value, period: date, ref },
    },
    window: `daily snapshot ${date}`,
    confidence: 0.72,
    confounders: [
      "daily provider snapshot; no causal movement is implied",
      "ASC analytics and sales availability can lag by provider",
    ],
    interpretations: [
      {
        lens: "provider_snapshot",
        claim: "records observed App Store Connect metric without causal attribution",
      },
    ],
    next_action: "Use as product feedback evidence; do not infer causality without reviewed hypothesis.",
    evidence_refs: [ref, `asc:app:${app.asc_app_id}`, `asc:metric:${normalizedMetric}`],
    observed_at: observedAt,
  };
}

function stableID(prefix: string, ...parts: string[]): string {
  return prefix + createHash("sha256").update(parts.join("\0")).digest("hex").slice(0, 12);
}

function writeExports(exports: SignalExport[], outDir: string): string[] {
  mkdirSync(outDir, { recursive: true });
  return exports.map((doc) => {
    const metric = doc.source.ref.split(":").at(-1);
    const path = join(outDir, `${doc.metric.after.period}.${safeName(doc.product_id)}.${metric}.${doc.signal_id}.json`);
    writeFileSync(path, `${JSON.stringify(doc, null, 2)}\n`);
    return path;
  });
}

function applyExports(paths: string[], compoundBin: string): string[] {
  return paths.map((path) => {
    const result = spawnSync(compoundBin, ["product", "signal", path], { encoding: "utf8" });
    if (result.status !== 0) {
      throw new Error(`compound product signal ${basename(path)} failed: ${result.stderr || result.stdout}`);
    }
    return result.stdout.trim();
  });
}

function safeName(value: string): string {
  return value.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

function todayISO(): string {
  return new Date().toISOString().slice(0, 10);
}

export { exportsForSnapshot, parseConfig, signalExport };

if (import.meta.main) {
  try {
    const args = parseArgs(process.argv.slice(2));
    if (typeof args.config !== "string") usage();
    const config = parseConfig(args.config);
    const outDir =
      typeof args["out-dir"] === "string"
        ? args["out-dir"]
        : `${process.env.COMPOUND_HOME ?? `${process.env.HOME}/.compound`}/state/asc-daily-signals`;
    const compoundBin = typeof args["compound-bin"] === "string" ? args["compound-bin"] : "compound";
    const exports =
      typeof args["asc-command"] === "string" && typeof args["asc-json"] !== "string"
        ? config.apps.flatMap((app) => exportsForSnapshot({ apps: [app] }, loadSnapshot(args, app), String(args.date ?? "")))
        : exportsForSnapshot(config, loadSnapshot(args), String(args.date ?? ""));
    if (exports.length === 0) throw new Error("no ASC metrics mapped");
    const paths = writeExports(exports, outDir);
    const applied = args.apply ? applyExports(paths, compoundBin) : [];
    const report = { status: "ok", exports: paths.length, paths, applied: applied.length };
    if (args.json) {
      console.log(JSON.stringify(report, null, 2));
    } else {
      console.log(`asc-daily-signals ok exports=${paths.length} applied=${applied.length}`);
      for (const path of paths) console.log(`- ${path}`);
    }
  } catch (error) {
    console.error(`asc-daily-signals: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
