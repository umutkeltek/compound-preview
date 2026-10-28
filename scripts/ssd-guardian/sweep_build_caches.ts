#!/usr/bin/env bun
/**
 * sweep_build_caches.ts — age-gated build artifact sweeper for ~/Projects.
 *
 * Targets: build/ .build/ target/ DerivedData/ node_modules/ .next/ .gradle/
 *          Pods/ .swiftpm/ .turbo/ dist/ .parcel-cache/
 *
 * Skip-list: ~/.cache ~/.local/share ~/.cargo ~/.rustup ~/.gradle/caches
 *            ~/.android/build-cache ~/Library /Volumes/ operating-system/
 *            node_modules/.cache/
 *
 * Read-only discovery by default; --apply records an explicit owner-review
 * deferral. Directory names and age alone cannot authorize deletion.
 */

import { spawnSync } from "node:child_process";
import { statSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { emit, measure } from "./report_only";

const APPLY = process.argv.includes("--apply");
const unknown = process.argv.slice(2).filter((arg) => arg !== "--apply");
if (unknown.length) { console.error("unknown arguments: " + unknown.join(", ")); process.exit(2); }
const HOME = homedir();
const PROJECTS = join(HOME, "Projects");
const AGE_DAYS = Number(process.env.SSD_GUARDIAN_AGE_DAYS ?? 7);
if (!Number.isSafeInteger(AGE_DAYS) || AGE_DAYS < 1) { console.error("invalid age bound"); process.exit(2); }

const TARGET_DIRS = ["build", ".build", "target", "DerivedData", "node_modules", ".next", ".gradle", "Pods", ".swiftpm", ".turbo", "dist", ".parcel-cache"];

const FIND_PRUNE = ["/.cache/", "/.local/share/", "/.cargo/registry/", "/.rustup/", "/.gradle/caches/", "/.android/build-cache", "/node_modules/.cache/", "/Library/", "/Volumes/", "/operating-system/"];

type Finding = {
  path: string;
  size_bytes: number | null;
  age_days: number;
  status: "reclaimed" | "planned" | "deferred" | "skipped_fresh" | "skipped_path" | "failed";
  reason?: string;
};

const alternatives = (items: string[], flag: string) => items.flatMap((item, i) => [...(i ? ["-o"] : []), flag, item]);
const r = spawnSync("/usr/bin/find", ["-P", PROJECTS, "-xdev", "-maxdepth", "6", "(",
  ...alternatives(FIND_PRUNE.map((p) => `*${p}*`), "-path"), ")", "-prune", "-o", "-type", "d", "(",
  ...alternatives(TARGET_DIRS, "-name"), ")", "-mtime", `+${AGE_DAYS - 1}`, "-print0", "-prune"],
  { stdio: "pipe", timeout: 45_000, maxBuffer: 8 * 1024 * 1024 });
if (r.error || r.status !== 0) {
  console.log(JSON.stringify({ status: "failed", reason: (r.stderr?.toString() ?? "").slice(0, 400) }));
  process.exit(1);
}

const raw = r.stdout?.toString() ?? "";
if (raw && !raw.endsWith("\0")) { console.error("truncated candidate inventory"); process.exit(2); }
const candidatePaths = raw ? raw.slice(0, -1).split("\0") : [];

const findings: Finding[] = [];
const BATCH = 1;
if (candidatePaths.length > 200) { console.error("candidate bound exceeded; refusing a partial cleanup plan"); process.exit(2); }
const deadline = Date.now() + 45_000;
for (let i = 0; i < candidatePaths.length; i += BATCH) {
  const batch = candidatePaths.slice(i, i + BATCH);
  await Promise.all(batch.map(async (p) => {
    if (Date.now() > deadline) { findings.push({ path: p, size_bytes: null, age_days: -1, status: "failed", reason: "plan time bound exceeded" }); return; }
    const observation = measure(p, new Set(), deadline);
    if (observation.status === "failed" || observation.size_bytes === null) { findings.push({ path: p, size_bytes: null, age_days: -1, status: "failed", reason: observation.note ?? "invalid measurement" }); return; }
    const size = observation.size_bytes;
    let mtimeMs: number;
    try { mtimeMs = statSync(p).mtimeMs; }
    catch { findings.push({ path: p, size_bytes: null, age_days: -1, status: "failed", reason: "metadata unreadable" }); return; }
    const ageDays = mtimeMs ? Math.floor((Date.now() - mtimeMs) / (24 * 60 * 60 * 1000)) : -1;
    // A folder name and directory mtime cannot prove rebuildability or clear
    // a work lease. Keep discovery here; the existing Compound retention and
    // worktree-retirement owners must review exact source/custody evidence.
    findings.push({ path: p, size_bytes: size, age_days: ageDays,
      status: "deferred",
      reason: "owner review required: classification, reconstruction and active-work evidence" });
  }));
}

findings.sort((a, b) => (b.size_bytes ?? 0) - (a.size_bytes ?? 0));
const totalReclaimed = findings.filter((f) => f.status === "reclaimed").reduce((acc, f) => acc + (f.size_bytes ?? 0), 0);
const failed = findings.filter((f) => f.status === "failed").length;

const report = {
  schema_version: "compound.ssd-guardian.sweep_build_caches.v3",
  mode: APPLY ? "apply" : "dry_run",
  age_days: AGE_DAYS,
  projects_root: PROJECTS,
  total_reclaimed_bytes: totalReclaimed,
  planned_reclaim_bytes: 0,
  candidate_bytes: findings.reduce((sum, f) => sum + (f.size_bytes ?? 0), 0),
  retry_required: APPLY || failed > 0,
  failed_count: failed,
  findings_count: findings.length,
  findings: findings.slice(0, 200),
};
emit("sweep", report, APPLY, failed > 0, findings.length > 0);
