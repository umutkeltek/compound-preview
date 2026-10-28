#!/usr/bin/env bun
/** Existing budget owner: measurement and deferral, regardless of policy/config. */
import { existsSync, readFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { flags, limit, measure, capacity, command, emit, GIB, DEFER_REASON } from "./report_only";
const args = flags("enforce_ssd_budget.ts", ["--no-discovery"]), home = homedir();
const manifestPath = process.env.SSD_BUDGET_MANIFEST ?? join(import.meta.dir, "ssd_budget.json");
const overlayPath = process.env.SSD_BUDGET_MANIFEST_OVERLAY ?? join(import.meta.dir, "../../docs_private/ssd_budget.local.json");
const base = JSON.parse(readFileSync(manifestPath, "utf8"));
const overlay = existsSync(overlayPath) ? JSON.parse(readFileSync(overlayPath, "utf8")) : {};
type Rule = { path: string; policy: string; cap_gib: number; why?: string };
const byPath = new Map<string, Rule>();
for (const rule of [...(base.paths ?? []), ...(overlay.paths ?? [])]) byPath.set(rule.path, rule);
const manifest = { ...base, ...overlay, paths: [...byPath.values()] };
if (manifest.paths.length > 200) throw new Error("budget rule bound exceeded");
const expand = (path: string) => path.replace("$HOME", home).replace("$TMPDIR", (process.env.TMPDIR ?? tmpdir()).replace(/\/$/, ""));
const seen = new Set<string>(), deadline = Date.now() + 45_000;
const results = manifest.paths.map((rule: Rule) => {
  const cap = limit(rule.cap_gib, 0), observation = measure(expand(rule.path), seen, deadline);
  const deferred = rule.policy !== "report_only" && observation.status !== "absent" && observation.status !== "failed";
  const over = observation.size_bytes !== null && observation.size_bytes > cap * GIB;
  return { path: rule.path, policy: rule.policy, ...observation, measurement_status: observation.status,
    status: observation.status === "failed" ? "failed" : observation.status === "absent" ? "absent" : deferred ? "deferred" : observation.status === "alias" ? "alias" : over ? "over_budget" : "ok",
    size_gib: observation.size_bytes === null ? null : observation.size_bytes / GIB,
    cap_gib: cap, over_budget: over, reclaimed_bytes: 0, planned_reclaim_bytes: 0,
    note: observation.note ?? (deferred ? DEFER_REASON : rule.why) };
});
const undeclared: { path: string; size_gib: number }[] = [], discovery_errors: string[] = [];
const declared = results.flatMap((r) => [expand(r.path), ...(r.physical_path ? [r.physical_path] : [])]);
const roots = args.has("--no-discovery") ? [] : manifest.scan_roots ?? [];
if (roots.length > 8) throw new Error("discovery root bound exceeded");
const alertBytes = limit(manifest.undeclared_alert_gib, 5) * GIB;
for (const raw of roots) {
  const root = expand(raw);
  try {
    if (Date.now() >= deadline) throw new Error("discovery time bound exceeded");
    const output = command("/usr/bin/du", ["-x", "-k", "-d", "2", root], Math.max(1, deadline - Date.now()));
    for (const line of output.trim().split("\n")) {
      const tab = line.indexOf("\t"), bytes = Number(line.slice(0, tab)) * 1024, path = line.slice(tab + 1);
      if (tab < 0 || !Number.isSafeInteger(bytes) || bytes < 0) throw new Error("invalid discovery measurement");
      if (bytes >= alertBytes && path !== root && !declared.some((d) => path === d || path.startsWith(d + "/") || d.startsWith(path + "/"))) undeclared.push({ path, size_gib: bytes / GIB });
    }
  } catch (error) { discovery_errors.push(String(error).slice(0, 240)); }
}
let freeBytes: number | null = null, capacity_error: string | null = null;
try { freeBytes = capacity(); } catch (error) { capacity_error = String(error); }
const floor = limit(manifest.free_floor_gib, 150), belowFloor = freeBytes !== null && freeBytes < floor * GIB;
const failed = !!capacity_error || discovery_errors.length > 0 || results.some((r) => r.status === "failed") || belowFloor;
const deferred = results.some((r) => r.status === "deferred");
emit("budget", { schema_version: "compound.ssd-guardian.budget_enforcement.v2", generated_at: new Date().toISOString(),
  mode: args.apply ? "apply" : "dry_run", results, undeclared, discovery_errors, capacity_error,
  free_bytes: freeBytes, free_gib: freeBytes === null ? null : freeBytes / GIB, free_floor_gib: floor, below_floor: belowFloor,
  total_reclaimed_gib: 0, alarm: failed || results.some((r) => r.over_budget),
  accounting: "allocated bytes per physical root; overlapping parent/child roots are not additive", overlay_source: existsSync(overlayPath) ? overlayPath : null,
}, args.apply, failed || results.some((r) => r.over_budget), deferred);
