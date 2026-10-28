#!/usr/bin/env bun
import {
  lstatSync,
  readFileSync,
  readdirSync,
} from "node:fs";
import { basename, join } from "node:path";
import { spawnSync } from "node:child_process";
import { flags, limit, measure, capacity, emit, DEFER_REASON } from "./report_only";

const GIB = 1024 ** 3;
const DEFAULT_FREE_FLOOR_BYTES = 140 * GIB;
const DEFAULT_GIANT_FILE_BYTES = 8 * GIB;
const DEFAULT_MIN_IDLE_SECONDS = 30 * 60;
const DEFAULT_TEMP_CACHE_MIN_AGE_SECONDS = 60 * 60;
const DEFAULT_TEMP_ROOT = "/private/tmp";
const GO_BUILD_CACHE_MARKER = "This directory holds cached build artifacts from the Go build system.";
export type DeletedOpenFile = {
  pid: number;
  command: string;
  fd: string;
  sizeBytes: number;
  path: string;
};

export type ModelProcess = {
  identifier: string;
  status: string;
  ttlSeconds: number | null;
  lastUsedAtMs: number | null;
};

export type GuardDecision = {
  action: "none" | "report_only";
  reasons: string[];
};

export type TemporaryCacheCandidate = {
  path: string;
  sizeBytes: number;
  ageSeconds: number;
};

export type TemporaryCacheAction = TemporaryCacheCandidate & {
  status: "deferred";
  reason: string;
  error?: string;
};

type CommandResult = {
  ok: boolean;
  stdout: string;
  stderr: string;
};

function run(command: string[], timeoutMs = 30_000): CommandResult {
  const result = spawnSync(command[0], command.slice(1), {
    encoding: "utf8",
    timeout: timeoutMs,
    maxBuffer: 32 * 1024 * 1024,
  });
  return {
    ok: result.status === 0,
    stdout: result.stdout ?? "",
    stderr: result.stderr ?? "",
  };
}

export function isRebuildableTemporaryGoCache(name: string): boolean {
  return name === "go-cache"
    || name === "gocache"
    || name.startsWith("gocache-");
}

export function isRebuildableTemporaryTree(name: string): boolean {
  // Candidate discovery only. Names/markers never prove every nested payload disposable.
  return isRebuildableTemporaryGoCache(name);
}

function hasGoBuildCacheMarker(path: string): boolean {
  try {
    return readFileSync(join(path, "README"), "utf8").includes(GO_BUILD_CACHE_MARKER);
  } catch {
    return false;
  }
}

export function temporaryGoCaches(
  root: string,
  minimumAgeSeconds: number,
  nowMs: number,
): TemporaryCacheCandidate[] {
  const candidates: TemporaryCacheCandidate[] = [];
  let entries;
  try {
    entries = readdirSync(root, { withFileTypes: true });
  } catch (error: any) {
    if (error?.code === "ENOENT") return candidates;
    throw error;
  }
  if (entries.length > 4096) throw new Error("temporary entry bound exceeded");
  const deadline = Date.now() + 45_000;
  for (const entry of entries) {
    if (!entry.isDirectory() || entry.isSymbolicLink() || !isRebuildableTemporaryTree(entry.name)) {
      continue;
    }
    const path = join(root, entry.name);
    try {
      const stat = lstatSync(path);
      const ageSeconds = Math.floor((nowMs - stat.mtimeMs) / 1000);
      if (
        !stat.isDirectory()
        || stat.isSymbolicLink()
        || ageSeconds < minimumAgeSeconds
        || (isRebuildableTemporaryGoCache(entry.name) && !hasGoBuildCacheMarker(path))
      ) {
        continue;
      }
      const observation = measure(path, new Set(), deadline);
      if (observation.status === "failed" || observation.size_bytes === null) throw new Error(observation.note ?? "temporary measurement failed");
      candidates.push({ path, sizeBytes: observation.size_bytes, ageSeconds });
    } catch (error: any) {
      if (error?.code !== "ENOENT") throw error;
    }
  }
  return candidates.sort((a, b) => b.sizeBytes - a.sizeBytes);
}

export function parseDeletedOpenFiles(raw: string): DeletedOpenFile[] {
  const files: DeletedOpenFile[] = [];
  let pid = 0;
  let command = "";
  let current: Partial<DeletedOpenFile> | null = null;

  const flush = () => {
    if (
      current?.fd !== undefined
      && current.sizeBytes !== undefined
      && current.path !== undefined
      && pid > 0
    ) {
      files.push({
        pid,
        command,
        fd: current.fd,
        sizeBytes: current.sizeBytes,
        path: current.path,
      });
    }
    current = null;
  };

  for (const line of raw.split(/\r?\n/)) {
    if (!line) continue;
    const field = line[0];
    const value = line.slice(1);
    if (field === "p") {
      flush();
      pid = Number(value);
      command = "";
    } else if (field === "c") {
      command = value;
    } else if (field === "f") {
      flush();
      current = { fd: value };
    } else if (field === "s" && current) {
      current.sizeBytes = Number(value);
    } else if (field === "n" && current) {
      current.path = value;
    }
  }
  flush();
  return files;
}

export function parseElapsedSeconds(value: string): number | null {
  const match = value.trim().match(/^(?:(\d+)-)?(?:(\d+):)?(\d+):(\d+)$/);
  if (!match) return null;
  const days = Number(match[1] ?? 0);
  const hours = Number(match[2] ?? 0);
  const minutes = Number(match[3]);
  const seconds = Number(match[4]);
  return (((days * 24) + hours) * 60 + minutes) * 60 + seconds;
}

export function decideGuardAction(input: {
  freeBytes: number;
  freeFloorBytes: number;
  giantFiles: DeletedOpenFile[];
  lmStudioHolderPids: Set<number>;
  holderAgesSeconds: Map<number, number>;
  models: ModelProcess[];
  hasActiveClients: boolean;
  minimumIdleSeconds: number;
  nowMs: number;
}): GuardDecision {
  const reasons: string[] = [];
  if (input.freeBytes >= input.freeFloorBytes) {
    return { action: "none", reasons: ["free_space_healthy"] };
  }
  reasons.push("free_space_below_floor");

  const lmStudioGiants = input.giantFiles.filter((file) =>
    input.lmStudioHolderPids.has(file.pid)
  );
  if (lmStudioGiants.length === 0) {
    return { action: "report_only", reasons: [...reasons, "no_lmstudio_giant_deleted_file"] };
  }
  reasons.push("lmstudio_holds_giant_deleted_file");

  if (input.hasActiveClients) {
    return { action: "report_only", reasons: [...reasons, "active_local_model_clients"] };
  }
  const holderOldEnough = lmStudioGiants.every(
    (file) => (input.holderAgesSeconds.get(file.pid) ?? 0) >= input.minimumIdleSeconds,
  );
  if (!holderOldEnough) {
    return { action: "report_only", reasons: [...reasons, "holder_too_recent"] };
  }
  if (input.models.length === 0 || input.models.some((model) => model.status !== "idle")) {
    return { action: "report_only", reasons: [...reasons, "models_not_proven_idle"] };
  }
  const modelsOldEnough = input.models.every((model) =>
    model.lastUsedAtMs !== null
    && input.nowMs - model.lastUsedAtMs >= input.minimumIdleSeconds * 1000
  );
  if (!modelsOldEnough) {
    return { action: "report_only", reasons: [...reasons, "model_too_recent_or_unknown"] };
  }
  return { action: "report_only", reasons: [...reasons, "owner_review_required_no_model_unload"] };
}

function macOSUserTemporaryRoot(): string {
  const result = run(["getconf", "DARWIN_USER_TEMP_DIR"]);
  const path = result.stdout.trim();
  return result.ok && (
      path.startsWith("/private/var/folders/")
      || path.startsWith("/var/folders/")
    )
    ? path
    : DEFAULT_TEMP_ROOT;
}

export function parseLoadedModels(raw: string): ModelProcess[] {
  try {
    const parsed = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.flatMap((row): ModelProcess[] => {
      const identifier = row.identifier ?? row.modelIdentifier ?? row.modelKey;
      if (typeof identifier !== "string" || identifier.length === 0) return [];
      return [{
        identifier,
        status: String(row.status ?? "unknown").toLowerCase(),
        ttlSeconds: typeof row.ttlMs === "number"
          ? Math.floor(row.ttlMs / 1000)
          : typeof row.ttl === "number"
            ? row.ttl
            : typeof row.ttlSeconds === "number" ? row.ttlSeconds : null,
        lastUsedAtMs: typeof row.lastUsedTime === "number" ? row.lastUsedTime : null,
      }];
    });
  } catch {
    return [];
  }
}

async function main(): Promise<void> {
  const args = flags("disk_pressure_guard.ts");
  const freeFloorBytes = limit(process.env.SSD_GUARDIAN_FREE_FLOOR_GIB, 140) * GIB;
  const giantThresholdBytes = limit(process.env.SSD_GUARDIAN_GIANT_FILE_GIB, 8) * GIB;
  const age = limit(process.env.SSD_GUARDIAN_TEMP_CACHE_MIN_AGE_SECONDS, DEFAULT_TEMP_CACHE_MIN_AGE_SECONDS);
  const errors: string[] = [];
  let beforeFreeBytes: number | null = null;
  try { beforeFreeBytes = capacity(); } catch (error) { errors.push(String(error)); }
  const root = process.env.SSD_GUARDIAN_TEMP_ROOT ?? macOSUserTemporaryRoot();
  let candidates: TemporaryCacheCandidate[] = [];
  try { if (beforeFreeBytes !== null && beforeFreeBytes < freeFloorBytes) candidates = temporaryGoCaches(root, age, Date.now()); }
  catch (error) { errors.push(String(error)); }
  const temporaryCacheActions: TemporaryCacheAction[] = candidates.map((candidate) => ({ ...candidate, status: "deferred", reason: DEFER_REASON }));
  const lsof = run(["lsof", "+L1", "-F", "pcfsn"], 30_000);
  if (!lsof.ok) errors.push("deleted-open-file inventory incomplete");
  const giantFiles = parseDeletedOpenFiles(lsof.stdout).filter((file) => file.sizeBytes >= giantThresholdBytes);
  let afterFreeBytes: number | null = null;
  try { afterFreeBytes = capacity(); } catch (error) { errors.push(String(error)); }
  const belowFloor = afterFreeBytes !== null && afterFreeBytes < freeFloorBytes;
  emit("pressure", { schema_version: "compound.ssd-guardian.pressure.v2", generated_at: new Date().toISOString(),
    mode: args.apply ? "apply" : "dry_run", beforeFreeBytes, afterFreeBytes,
    reclaimedBytes: 0, observedFreeChangeBytes: afterFreeBytes === null || beforeFreeBytes === null ? null : afterFreeBytes - beforeFreeBytes,
    freeChangeAttribution: "concurrent volume change; guardian performed no reclaim",
    thresholds: { freeFloorBytes, giantThresholdBytes, temporaryCacheMinimumAgeSeconds: age },
    temporaryCacheCandidates: candidates, temporaryCacheActions,
    giantDeletedFiles: giantFiles.slice(0, 20).map((file) => ({ pid: file.pid, command: file.command, fd: file.fd, sizeBytes: file.sizeBytes, name: basename(file.path) })),
    decision: { action: afterFreeBytes === null || belowFloor ? "report_only" : "none", reasons: afterFreeBytes === null ? ["capacity_unknown", DEFER_REASON] : belowFloor ? ["free_space_below_floor", DEFER_REASON] : ["free_space_healthy"] },
    actions: [], model_unload: "deferred", errors,
  }, args.apply, errors.length > 0 || belowFloor, candidates.length > 0);
}
if (import.meta.main) await main();
