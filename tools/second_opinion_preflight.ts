#!/usr/bin/env bun

import { existsSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

type Status = "ok" | "warn" | "fail";

type SessionMeta = {
  status?: string;
  model?: string;
  mode?: string;
  createdAt?: string;
  updatedAt?: string | null;
  conversationUrl?: string | null;
  outputPath?: string | null;
  response?: unknown;
  error?: unknown;
};

type Check = {
  id: string;
  status: Status;
  summary: string;
  next_action?: string;
};

type Action = {
  id: string;
  kind: string;
  command: string;
  boundary: string;
  prerequisites?: string[];
  expected_validation?: string[];
  stop_conditions?: string[];
  requires_operator?: boolean;
};

type PreflightOptions = {
  sessionID: string;
  sessionsRoot: string;
  answerPath?: string;
  sourceObservationPath?: string;
};

type PreflightReport = {
  schema_version: "compound.second_opinion_preflight.v0.1";
  status: Status;
  session_id: string;
  session: {
    exists: boolean;
    status?: string;
    model?: string;
    mode?: string;
    created_at?: string;
    updated_at?: string | null;
    has_conversation_url: boolean;
    has_response: boolean;
    has_error: boolean;
    output_log_bytes: number;
    output_has_answer_marker: boolean;
  };
  answer: {
    checked: boolean;
    exists: boolean;
    bytes: number;
  };
  checks: Check[];
  actions: Action[];
};

function usage(): never {
  console.error(`usage: bun tools/second_opinion_preflight.ts --session <id> [--sessions-root <dir>] [--answer <private-answer.md>] [--source-observation <private-source-observation.yaml>] [--json]

Preflights a private second-opinion session before harvesting/importing it as Compound no-promote evidence. It reads local session metadata/log sizes only; it does not attach to a browser, harvest answers, or run source observe.`);
  process.exit(2);
}

function parseArgs(argv: string[]): Record<string, string | boolean> {
  const out: Record<string, string | boolean> = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--json") {
      out.json = true;
      continue;
    }
    if (arg === "--session" || arg === "--sessions-root" || arg === "--answer" || arg === "--source-observation") {
      const value = argv[++i];
      if (!value) usage();
      out[arg.slice(2)] = value;
      continue;
    }
    usage();
  }
  return out;
}

function statusFromChecks(checks: Check[]): Status {
  if (checks.some((check) => check.status === "fail")) return "fail";
  if (checks.some((check) => check.status === "warn")) return "warn";
  return "ok";
}

function readJSON(path: string): unknown {
  return JSON.parse(readFileSync(path, "utf8"));
}

function fileBytes(path: string | undefined): number {
  if (!path || !existsSync(path)) return 0;
  const stat = statSync(path);
  return stat.isFile() ? stat.size : 0;
}

function defaultSessionsRoot(): string {
  return join(process.env.HOME || "", ".oracle", "sessions");
}

function answerMarker(output: string): boolean {
  return /\bAnswer\s*:/i.test(output) || /Saved to:/i.test(output);
}

export function buildPreflightReport(opts: PreflightOptions): PreflightReport {
  const sessionDir = join(opts.sessionsRoot, opts.sessionID);
  const metaPath = join(sessionDir, "meta.json");
  const outputPath = join(sessionDir, "output.log");
  const checks: Check[] = [];

  let meta: SessionMeta = {};
  let sessionExists = existsSync(sessionDir);
  if (!sessionExists) {
    checks.push({
      id: "session_dir",
      status: "fail",
      summary: "second-opinion session directory missing",
      next_action: "check the session id with the private backend status command before harvesting",
    });
  } else {
    checks.push({ id: "session_dir", status: "ok", summary: "second-opinion session directory exists" });
  }

  if (sessionExists && existsSync(metaPath)) {
    try {
      meta = readJSON(metaPath) as SessionMeta;
      checks.push({ id: "metadata", status: "ok", summary: "session metadata readable" });
    } catch (err) {
      checks.push({
        id: "metadata",
        status: "fail",
        summary: `session metadata unreadable: ${err instanceof Error ? err.message : String(err)}`,
        next_action: "repair or ignore this session; do not import an unverified answer",
      });
    }
  } else if (sessionExists) {
    checks.push({
      id: "metadata",
      status: "fail",
      summary: "session metadata missing",
      next_action: "reattach or rerun only if the old session cannot be recovered",
    });
  }

  const outputLogBytes = fileBytes(outputPath);
  const output = outputLogBytes > 0 ? readFileSync(outputPath, "utf8") : "";
  const hasAnswerMarker = answerMarker(output);
  const answerBytes = fileBytes(opts.answerPath);
  const answerExists = answerBytes > 0;
  const hasResponse = meta.response != null;
  const hasConversationURL = typeof meta.conversationUrl === "string" && meta.conversationUrl.trim() !== "";
  const hasError = meta.error != null;
  const sessionStatus = String(meta.status || "");

  if (!sessionExists) {
    // Already covered by session_dir; keep downstream checks quiet.
  } else if (hasError) {
    checks.push({
      id: "session_status",
      status: "fail",
      summary: "session metadata records an error",
      next_action: "inspect private session logs; do not import answer evidence from this run",
    });
  } else if (answerExists) {
    checks.push({ id: "answer_file", status: "ok", summary: "saved answer file exists" });
  } else if (hasResponse || hasAnswerMarker) {
    checks.push({
      id: "answer_file",
      status: "warn",
      summary: "session appears to have answer material but no saved private answer file was provided",
      next_action: "harvest the session into a private answer file before source observe",
    });
  } else if (sessionStatus === "running") {
    checks.push({
      id: "answer_file",
      status: "warn",
      summary: "session is still running or detached and no saved answer file exists",
      next_action: "reattach or harvest the session; stop if no live tab or response is available",
    });
  } else {
    checks.push({
      id: "answer_file",
      status: "warn",
      summary: "no saved answer file exists",
      next_action: "harvest a completed session before importing evidence",
    });
  }

  const sourceObservationProvided = Boolean(opts.sourceObservationPath && opts.sourceObservationPath.trim());
  if (answerExists && sourceObservationProvided) {
    checks.push({ id: "source_observation", status: "ok", summary: "source observation path supplied for no-promote import" });
  } else if (answerExists) {
    checks.push({
      id: "source_observation",
      status: "warn",
      summary: "answer exists but source observation path is missing",
      next_action: "shape a compound.source_observation.v0.1 file before importing",
    });
  }

  const answerArg = opts.answerPath || "<private-answer.md>";
  const sourceObservationArg = opts.sourceObservationPath || "<private-source-observation.yaml>";
  const actions: Action[] = [
    {
      id: "harvest_answer",
      kind: "operator_recovery",
      command: `oracle session ${shellQuote(opts.sessionID)} --harvest --write-output ${shellQuote(answerArg)}`,
      boundary: "operator/private backend action; saves answer privately; does not make answer authoritative",
      prerequisites: ["session directory exists", "private backend has a live or recoverable completed response"],
      expected_validation: ["private answer file exists and is non-empty"],
      stop_conditions: ["no live tab", "session has no response", "metadata records error"],
      requires_operator: true,
    },
    {
      id: "import_no_promote_evidence",
      kind: "source_observe",
      command: `compound source observe ${shellQuote(sourceObservationArg)}`,
      boundary: "imports second-opinion answer as SourceObservation -> candidate -> NoPromoteDecision; no memory promotion",
      prerequisites: ["private answer file exists", "source observation maps promote_allowed=false"],
      expected_validation: ["source_observation proof line", "candidate proof line", "no_promote proof line"],
      stop_conditions: ["answer file missing", "source observation missing", "source observation would promote truth"],
      requires_operator: true,
    },
  ];

  return {
    schema_version: "compound.second_opinion_preflight.v0.1",
    status: statusFromChecks(checks),
    session_id: opts.sessionID,
    session: {
      exists: sessionExists,
      status: sessionStatus || undefined,
      model: meta.model,
      mode: meta.mode,
      created_at: meta.createdAt,
      updated_at: meta.updatedAt,
      has_conversation_url: hasConversationURL,
      has_response: hasResponse,
      has_error: hasError,
      output_log_bytes: outputLogBytes,
      output_has_answer_marker: hasAnswerMarker,
    },
    answer: {
      checked: Boolean(opts.answerPath),
      exists: answerExists,
      bytes: answerBytes,
    },
    checks,
    actions,
  };
}

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

function printHuman(report: PreflightReport): void {
  console.log(`second_opinion_preflight status=${report.status} session=${report.session_id}`);
  for (const check of report.checks) {
    console.log(`check ${check.id}: ${check.status} ${check.summary}`);
    if (check.next_action) console.log(`  next: ${check.next_action}`);
  }
  for (const action of report.actions) {
    console.log(`action ${action.id}: ${action.command}`);
    console.log(`  boundary: ${action.boundary}`);
  }
}

if (import.meta.main) {
  const args = parseArgs(process.argv.slice(2));
  const sessionID = typeof args.session === "string" ? args.session.trim() : "";
  if (!sessionID) usage();
  const report = buildPreflightReport({
    sessionID,
    sessionsRoot: typeof args["sessions-root"] === "string" ? args["sessions-root"] : defaultSessionsRoot(),
    answerPath: typeof args.answer === "string" ? args.answer : undefined,
    sourceObservationPath: typeof args["source-observation"] === "string" ? args["source-observation"] : undefined,
  });
  if (args.json) {
    console.log(JSON.stringify(report, null, 2));
  } else {
    printHuman(report);
  }
  process.exit(report.status === "fail" ? 1 : 0);
}
