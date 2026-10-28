/**
 * reclaim_selectors.ts — pure eviction math for daily_toolchain_reclaim.ts.
 *
 * Split out from the reclaimer so the decision of *what to delete* can be
 * tested without touching a filesystem. Everything here is total and
 * side-effect free: callers do the IO.
 */

export type CacheEntry = { mtime: number; size: number; path: string };
export type CacheDir = { path: string; newest: number; size: number };

export type Eviction<T> = { doomed: T[]; freed: number; total: number };

/**
 * Age gate first, then least-recently-used eviction until the survivors fit
 * under `capBytes`.
 *
 * Both passes are needed. The age gate alone cannot bound a hot cache — the
 * measured go-build cache held 52 GiB with zero bytes older than three days —
 * and the cap alone would evict entries that are large but still in active use
 * ahead of small stale ones. Age first, then LRU, evicts the genuinely dead
 * before touching anything warm.
 */
export function selectEvictions<T extends { size: number }>(
  items: T[],
  capBytes: number,
  isStale: (item: T) => boolean,
  recency: (item: T) => number,
): Eviction<T> {
  const total = items.reduce((a, i) => a + i.size, 0);
  const doomed = items.filter(isStale);
  let freed = doomed.reduce((a, i) => a + i.size, 0);

  if (total - freed > capBytes) {
    const survivors = items.filter((i) => !isStale(i)).sort((a, b) => recency(a) - recency(b));
    for (const s of survivors) {
      if (total - freed <= capBytes) break;
      doomed.push(s);
      freed += s.size;
    }
  }
  return { doomed, freed, total };
}

export function selectCacheEntryEvictions(entries: CacheEntry[], capBytes: number, ageCutoff: number): Eviction<CacheEntry> {
  return selectEvictions(entries, capBytes, (e) => e.mtime < ageCutoff, (e) => e.mtime);
}

export function selectCacheDirEvictions(dirs: CacheDir[], capBytes: number, ageCutoff: number): Eviction<CacheDir> {
  return selectEvictions(dirs, capBytes, (d) => d.newest < ageCutoff, (d) => d.newest);
}

export type ReclaimStep = {
  id: string;
  status: "executed" | "skipped" | "failed";
  reason?: string;
  reclaimed_bytes?: number;
};

/**
 * Reject unknown arguments.
 *
 * A cleanup script that ignores `--aply` dry-runs forever while its launchd job
 * logs a clean exit 0 — the silent-no-op failure this guardian exists to catch,
 * reintroduced at the argument parser. Both entrypoints share this so the rule
 * has one owner rather than two copies that drift.
 */
export function parseFlags(args: string[], allowed: readonly string[]): { unknown: string[]; has: (flag: string) => boolean } {
  const allowSet = new Set(allowed);
  return { unknown: args.filter((a) => !allowSet.has(a)), has: (flag) => args.includes(flag) };
}

/**
 * Turn executed step records into the run's verdict.
 *
 * A scheduled reclaim that records `status: "failed"` and still exits 0 is
 * indistinguishable from one that worked; the timer reports green while nothing
 * gets reclaimed. A failed step is an alarm and a nonzero exit, always.
 */
export function summarizeAlarm(steps: ReclaimStep[]): { alarm: boolean; alarm_reasons: string[]; exit_code: number } {
  const failures = steps.filter((s) => s.status === "failed");
  return {
    alarm: failures.length > 0,
    alarm_reasons: failures.map((f) => `${f.id}: ${f.reason ?? "failed"}`),
    exit_code: failures.length > 0 ? 2 : 0,
  };
}

/**
 * Parse `stat -f '%m %z %N'` output into records, splitting on RECORD
 * boundaries rather than newlines.
 *
 * A pathname may legally contain a newline. Splitting the stat stream on "\n"
 * then yields a TRUNCATED path — and this output is fed straight to `rm -rf`, so
 * a truncated path is a request to delete the wrong thing. Each record begins
 * with "<mtime> <size> "; the pathname is everything up to the next such prefix,
 * embedded newlines included.
 */
export function parseStatRecords(out: string): CacheEntry[] {
  const re = /(?:^|\n)(\d+) (\d+) /g;
  const heads: { mtime: number; size: number; from: number; at: number }[] = [];
  let m: RegExpExecArray | null;
  while ((m = re.exec(out)) !== null) {
    heads.push({ mtime: Number(m[1]) * 1000, size: Number(m[2]), from: m.index + m[0].length, at: m.index });
    re.lastIndex = m.index + m[0].length;
  }
  const entries: CacheEntry[] = [];
  for (let i = 0; i < heads.length; i++) {
    const end = i + 1 < heads.length ? heads[i + 1].at : out.length;
    const path = out.slice(heads[i].from, end).replace(/\n$/, "");
    if (path) entries.push({ mtime: heads[i].mtime, size: heads[i].size, path });
  }
  return entries;
}

/**
 * Read a numeric limit from the environment, refusing one that cannot be
 * trusted.
 *
 * A negative cap evicts the ENTIRE cache (every survivor is "over budget"); a
 * non-numeric value becomes NaN, and every `total - freed > NaN` comparison is
 * false, so the cap silently stops existing. A destructive job must reject a
 * limit it cannot parse, never guess at one.
 */
export function parseLimit(name: string, raw: string | undefined, fallback: number): { ok: true; value: number } | { ok: false; error: string } {
  if (raw === undefined || raw.trim() === "") return { ok: true, value: fallback };
  const v = Number(raw);
  if (!Number.isFinite(v) || v < 0) {
    return { ok: false, error: `invalid ${name}=${JSON.stringify(raw)}: expected a finite, non-negative number` };
  }
  return { ok: true, value: v };
}
