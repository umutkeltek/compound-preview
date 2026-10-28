#!/usr/bin/env bun
/** Existing drift owner: observe aliases; never probe, merge, relocate or overwrite. */
import { lstatSync, readlinkSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { flags, emit, DEFER_REASON } from "./report_only";
const args = flags("fix_work_symlink_drift.ts"), home = homedir();
const work = process.env.SSD_GUARDIAN_WORK_DIR ?? "/Volumes/Work";
const results = [
  ["go-build", "go"], ["Homebrew", "Homebrew"], ["compound-proof-gocache", "compound-proof-gocache"],
].map(([label, destination]) => {
  const source = join(home, "Library/Caches", label), target = join(work, "Caches", destination);
  try {
    const stat = lstatSync(source), current = stat.isSymbolicLink() ? readlinkSync(source) : null;
    return { label, source, target, current, status: "deferred", reason: label === "compound-proof-gocache" ? "proof_cache_location_is_owner_managed; preserve directory and exact external-link backup" : DEFER_REASON };
  } catch (error: any) { return { label, source, target, status: error?.code === "ENOENT" ? "deferred" : "failed", reason: error?.code === "ENOENT" ? "missing source; link creation requires owner review" : String(error) }; }
});
emit("drift", { schema_version: "compound.ssd-guardian.work_symlink_drift.v2", mode: args.apply ? "apply" : "dry_run", results, total_moved_bytes: 0 }, args.apply, results.some((r) => r.status === "failed"), true);
