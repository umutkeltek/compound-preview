import {
  mkdirSync,
  mkdtempSync,
  rmSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, test } from "bun:test";
import {
  decideGuardAction,
  isRebuildableTemporaryGoCache,
  isRebuildableTemporaryTree,
  parseDeletedOpenFiles,
  parseLoadedModels,
  parseElapsedSeconds,
  temporaryGoCaches,
  type DeletedOpenFile,
  type ModelProcess,
} from "./disk_pressure_guard";

const GIB = 1024 ** 3;
const NOW_MS = 1_800_000_000_000;
const giant: DeletedOpenFile = {
  pid: 92740,
  command: "node",
  fd: "17u",
  sizeBytes: 24_223_629_225,
  path: "/private/tmp/deleted-model",
};
const idleModel: ModelProcess = {
  identifier: "model-local",
  status: "idle",
  ttlSeconds: null,
  lastUsedAtMs: NOW_MS - 7200 * 1000,
};

describe("deleted-open file parser", () => {
  test("retains process identity across multiple file records", () => {
    const rows = parseDeletedOpenFiles([
      "p92740",
      "cnode",
      "f17u",
      "s24223629225",
      "n/private/tmp/deleted-model",
      "f18u",
      "s1024",
      "n/private/tmp/small",
      "p42",
      "cother",
      "f3u",
      "s9000",
      "n/private/tmp/other",
    ].join("\n"));
    expect(rows).toEqual([
      giant,
      {
        pid: 92740,
        command: "node",
        fd: "18u",
        sizeBytes: 1024,
        path: "/private/tmp/small",
      },
      {
        pid: 42,
        command: "other",
        fd: "3u",
        sizeBytes: 9000,
        path: "/private/tmp/other",
      },
    ]);
  });

  test("parses LM Studio TTL and last-use evidence", () => {
    expect(parseLoadedModels(JSON.stringify([{
      identifier: "ssd-guard-test",
      status: "IDLE",
      ttlMs: 60_000,
      lastUsedTime: NOW_MS,
    }]))).toEqual([{
      identifier: "ssd-guard-test",
      status: "idle",
      ttlSeconds: 60,
      lastUsedAtMs: NOW_MS,
    }]);
  });
});

describe("guard decision", () => {
  const base = {
    freeBytes: 90 * GIB,
    freeFloorBytes: 140 * GIB,
    giantFiles: [giant],
    lmStudioHolderPids: new Set([giant.pid]),
    holderAgesSeconds: new Map([[giant.pid, 7200]]),
    models: [idleModel],
    hasActiveClients: false,
    minimumIdleSeconds: 1800,
    nowMs: NOW_MS,
  };

  test("defers even a proven old idle LM Studio holder", () => {
    expect(decideGuardAction(base).action).toBe("report_only");
  });

  test("does nothing when free space is healthy", () => {
    expect(decideGuardAction({ ...base, freeBytes: 150 * GIB }).action).toBe("none");
  });

  test("refuses when a local client is connected", () => {
    expect(decideGuardAction({ ...base, hasActiveClients: true }).action).toBe("report_only");
  });

  test("refuses a generic giant deleted-file holder", () => {
    expect(decideGuardAction({ ...base, lmStudioHolderPids: new Set() }).action).toBe("report_only");
  });

  test("refuses an active or recent model", () => {
    expect(decideGuardAction({
      ...base,
      models: [{ ...idleModel, status: "generating" }],
    }).action).toBe("report_only");
    expect(decideGuardAction({
      ...base,
      holderAgesSeconds: new Map([[giant.pid, 60]]),
    }).action).toBe("report_only");
    expect(decideGuardAction({
      ...base,
      models: [{ ...idleModel, lastUsedAtMs: NOW_MS - 60 * 1000 }],
    }).action).toBe("report_only");
  });
});

describe("elapsed process age", () => {
  test("parses macOS ps elapsed formats", () => {
    expect(parseElapsedSeconds("17:08")).toBe(1028);
    expect(parseElapsedSeconds("02:17:08")).toBe(8228);
    expect(parseElapsedSeconds("2-03:17:08")).toBe(184628);
    expect(parseElapsedSeconds("not-a-duration")).toBeNull();
  });
});

describe("temporary Go cache selection", () => {
  test("matches only known rebuildable cache root names", () => {
    expect(isRebuildableTemporaryGoCache("gocache")).toBe(true);
    expect(isRebuildableTemporaryGoCache("gocache-proof-7")).toBe(true);
    expect(isRebuildableTemporaryGoCache("go-cache")).toBe(true);
    expect(isRebuildableTemporaryGoCache("go-cache-project")).toBe(false);
    expect(isRebuildableTemporaryGoCache("compound-copy")).toBe(false);
    expect(isRebuildableTemporaryGoCache("source")).toBe(false);
  });

  test("matches only explicit rebuildable macOS temp-tree families", () => {
    expect(isRebuildableTemporaryTree("agent-session-backup-deadbeef")).toBe(false);
    expect(isRebuildableTemporaryTree("compound-proofcheck-sealed-123")).toBe(false);
    expect(isRebuildableTemporaryTree("go-build123456")).toBe(false);
    expect(isRebuildableTemporaryTree("switchback-tap-bodylog-http-123")).toBe(false);
    expect(isRebuildableTemporaryTree("switchback-forward-proxy-https-123")).toBe(false);
    expect(isRebuildableTemporaryTree("pytest-of-umut")).toBe(false);
    expect(isRebuildableTemporaryTree("agent-session-backups-important")).toBe(false);
    expect(isRebuildableTemporaryTree("compound-copy")).toBe(false);
  });

  test("selects only old real directories and never symlinks or user trees", () => {
    const root = mkdtempSync(join(tmpdir(), "ssd-guardian-cache-test-"));
    try {
      const oldCache = join(root, "gocache-old");
      const freshCache = join(root, "gocache-fresh");
      const staleBackup = join(root, "agent-session-backup-stale");
      const freshBackup = join(root, "agent-session-backup-fresh");
      const userTree = join(root, "compound-copy");
      const unmarkedCache = join(root, "gocache-user-source");
      mkdirSync(oldCache);
      mkdirSync(freshCache);
      mkdirSync(staleBackup);
      mkdirSync(freshBackup);
      writeFileSync(join(staleBackup, "manifest.json"), "{}\n");
      mkdirSync(userTree);
      mkdirSync(unmarkedCache);
      writeFileSync(
        join(oldCache, "README"),
        "This directory holds cached build artifacts from the Go build system.\n",
      );
      const nowMs = Date.now();
      const old = new Date(nowMs - 8 * 60 * 60 * 1000);
      utimesSync(oldCache, old, old);
      utimesSync(staleBackup, old, old);
      utimesSync(unmarkedCache, old, old);
      symlinkSync(oldCache, join(root, "gocache-link"));

      const candidates = temporaryGoCaches(root, 60 * 60, nowMs);
      expect(candidates).toHaveLength(1);
      expect(candidates.map((candidate) => candidate.path).sort()).toEqual([
        oldCache,
      ].sort());
      expect(candidates.every((candidate) => candidate.ageSeconds === 8 * 60 * 60)).toBe(true);
      expect(candidates.every((candidate) => candidate.sizeBytes > 0)).toBe(true);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
