#!/usr/bin/env bun
import { spawnSync } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  rmSync,
  statSync,
} from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join, resolve } from "node:path";

type Config = {
  compoundBin: string;
  compoundHome: string;
  backupLocalDir: string;
  backupRemote?: string;
  keepLocal: number;
};

type RunResult = {
  stdout: string;
  stderr: string;
};

function usage(): never {
  console.log(`usage: bun tools/compound_home_backup.ts [--drill] [--help]

Environment:
  COMPOUND_BIN       compound binary (default: compound)
  COMPOUND_HOME      home to back up (default: ~/.compound)
  BACKUP_LOCAL_DIR   local archive directory (default: ~/.compound-backups)
  BACKUP_REMOTE      optional rsync target, for example host:/path
  KEEP_LOCAL         local archives to keep after pruning (default: 7)

Normal flow: compound home backup --target <archive> -> compound home verify <archive> -> optional rsync -> local prune.
Drill flow: restore newest local archive into a scratch COMPOUND_HOME, then run compound home doctor and compound memory doctor --json.`);
  process.exit(0);
}

function parseArgs(argv: string[]): { drill: boolean } {
  let drill = false;
  for (const arg of argv) {
    if (arg === "--help" || arg === "-h") usage();
    if (arg === "--drill") {
      drill = true;
      continue;
    }
    throw new Error(`unknown argument ${arg}`);
  }
  return { drill };
}

function expandHome(path: string): string {
  if (path === "~") return homedir();
  if (path.startsWith("~/")) return join(homedir(), path.slice(2));
  return path;
}

function loadConfig(env: NodeJS.ProcessEnv): Config {
  const keepLocal = Number.parseInt(env.KEEP_LOCAL ?? "7", 10);
  if (!Number.isFinite(keepLocal) || keepLocal < 0) {
    throw new Error(`KEEP_LOCAL must be a non-negative integer, got ${env.KEEP_LOCAL}`);
  }
  return {
    compoundBin: env.COMPOUND_BIN || "compound",
    compoundHome: resolve(expandHome(env.COMPOUND_HOME || "~/.compound")),
    backupLocalDir: resolve(expandHome(env.BACKUP_LOCAL_DIR || "~/.compound-backups")),
    backupRemote: env.BACKUP_REMOTE?.trim() || undefined,
    keepLocal,
  };
}

function childEnv(cfg: Config, extra: Record<string, string> = {}): NodeJS.ProcessEnv {
  return {
    ...process.env,
    COMPOUND_HOME: cfg.compoundHome,
    ...extra,
  };
}

function run(command: string, args: string[], env: NodeJS.ProcessEnv): RunResult {
  const result = spawnSync(command, args, { encoding: "utf8", env });
  const stdout = result.stdout ?? "";
  const stderr = result.stderr ?? "";
  if (result.error) {
    throw new Error(`${command} ${args.join(" ")} failed: ${result.error.message}`);
  }
  if (result.status !== 0) {
    const output = [stdout.trim(), stderr.trim()].filter(Boolean).join("\n");
    throw new Error(`${command} ${args.join(" ")} exited ${result.status}${output ? `\n${output}` : ""}`);
  }
  return { stdout, stderr };
}

function isoForFilename(date = new Date()): string {
  return date.toISOString().replace(/\.\d{3}Z$/, "Z");
}

function archiveEntries(dir: string): { path: string; mtimeMs: number }[] {
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { withFileTypes: true })
    .filter((entry) => entry.isFile() && isHomeArchiveName(entry.name))
    .map((entry) => {
      const path = join(dir, entry.name);
      return { path, mtimeMs: statSync(path).mtimeMs };
    })
    .sort((a, b) => b.mtimeMs - a.mtimeMs);
}

function isHomeArchiveName(name: string): boolean {
  return (name.startsWith("home-") || name.startsWith("compound-home-")) && name.endsWith(".tar.gz") && !name.includes(".tmp");
}

function newestArchive(dir: string): string {
  const [newest] = archiveEntries(dir);
  if (!newest) throw new Error(`no local home archives found in ${dir}`);
  return newest.path;
}

function pruneLocal(dir: string, keep: number): string[] {
  const entries = archiveEntries(dir);
  const removed: string[] = [];
  for (const entry of entries.slice(keep).reverse()) {
    rmSync(entry.path);
    removed.push(entry.path);
  }
  return removed;
}

function parseMetric(output: string, name: string): number | undefined {
  const re = new RegExp(`^- ${name}:\\s*(\\d+)\\s*$`, "m");
  const match = output.match(re);
  return match ? Number.parseInt(match[1], 10) : undefined;
}

function countFiles(root: string): number {
  let count = 0;
  for (const entry of readdirSync(root, { withFileTypes: true })) {
    const path = join(root, entry.name);
    if (entry.isDirectory()) count += countFiles(path);
    if (entry.isFile()) count += 1;
  }
  return count;
}

function backup(cfg: Config): void {
  mkdirSync(cfg.backupLocalDir, { recursive: true });
  const archive = join(cfg.backupLocalDir, `home-${isoForFilename()}-${process.pid}-${Math.random().toString(16).slice(2)}.tar.gz`);
  run(cfg.compoundBin, ["home", "backup", "--target", archive], childEnv(cfg));
  const verify = run(cfg.compoundBin, ["home", "verify", archive], childEnv(cfg));
  const files = parseMetric(verify.stdout, "files");
  let remote = "skipped";
  let remoteUploaded = false;
  if (cfg.backupRemote) {
    const target = cfg.backupRemote.endsWith("/") ? cfg.backupRemote : `${cfg.backupRemote}/`;
    run("rsync", ["-a", archive, target], process.env);
    remote = target;
    remoteUploaded = true;
  } else if (cfg.keepLocal === 0) {
    throw new Error("KEEP_LOCAL=0 requires BACKUP_REMOTE; refusing to prune the last local verified copy");
  }
  const bytes = statSync(archive).size;
  const safeKeep = Math.max(cfg.keepLocal, 1);
  const pruned = remoteUploaded ? pruneLocal(cfg.backupLocalDir, cfg.keepLocal) : pruneLocal(cfg.backupLocalDir, safeKeep);
  console.log(
    `compound_home_backup status=ok archive=${archive} bytes=${bytes} files=${files ?? "unknown"} verified=true remote=${remote} pruned=${pruned.length}`,
  );
}

function drill(cfg: Config): void {
  const archive = newestArchive(cfg.backupLocalDir);
  const verify = run(cfg.compoundBin, ["home", "verify", archive], childEnv(cfg));
  const archiveFiles = parseMetric(verify.stdout, "files");
  const scratch = mkdtempSync(join(tmpdir(), "compound-home-drill-"));
  try {
    run(cfg.compoundBin, ["home", "restore", archive, "--target", scratch], childEnv(cfg));
    const restoredFiles = countFiles(scratch);
    const countsMatch = archiveFiles === undefined || archiveFiles === restoredFiles;
    const drillEnv = childEnv(cfg, { COMPOUND_HOME: scratch });
    run(cfg.compoundBin, ["home", "doctor"], drillEnv);
    const memory = run(cfg.compoundBin, ["memory", "doctor", "--json"], drillEnv);
    JSON.parse(memory.stdout);
    if (!countsMatch) {
      throw new Error(`restored file count mismatch: archive_files=${archiveFiles} restored_files=${restoredFiles}`);
    }
    console.log(
      `compound_home_backup_drill status=ok archive=${archive} scratch=${scratch} archive_files=${archiveFiles ?? "unknown"} restored_files=${restoredFiles} counts_match=${countsMatch} home_doctor=ok memory_doctor=ok`,
    );
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

try {
  const args = parseArgs(process.argv.slice(2));
  const cfg = loadConfig(process.env);
  if (args.drill) drill(cfg);
  else backup(cfg);
} catch (err) {
  const message = err instanceof Error ? err.message : String(err);
  console.error(`compound_home_backup status=fail error=${message}`);
  process.exit(1);
}
