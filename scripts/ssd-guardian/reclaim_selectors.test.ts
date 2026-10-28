import { describe, expect, test } from "bun:test";
import { parseFlags, parseLimit, parseStatRecords, selectCacheDirEvictions, selectCacheEntryEvictions, summarizeAlarm } from "./reclaim_selectors";

const GIB = 1024 ** 3;
const DAY = 86_400_000;
const NOW = 1_800_000_000_000;

const entry = (ageDays: number, sizeGib: number, path: string) => ({ mtime: NOW - ageDays * DAY, size: sizeGib * GIB, path });

describe("selectCacheEntryEvictions", () => {
  test("evicts nothing when the cache is under cap and everything is fresh", () => {
    const r = selectCacheEntryEvictions([entry(0, 5, "a"), entry(1, 5, "b")], 20 * GIB, NOW - 2 * DAY);
    expect(r.doomed).toEqual([]);
    expect(r.freed).toBe(0);
  });

  test("age gate removes stale entries even when the cache is under cap", () => {
    const r = selectCacheEntryEvictions([entry(0, 1, "hot"), entry(9, 2, "stale")], 100 * GIB, NOW - 2 * DAY);
    expect(r.doomed.map((d) => d.path)).toEqual(["stale"]);
    expect(r.freed).toBe(2 * GIB);
  });

  test("cap evicts oldest-first and stops as soon as the survivors fit", () => {
    // 40 GiB of fresh entries against a 20 GiB cap: the two oldest go.
    const r = selectCacheEntryEvictions(
      [entry(0, 10, "newest"), entry(1, 10, "mid"), entry(1.5, 10, "older"), entry(1.9, 10, "oldest")],
      20 * GIB,
      NOW - 3 * DAY,
    );
    expect(r.doomed.map((d) => d.path)).toEqual(["oldest", "older"]);
    expect(r.total - r.freed).toBeLessThanOrEqual(20 * GIB);
  });

  test("a cache that is entirely hot is still capped — the regression that age-gating alone missed", () => {
    const hot = Array.from({ length: 52 }, (_, i) => entry(i / 48, 1, `e${i}`));
    const r = selectCacheEntryEvictions(hot, 20 * GIB, NOW - 3 * DAY);
    expect(r.doomed.length).toBe(32);
    expect(r.total - r.freed).toBeLessThanOrEqual(20 * GIB);
  });

  test("an empty cache is a no-op, not a division by zero", () => {
    const r = selectCacheEntryEvictions([], 20 * GIB, NOW);
    expect(r).toEqual({ doomed: [], freed: 0, total: 0 });
  });

  test("a single entry larger than the cap is evicted rather than left over budget", () => {
    const r = selectCacheEntryEvictions([entry(0, 50, "whale")], 20 * GIB, NOW - 3 * DAY);
    expect(r.doomed.map((d) => d.path)).toEqual(["whale"]);
  });
});

describe("selectCacheDirEvictions", () => {
  const dir = (ageDays: number, sizeGib: number, path: string) => ({ path, newest: NOW - ageDays * DAY, size: sizeGib * GIB });

  test("abandoned per-hash caches are evicted whole", () => {
    const r = selectCacheDirEvictions([dir(0, 5, "live"), dir(6, 5, "abandoned")], 100 * GIB, NOW - 2 * DAY);
    expect(r.doomed.map((d) => d.path)).toEqual(["abandoned"]);
  });

  test("staleness uses newest-content time, so a dir touched today survives the gate", () => {
    const r = selectCacheDirEvictions([dir(0.1, 40, "busy")], 100 * GIB, NOW - 2 * DAY);
    expect(r.doomed).toEqual([]);
  });

  test("cap still applies when every per-hash cache is fresh", () => {
    const r = selectCacheDirEvictions([dir(0, 30, "a"), dir(1, 30, "b"), dir(1.5, 30, "c")], 40 * GIB, NOW - 3 * DAY);
    expect(r.total - r.freed).toBeLessThanOrEqual(40 * GIB);
    expect(r.doomed.map((d) => d.path)).toEqual(["c", "b"]);
  });
});

describe("parseFlags", () => {
  const ALLOWED = ["--apply", "--force", "--help"] as const;

  test("accepts the declared flags", () => {
    const f = parseFlags(["--apply", "--force"], ALLOWED);
    expect(f.unknown).toEqual([]);
    expect(f.has("--apply")).toBe(true);
    expect(f.has("--help")).toBe(false);
  });

  test("a typo is reported, never silently dropped — the regression that made --aply a no-op", () => {
    expect(parseFlags(["--aply"], ALLOWED).unknown).toEqual(["--aply"]);
    expect(parseFlags(["--froce"], ALLOWED).unknown).toEqual(["--froce"]);
  });

  test("a typo does not smuggle through as its intended flag", () => {
    // The whole point: --aply must not turn into an apply, and must not be ignored.
    expect(parseFlags(["--aply"], ALLOWED).has("--apply")).toBe(false);
  });

  test("no arguments is the valid dry-run case", () => {
    expect(parseFlags([], ALLOWED).unknown).toEqual([]);
  });

  test("every unknown argument is reported, not just the first", () => {
    expect(parseFlags(["--aply", "--nope", "--force"], ALLOWED).unknown).toEqual(["--aply", "--nope"]);
  });
});

describe("summarizeAlarm", () => {
  test("a run whose steps all worked exits 0 and does not alarm", () => {
    const v = summarizeAlarm([
      { id: "go-build-cap", status: "executed", reclaimed_bytes: 1 },
      { id: "cargo-prune", status: "skipped", reason: "cargo live" },
    ]);
    expect(v).toEqual({ alarm: false, alarm_reasons: [], exit_code: 0 });
  });

  test("one failed step exits nonzero — the regression that let a broken nightly log green", () => {
    const v = summarizeAlarm([
      { id: "go-build-cap", status: "executed", reclaimed_bytes: 5 },
      { id: "brew-cleanup", status: "failed", reason: "brew: command not found" },
    ]);
    expect(v.alarm).toBe(true);
    expect(v.exit_code).toBe(2);
    expect(v.alarm_reasons).toEqual(["brew-cleanup: brew: command not found"]);
  });

  test("reclaiming bytes does not excuse a failed step", () => {
    // A partially-working run is still a run that needs an operator to look.
    const v = summarizeAlarm([
      { id: "go-build-cap", status: "executed", reclaimed_bytes: 40 * 1024 ** 3 },
      { id: "simctl-prune", status: "failed", reason: "xcrun missing" },
    ]);
    expect(v.exit_code).toBe(2);
  });

  test("every failure is named, so the log says which step did nothing", () => {
    const v = summarizeAlarm([
      { id: "a", status: "failed", reason: "boom" },
      { id: "b", status: "failed" },
    ]);
    expect(v.alarm_reasons).toEqual(["a: boom", "b: failed"]);
  });

  test("skipped is a guard doing its job, not a failure", () => {
    const v = summarizeAlarm([{ id: "go-build-cap", status: "skipped", reason: "go toolchain live" }]);
    expect(v.exit_code).toBe(0);
  });

  test("an empty step list is not an alarm", () => {
    expect(summarizeAlarm([])).toEqual({ alarm: false, alarm_reasons: [], exit_code: 0 });
  });
});

describe("parseStatRecords", () => {
  test("parses ordinary records", () => {
    const r = parseStatRecords("1700000000 4096 /a/b/c\n1700000001 8192 /a/b/d\n");
    expect(r).toEqual([
      { mtime: 1700000000000, size: 4096, path: "/a/b/c" },
      { mtime: 1700000001000, size: 8192, path: "/a/b/d" },
    ]);
  });

  test("a pathname containing a newline survives intact — splitting on \\n truncated it into a DIFFERENT path, and this output is fed to rm", () => {
    const r = parseStatRecords("1700000000 10 /cache/we\nird/file\n1700000001 20 /cache/next\n");
    expect(r.map((e) => e.path)).toEqual(["/cache/we\nird/file", "/cache/next"]);
    // The old line-split produced "/cache/we" — an existing parent directory.
    expect(r.map((e) => e.path)).not.toContain("/cache/we");
  });

  test("a pathname whose embedded line mimics a record header is not mis-split", () => {
    const r = parseStatRecords("1700000000 10 /cache/a\n99 88 still-the-same-path\n1700000001 20 /cache/b\n");
    expect(r.length).toBe(3);
    expect(r[1].path).toBe("still-the-same-path");
  });

  test("empty output is no entries, not a bogus record", () => {
    expect(parseStatRecords("")).toEqual([]);
    expect(parseStatRecords("\n")).toEqual([]);
  });

  test("a record with no trailing newline is still read", () => {
    expect(parseStatRecords("1700000000 5 /only").map((e) => e.path)).toEqual(["/only"]);
  });

  test("paths with spaces are preserved whole", () => {
    expect(parseStatRecords("1700000000 5 /Application Support/x y\n")[0].path).toBe("/Application Support/x y");
  });
});

describe("parseLimit", () => {
  test("absent or empty falls back to the default", () => {
    expect(parseLimit("CAP", undefined, 20)).toEqual({ ok: true, value: 20 });
    expect(parseLimit("CAP", "", 20)).toEqual({ ok: true, value: 20 });
  });

  test("a valid number is used", () => {
    expect(parseLimit("CAP", "40", 20)).toEqual({ ok: true, value: 40 });
    expect(parseLimit("CAP", "0", 20)).toEqual({ ok: true, value: 0 });
  });

  test("a negative cap is refused — it would make every entry over budget and evict the whole cache", () => {
    const r = parseLimit("CAP", "-1", 20);
    expect(r.ok).toBe(false);
  });

  test("a non-numeric cap is refused — NaN silently disables the cap instead of applying it", () => {
    expect(parseLimit("CAP", "twenty", 20).ok).toBe(false);
    expect(parseLimit("CAP", "20GiB", 20).ok).toBe(false);
  });

  test("Infinity is refused rather than treated as no cap", () => {
    expect(parseLimit("CAP", "Infinity", 20).ok).toBe(false);
  });

  test("the refusal names the variable and the value so the operator can fix it", () => {
    const r = parseLimit("SSD_GUARDIAN_GO_CACHE_CAP_GIB", "-5", 20);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toContain("SSD_GUARDIAN_GO_CACHE_CAP_GIB");
  });
});
