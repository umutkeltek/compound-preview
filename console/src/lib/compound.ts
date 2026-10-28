import { execFile } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdir, readdir, readFile, rename, rm, stat, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { homedir } from "node:os";
import { tmpdir } from "node:os";
import { isAbsolute, join, resolve } from "node:path";
import { promisify } from "node:util";

import { consoleChildEnv } from "./console-child-env.ts";

const exec = promisify(execFile);

/** Runtime home: never repo. Resolved to an absolute path ONCE at module load
 * (process start) so it is the frozen instance pin — no per-request value can
 * retarget the home a mutation writes. Mirrors instance-pin.ts's resolution;
 * the instance-pin smoke asserts the two never drift. */
export const COMPOUND_HOME = resolve(
  (process.env.COMPOUND_HOME ?? "").trim() || `${homedir()}/.compound`,
);

/** Direct private readers are retired. The legacy fallback environment cannot
 * reactivate them, so every standard consumer remains projection-only. */
export function privateFallbacksEnabled(): boolean {
  return false;
}

function resolveBin(): string {
  const candidates = [
    process.env.COMPOUND_BIN,
    `${homedir()}/.local/bin/compound`,
    "compound",
  ].filter(Boolean) as string[];

  for (const candidate of candidates) {
    if (candidate === "compound" || existsSync(candidate)) return candidate;
  }
  return "compound";
}

export const COMPOUND_BIN = resolveBin();

export type Json = Record<string, any>;

export type InstalledBinaryFreshness = {
  status: "fresh" | "stale" | "missing" | "unknown";
  binary: string;
  installedBuild: string;
  head: string;
  message: string;
};

export type CompoundRunResult = {
  stdout: string;
  stderr: string;
};

export type CompoundRunFailure = CompoundRunResult & {
  message: string;
};

function firstJsonObject(text: string): string | null {
  const start = text.indexOf("{");
  if (start < 0) return null;

  let depth = 0;
  let inString = false;
  let escaped = false;

  for (let i = start; i < text.length; i++) {
    const ch = text[i];

    if (inString) {
      if (escaped) {
        escaped = false;
      } else if (ch === "\\") {
        escaped = true;
      } else if (ch === "\"") {
        inString = false;
      }
      continue;
    }

    if (ch === "\"") {
      inString = true;
      continue;
    }
    if (ch === "{") depth++;
    if (ch === "}") {
      depth--;
      if (depth === 0) return text.slice(start, i + 1);
    }
  }

  return null;
}

export async function runCompound(args: string[]): Promise<CompoundRunResult> {
  const { stdout, stderr } = await exec(COMPOUND_BIN, args, {
    env: consoleChildEnv({ COMPOUND_HOME }),
    timeout: 30_000,
    maxBuffer: 64 * 1024 * 1024,
  });
  return { stdout, stderr };
}

export function compoundFailure(error: unknown): CompoundRunFailure {
  const err = error as Partial<CompoundRunFailure> & {
    stdout?: unknown;
    stderr?: unknown;
  };
  return {
    message: typeof err.message === "string" ? err.message : "compound command failed",
    stdout: typeof err.stdout === "string" ? err.stdout : "",
    stderr: typeof err.stderr === "string" ? err.stderr : "",
  };
}

async function execRunJson(args: string[], timeoutMs: number): Promise<Json | null> {
  const { stdout } = await exec(COMPOUND_BIN, args, {
    env: consoleChildEnv({ COMPOUND_HOME }),
    timeout: timeoutMs,
    maxBuffer: 64 * 1024 * 1024,
  });
  const json = firstJsonObject(stdout.trim());
  return json ? JSON.parse(json) : null;
}

/* ---------- runJson/runProjection memo: TTL cache + stale-while-revalidate +
 * in-flight de-dup + never-cache-null-over-good ----------
 * Every `compound ... --json` re-exec is a subprocess spawn; without this,
 * every request (and every N+1 caller within a request) pays that cost again.
 * Keyed by JSON.stringify(argv):
 *  - fresh success (<25s old, nothing has failed since): served straight from
 *    cache, no exec - `live`.
 *  - a last-good value exists but is past TTL, or the latest attempt to
 *    refresh it failed: served immediately from cache (never blocks the
 *    request) - `stale` - while at most one background exec refreshes it.
 *  - no last-good value has EVER landed for this argv: `unreachable`. A cold
 *    caller (no cache entry, no in-flight exec) awaits the first real attempt
 *    so it can report the genuine failure instead of an optimistic default.
 *  - cold + concurrent callers for the SAME argv (e.g. Promise.all fan-out):
 *    collapse into one in-flight exec when its timeout can satisfy the new
 *    caller. A caller with a longer deadline starts one replacement exec so a
 *    cheap 15s layout read cannot SIGTERM a 45s page projection. This still
 *    kills same-budget N+1 fan-outs (packets per work item, repeated stats).
 *  - invalidation advances a generation; execs that started before it may
 *    settle for their original caller but cannot repopulate the current cache.
 *
 * THE LAW (packet: console-honest-degradation, 2026-07-14 - the "clean seam"
 * bug): a failed or timed-out attempt NEVER overwrites a good cached value
 * with null, and NEVER stamps a fresh success timestamp on a failure. The old
 * code's cold-failure catch did `storeRunJsonEntry(key, null)`, which stamped
 * a fresh `storedAt` on a null value - for the next TTL window every reader
 * saw "no data" as a confirmed, current fact, and `null` mapped straight to
 * the doctrine empty state ("Clean seam. Nothing waits on you.")
 * pixel-identical to a genuinely healthy dashboard. Now a failed attempt only
 * ever updates `lastError`/`lastErrorAt`; `value`/`storedAt` (the last-good
 * facts) move forward ONLY on a real success.
 *
 * Bounded to ~50 entries (oldest-touched evicted first). Never throws - same
 * fail-soft contract as the pre-cache runJson. Mutations call
 * invalidateRunJsonCache() so router.refresh() always shows fresh truth.
 */
const RUN_JSON_TTL_MS = 25_000;
const RUN_JSON_MAX_ENTRIES = 50;
const DEFAULT_RUN_JSON_TIMEOUT_MS = 15_000;
/**
 * Budget for the heavy whole-home projections (attention, workbench, reviews,
 * coordination, memory candidates, home doctor). On the operator's live home
 * each costs 30-75s COLD SERIALLY, but the briefing fires several in parallel
 * and they thrash on home I/O together (measured: four doors concurrently =
 * ~117s wall on 16 cores), so any budget below the concurrent worst case never
 * lands a first success and the surface renders the unreachable seam forever.
 * The first landing populates the shared TTL cache; every later load serves
 * warm data (25s TTL) or last-good (stale-while-revalidate) without re-exec.
 */
export const HEAVY_PROJECTION_TIMEOUT_MS = 180_000;

type RunJsonCacheEntry = {
  /** last SUCCESSFUL payload; null only if a success has never landed. */
  value: Json | null;
  /** ms epoch of that last success; null only if a success has never landed. */
  storedAt: number | null;
  revalidating: boolean;
  /** message from the most recent failed attempt, if any (may postdate storedAt). */
  lastError: string | null;
  lastErrorAt: number | null;
};

type RunJsonInflightEntry = {
  timeoutMs: number;
  token: symbol;
  promise: Promise<RunJsonCacheEntry>;
};

/**
 * Cache storage lives on globalThis, NOT module scope. This console's runtime
 * materializes a fresh module instance per server render (measured 2026-08-31:
 * every request saw an empty cache and re-executed every projection, and the
 * page+layout pair executed the same argv twice per render), so any
 * module-level memo is dead weight. globalThis is shared by every module
 * instance in the process, which both persists last-good data across requests
 * and collapses same-argv fan-outs (page+layout) through the in-flight map.
 */
type RunJsonCacheGlobals = {
  cache: Map<string, RunJsonCacheEntry>;
  inflight: Map<string, RunJsonInflightEntry>;
  generation: number;
  diskWork: Promise<void>;
  binaryFreshness:
    | {
        storedAt: number;
        value: InstalledBinaryFreshness;
      }
    | null;
};

const runJsonGlobals = ((globalThis as typeof globalThis & {
  __compoundConsoleRunJson?: RunJsonCacheGlobals;
}).__compoundConsoleRunJson ??= {
  cache: new Map<string, RunJsonCacheEntry>(),
  inflight: new Map<string, RunJsonInflightEntry>(),
  generation: 0,
  diskWork: Promise.resolve(),
  binaryFreshness: null,
});

const runJsonCache = runJsonGlobals.cache;
const runJsonInflight = runJsonGlobals.inflight;

// Module reloads can retain an older global cache object without this field.
runJsonGlobals.diskWork ??= Promise.resolve();

function queueRunJsonDiskWork<T>(work: () => Promise<T>): Promise<T> {
  const result = runJsonGlobals.diskWork.then(work);
  // A failed cache optimization must not poison later invalidations or reads.
  runJsonGlobals.diskWork = result.then(() => undefined, () => undefined);
  return result;
}


function pruneRunJsonCache() {
  while (runJsonCache.size > RUN_JSON_MAX_ENTRIES) {
    const oldestKey = runJsonCache.keys().next().value;
    if (oldestKey === undefined) break;
    runJsonCache.delete(oldestKey);
  }
}

/** A completion discarded by invalidation may observe a newer entry, but it
 * must never put its own pre-invalidation result back into the cache. */
function runJsonEntryAfterDiscard(key: string): RunJsonCacheEntry {
  return runJsonCache.get(key) ?? {
    value: null,
    storedAt: null,
    revalidating: false,
    lastError: null,
    lastErrorAt: null,
  };
}

/** A real success: the only path allowed to move `value`/`storedAt` forward. */
function commitRunJsonSuccess(
  key: string,
  value: Json | null,
  startedGeneration: number,
): RunJsonCacheEntry {
  if (startedGeneration !== runJsonGlobals.generation) return runJsonEntryAfterDiscard(key);
  // Exit 0 but no parseable JSON object in stdout is not a confirmed empty
  // result - bookkeep it as a failure so never-cache-null holds even when the
  // exec itself didn't reject.
  if (value === null) {
    return commitRunJsonFailure(key, "compound produced no parseable JSON output", startedGeneration);
  }
  // delete-then-set moves the key to the end of Map's iteration order, so the
  // eviction above naturally drops the least-recently-refreshed entry first.
  runJsonCache.delete(key);
  const storedAt = Date.now();
  const entry: RunJsonCacheEntry = { value, storedAt, revalidating: false, lastError: null, lastErrorAt: null };
  runJsonCache.set(key, entry);
  pruneRunJsonCache();
  // Write-through to the disk tier so other render processes (and the next
  // server restart) inherit this last-good fact. Fire-and-forget: disk is an
  // optimization tier, never a correctness gate.
  void writeRunJsonDiskEntry(key, value, storedAt);
  return entry;
}

/** A failed (or parse-empty) attempt: NEVER touches `value`/`storedAt` - only
 * stamps the failure so status derivation can tell `stale` from `live`. */
function commitRunJsonFailure(
  key: string,
  message: string,
  startedGeneration: number,
): RunJsonCacheEntry {
  if (startedGeneration !== runJsonGlobals.generation) return runJsonEntryAfterDiscard(key);
  const prev = runJsonCache.get(key);
  runJsonCache.delete(key);
  const entry: RunJsonCacheEntry = {
    value: prev?.value ?? null,
    storedAt: prev?.storedAt ?? null,
    revalidating: false,
    lastError: message,
    lastErrorAt: Date.now(),
  };
  runJsonCache.set(key, entry);
  pruneRunJsonCache();
  return entry;
}

/* ---------- process-surviving disk tier ----------
 * This console's runtime renders each dynamic request in a FRESH child process
 * (measured 2026-08-31: consecutive requests logged different pids), so no
 * in-process store — module scope or globalThis — can persist last-good data
 * between requests. The disk tier gives every process the same view:
 *  - write-through on every real success (atomic tmp+rename, 0600),
 *  - read on a memory miss, feeding the SAME TTL/stale machinery, so after the
 *    first-ever landing every request serves warm data instantly while one
 *    background exec revalidates,
 *  - a server restart degrades honestly to `stale` (last-good on disk) instead
 *    of `unreachable` — the value on disk IS the last-good fact.
 * Files live under the user's tmpdir (private to the uid), keyed by
 * sha256(argv); invalidateRunJsonCache() clears the dir on mutations.
 */
const RUN_JSON_DISK_DIR = join(tmpdir(), "compound-console-run-json");
const RUN_JSON_DISK_MAX_FILES = 64;

function runJsonDiskPath(key: string): string {
  return join(RUN_JSON_DISK_DIR, createHash("sha256").update(key).digest("hex") + ".json");
}

async function readRunJsonDiskEntry(key: string): Promise<RunJsonCacheEntry | null> {
  return queueRunJsonDiskWork(async () => {
    try {
      const raw = await readFile(runJsonDiskPath(key), "utf8");
      const parsed = JSON.parse(raw) as { storedAt?: unknown; value?: unknown };
      if (typeof parsed.storedAt !== "number" || parsed.value == null || typeof parsed.value !== "object") {
        return null;
      }
      return {
        value: parsed.value as Json,
        storedAt: parsed.storedAt,
        revalidating: false,
        lastError: null,
        lastErrorAt: null,
      };
    } catch {
      return null;
    }
  });
}

async function writeRunJsonDiskEntry(key: string, value: Json, storedAt: number): Promise<void> {
  return queueRunJsonDiskWork(async () => {
    try {
      await mkdir(RUN_JSON_DISK_DIR, { recursive: true });
      const final = runJsonDiskPath(key);
      const tmp = `${final}.${process.pid}.${Math.random().toString(36).slice(2)}.tmp`;
      await writeFile(tmp, JSON.stringify({ storedAt, value }), { mode: 0o600 });
      await rename(tmp, final);
      // Bound the dir: once past the cap, unlink the oldest files. Cheap readdir,
      // best-effort — a failed prune only costs disk, never correctness.
      const names = await readdir(RUN_JSON_DISK_DIR);
      if (names.length <= RUN_JSON_DISK_MAX_FILES) return;
      const stats = await Promise.all(
        names.filter((n) => n.endsWith(".json")).map(async (n) => {
          const full = join(RUN_JSON_DISK_DIR, n);
          try {
            return { full, mtime: (await stat(full)).mtimeMs };
          } catch {
            return null;
          }
        }),
      );
      const ordered = stats.filter((s): s is { full: string; mtime: number } => s !== null).sort((a, b) => a.mtime - b.mtime);
      for (const victim of ordered.slice(0, ordered.length - RUN_JSON_DISK_MAX_FILES)) {
        await rm(victim.full, { force: true }).catch(() => undefined);
      }
    } catch {
      // Disk is an optimization tier; a failed write only costs warmth.
    }
  });
}

function clearRunJsonDiskEntries(): void {
  // Serialize removal after prior writes and before subsequent cold reads.
  // Fire-and-forget deletion alone lets an invalidated value return as live.
  void queueRunJsonDiskWork(async () => {
    try {
      const names = await readdir(RUN_JSON_DISK_DIR);
      await Promise.all(names.map((name) => rm(join(RUN_JSON_DISK_DIR, name), { force: true }).catch(() => undefined)));
    } catch {
      // A missing or unreadable optimization tier still permits real execution.
    }
  });
}
function startRunJsonInflight(
  key: string,
  args: string[],
  timeoutMs: number,
): Promise<RunJsonCacheEntry> {
  const inflight = runJsonInflight.get(key);
  if (inflight && inflight.timeoutMs >= timeoutMs) return inflight.promise;

  const token = Symbol(key);
  const startedGeneration = runJsonGlobals.generation;
  const promise = execRunJson(args, timeoutMs)
    .then((value) => commitRunJsonSuccess(key, value, startedGeneration))
    .catch((error) => commitRunJsonFailure(key, compoundFailure(error).message, startedGeneration))
    .finally(() => {
      if (runJsonInflight.get(key)?.token === token) runJsonInflight.delete(key);
    });
  runJsonInflight.set(key, { timeoutMs, token, promise });
  return promise;
}

/**
 * The one place a `compound ... --json` argv is actually resolved: exec, TTL
 * cache, stale-while-revalidate, in-flight de-dup, never-cache-null. Both
 * `runJson` (value-only, back-compat) and `runProjection` (full honest-
 * degradation model below) read through this shared memo.
 */
async function readRunJsonEntry(args: string[], timeoutMs: number): Promise<RunJsonCacheEntry> {
  // Key includes COMPOUND_BIN so the disk tier cannot resurrect a last-good
  // payload written by a different executable into a process whose binary is
  // broken or pointed somewhere else (e.g. a smoke probing with
  // COMPOUND_BIN=bun must see a cold cache, not the operator's real success).
  const key = `${COMPOUND_BIN}\0${JSON.stringify(args)}`;
  let entry = runJsonCache.get(key) ?? null;

  if (!entry) {
    // Memory miss: this process has never seen the argv. The disk tier may
    // still carry a last-good fact from another process or a prior run.
    const diskGeneration = runJsonGlobals.generation;
    const disk = await readRunJsonDiskEntry(key);
    if (diskGeneration !== runJsonGlobals.generation) {
      return readRunJsonEntry(args, timeoutMs);
    }
    if (disk) {
      runJsonCache.delete(key);
      runJsonCache.set(key, disk);
      entry = disk;
    }
  }

  if (entry) {
    const fresh = entry.storedAt !== null && Date.now() - entry.storedAt < RUN_JSON_TTL_MS;
    if (fresh) return entry;

    // Stale values return immediately while they revalidate. A cached cold
    // failure still awaits its retry, because it has no truthful value to
    // serve. Track both paths in the shared in-flight map so a longer caller
    // can replace a shorter refresh instead of inheriting its timeout.
    entry.revalidating = true;
    const refresh = startRunJsonInflight(key, args, timeoutMs);
    return entry.storedAt === null ? refresh : entry;
  }

  return startRunJsonInflight(key, args, timeoutMs);
}

/**
 * Run read-only compound verb that emits JSON. The console surfaces mutation
 * commands but only API routes below execute allowlisted mutations.
 */
export async function runJson(args: string[]): Promise<Json | null> {
  const entry = await readRunJsonEntry(args, DEFAULT_RUN_JSON_TIMEOUT_MS);
  return entry.value;
}

export type ProjectionStatus = "live" | "stale" | "unreachable";

/**
 * The honest-degradation read model (packet: console-honest-degradation). A
 * door being down is a fact the console states, never something it papers
 * over with the last shape that happened to parse.
 *  - `live`: a fresh success landed within the TTL and nothing has failed since.
 *  - `stale`: a last-good value exists, but it is past TTL or the latest
 *    refresh attempt failed - render the last-good value, never invent a gap.
 *  - `unreachable`: no success has EVER landed for this argv - render nothing
 *    invented; the caller shows the degraded seam, not a doctrine empty state
 *    (that state is earned only by a genuinely empty `live` read).
 */
export type ProjectionRead = {
  status: ProjectionStatus;
  value: Json | null;
  /** the payload's own `generated_at` field, when the current value carries one. */
  generatedAt: string | null;
  /** ISO timestamp of the last successful fetch; null iff status is `unreachable`. */
  lastGoodAt: string | null;
  /** message from the most recent failed attempt, if any. */
  error: string | null;
  elapsedMs: number;
};

function projectionStatus(entry: RunJsonCacheEntry, now: number): ProjectionStatus {
  if (entry.storedAt === null) return "unreachable";
  const failedSinceSuccess = entry.lastErrorAt !== null && entry.lastErrorAt > entry.storedAt;
  const fresh = now - entry.storedAt < RUN_JSON_TTL_MS;
  return fresh && !failedSinceSuccess ? "live" : "stale";
}

/**
 * Run a read-only compound verb and return the full honest-degradation model
 * instead of a bare value - see `ProjectionRead`. Shares the exact cache
 * `runJson` uses (same key = JSON.stringify(argv)), so a page reading the same
 * argv through both never pays for two subprocess executions. `opts.timeoutMs`
 * overrides the default per call site (e.g. attention/workbench need ~45s
 * today; see console/src/app/page.tsx).
 */
export async function runProjection(args: string[], opts?: { timeoutMs?: number }): Promise<ProjectionRead> {
  const startedAt = Date.now();
  const entry = await readRunJsonEntry(args, opts?.timeoutMs ?? DEFAULT_RUN_JSON_TIMEOUT_MS);
  const now = Date.now();
  return {
    status: projectionStatus(entry, now),
    value: entry.value,
    generatedAt: typeof entry.value?.generated_at === "string" ? entry.value.generated_at : null,
    lastGoodAt: entry.storedAt !== null ? new Date(entry.storedAt).toISOString() : null,
    error: entry.lastError,
    elapsedMs: now - startedAt,
  };
}

/** Mutations advance the generation before clearing state so an exec already
 * running for the old snapshot cannot repopulate this cache after refresh. */
export function invalidateRunJsonCache(): void {
  runJsonGlobals.generation += 1;
  runJsonCache.clear();
  runJsonInflight.clear();
  runJsonGlobals.binaryFreshness = null;
  // A mutation wants the next render to read post-mutation truth: the disk
  // tier must not resurrect pre-mutation last-good values.
  clearRunJsonDiskEntries();
}

/** Worktree inventory is scoped by an explicit operator repository binding.
 * Never substitute the console source checkout or cwd for the user's repo. */
export async function getWorktreeInventory(repo = process.env.COMPOUND_WORKTREE_REPO ?? ""): Promise<{
  repo: string | null; read: ProjectionRead | null;
}> {
  if (!repo || !isAbsolute(repo)) return { repo: null, read: null };
  return {
    repo,
    read: await runProjection(["executors", "worktrees", "--repo", repo, "--json"], { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS }),
  };
}

function repoRootFromConsole(): string {
  const candidates = [
    process.env.COMPOUND_REPO_ROOT,
    process.env.COMPOUND_REPO,
    resolve(process.cwd(), ".."),
    process.cwd(),
  ].filter(Boolean) as string[];
  for (const candidate of candidates) {
    if (existsSync(resolve(candidate, "go.mod"))) return candidate;
  }
  return resolve(process.cwd(), "..");
}

function parseBuildSHA(versionOutput: string): string {
  return versionOutput.match(/\(build ([0-9a-fA-F]+|dev)\)/)?.[1] ?? "";
}

function classifyBuild(installedBuild: string, head: string): InstalledBinaryFreshness["status"] {
  if (!installedBuild || installedBuild === "dev" || !head) return "unknown";
  if (head.startsWith(installedBuild) || installedBuild.startsWith(head)) return "fresh";
  return "stale";
}

export async function getInstalledBinaryFreshness(): Promise<InstalledBinaryFreshness> {
  if (runJsonGlobals.binaryFreshness && Date.now() - runJsonGlobals.binaryFreshness.storedAt < RUN_JSON_TTL_MS) {
    return runJsonGlobals.binaryFreshness.value;
  }

  const binary = COMPOUND_BIN;
  const binaryLooksMissing = binary !== "compound" && !existsSync(binary);
  if (binaryLooksMissing) {
    const value: InstalledBinaryFreshness = {
      status: "missing",
      binary,
      installedBuild: "",
      head: "",
      message: `Installed compound binary is missing at ${binary}.`,
    };
    runJsonGlobals.binaryFreshness = { storedAt: Date.now(), value };
    return value;
  }

  try {
    const [{ stdout: versionOut }, { stdout: headOut }] = await Promise.all([
      exec(binary, ["version"], { env: consoleChildEnv(), timeout: 1_500 }),
      exec("git", ["-C", repoRootFromConsole(), "rev-parse", "--short", "HEAD"], {
        env: consoleChildEnv(),
        timeout: 1_500,
      }),
    ]);
    const installedBuild = parseBuildSHA(versionOut);
    const head = headOut.trim();
    const status = classifyBuild(installedBuild, head);
    const value: InstalledBinaryFreshness = {
      status,
      binary,
      installedBuild,
      head,
      message:
        status === "stale"
          ? `Installed compound is ${installedBuild}; source HEAD is ${head}.`
          : status === "fresh"
            ? `Installed compound matches source HEAD ${head}.`
            : "Installed compound build freshness is unknown.",
    };
    runJsonGlobals.binaryFreshness = { storedAt: Date.now(), value };
    return value;
  } catch {
    const value: InstalledBinaryFreshness = {
      status: "unknown",
      binary,
      installedBuild: "",
      head: "",
      message: "Installed compound build freshness is unknown.",
    };
    runJsonGlobals.binaryFreshness = { storedAt: Date.now(), value };
    return value;
  }
}

/* ---------- defensive accessors (schemas evolve; never crash surface) ---------- */

export const arr = (value: any): any[] => (Array.isArray(value) ? value : []);
export const num = (value: any): number =>
  typeof value === "number" ? value : 0;
export const str = (value: any): string =>
  typeof value === "string" ? value : "";

/** Relative-time helpers live in the client-safe `@/lib/time` module — import
 * them from there. (No re-export here: this file pulls in node: builtins and the
 * scripts/*.mjs smokes load it under node --experimental-strip-types, which
 * cannot resolve extensionless TS imports.) */

/* ---------- fetchers ---------- */

export async function getWorkbench(): Promise<Json | null> {
  return (await runProjection(["workbench", "--json"], { timeoutMs: HEAVY_PROJECTION_TIMEOUT_MS })).value;
}

export async function getReviews(opts?: { timeoutMs?: number }): Promise<Json | null> {
  const entry = await readRunJsonEntry(
    ["reviews", "--json"],
    opts?.timeoutMs ?? HEAVY_PROJECTION_TIMEOUT_MS,
  );
  return entry.value;
}

export async function getSourceConnections(): Promise<Json | null> {
  return runJson(["source", "connections", "--json"]);
}

export async function getExecutors(): Promise<Json | null> {
  return runJson(["executors", "--json"]);
}

export async function getWorkList(): Promise<Json | null> {
  return runJson(["work", "list", "--json"]);
}

export async function getWorkLeases(): Promise<Json | null> {
  return runJson(["work", "leases", "--json"]);
}

export async function getWork(id: string): Promise<Json | null> {
  // Work owns explicit IDs as well as generated ones; they are not hex-only.
  // Keep one safe argv/path element and let the owned read resolve its identity.
  if (!id.startsWith("work_") || id.length === 5 || id.trim() !== id || /[/\\\p{Cc}]/u.test(id)) return null;
  return runJson(["work", "show", id]);
}

/** Pull review rows out reviews report, whatever field is named. */
export function reviewItems(reviews: Json | null): Json[] {
  if (!reviews) return [];
  for (const key of ["items", "actions", "reviews", "queue", "review_actions"]) {
    const value = arr(reviews[key]);
    if (value.length) return value;
  }
  return [];
}

/** Human sentence for generic review rows. */
export function reviewSentence(item: Json): string {
  const area = str(item.area) || str(item.review_area);
  const action = str(item.action) || str(item.next) || str(item.reason) || str(item.next_action);
  if (action) return action;
  if (area === "external-repo-intake") {
    if (!privateFallbacksEnabled()) return "External repo awaits a projected review action.";
    return "External repo awaits decision: fill review YAML, then record it.";
  }
  return "Awaiting review.";
}

/** Exact command on generic review row: copied, never auto-run by card render. */
export function reviewCommand(item: Json): string {
  const area = str(item.area) || str(item.review_area);
  const id = str(item.id) || str(item.review_id);
  const path = str(item.path) || str(item.review_path);
  if (area === "external-repo-intake" && !privateFallbacksEnabled()) {
    return "compound reviews --area external-repo-intake";
  }
  if (area === "external-repo-intake" && (path || id)) {
    const yaml = path || `${COMPOUND_HOME}/reviews/external-repo-intake/${id}.review.yaml`;
    return `compound capability external-intake-review ${yaml.replace(/\.json$/, ".review.yaml")}`;
  }
  if (area) return `compound reviews --area ${area}`;
  return "compound reviews";
}
