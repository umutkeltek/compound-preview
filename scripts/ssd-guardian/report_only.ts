/** Scheduled guardians observe and report. No flag/config grants disposal authority. */
import { spawnSync } from "node:child_process";
import { lstatSync, realpathSync, mkdirSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { randomUUID } from "node:crypto";

export const GIB = 1024 ** 3;
export const DEFER_REASON = "owner_review_required: scheduled reporting cannot authorize deletion, model unload, cache relocation or snapshot thinning; use a separately approved sealed manifest and fresh checks";
export function flags(name: string, extra: string[] = []) {
  const allowed = ["--apply", "--help", ...extra], args = process.argv.slice(2);
  const unknown = args.filter((arg) => !allowed.includes(arg));
  if (unknown.length) { console.error(`unknown argument(s): ${unknown.join(", ")}\nallowed: ${allowed.join(", ")}`); process.exit(2); }
  if (args.includes("--help")) { console.log(`usage: ${name} ${allowed.join(" ")}\n--apply writes a report only; no destructive action is available.`); process.exit(0); }
  return { apply: args.includes("--apply"), has: (arg: string) => args.includes(arg) };
}
export function limit(value: unknown, fallback: number): number {
  const n = value === undefined ? fallback : Number(value);
  if (!Number.isFinite(n) || n < 0) throw new Error("invalid non-negative reporting limit");
  return n;
}
export function command(program: string, args: string[], timeout = 45_000): string {
  const result = spawnSync(program, args, { encoding: "utf8", timeout, maxBuffer: 8 * 1024 * 1024 });
  if (result.error || result.status !== 0) throw new Error(`${program} observation failed: ${result.error?.message ?? result.stderr?.trim() ?? result.status}`);
  return result.stdout;
}
export function capacity(): number {
  const fields = command("/bin/df", ["-k", "/System/Volumes/Data"]).trim().split(/\r?\n/).at(-1)?.trim().split(/\s+/) ?? [];
  const value = Number(fields[3]) * 1024;
  if (!fields[3] || !Number.isSafeInteger(value) || value < 0) throw new Error("invalid free-byte observation");
  return value;
}
export type Measurement = { physical_path?: string; size_bytes: number | null; status: "measured" | "alias" | "absent" | "failed"; note?: string };
export function measure(path: string, seen = new Set<string>(), deadline = Date.now() + 45_000): Measurement {
  try {
    try { lstatSync(path); } catch (error: any) { if (error?.code === "ENOENT") return { status: "absent", size_bytes: 0 }; throw error; }
    const physical = realpathSync(path);
    if (seen.has(physical)) return { status: "alias", physical_path: physical, size_bytes: 0, note: "physical root already measured in this report" };
    if (Date.now() >= deadline) throw new Error("measurement time bound exceeded; no partial success");
    const output = command("/usr/bin/du", ["-x", "-sk", physical], Math.max(1, deadline - Date.now()));
    const match = output.match(/^(\d+)\s/), bytes = match ? Number(match[1]) * 1024 : NaN;
    if (!Number.isSafeInteger(bytes) || bytes < 0) throw new Error("invalid allocated-byte observation");
    seen.add(physical);
    return { status: "measured", physical_path: physical, size_bytes: bytes };
  } catch (error) { return { status: "failed", size_bytes: null, note: String(error).slice(0, 240) }; }
}
// Append-only report files. --apply never overwrites or removes earlier evidence.
export function publish(kind: string, report: object, apply: boolean): string | null {
  if (!apply) return null;
  const root = process.env.COMPOUND_SSD_REPORT_DIR ?? join(process.env.COMPOUND_HOME ?? join(homedir(), ".compound"), "state", "ssd-guardian");
  const dir = join(root, kind);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const path = join(dir, `${kind}-${new Date().toISOString().replace(/[:.]/g, "-")}-${randomUUID()}.json`);
  writeFileSync(path, JSON.stringify(report, null, 2) + "\n", { mode: 0o600, flag: "wx" });
  return path;
}
export function emit(kind: string, report: object, apply: boolean, failed: boolean, deferred: boolean) {
  const body = { ...report, mode: "report_only", requested_apply: apply, execution_policy: "report_only", mutation_authorized: false,
    reclaimed_bytes: 0, planned_reclaim_bytes: 0, retry_required: failed || deferred,
    retry_scope: "reobserve on existing cadence; destructive work requires separate owner review" };
  let receiptPath: string | null = null, publication_error: string | null = null;
  try { receiptPath = publish(kind, body, apply); }
  catch (error) { publication_error = String(error).slice(0, 240); failed = true; body.retry_required = true; }
  console.log(JSON.stringify({ ...body, receiptPath, publication_error }, null, 2));
  process.exitCode = failed ? 2 : deferred ? 3 : 0;
}
