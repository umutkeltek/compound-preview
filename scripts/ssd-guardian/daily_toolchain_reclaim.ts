#!/usr/bin/env bun
/** Existing daily owner: measure toolchain growth and defer all cleanup. */
import { homedir } from "node:os";
import { join } from "node:path";
import { flags, limit, measure, capacity, emit, GIB, DEFER_REASON } from "./report_only";

const args = flags("daily_toolchain_reclaim.ts", ["--force"]);
const home = homedir(), work = process.env.SSD_GUARDIAN_WORK_DIR ?? "/Volumes/Work";
const rows = [
  { id: "go-build-cap", path: process.env.SSD_GUARDIAN_GO_CACHE_DIR ?? join(home, "Library/Caches/go-build"), cap: limit(process.env.SSD_GUARDIAN_GO_CACHE_CAP_GIB, 20) },
  { id: "proof-gocache-prune", path: process.env.SSD_GUARDIAN_PROOF_CACHE_DIR ?? join(home, "Library/Caches/compound-proof-gocache"), cap: limit(process.env.SSD_GUARDIAN_PROOF_CACHE_CAP_GIB, 20) },
  { id: "proof-gocache-prune-work", path: process.env.SSD_GUARDIAN_WORK_PROOF_CACHE_DIR ?? join(work, "Caches/compound-proof-gocache"), cap: limit(process.env.SSD_GUARDIAN_WORK_PROOF_CACHE_CAP_GIB, 80) },
  { id: "cargo-prune", path: join(home, ".cargo/registry"), cap: 5 },
  { id: "brew-cleanup", path: join(home, "Library/Caches/Homebrew"), cap: 3 },
  { id: "simulator-erase", path: join(home, "Library/Developer/CoreSimulator/Devices"), cap: 15 },
];
const seen = new Set<string>(), deadline = Date.now() + 45_000;
const steps = rows.map((row) => {
  const observation = measure(row.path, seen, deadline);
  return { id: row.id, path: row.path, ...observation, measurement_status: observation.status,
    status: observation.status === "failed" ? "failed" : observation.status === "absent" ? "skipped" : "deferred",
    cap_gib: row.cap, remaining_over_budget_bytes: observation.size_bytes === null ? null : Math.max(0, observation.size_bytes - row.cap * GIB),
    reclaimed_bytes: 0, reason: observation.note ?? DEFER_REASON };
});
let freeBytes: number | null = null;
try { freeBytes = capacity(); } catch (error) { steps.push({ id: "capacity", path: "/System/Volumes/Data", status: "failed", measurement_status: "failed", size_bytes: null, cap_gib: 0, remaining_over_budget_bytes: null, reclaimed_bytes: 0, reason: String(error) }); }
const failed = steps.some((step) => step.status === "failed" || (step.remaining_over_budget_bytes ?? 0) > 0) || (freeBytes !== null && freeBytes < 150 * GIB);
emit("daily", { schema_version: "compound.ssd-guardian.daily_toolchain_reclaim.v2", generated_at: new Date().toISOString(),
  mode: args.apply ? "apply" : "dry_run", force_requested: args.has("--force"), free_bytes: freeBytes, free_floor_gib: 150,
  accounting: "allocated bytes per physical root; overlapping roots are not additive; no reclaim estimate",
  steps, total_reclaimed_bytes: 0, alarm: failed, deferred_steps: steps.filter((s) => s.status === "deferred").map((s) => s.id),
}, args.apply, failed, steps.some((step) => step.status === "deferred"));
