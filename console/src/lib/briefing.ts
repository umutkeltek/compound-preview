/*
 * Briefing data-shaping — pure, server-safe, deterministic (no LLM, no fs, no
 * subprocess). The page (src/app/page.tsx) fetches projections; this module
 * turns them into the exact shapes the briefing components render. Ranking is
 * NOT here — it is the server-side job of `compound attention --json`; this only
 * translates already-ranked/already-projected data into view models.
 *
 * Client-side leverage ranking (`selectHighestLeverageGate`) was deleted in
 * Phase 2; only the age helpers and these projection-to-view-model mappers live
 * here now.
 */

import type { AttentionGroup } from "@/lib/attention";
import { arr, num, str, type Json, type ProjectionStatus, type ProjectionRead } from "@/lib/compound";
import { parseActionAuthorityBundle } from "@/lib/action-authority";
import type { AuthorityFace } from "@/representation";
import { toDate } from "@/lib/time";
import { genericActionProjectionEligible, genericActionRevision, OPERATOR_INPUTS } from "@/lib/generic-actions";
import { specFor } from "@/lib/decision-registry";
import { LEASE_HISTORY_LABEL } from "@/components/agents/executor-model";
import type { ContinueRequest } from "@/components/ContinueButton";
import type { StateTone } from "@/components/kit";

/* ---------- age helpers (presentation) ---------- */

export function humanAge(value: unknown): string {
  const date = value instanceof Date ? value : value ? new Date(value as string | number) : null;
  if (!date || Number.isNaN(date.getTime())) return "";
  const seconds = Math.max(0, Math.floor((Date.now() - date.getTime()) / 1000));
  if (seconds < 90) return "now";
  if (seconds < 90 * 60) return `${Math.round(seconds / 60)}m`;
  if (seconds < 36 * 3600) return `${Math.round(seconds / 3600)}h`;
  return `${Math.round(seconds / 86400)}d`;
}

export function isOld(value: unknown): boolean {
  const date = value instanceof Date ? value : value ? new Date(value as string | number) : null;
  if (!date || Number.isNaN(date.getTime())) return false;
  return Date.now() - date.getTime() >= 36 * 3600 * 1000;
}

/* ---------- THE headline's inline governed act ----------
 * Maps one AttentionGroup.action onto a governed /api/decision/execute payload.
 * Returns null when there is NO safe inline act — honest: the headline then
 * shows only a "Review" deep-link, never a fake button. The mapping mirrors how
 * the Reviews page already routes each action shape, so the same click runs the
 * same governed argv through the same three gates.
 */

export type HeadlineActDescriptor = {
  /** which /api/decision/execute pathway resolves + gates the argv server-side. */
  pathway: "bespoke" | "generic";
  area: string;
  itemId: string;
  /** bespoke pathway only. */
  option?: string;
  /** generic pathway only. */
  actionId?: string;
  /** Exact rendered generic owner action; server returns 409 on drift. */
  expectedRevision?: string;
  /** Exact Go-owned authority identity carried to the click-time preflight. */
  actionIntentId?: string;
  expectedAuthorityRevision?: string;
  authority?: AuthorityFace;
  /** Bespoke projection fields shown in the confirmation command. */
  fields?: Record<string, string>;
  label: string;
  /** display-only preview (server re-resolves the real argv before running). */
  argv: string[];
  requiresConfirmation: boolean;
  rationaleRequired: boolean;
};

export function headlineAct(
  group: AttentionGroup | null,
  projectionStatus?: ProjectionStatus,
): HeadlineActDescriptor | null {
  const action = group?.action;
  if (!group || !action) return null;
  const authorityBound = projectionStatus !== undefined;
  const authority = authorityBound ? parseActionAuthorityBundle(action) : null;
  if (
    authorityBound &&
    (projectionStatus !== "live" || !authority || !authority.current || !authority.allowed)
  ) return null;
  // Copy-only backend forms (e.g. panel per-item `/bin/sh -lc`) are never run
  // from a card — honest, no button.
  if (action.kind === "copy_only") return null;

  const argv = arr(action.argv).map(String);
  if (argv.length < 2 || argv[0] !== "compound") return null;
  const bare = argv.slice(1); // drop leading "compound"
  const label = authority?.wording || str(action.label) || "Act";
  const requiresConfirmation = Boolean(action.requires_confirmation) || Boolean(specFor(str(group.area))?.requiresConfirmation);

  // Panel run apply — exact owner action + exact argv shape only.
  if (!authorityBound && str(action.id) === "panel_run_apply") {
    const runId = bare[3] ?? "";
    if (
      bare.length !== 5 ||
      bare[0] !== "panel" ||
      bare[1] !== "run" ||
      bare[2] !== "--plan" ||
      bare[4] !== "--apply" ||
      !/^prun_[A-Za-z0-9_]+$/.test(runId)
    ) return null;
    return { pathway: "bespoke", area: "panel-apply", itemId: runId, option: "apply", label, argv, requiresConfirmation: true, rationaleRequired: false };
  }

  // Panel grouped verdict record — bespoke `panel-verdict-record-group`.
  if (!authorityBound && str(action.id) === "panel_verdict_record_group") {
    const groupKey = str(group.key);
    if (
      bare.length !== 8 ||
      bare[0] !== "panel" ||
      bare[1] !== "verdict-record" ||
      bare[2] !== "--group" ||
      bare[3] !== groupKey ||
      bare[4] !== "--rationale" ||
      !bare[5] ||
      bare[6] !== "--reviewed-by" ||
      !bare[7]
    ) return null;
    return {
      pathway: "bespoke",
      area: "panel-verdict-record-group",
      itemId: groupKey,
      option: "record",
      label,
      argv,
      requiresConfirmation: true,
      rationaleRequired: false,
      fields: { rationale: bare[5], reviewed_by: bare[7] },
    };
  }

  // Workbench dispatch actions don't carry a stable work_id in the attention
  // group (the guard in decision-execute re-checks the live dispatch target),
  // so we don't inline them here — the Continue panel already owns that seat.
  if (str(action.id).startsWith("workbench.")) return null;

  // Generic governed pathway — only for areas the backend has CERTIFIED
  // (generic-actions GENERIC_ACTION_ALLOWLIST). An un-certified area would 403
  // on click, so we render no button for it (honest).
  const area = str(group.area);
  if ((!specFor(area) || authorityBound) && genericActionProjectionEligible(area, argv)) {
    const itemIDs = group.item_ids.filter(Boolean);
    const itemId = action.kind === "batch" ? str(group.key) : itemIDs.length === 1 ? itemIDs[0] : "";
    if (!itemId) return null;
    return {
      pathway: "generic",
      area,
      itemId,
      actionId: str(action.id),
      expectedRevision: genericActionRevision(action),
      actionIntentId: authority?.actionIntentId,
      expectedAuthorityRevision: authority?.authorityRevision,
      authority: authority ?? undefined,
      label,
      argv,
      requiresConfirmation,
      rationaleRequired: Boolean(OPERATOR_INPUTS[area]),
    };
  }

  return null;
}

/* ---------- read-only pipeline strip (7 envelope stages) ----------
 * The envelope spine at board scale (DESIGN.md §5 signature moment A). Counts
 * come straight from the workbench projection's `counts` — WIP stages from the
 * live `by_state` split, throughput stages from the cumulative totals. Read-only
 * here; the Work screen owns the governed moves.
 */

export type PipelineStage = {
  key: string;
  label: string;
  count: number;
  tone: StateTone;
  /** true for the executing stage — gets the one beating node. */
  live?: boolean;
  hint: string;
};

export function pipelineStages(workbench: Json | null): PipelineStage[] {
  const counts = (workbench?.counts ?? {}) as Json;
  const byState = (counts.by_state ?? {}) as Record<string, number>;
  const st = (k: string): number => num(byState[k]);
  const execute = st("running") + st("verifying") || num(counts.in_progress);
  const terminal = num(counts.terminal) || st("completed") + st("failed") + st("cancelled");

  return [
    { key: "signal", label: "Signal", count: num(counts.admission_previews), tone: "draft", hint: "Suggested work offered, not yet prepared" },
    { key: "propose", label: "Suggest", count: st("proposed"), tone: "draft", hint: "Suggested work waiting to be added" },
    { key: "admit", label: "Add to work", count: st("admitted"), tone: "seam", hint: "Add a job to the work queue" },
    { key: "claim", label: "Start", count: st("ready") + st("claimed"), tone: "queued", hint: "Ready or started by an executor" },
    { key: "execute", label: "Execute", count: execute, tone: "live", live: execute > 0, hint: "Running or verifying" },
    { key: "receipt", label: "Proof", count: st("completed"), tone: "done", hint: "Completed with proof" },
    { key: "learn", label: "Learn", count: terminal, tone: "done", hint: "Learning updated from completed changes" },
  ];
}

/* ---------- autonomy posture (topbar chip + KPI tile) ---------- */

export type AutonomyPosture = {
  installed: boolean;
  level: string;
  mode: string;
  /** short plain-language posture, e.g. "closure over expansion". */
  posture: string;
  /** one-sentence doctrine gloss. */
  detail: string;
  tone: "good" | "seam";
};

export function autonomyPosture(auth: Json | null): AutonomyPosture {
  const installed = Boolean(auth?.installed);
  const level = str(auth?.autonomy_level);
  const mode = str(auth?.mode);
  const posture = mode ? mode.replace(/_/g, " ") : "unknown";
  const detail =
    mode === "closure_over_expansion"
      ? "Closes open work before it expands scope — proposes, never self-authorizes."
      : installed
        ? "Governed by imported operator authority."
        : "No operator authority imported — fail-closed.";
  return { installed, level: level || "—", mode, posture, detail, tone: installed && level ? "good" : "seam" };
}

/* ---------- self-healing (home doctor hygiene) ---------- */

export type HygieneFlag = {
  key: string;
  label: string;
  count: number;
  command: string;
  tone: "seam" | "crit";
};

export type SelfHealing = {
  ok: boolean;
  observedOK: boolean;
  observation: ProjectionStatus | "invalid";
  lastGoodAt: string | null;
  flags: HygieneFlag[];
  checkedDirs: string;
};

export function selfHealing(read: ProjectionRead): SelfHealing {
  const unknown: SelfHealing = {
    ok: false, observedOK: false, observation: read.status === "unreachable" ? "unreachable" : "invalid",
    lastGoodAt: read.lastGoodAt, flags: [], checkedDirs: "",
  };
  const doctor = read.value;
  if (read.status === "unreachable" || !doctor) return unknown;
  const oh = doctor.operational_hygiene as Json | undefined;
  // A successful read is not proof of a complete doctor report. Do not
  // coerce absent booleans/counts to healthy defaults.
  if (doctor.schema_version !== "compound.home_doctor_report.v0.1" ||
      !["ok", "fail"].includes(str(doctor.verdict)) ||
      typeof doctor.manifest_ok !== "boolean" ||
      typeof doctor.generated_state_in_repo_source !== "boolean" ||
      !Number.isInteger(doctor.dirs_ok) || !Number.isInteger(doctor.dirs_total) ||
      num(doctor.dirs_total) <= 0 || num(doctor.dirs_ok) < 0 || num(doctor.dirs_ok) > num(doctor.dirs_total) ||
      typeof oh?.ok !== "boolean" ||
      !Number.isInteger(oh.terminal_hot_work_count) || num(oh.terminal_hot_work_count) < 0 ||
      !Number.isInteger(oh.stale_lease_count) || num(oh.stale_lease_count) < 0) return unknown;
  const flags: HygieneFlag[] = [];

  const hotWork = num(oh.terminal_hot_work_count);
  if (hotWork > 0) {
    flags.push({ key: "hot-work", label: "Terminal work not archived", count: hotWork, command: "compound home archive-history --kind work --dry-run", tone: "seam" });
  }
  const staleLeases = num(oh.stale_lease_count);
  if (staleLeases > 0) {
    // Age-independent residue (lease files for terminal/archived/missing work),
    // NOT the Agents band's "live leases past threshold" — labeled distinctly so
    // the two surfaces can never be conflated. Shared label = one source.
    flags.push({ key: "stale-leases", label: LEASE_HISTORY_LABEL, count: staleLeases, command: "compound home archive-history --kind lease --dry-run", tone: "seam" });
  }
  if (doctor.manifest_ok === false) {
    flags.push({ key: "manifest", label: "Home manifest drift", count: 1, command: "compound home doctor", tone: "crit" });
  }
  const dirsOk = num(doctor.dirs_ok);
  const dirsTotal = num(doctor.dirs_total);
  if (dirsTotal > 0 && dirsOk < dirsTotal) {
    flags.push({ key: "dirs", label: "Home directories missing", count: dirsTotal - dirsOk, command: "compound home doctor", tone: "crit" });
  }
  if (doctor.generated_state_in_repo_source === true) {
    flags.push({ key: "repo-state", label: "Generated state inside repo", count: 1, command: "compound home doctor", tone: "crit" });
  }

  if ((doctor.verdict === "fail" || !oh.ok) && flags.length === 0) {
    flags.push({ key: "doctor", label: "Doctor reported a failure — inspect full report", count: 1, command: "compound home doctor --json", tone: "crit" });
  }
  const observedOK = flags.length === 0 && doctor.verdict === "ok" && oh.ok === true;
  return { ok: read.status === "live" && observedOK, observedOK, observation: read.status,
    lastGoodAt: read.lastGoodAt, flags, checkedDirs: `${dirsOk}/${dirsTotal} dirs` };
}

/* ---------- Continue panel derivation (kit-styled ContinueCard) ----------
 * Pure port of the ContinuePanel state machine so the briefing can render a
 * kit-token Continue card without touching the shared ContinuePanel component
 * (which the Work screen also renders). ContinueButton is reused for the act.
 */

export type ContinueTone = StateTone;

export type ContinueModel = {
  headline: string;
  body: string;
  tone: ContinueTone;
  chips: { label: string; active: boolean }[];
  actor: string;
  request: ContinueRequest | null;
  blockedOn: string[];
};

function itemWorkId(item: Json): string {
  return str(item.work_id) || str(item.id) || str(item.displayId);
}

function continueActionLabel(action: string): string {
  switch (action) {
    case "admit":
      return "add suggested work";
    case "prepare":
      return "prepare work";
    case "claim":
      return "start work";
    case "advance":
      return "continue work";
    case "close":
      return "finish with proof";
    default:
      return action ? action.replace(/_/g, " ") : "continue work";
  }
}

function continueRequest(workId: string, actor: string, note: string): ContinueRequest {
  return {
    area: "work-continue",
    itemId: workId,
    option: "continue",
    fields: { actor },
    command: ["compound", "work", "continue", "--actor", actor, "--expect-work", workId, "--until-wait", "--json"],
    note,
  };
}

export function deriveContinue(workbench: Json | null, open: Json[]): ContinueModel {
  const next = (workbench?.dispatch_next ?? {}) as Json;
  const canRun = Boolean(next.can_run);
  const action = str(next.action);
  const actor = str(next.actor) || "codex";
  const workId = str(next.work_id);
  const waitReason = str(next.wait_reason);
  const blockedOn = arr(next.blocked_on).map(String);

  const admissionPreviews = arr(workbench?.admission_previews);
  const continuableAdmissionPreviews = admissionPreviews.filter(
    (p) =>
      Boolean(str(p?.work_id)) &&
      Boolean(p?.policy_allowed) &&
      !p?.requires_operator &&
      !str(p?.admission_blocked_reason) &&
      str(p?.staleness) === "fresh",
  );
  const reviewCardIds = new Set(open.flatMap((w) => arr(w.review_cards).map((c) => str(c.id)).filter(Boolean)));
  const blockedAdmissionPreviewCount = admissionPreviews.filter(
    (p) => !continuableAdmissionPreviews.includes(p) && !reviewCardIds.has(str(p?.action_context?.source_id)),
  ).length;
  const admissionPreviewWorkId = str(continuableAdmissionPreviews[0]?.work_id);

  const inboxItems = open.filter((w) => ["proposed", "admitted"].includes(str(w.state)));
  const inboxWorkId = itemWorkId(inboxItems[0] ?? {});
  const intakeCount = inboxItems.length + continuableAdmissionPreviews.length;
  const readyCount = open.filter((w) => str(w.state) === "ready").length;
  const operatorGateCount =
    open.reduce((total, w) => total + arr(w.review_cards).length, 0) + blockedAdmissionPreviewCount;

  let headline = "Nothing to continue";
  let body = "Compound sees no safe next move right now.";
  let tone: ContinueTone = "queued";
  let request: ContinueRequest | null = null;

  if (canRun && workId) {
    headline = `Next — ${continueActionLabel(action)}`;
    body = `Compound can move ${workId} until the next real wait.`;
    tone = "live";
    request = continueRequest(workId, actor, "Moves safe prep/start/finish steps until the next real wait. Does not run task commands.");
  } else if (readyCount > 0) {
    headline = "Ready to start";
    body = `${readyCount} item${readyCount === 1 ? "" : "s"} can be started by an executor.`;
    tone = "done";
  } else if (intakeCount > 0) {
    headline = "Prepare suggested work";
    body = `${intakeCount} suggested task${intakeCount === 1 ? "" : "s"} can be prepared before an executor starts.`;
    tone = "seam";
    const target = inboxWorkId || admissionPreviewWorkId;
    request = target
      ? continueRequest(target, actor, "Adds, prepares, and starts one suggested task until the next real wait. Does not run task commands.")
      : null;
  } else if (operatorGateCount > 0) {
    headline = "Needs your decision";
    body =
      operatorGateCount === 1
        ? "1 item needs judgment, private input, or an unblock step."
        : `${operatorGateCount} items need judgment, private input, or an unblock step.`;
    tone = "seam";
  } else if (waitReason) {
    headline = "Waiting";
    body = waitReason.replace(/_/g, " ");
    tone = "queued";
  }

  return {
    headline,
    body,
    tone,
    actor,
    request,
    blockedOn: canRun ? [] : blockedOn,
    chips: [
      { label: `${readyCount} ready`, active: readyCount > 0 },
      { label: `${intakeCount} inbox`, active: intakeCount > 0 },
      { label: `${operatorGateCount} needs me`, active: operatorGateCount > 0 },
    ],
  };
}

/* ---------- week-over-week delta (KPI DeltaPills, §9.6) ----------
 * An HONEST delta: this-7d count minus prior-7d count, but only `covered` when
 * the fetched window demonstrably reaches back ≥14 days (else the prior week is
 * truncated by the row cap and the delta would lie). Consumers render the
 * DeltaPill only when `covered`.
 */

export function weeklyChange(
  items: Json[],
  tsOf: (item: Json) => number,
  matches: (item: Json) => boolean,
  now = Date.now(),
): { delta: number; covered: boolean } {
  const week = 7 * 24 * 3600 * 1000;
  const thisFrom = now - week;
  const priorFrom = now - 2 * week;
  let thisCount = 0;
  let priorCount = 0;
  let oldest = Number.POSITIVE_INFINITY;
  for (const item of items) {
    const ts = tsOf(item);
    if (ts > 0) oldest = Math.min(oldest, ts);
    if (!matches(item)) continue;
    if (ts >= thisFrom) thisCount++;
    else if (ts >= priorFrom) priorCount++;
  }
  const covered = Number.isFinite(oldest) && oldest <= priorFrom;
  return { delta: thisCount - priorCount, covered };
}

/** Parse the first present lifecycle timestamp of a projection row to epoch ms. */
export function rowTsMs(item: Json): number {
  const d = toDate(item.claimed_at ?? item.created_at ?? item.generated_at ?? item.updated_at);
  return d ? d.getTime() : 0;
}

/** Preserve the memory read's evidence state through both home-page cards. */
export type MemoryBriefing = {
  observation: ProjectionStatus | "invalid";
  lastGoodAt: string | null;
  generatedAt: string | null;
  totals: { promoted: number; parked: number; awaiting: number; reopened: number } | null;
  promotedThisWeek: number | null;
  delta: number | null;
};

export function memoryBriefing(read: ProjectionRead, now = Date.now()): MemoryBriefing {
  const base = { observation: read.status, lastGoodAt: read.lastGoodAt, generatedAt: read.generatedAt,
    totals: null, promotedThisWeek: null, delta: null };
  if (read.status === "unreachable" || !read.value) return { ...base, observation: "unreachable" };
  const counts = (read.value.counts as Json | undefined)?.by_state;
  if (!counts || typeof counts !== "object" || Array.isArray(counts) ||
      !Array.isArray(read.value.items) ||
      Object.values(counts).some(value => typeof value !== "number" || !Number.isSafeInteger(value) || value < 0)) {
    return { ...base, observation: "invalid" };
  }
  const byState = counts as Record<string, number>;
  const items = arr(read.value.items);
  const observedByState: Record<string, number> = {};
  for (const item of items) {
    const state = item && typeof item === "object" ? str(item.state) : "";
    if (!state) return { ...base, observation: "invalid" };
    observedByState[state] = (observedByState[state] ?? 0) + 1;
  }
  if (Object.entries(observedByState).some(([state, count]) => (byState[state] ?? 0) < count)) {
    return { ...base, observation: "invalid" };
  }
  const change = weeklyChange(items, rowTsMs, item => str(item.state) === "promoted", now);
  const live = read.status === "live";
  return {
    ...base,
    totals: {
      promoted: byState.promoted ?? 0,
      parked: byState.parked ?? 0,
      awaiting: byState.proposed ?? 0,
      reopened: (byState.needs_review ?? 0) + items.filter(item =>
        str(item.state) !== "needs_review" && str(item.review_state) === "needs_review").length,
    },
    promotedThisWeek: live ? items.filter(item => {
      const date = toDate(item.created_at);
      return str(item.state) === "promoted" && date != null && date.getTime() >= now - 7 * 24 * 3_600_000;
    }).length : null,
    delta: live && change.covered ? change.delta : null,
  };
}

export function memoryBriefingNotice(memory: MemoryBriefing): string {
  if (memory.observation === "invalid") return "Memory report incomplete";
  if (!memory.totals) return "Memory unavailable";
  if (memory.observation === "stale") return "Last known memory; current read unavailable";
  return "";
}

/* ---------- oldest-age helper for the attention pill ---------- */

export function oldestCreatedAt(groups: AttentionGroup[]): Date | undefined {
  return groups
    .map((g) => toDate(g.oldest_created_at))
    .filter((d): d is Date => Boolean(d))
    .sort((a, b) => a.getTime() - b.getTime())[0];
}
