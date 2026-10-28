#!/usr/bin/env bun
// Rented-edge tool: convert a synthetic-playtest run directory (raw pf*.json findings +
// rec-pf*.json recorded wrappers) into a compound.observation_batch.v0.1 JSON so visual/UX
// findings enter the SAME adjudicated intake as every other tunnel (parked source
// observations, never product signals). Adapters observe; nobody promotes from the field.
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";

const BATCH_SCHEMA = "compound.observation_batch.v0.1";
const FINDING_SCHEMA = "playtest.finding.v0.1";
const RECORD_SCHEMA = "compound.review_finding_record.v0.1";
const COLLECTOR = "tools/playtest_findings_observe.ts";

// Descriptive enum from the playtest.finding.v0.1 seam. It documents, it does not gate:
// a finding whose type falls outside this set is still emitted verbatim, with a warning.
const KNOWN_FINDING_TYPES = new Set([
  "false_completion_feedback",
  "dead_end",
  "jargon_wall",
  "hidden_affordance",
  "wrong_default",
  "affordance_missing",
  "comprehension_failure",
  "nav_loop",
  "slow_surface",
]);

// Evidence sub-keys handled specially (not folded into the generic content renderer).
const EVIDENCE_STRUCTURAL_KEYS = new Set(["member_findings", "screenshots"]);

export type BuildOptions = { product: string; capturedAt?: string };
export type EmitOptions = BuildOptions & { homePath: string };

export type PlaytestFinding = {
  schema_version?: string;
  id?: string;
  severity?: string;
  type?: string;
  title?: string;
  evidence?: Record<string, unknown>;
  repro?: unknown;
  source?: string;
};

export type ReviewFindingRecord = {
  schema_version?: string;
  id?: string;
  work_id?: string;
  created_at?: string;
  summary?: string;
  target_path?: string;
  artifact_path?: string;
  source_artifacts?: Array<{ path?: string; artifact_path?: string; hash?: string }>;
  evidence_refs?: string[];
};

export type ObservationDimensions = Record<string, string>;

export type Observation = {
  id: string;
  observed_at: string;
  kind: "system";
  title?: string;
  content?: string;
  dimensions: ObservationDimensions;
  raw_ref?: string;
  confidence: number;
  evidence_refs: string[];
};

export type ObservationBatch = {
  schema_version: typeof BATCH_SCHEMA;
  source: { kind: string; ref: string; captured_at: string; collector: string; privacy: "private" };
  subject: { kind: "product"; id: string; surface: "playtest" };
  observations: Observation[];
};

export type FindingPair = {
  findingFile: string;
  finding: PlaytestFinding;
  record: ReviewFindingRecord | null;
};

export type BuildResult = { batch: ObservationBatch; warnings: string[]; pairs: FindingPair[] };

export type EmitReport = {
  run: string;
  product: string;
  batchPath: string | null;
  newCount: number;
  seenCount: number;
  warnings: string[];
  observationIds: string[];
};

type SeenLedger = {
  schema_version: "compound.playtest_findings_seen.v0.1";
  run: string;
  updated_at: string;
  seen: Array<{ id: string; finding_id: string; batch: string }>;
};

const MAX_DIMENSION_VALUE = 512;
const MAX_TITLE = 512;
const MAX_EVIDENCE_REFS = 32;
const MAX_EVIDENCE_REF_LEN = 256;

function findingsDir(runDir: string): string {
  const nested = join(runDir, "findings");
  if (existsSync(nested) && statSync(nested).isDirectory()) return nested;
  // Allow pointing --run-dir straight at a findings directory too.
  return runDir;
}

function runRef(runDir: string): string {
  // Source.Ref is the run's basename; if --run-dir points at .../findings, use its parent.
  const base = basename(runDir.replace(/\/+$/, ""));
  return base === "findings" ? basename(runDir.replace(/\/+findings\/*$/, "")) : base;
}

export function collectFindingPairs(runDir: string): { pairs: FindingPair[]; warnings: string[] } {
  const dir = findingsDir(runDir);
  const warnings: string[] = [];
  const raws: Array<{ file: string; finding: PlaytestFinding }> = [];
  const records: Array<{ file: string; record: ReviewFindingRecord }> = [];

  const files = readdirSync(dir)
    .filter((f) => f.endsWith(".json"))
    .sort();
  for (const file of files) {
    const raw = JSON.parse(readFileSync(join(dir, file), "utf8"));
    const schema = typeof raw?.schema_version === "string" ? raw.schema_version : "";
    if (schema === FINDING_SCHEMA) {
      raws.push({ file, finding: raw as PlaytestFinding });
    } else if (schema === RECORD_SCHEMA) {
      records.push({ file, record: raw as ReviewFindingRecord });
    } else {
      warnings.push(`skipped ${file}: unrecognized schema_version ${schema || "(missing)"}`);
    }
  }

  // Pair each recorded wrapper to a raw finding by source_artifacts[].path basename.
  const recordByRawBase = new Map<string, { file: string; record: ReviewFindingRecord }>();
  for (const rec of records) {
    for (const artifact of rec.record.source_artifacts ?? []) {
      const p = typeof artifact?.path === "string" ? artifact.path : "";
      if (p) recordByRawBase.set(basename(p), rec);
    }
  }

  const pairs: FindingPair[] = raws.map(({ file, finding }) => {
    const rec = recordByRawBase.get(basename(file)) ?? null;
    return { findingFile: file, finding, record: rec ? rec.record : null };
  });
  return { pairs, warnings };
}

function normalizeRfc3339(value: string | undefined, fallback: string): string {
  const raw = (value ?? "").trim();
  const candidate = raw || fallback;
  const ms = Date.parse(candidate);
  if (!Number.isFinite(ms)) return fallback;
  // Drop fractional seconds to a clean RFC3339 second-precision instant.
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

function truncate(value: string, max: number): string {
  if (value.length <= max) return value;
  return value.slice(0, max - 1).trimEnd() + "…";
}

function asStringArray(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((v): v is string => typeof v === "string" && v.trim() !== "");
}

function titleize(key: string): string {
  const label = key.replace(/_/g, " ").trim();
  if (label === "") return "";
  return label.charAt(0).toUpperCase() + label.slice(1);
}

function safeEvidenceRef(ref: string): string | null {
  const value = ref.trim();
  if (value === "" || value.length > MAX_EVIDENCE_REF_LEN) return null;
  return value;
}

function artifactHashRef(hash: string | undefined): string | null {
  const h = (hash ?? "").trim();
  if (h === "") return null;
  // The intake door allows the artifact_sha256: ref scheme; raw sha256: is not allowed.
  const hex = h.startsWith("sha256:") ? h.slice("sha256:".length) : h;
  if (!/^[0-9a-fA-F]{16,128}$/.test(hex)) return null;
  return `artifact_sha256:${hex.toLowerCase()}`;
}

function buildContent(finding: PlaytestFinding): string {
  const evidence = (finding.evidence ?? {}) as Record<string, unknown>;
  const lines: string[] = [];
  const severity = (finding.severity ?? "").trim();
  const type = (finding.type ?? "").trim();
  if (severity || type) lines.push(`Severity: ${severity || "unknown"} | Type: ${type || "unknown"}`);

  // Faithful, shape-agnostic render of the evidence object: known belief/truth/impact keys
  // first (in a stable order), then any remaining scalar/array string fields verbatim.
  const orderedKeys = [
    "player_belief",
    "ledger_truth",
    "impact",
    ...Object.keys(evidence).filter(
      (k) => !["player_belief", "ledger_truth", "impact"].includes(k) && !EVIDENCE_STRUCTURAL_KEYS.has(k),
    ),
  ];
  for (const key of orderedKeys) {
    if (!(key in evidence)) continue;
    const value = evidence[key];
    if (typeof value === "string" && value.trim() !== "") {
      lines.push(`${titleize(key)}: ${value.trim()}`);
    } else if (Array.isArray(value)) {
      const items = asStringArray(value);
      if (items.length > 0) lines.push(`${titleize(key)}: ${items.join("; ")}`);
    }
  }

  const memberFindings = asStringArray(evidence.member_findings);
  if (memberFindings.length > 0) lines.push(`Member findings: ${memberFindings.join("; ")}`);

  const repro = asStringArray(finding.repro);
  if (repro.length > 0) lines.push(`Repro:\n${repro.map((r) => `- ${r}`).join("\n")}`);

  const source = (finding.source ?? "").trim();
  if (source) lines.push(`Source: ${source}`);

  return lines.join("\n");
}

export function buildObservation(
  pair: FindingPair,
  runRefValue: string,
  opts: BuildOptions,
  capturedAt: string,
): { observation: Observation; warnings: string[] } {
  const { finding, record, findingFile } = pair;
  const warnings: string[] = [];
  const findingId = (finding.id ?? basename(findingFile).replace(/\.json$/, "")).trim();
  const type = (finding.type ?? "").trim();
  if (type && !KNOWN_FINDING_TYPES.has(type)) {
    warnings.push(`unknown finding type ${type} for ${findingId} (carried verbatim; enum is descriptive, not a gate)`);
  }
  if (!record) {
    warnings.push(`partial provenance for ${findingId}: no recorded wrapper (rec-*.json) paired to ${findingFile}; evidence still emitted`);
  }

  const evidence = (finding.evidence ?? {}) as Record<string, unknown>;
  const dimensions: ObservationDimensions = { run: runRefValue, finding_id: findingId };
  if (finding.severity) dimensions.severity = truncate(finding.severity.trim(), MAX_DIMENSION_VALUE);
  if (type) dimensions.finding_type = truncate(type, MAX_DIMENSION_VALUE);
  dimensions.provenance = record ? "recorded" : "partial";
  if (record?.work_id) dimensions.work_id = truncate(record.work_id.trim(), MAX_DIMENSION_VALUE);

  // Evidence refs: screenshots + raw finding path + recorded wrapper path + artifact hashes.
  const refs: string[] = [];
  for (const shot of asStringArray(evidence.screenshots)) refs.push(shot);
  refs.push(join("findings", basename(findingFile)));
  if (record) {
    const recPath = (record.target_path ?? record.artifact_path ?? "").trim();
    if (recPath) refs.push(join("findings", basename(recPath)));
    for (const artifact of record.source_artifacts ?? []) {
      const hashRef = artifactHashRef(artifact?.hash);
      if (hashRef) refs.push(hashRef);
    }
  }
  const evidenceRefs = dedupe(
    refs.map(safeEvidenceRef).filter((r): r is string => r !== null),
  ).slice(0, MAX_EVIDENCE_REFS);

  const observation: Observation = {
    id: `playtest-${runRefValue}-${findingId}`,
    observed_at: normalizeRfc3339(record?.created_at, capturedAt),
    kind: "system",
    title: finding.title ? truncate(finding.title.trim(), MAX_TITLE) : undefined,
    content: buildContent(finding),
    dimensions,
    raw_ref: join("findings", basename(findingFile)),
    confidence: record ? 0.7 : 0.5,
    evidence_refs: evidenceRefs,
  };
  return { observation, warnings };
}

export function buildPlaytestBatch(runDir: string, opts: BuildOptions): BuildResult {
  const product = (opts.product ?? "").trim();
  if (!product) throw new Error("--product <product_id> is required (playtest labels are synthetic)");
  const capturedAt = normalizeRfc3339(opts.capturedAt, new Date().toISOString());
  const ref = runRef(runDir);
  const { pairs, warnings: collectWarnings } = collectFindingPairs(runDir);
  const warnings = [...collectWarnings];

  const observations: Observation[] = [];
  for (const pair of pairs) {
    const { observation, warnings: obsWarnings } = buildObservation(pair, ref, opts, capturedAt);
    observations.push(observation);
    warnings.push(...obsWarnings);
  }

  const batch: ObservationBatch = {
    schema_version: BATCH_SCHEMA,
    source: { kind: "playtest_findings", ref, captured_at: capturedAt, collector: COLLECTOR, privacy: "private" },
    subject: { kind: "product", id: product, surface: "playtest" },
    observations,
  };
  return { batch, warnings, pairs };
}

function observationsDir(homePath: string): string {
  return join(homePath, "state", "playtest-observations");
}

function seenLedgerPath(homePath: string, ref: string): string {
  return join(observationsDir(homePath), `seen-${ref}.json`);
}

function loadSeenLedger(homePath: string, ref: string): SeenLedger {
  const path = seenLedgerPath(homePath, ref);
  if (!existsSync(path)) {
    return { schema_version: "compound.playtest_findings_seen.v0.1", run: ref, updated_at: "", seen: [] };
  }
  const parsed = JSON.parse(readFileSync(path, "utf8")) as SeenLedger;
  return {
    schema_version: "compound.playtest_findings_seen.v0.1",
    run: ref,
    updated_at: parsed.updated_at ?? "",
    seen: Array.isArray(parsed.seen) ? parsed.seen : [],
  };
}

export function emitPlaytestObservations(runDir: string, opts: EmitOptions): EmitReport {
  const homePath = (opts.homePath ?? "").trim();
  if (!homePath) throw new Error("COMPOUND_HOME (or --home) required: seen-ledger and batch output live under it");
  const { batch, warnings } = buildPlaytestBatch(runDir, opts);
  const ref = batch.source.ref;
  const dir = observationsDir(homePath);
  mkdirSync(dir, { recursive: true });

  const ledger = loadSeenLedger(homePath, ref);
  const seenIds = new Set(ledger.seen.map((row) => row.id));
  const fresh = batch.observations.filter((obs) => !seenIds.has(obs.id));

  if (fresh.length === 0) {
    return {
      run: ref,
      product: batch.subject.id,
      batchPath: null,
      newCount: 0,
      seenCount: ledger.seen.length,
      warnings,
      observationIds: [],
    };
  }

  // Name the batch by the set of new finding ids so re-emits are deterministic and
  // incremental runs (new findings only) never collide with an earlier batch.
  const hash = createHash("sha256").update(fresh.map((o) => o.id).join("\n")).digest("hex").slice(0, 12);
  const batchPath = join(dir, `batch-${ref}-${hash}.json`);
  const outBatch: ObservationBatch = { ...batch, observations: fresh };
  writeFileSync(batchPath, JSON.stringify(outBatch, null, 2) + "\n", "utf8");

  const now = new Date().toISOString().replace(/\.\d{3}Z$/, "Z");
  const updated: SeenLedger = {
    schema_version: "compound.playtest_findings_seen.v0.1",
    run: ref,
    updated_at: now,
    seen: [
      ...ledger.seen,
      ...fresh.map((obs) => ({ id: obs.id, finding_id: obs.dimensions.finding_id, batch: basename(batchPath) })),
    ],
  };
  writeFileSync(seenLedgerPath(homePath, ref), JSON.stringify(updated, null, 2) + "\n", "utf8");

  return {
    run: ref,
    product: batch.subject.id,
    batchPath,
    newCount: fresh.length,
    seenCount: updated.seen.length,
    warnings,
    observationIds: fresh.map((o) => o.id),
  };
}

function dedupe(values: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const value of values) {
    if (seen.has(value)) continue;
    seen.add(value);
    out.push(value);
  }
  return out;
}

function parseArgs(argv: string[]): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith("--")) usage();
    const key = arg.slice(2);
    const next = argv[i + 1];
    if (!next || next.startsWith("--")) out[key] = true;
    else {
      out[key] = next;
      i++;
    }
  }
  return out;
}

function usage(): never {
  console.error(
    "usage: bun tools/playtest_findings_observe.ts --run-dir <path> --product <product_id> [--captured-at RFC3339] [--home <dir>] [--json]",
  );
  process.exit(2);
}

function main(): void {
  const args = parseArgs(process.argv.slice(2));
  const runDir = typeof args["run-dir"] === "string" ? args["run-dir"] : "";
  const product = typeof args.product === "string" ? args.product : "";
  if (!runDir || !product) usage();
  const homePath = (typeof args.home === "string" ? args.home : process.env.COMPOUND_HOME) ?? "";
  const capturedAt = typeof args["captured-at"] === "string" ? args["captured-at"] : undefined;
  const report = emitPlaytestObservations(runDir, { product, capturedAt, homePath });

  if (args.json) {
    process.stdout.write(JSON.stringify(report, null, 2) + "\n");
  } else if (report.batchPath) {
    process.stdout.write(`wrote ${report.batchPath} (${report.newCount} new observation(s))\n`);
  } else {
    process.stdout.write(`no new observations (${report.seenCount} already seen)\n`);
  }
  for (const warning of report.warnings) console.error(`warning: ${warning}`);
  console.error(
    `playtest-findings-observe run=${report.run} product=${report.product} new=${report.newCount} seen=${report.seenCount}`,
  );
  if (report.batchPath) {
    console.error(`next: compound source observe-batch ${report.batchPath} --dry-run --json`);
  }
}

if (import.meta.main) {
  try {
    main();
  } catch (err) {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(1);
  }
}
