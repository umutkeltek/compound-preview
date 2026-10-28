import { expect, test } from "bun:test";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, readdirSync, lstatSync, readlinkSync, realpathSync, symlinkSync, utimesSync, rmSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, basename } from "node:path";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
const scripts = join(import.meta.dir, "../scripts/ssd-guardian");
const preload = join(scripts, "test-fixtures/report_only_preload.ts");
const entries = ["daily_toolchain_reclaim.ts", "enforce_ssd_budget.ts", "disk_pressure_guard.ts", "weekly_reclaim.ts", "fix_work_symlink_drift.ts", "sweep_build_caches.ts"];
function fixture() {
  const root = realpathSync(mkdtempSync(join(tmpdir(), "guardian-report-fixture-")));
  const old = new Date(Date.now() - 90 * 86_400_000);
  const paths = ["physical-go/aa/compiler-d", "Library/Caches/compound-proof-gocache/0123456789abcdef/aa/cache-d", "work/Caches/compound-proof-gocache/0123456789abcdef/aa/cache-d", "Library/Caches/Homebrew/download", ".cargo/registry/src/source/package", ".cargo/registry/cache/source/package.crate", "Library/Developer/CoreSimulator/Devices/device/data", "Projects/product/build/original", "Projects/product/.build/original", ".codex/sessions/original.jsonl", ".claude/projects/original.jsonl", ".switchback/state/body/index.sqlite", ".switchback/state/body/index.sqlite-wal", ".switchback/state/body/index.sqlite-shm", ".compound/exports/backup.tar", "temp/gocache-mixed/model.gguf", "temp/gocache-mixed/sessions/history.jsonl", "temp/gocache-mixed/body/original", "temp/agent-session-backup-fixture/original", "temp/switchback-forward-proxy-fixture/original", "temp/compound-proofcheck-sealed-fixture/original", "backups/staging/original", "literal-$(touch SHOULD_NOT_EXIST)-`touch ALSO_NOT_CREATED`/original"];
  for (const path of paths) { const full = join(root, path); mkdirSync(join(full, ".."), { recursive: true }); writeFileSync(full, "synthetic original " + path); utimesSync(full, old, old); }
  writeFileSync(join(root, "temp/gocache-mixed/README"), "This directory holds cached build artifacts from the Go build system.");
  symlinkSync(join(root, "physical-go"), join(root, "Library/Caches/go-build"));
  symlinkSync(join(root, "work/Caches/compound-proof-gocache"), join(root, "Library/Caches/compound-proof-gocache.external-link-backup-exact"));
  for (const path of ["temp/gocache-mixed", "Projects/product/build", "Projects/product/.build"]) utimesSync(join(root,path),old,old);
  const policies = ["age_prune", "keep_newest", "dir_lru_cap", "lru_cap", "invented_apply_policy", "report_only"];
  writeFileSync(join(root, "manifest.json"), JSON.stringify({ free_floor_gib: 0, undeclared_alert_gib: 0, scan_roots: [join(root,"literal-$(touch SHOULD_NOT_EXIST)-`touch ALSO_NOT_CREATED`")], paths: policies.map((policy, i) => ({ path: join(root, ["temp", ".compound/exports", "Library/Caches/compound-proof-gocache", "Library/Caches/go-build", ".codex/sessions", "physical-go"][i]), policy, cap_gib: 0, keep: 0, age_days: 0 })) }));
  writeFileSync(join(root,"overlay.json"), JSON.stringify({ paths: [{ path: join(root,"backups/staging"), policy:"lru_cap", cap_gib:0 }] }));
  mkdirSync(join(root, "reports")); writeFileSync(join(root, "reports/retained-evidence.json"), "retained earlier receipt");
  return root;
}
function snapshot(root: string): any[] {
  const rows: any[] = [];
  function visit(path: string) {
    for (const name of readdirSync(path).sort()) {
      const full = join(path, name); if (full === join(root,"reports") || full === join(root,"trace.jsonl")) continue;
      const st = lstatSync(full), rel = full.slice(root.length + 1);
      // Parent directory times can change as the permitted report/trace files are created.
      rows.push([rel, st.ino, st.mode, st.isDirectory() ? null : st.size, st.isDirectory() ? null : st.mtimeMs, st.isSymbolicLink() ? readlinkSync(full) : st.isFile() ? createHash("sha256").update(readFileSync(full)).digest("hex") : null]);
      if (st.isDirectory()) visit(full);
    }
  } visit(root); return rows;
}
function run(root: string, script: string, args: string[], scenario: object = {}) {
  const result = spawnSync(process.execPath, ["--preload", preload, join(scripts,script), ...args], { encoding:"utf8", timeout:30_000, env: {
    ...process.env, BUN_RUNTIME_TRANSPILER_CACHE_PATH:"0", HOME:root, TMPDIR:join(root,"temp"), COMPOUND_HOME:join(root,".compound"), COMPOUND_SSD_REPORT_DIR:join(root,"reports"),
    GUARDIAN_FIXTURE_ROOT:root, GUARDIAN_FIXTURE_SCENARIO:JSON.stringify(scenario),
    SSD_BUDGET_MANIFEST:join(root,"manifest.json"), SSD_BUDGET_MANIFEST_OVERLAY:join(root,"overlay.json"),
    SSD_GUARDIAN_WORK_DIR:join(root,"work"), SSD_GUARDIAN_TEMP_ROOT:join(root,"temp"),
    SSD_GUARDIAN_GO_CACHE_DIR:join(root,"Library/Caches/go-build"), SSD_GUARDIAN_PROOF_CACHE_DIR:join(root,"Library/Caches/compound-proof-gocache"), SSD_GUARDIAN_WORK_PROOF_CACHE_DIR:join(root,"work/Caches/compound-proof-gocache"),
    SSD_GUARDIAN_GO_CACHE_CAP_GIB:"0", SSD_GUARDIAN_PROOF_CACHE_CAP_GIB:"0", SSD_GUARDIAN_WORK_PROOF_CACHE_CAP_GIB:"0", SSD_GUARDIAN_GO_CACHE_AGE_DAYS:"0", SSD_GUARDIAN_CARGO_AGE_DAYS:"0", SSD_GUARDIAN_SIM_AGE_DAYS:"0", SSD_GUARDIAN_TEMP_CACHE_MIN_AGE_SECONDS:"0", SSD_GUARDIAN_FREE_FLOOR_GIB:"200", SSD_GUARDIAN_AGE_DAYS:"1",
  }});
  const trace = existsSync(join(root,"trace.jsonl")) ? readFileSync(join(root,"trace.jsonl"),"utf8").trim().split("\n").filter(Boolean).map((line) => JSON.parse(line)) : [];
  expect(trace.filter((row) => row.forbidden)).toEqual([]);
  expect(result.error).toBeUndefined();
  return { ...result, trace, report: result.stdout.trim() ? JSON.parse(result.stdout) : null };
}
function owned(testBody: (root: string) => void) { const root = fixture(); try { testBody(root); } finally { rmSync(root,{recursive:true,force:true}); } }
for (const name of entries) test(`scheduled report-only boundary: ${name} --apply`, () => owned((root) => {
  const before = snapshot(root);
  const args = ["--apply", ...(name === "daily_toolchain_reclaim.ts" ? ["--force"] : [])];
  const { status, report, trace } = run(root,name,args,{ low_disk:true, backup:"fresh" });
  expect([2,3]).toContain(status); expect(report.mode).toBe("report_only"); expect(report.requested_apply).toBe(true); expect(report.execution_policy).toBe("report_only"); expect(report.mutation_authorized).toBe(false);
  expect(report.reclaimed_bytes).toBe(0); expect(report.planned_reclaim_bytes).toBe(0); expect(report.retry_required).toBe(true);
  expect(snapshot(root)).toEqual(before); expect(readFileSync(join(root,"reports/retained-evidence.json"),"utf8")).toBe("retained earlier receipt");
  expect(trace.some((row) => row.report_write === "exclusive-create")).toBe(true);
  if (name === "daily_toolchain_reclaim.ts") { const go = report.steps.find((row: any) => row.id === "go-build-cap"); expect(go.physical_path).toBe(join(root,"physical-go")); expect(go.size_bytes).toBeGreaterThan(0); expect(go.status).toBe("deferred"); }
  if (name === "enforce_ssd_budget.ts") { expect(report.results.filter((row: any) => row.policy !== "report_only").every((row: any) => row.status === "deferred")).toBe(true); expect(report.results.at(-2).measurement_status).toBe("alias"); }
  if (name === "disk_pressure_guard.ts") { expect(report.temporaryCacheCandidates).toHaveLength(1); expect(report.temporaryCacheActions[0].status).toBe("deferred"); expect(report.observedFreeChangeBytes).toBeGreaterThan(0); expect(report.reclaimedBytes).toBe(0); expect(report.actions).toEqual([]); }
}));
for (const backup of ["fresh", "stale", "missing", "unknown", "malformed", "failure", "timeout"]) test(`weekly continues observations without mutation: backup=${backup}`, () => owned((root) => {
  const before = snapshot(root), { report, trace } = run(root,"weekly_reclaim.ts",["--apply"],{backup});
  expect(report.steps.find((s: any) => s.id === "tmutil-thin-snapshots").status).toBe("deferred");
  expect(report.steps.find((s: any) => s.id === "capture-coverage-audit").status).toBe("deferred");
  expect(trace.filter((row) => row.command === process.execPath).map((row) => basename(row.args[0]))).toEqual(["check_backup_freshness.ts","fix_work_symlink_drift.ts","sweep_build_caches.ts"]);
  expect(report.steps.find((s: any) => s.id === "backup-freshness-check").status).toBe(["fresh","stale","missing"].includes(backup) ? "observed" : "failed");
  expect(snapshot(root)).toEqual(before);
}));
for (const child of ["fix_work_symlink_drift.ts", "sweep_build_caches.ts"]) test(`weekly cannot fall through a child failure: ${child}`, () => owned((root) => {
  const { status,report } = run(root,"weekly_reclaim.ts",["--apply"],{backup:"fresh",child_error:child});
  expect(status).toBe(2); expect(report.steps.filter((s:any) => s.status === "failed")).toHaveLength(1); expect(report.steps.at(-1).status).toBe("deferred");
}));
for (const name of ["daily_toolchain_reclaim.ts","enforce_ssd_budget.ts","disk_pressure_guard.ts"]) test(`unreadable measurement is not empty success: ${name}`, () => owned((root) => {
  const { status,report } = run(root,name,["--apply"],{measure_error:true,low_disk:true});
  expect(status).toBe(2); expect(report.reclaimed_bytes).toBe(0); expect(JSON.stringify(report)).toContain("measurement");
}));
for (const name of entries) test(`dry report does not persist: ${name}`, () => owned((root) => {
  const { trace,report } = run(root,name,[],{backup:"fresh"}); expect(trace.some((row) => row.report_write)).toBe(false); expect(report.receiptPath).toBeNull();
}));
test("both budget schedule argument vectors remain read-only", () => owned((root) => {
  const { report,trace } = run(root,"enforce_ssd_budget.ts",["--apply","--no-discovery"]);
  expect(report.reclaimed_bytes).toBe(0); expect(trace.some((r) => r.args?.includes("-d"))).toBe(false);
}));

for (const child of ["fix_work_symlink_drift.ts", "sweep_build_caches.ts"]) for (const exit of [0,3]) test(`weekly rejects malformed ${child} code ${exit}`, () => owned((root) => {
  const { status,report } = run(root,"weekly_reclaim.ts",["--apply"],{backup:"fresh",malformed_child:child,child_exit:exit});
  expect(status).toBe(2); expect(report.steps.filter((s:any) => s.status === "failed")).toHaveLength(1); expect(report.steps.at(-1).status).toBe("deferred");
}));
for (const error of ["ENOSPC", "EACCES", "EEXIST"]) test(`report publication failure preserves observations: ${error}`, () => owned((root) => {
  const { status,report } = run(root,"daily_toolchain_reclaim.ts",["--apply"],{publish_error:error});
  expect(status).toBe(2); expect(report.publication_error).toContain(error); expect(report.receiptPath).toBeNull(); expect(report.steps.length).toBeGreaterThan(0); expect(report.reclaimed_bytes).toBe(0);
}));
test("pressure capacity failure preserves report with unknown size", () => owned((root) => {
  const { status,report } = run(root,"disk_pressure_guard.ts",["--apply"],{df_error:true});
  expect(status).toBe(2); expect(report.beforeFreeBytes).toBeNull(); expect(report.afterFreeBytes).toBeNull(); expect(report.errors).not.toHaveLength(0); expect(report.decision.action).toBe("report_only"); expect(report.decision.reasons).toContain("capacity_unknown"); expect(report.decision.reasons).not.toContain("free_space_healthy");
}));
test("empty successful sweep measurement is unavailable", () => owned((root) => {
  const { status,report } = run(root,"sweep_build_caches.ts",["--apply"],{empty_measure:true});
  expect(status).toBe(2); expect(report.findings.filter((f:any) => f.status === "failed").length).toBeGreaterThan(0); expect(report.findings[0].size_bytes).toBeNull();
}));

for (const name of entries) test(`unknown flags cannot broaden ${name}`, () => owned((root) => {
  const { status,report,trace } = run(root,name,["--apply","--delete-approved"]);
  expect(status).toBe(2); expect(report).toBeNull(); expect(trace).toEqual([]);
}));
test("declared zero caps do not claim successful enforcement", () => owned((root) => {
  const { report } = run(root,"enforce_ssd_budget.ts",["--apply"]);
  expect(report.results.some((row:any) => row.status === "enforced")).toBe(false);
  expect(report.results.find((row:any) => row.policy === "lru_cap").size_bytes).toBeGreaterThan(0);
}));
