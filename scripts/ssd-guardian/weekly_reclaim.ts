#!/usr/bin/env bun
/** Existing weekly owner: audits and explicit deferrals, including snapshot thinning. */
import { spawnSync } from "node:child_process";
import { join } from "node:path";
import { flags, emit, DEFER_REASON } from "./report_only";
const args = flags("weekly_reclaim.ts");
const steps: { id: string; status: string; reason?: string; exit_code?: number }[] = [];
const observations: Record<string, string> = {};
for (const [id, name, childArgs] of [
  ["backup-freshness-check", "check_backup_freshness.ts", []],
  ["symlink-drift-fix", "fix_work_symlink_drift.ts", []],
  ["build-cache-sweep", "sweep_build_caches.ts", []],
] as const) {
  const r = spawnSync(process.execPath, [join(import.meta.dir, name), ...childArgs], { encoding: "utf8", timeout: 60_000, maxBuffer: 1024 * 1024 });
  let status = r.error || (r.status !== 0 && r.status !== 3) ? "failed" : r.status === 3 ? "deferred" : "observed";
  let invalid = false;
  try {
    const observation = JSON.parse(r.stdout);
    if (id === "backup-freshness-check") invalid = !["fresh", "stale", "missing"].includes(observation.status);
    else invalid = observation.execution_policy !== "report_only" || observation.mutation_authorized !== false
      || observation.schema_version !== (id === "symlink-drift-fix" ? "compound.ssd-guardian.work_symlink_drift.v2" : "compound.ssd-guardian.sweep_build_caches.v3");
  } catch { invalid = true; }
  if (invalid) status = "failed";
  observations[id] = r.stdout?.slice(0, 64 * 1024) ?? "";
  steps.push({ id, status, exit_code: r.status ?? undefined, reason: status === "failed" ? (invalid ? "invalid or missing child observation" : String(r.error ?? r.stderr).slice(0, 200)) : undefined });
}
// A fresh backup and successful children grant no disposal authority. There is
// no tmutil command or alternative fall-through path after child deferrals.
steps.push({ id: "capture-coverage-audit", status: "deferred", reason: "existing capture audit may recover SQLite WAL state; consult Switchback owner without opening protected capture databases" });
for (const id of ["cache-relocation", "project-cleanup", "tmutil-thin-snapshots"]) steps.push({ id, status: "deferred", reason: DEFER_REASON });
emit("weekly", { schema_version: "compound.ssd-guardian.weekly_reclaim.v2", mode: args.apply ? "apply" : "dry_run",
  steps, observations, total_reclaimed_bytes: 0 }, args.apply, steps.some((s) => s.status === "failed"), true);
