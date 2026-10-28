#!/usr/bin/env bun

import { existsSync, mkdtempSync, readdirSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

type Args = Record<string, string | boolean>;

type Check = {
  name: string;
  ok: boolean;
  detail?: string;
};

type Action = {
  kind: string;
  command: string;
  boundary: string;
  prerequisites?: string[];
  proof?: string;
  expected_validation?: string[];
  rollback?: string;
  stop_conditions?: string[];
  requires_operator?: boolean;
};

type KnowledgeImportReport = {
  items: number;
  imported: number;
  reused: number;
  echoes?: number;
  evidence_ids?: string[];
  candidate_ids?: string[];
  extraction_ids?: string[];
};

type ImportSafetyReport = {
  evidence_count: number;
  private_evidence_count: number;
  max_imported_quote_chars: number;
  candidate_count: number;
  private_candidate_count: number;
  candidate_trace_refs_match_evidence: boolean;
  promoted_claim_count: number;
};

type PreflightReport = {
  schema_version: "compound.chatgpt_conversation_preflight.v0.1";
  status: "ok" | "fail";
  input: string;
  temp_home: string;
  export_path: string;
  source_ref: string;
  limit: number;
  min_chars: number;
  max_content_chars: number;
  export_summary: {
    privacy?: string;
    source_kind?: string;
    item_count: number;
    max_observed_content_chars: number;
    refs: string[];
  };
  import_safety: ImportSafetyReport;
  first_import: KnowledgeImportReport;
  second_import: KnowledgeImportReport;
  checks: Check[];
  actions: Action[];
};

function parseArgs(argv: string[]): Args {
  const out: Args = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith("--")) {
      if (!out.input) {
        out.input = arg;
        continue;
      }
      usage();
    }
    const key = arg.slice(2);
    const next = argv[i + 1];
    if (!next || next.startsWith("--")) {
      out[key] = true;
    } else {
      out[key] = next;
      i++;
    }
  }
  return out;
}

function usage(): never {
  console.error(
    "usage: bun tools/chatgpt_conversation_preflight.ts <conversation-export-file-or-dir> [--compound-command compound] [--limit 5] [--min-chars 500] [--max-content-chars 24000] [--source-ref REF] [--captured-at RFC3339]",
  );
  process.exit(2);
}

function numberFlag(value: string | boolean | undefined, fallback: number): number {
  if (typeof value !== "string") return fallback;
  const n = Number(value);
  return Number.isFinite(n) ? n : fallback;
}

function stringFlag(value: string | boolean | undefined, fallback: string): string {
  return typeof value === "string" && value.trim() !== "" ? value : fallback;
}

function run(command: string, args: string[], env: Record<string, string> = {}) {
  const result = spawnSync(command, args, {
    cwd: process.cwd(),
    env: { ...process.env, ...env },
    encoding: "utf8",
  });
  if (result.error) {
    throw result.error;
  }
  if (result.status !== 0) {
    throw new Error(
      `${command} ${args.join(" ")} failed with status ${result.status}\nstdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
    );
  }
  return result.stdout;
}

function runJSON<T>(command: string, args: string[], env: Record<string, string> = {}): T {
  const stdout = run(command, args, env);
  return JSON.parse(stdout) as T;
}

function shellQuote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

function ok(name: string, condition: boolean, detail?: string): Check {
  return { name, ok: condition, detail };
}

function readJSONL<T>(path: string): T[] {
  if (!existsSync(path)) return [];
  return readFileSync(path, "utf8")
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => JSON.parse(line) as T);
}

function readCandidateFiles(path: string): Array<Record<string, unknown>> {
  if (!existsSync(path)) return [];
  return readdirSync(path)
    .filter((name) => name.endsWith(".json"))
    .map((name) => JSON.parse(readFileSync(join(path, name), "utf8")) as Record<string, unknown>);
}

function buildImportSafetyReport(tempHome: string, evidenceIDs: string[]): ImportSafetyReport {
  const evidenceIDSet = new Set(evidenceIDs);
  const evidence = readJSONL<Record<string, unknown>>(join(tempHome, "ledger", "evidence.jsonl"));
  const importedEvidence = evidence.filter((item) => typeof item.id === "string" && evidenceIDSet.has(item.id));
  const candidates = readCandidateFiles(join(tempHome, "inbox", "memory-candidates"));
  const claims = readJSONL<Record<string, unknown>>(join(tempHome, "ledger", "claims.jsonl"));
  const quoteLengths = importedEvidence.map((item) =>
    typeof item.quote_text === "string" ? item.quote_text.length : 0,
  );
  return {
    evidence_count: importedEvidence.length,
    private_evidence_count: importedEvidence.filter((item) => item.visibility === "private").length,
    max_imported_quote_chars: quoteLengths.length > 0 ? Math.max(...quoteLengths) : 0,
    candidate_count: candidates.length,
    private_candidate_count: candidates.filter((item) => item.visibility === "private").length,
    candidate_trace_refs_match_evidence: candidates.every(
      (item) => typeof item.source_trace_ref === "string" && evidenceIDSet.has(item.source_trace_ref),
    ),
    promoted_claim_count: claims.length,
  };
}

function buildActions(exportPath: string): Action[] {
  const quoted = shellQuote(exportPath);
  return [
    {
      kind: "live_backup",
      command: "compound home backup --local",
      boundary: "operator-only live COMPOUND_HOME safety step before importing private conversation evidence",
      prerequisites: ["preflight_status_ok"],
      proof: "backup command prints archive path; keep it before import/extraction",
      expected_validation: ["backup_archive_path_recorded"],
      stop_conditions: ["preflight_status_not_ok", "backup_command_failed"],
      requires_operator: true,
    },
    {
      kind: "live_import",
      command: `compound knowledge import ${quoted} --json`,
      boundary: "operator-only live private data import; imports evidence/extraction requests, not promoted memory truth",
      prerequisites: ["preflight_status_ok", "live_backup"],
      proof: "report imported/reused counts; rerun must be idempotent",
      expected_validation: [
        "knowledge import report imported_or_reused_gt_0",
        "second import report imported_eq_0",
        "extraction_requests_created_for_imported_items",
      ],
      rollback: "restore the pre-import home backup if live import is wrong; do not hand-delete memory files",
      stop_conditions: ["preflight_status_not_ok", "backup_missing", "live_import_report_invalid"],
      requires_operator: true,
    },
    {
      kind: "extract_pending",
      command:
"compound knowledge extract --pending --openai-compatible-url <local-openai-compatible-url> --model auto/extract --json",
      boundary: "operator-controlled model lane; creates reviewable candidates, not promoted claims",
      prerequisites: ["live_import", "operator_model_lane_selected"],
      proof: "extraction report records route/model evidence and candidate IDs",
      expected_validation: ["extraction_report_records_route_evidence", "candidate_ids_created_or_reused"],
      stop_conditions: ["live_import_missing", "model_lane_unavailable", "candidate_extraction_failed"],
      requires_operator: true,
    },
    {
      kind: "review_candidates",
      command: "compound panel plan --target-kind memory_candidate --state parked --json && compound reviews --area panel-verdicts --json",
      boundary: "review planning only; promotion remains an explicit operator act",
      prerequisites: ["extract_pending"],
      proof: "review queue exposes candidates with decision cards/actions",
      expected_validation: ["review queue exposes decision cards", "review queue exposes bounded actions"],
      stop_conditions: ["candidate_count_zero", "review_surface_missing_decision_cards"],
      requires_operator: true,
    },
    {
      kind: "recall_proof_after_single_promotion",
      command:
        'compound memory recall <entity> --json && compound orient --harness codex --task "<paraphrase topic, not copied claim text>" --json',
      boundary: "only after operator promotes at most one reviewed claim",
      prerequisites: ["operator_promoted_at_most_one_claim"],
      proof: "the same promoted claim appears in recall and task-relevant ContextPack active_memory",
      expected_validation: ["recall_returns_promoted_claim", "orient_active_memory_contains_same_claim"],
      stop_conditions: ["unreviewed_bulk_promotion_requested", "recall_empty_after_promotion"],
      requires_operator: true,
    },
  ];
}

function preflight(argv: string[]): PreflightReport {
  const args = parseArgs(argv);
  const input = stringFlag(args.input, "");
  if (!input) usage();

  const limit = numberFlag(args.limit, 5);
  const minChars = numberFlag(args["min-chars"], 500);
  const maxContentChars = numberFlag(args["max-content-chars"], 24000);
  const sourceRef = stringFlag(args["source-ref"], "conversation-export:openai-conversations-small-batch");
  const capturedAt = stringFlag(args["captured-at"], new Date().toISOString());
  const compound = stringFlag(args["compound-command"], "compound");
  const tempRoot = mkdtempSync(join(tmpdir(), "compound-chatgpt-preflight-"));
  const tempHome = join(tempRoot, "home");
  const exportPath = join(tempRoot, "chatgpt-batch.json");

  run(compound, ["home", "init"], { COMPOUND_HOME: tempHome });

  run("bun", [
    "tools/chatgpt_conversation_edge.ts",
    input,
    "--out",
    exportPath,
    "--limit",
    String(limit),
    "--min-chars",
    String(minChars),
    "--max-content-chars",
    String(maxContentChars),
    "--source-ref",
    sourceRef,
    "--captured-at",
    capturedAt,
  ]);

  const exportDoc = JSON.parse(readFileSync(exportPath, "utf8"));
  const items = Array.isArray(exportDoc.items) ? exportDoc.items : [];
  const contentLengths = items.map((item: { content?: unknown }) =>
    typeof item.content === "string" ? item.content.length : 0,
  );
  const maxObservedContentChars = contentLengths.length > 0 ? Math.max(...contentLengths) : 0;
  const refs = items
    .map((item: { ref?: unknown }) => (typeof item.ref === "string" ? item.ref : ""))
    .filter(Boolean);

  const first = runJSON<KnowledgeImportReport>(compound, ["knowledge", "import", exportPath, "--json"], {
    COMPOUND_HOME: tempHome,
  });
  const second = runJSON<KnowledgeImportReport>(compound, ["knowledge", "import", exportPath, "--json"], {
    COMPOUND_HOME: tempHome,
  });
  const importSafety = buildImportSafetyReport(tempHome, first.evidence_ids ?? []);

  const checks = [
    ok("privacy_private", exportDoc.privacy === "private", `privacy=${exportDoc.privacy ?? ""}`),
    ok("source_kind_chatgpt", exportDoc.source?.kind === "chatgpt", `source.kind=${exportDoc.source?.kind ?? ""}`),
    ok("bounded_small_batch", items.length > 0 && items.length <= limit, `items=${items.length} limit=${limit}`),
    ok(
      "content_bound",
      contentLengths.every((length: number) => length <= maxContentChars),
      `max_observed=${maxObservedContentChars} max=${maxContentChars}`,
    ),
    ok("first_import_created_evidence", first.imported > 0, `imported=${first.imported}`),
    ok("second_import_idempotent", second.imported === 0 && second.reused >= first.imported, `imported=${second.imported} reused=${second.reused}`),
    ok(
      "extraction_requests_created",
      (first.extraction_ids?.length ?? 0) === items.length,
      `extraction_ids=${first.extraction_ids?.length ?? 0} items=${items.length}`,
    ),
    ok(
      "imported_evidence_private",
      importSafety.evidence_count === (first.evidence_ids?.length ?? 0) &&
        importSafety.private_evidence_count === importSafety.evidence_count,
      `private=${importSafety.private_evidence_count} evidence=${importSafety.evidence_count}`,
    ),
    ok(
      "imported_evidence_quote_bound",
      importSafety.max_imported_quote_chars > 0 && importSafety.max_imported_quote_chars <= 4000,
      `max_imported_quote_chars=${importSafety.max_imported_quote_chars}`,
    ),
    ok(
      "imported_candidates_private",
      importSafety.candidate_count > 0 && importSafety.private_candidate_count === importSafety.candidate_count,
      `private=${importSafety.private_candidate_count} candidates=${importSafety.candidate_count}`,
    ),
    ok(
      "candidate_trace_refs_match_evidence",
      importSafety.candidate_trace_refs_match_evidence,
      `candidate_count=${importSafety.candidate_count}`,
    ),
    ok("no_temp_promoted_claims", importSafety.promoted_claim_count === 0, `claims=${importSafety.promoted_claim_count}`),
  ];

  return {
    schema_version: "compound.chatgpt_conversation_preflight.v0.1",
    status: checks.every((check) => check.ok) ? "ok" : "fail",
    input,
    temp_home: tempHome,
    export_path: exportPath,
    source_ref: sourceRef,
    limit,
    min_chars: minChars,
    max_content_chars: maxContentChars,
    export_summary: {
      privacy: exportDoc.privacy,
      source_kind: exportDoc.source?.kind,
      item_count: items.length,
      max_observed_content_chars: maxObservedContentChars,
      refs,
    },
    import_safety: importSafety,
    first_import: first,
    second_import: second,
    checks,
    actions: buildActions(exportPath),
  };
}

if (import.meta.main) {
  try {
    const report = preflight(process.argv.slice(2));
    process.stdout.write(JSON.stringify(report, null, 2) + "\n");
    if (report.status !== "ok") process.exit(1);
  } catch (err) {
    console.error(err instanceof Error ? err.message : String(err));
    process.exit(1);
  }
}

export { preflight };
