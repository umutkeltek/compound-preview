#!/usr/bin/env bun
/**
 * check_backup_freshness.ts — verify the agent-backup receipt is recent enough
 * to justify retention steps in the weekly reclaim.
 *
 * The weekly reclaim orchestrator calls this BEFORE it would prune session
 * history or any other "needs backup proof" path. The rule: a backup receipt
 * is fresh if the most recent file under ~/.hermes/cron/output/<job-id>/ is
 * less than 7 days old. JSON or markdown outputs both count.
 *
 * Returns:
 *   - exit 0 + status=fresh       (caller may proceed)
 *   - exit 0 + status=stale       (caller should report but skip destructive)
 *   - exit 0 + status=missing     (no receipts ever)
 */

import { existsSync, readdirSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const HOME = homedir();
const RECEIPT_DIR = join(HOME, ".hermes/cron/output/1dc135fe0275");
const FRESHNESS_DAYS = 7;

export type Status = "fresh" | "stale" | "missing";

/** Every status this checker can emit. Callers gate against THIS list. */
export const STATUSES: readonly Status[] = ["fresh", "stale", "missing"];

/**
 * The one place that decides whether a backup status clears a destructive
 * step. Callers must not re-spell the literal: weekly_reclaim.ts once tested
 * `status !== "ok"`, a value this checker never emits, so every apply run
 * aborted while logging green.
 */
export function backupGateAllowsApply(status: string): boolean {
  return status === "fresh";
}

function status(): { status: Status; latest: string | null; age_hours: number | null } {
  if (!existsSync(RECEIPT_DIR)) return { status: "missing", latest: null, age_hours: null };
  const files = readdirSync(RECEIPT_DIR)
    .map((f) => ({ f, mtimeMs: statSync(join(RECEIPT_DIR, f)).mtimeMs }))
    .sort((a, b) => b.mtimeMs - a.mtimeMs);
  if (files.length === 0) return { status: "missing", latest: null, age_hours: null };
  const top = files[0];
  const ageMs = Date.now() - top.mtimeMs;
  const ageHours = ageMs / (60 * 60 * 1000);
  return {
    status: ageMs < FRESHNESS_DAYS * 24 * 60 * 60 * 1000 ? "fresh" : "stale",
    latest: top.f,
    age_hours: Math.round(ageHours * 10) / 10,
  };
}

// Guarded so callers can import the gate without triggering the CLI's exit.
if (import.meta.main) {
  console.log(JSON.stringify(status(), null, 2));
  process.exit(0);
}
