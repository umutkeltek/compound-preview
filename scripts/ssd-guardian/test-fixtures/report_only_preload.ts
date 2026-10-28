// Synthetic child only: deny every unlisted process and every data mutation.
import { mock } from "bun:test";
import * as fs from "node:fs";
import * as cp from "node:child_process";
import * as os from "node:os";
import { join, resolve, relative, isAbsolute, basename } from "node:path";
const root = fs.realpathSync(process.env.GUARDIAN_FIXTURE_ROOT!);
if (!basename(root).startsWith("guardian-report-fixture-")) throw new Error("owned fixture required");
const reports = join(root, "reports"), trace = join(root, "trace.jsonl"), source = resolve(import.meta.dir, "..");
const scenario = JSON.parse(process.env.GUARDIAN_FIXTURE_SCENARIO ?? "{}");
const realFs = { ...fs };
const realAppend = fs.appendFileSync, realSpawn = cp.spawnSync;
const record = (event: object) => realAppend(trace, JSON.stringify(event) + "\n");
function within(base: string, path: any) { const rel = relative(base, resolve(String(path))); return !isAbsolute(rel) && rel !== ".." && !rel.startsWith("../"); }
function readPath(path: any) { if (!within(root, path) && !within(source, path)) forbidden("read outside fixture", String(path)); }
function forbidden(operation: string, target: any): never { record({ forbidden: operation, target: String(target) }); throw new Error(`forbidden fixture effect: ${operation}`); }
const spy: any = { ...fs };
for (const name of ["rmSync", "unlinkSync", "rmdirSync", "renameSync", "copyFileSync", "cpSync", "symlinkSync", "linkSync", "truncateSync", "chmodSync", "chownSync", "utimesSync", "writeSync", "appendFileSync"])
  spy[name] = (...args: any[]) => forbidden(name, args[0]);
for (const name of ["lstatSync", "statSync", "realpathSync", "readdirSync", "readlinkSync", "existsSync", "readFileSync"])
  spy[name] = (path: any, ...rest: any[]) => { readPath(path); return (realFs as any)[name](path, ...rest); };
spy.mkdirSync = (path: any, options: any) => { if (!within(reports, path)) forbidden("mkdirSync", path); record({ report_write: "mkdir", path }); return realFs.mkdirSync(path, options); };
spy.writeFileSync = (path: any, data: any, options: any) => { if (!within(reports, path) || options?.flag !== "wx") forbidden("writeFileSync", path); record({ report_write: "exclusive-create", path }); if (scenario.publish_error) throw Object.assign(new Error(scenario.publish_error), { code: scenario.publish_error }); return realFs.writeFileSync(path, data, options); };
spy.openSync = (path: any, flags: any, ...rest: any[]) => {
  readPath(path);
  if (typeof flags === "string" ? flags !== "r" : flags & (fs.constants.O_WRONLY | fs.constants.O_RDWR | fs.constants.O_CREAT | fs.constants.O_TRUNC | fs.constants.O_APPEND)) forbidden("open for write", path);
  return (realFs.openSync as any)(path, flags, ...rest);
};
mock.module("node:fs", () => spy);
mock.module("node:os", () => ({ ...os, homedir: () => root, tmpdir: () => join(root, "temp") }));
mock.module("bun:sqlite", () => ({ Database: class { constructor(path: string) { forbidden("sqlite open", path); } } }));
function result(stdout = "", status = 0) { return { status, stdout, stderr: "", error: undefined }; }
let dfCount = 0;
mock.module("node:child_process", () => ({ ...cp,
  spawnSync(command: string, args: string[], options: any = {}) {
    record({ command, args });
    if (["/bin/df", "df"].includes(command)) {
      dfCount++; if (scenario.df_error) return result("invalid capacity");
      const available = scenario.low_disk ? (dfCount === 1 ? 1000 : 2000) : 500 * 1024 * 1024;
      return result(`Filesystem 1024-blocks Used Available Capacity\nfixture ${2_000_000_000} ${1_000_000_000} ${available} 50%\n`);
    }
    if (command === "getconf" && args[0] === "DARWIN_USER_TEMP_DIR") return result(join(root, "temp") + "\n");
    if (command === "lsof") return result(`p424242\ncLM Studio\nf17\ns34359738368\nn${root}/temp/deleted-model\n`);
    if (["/usr/bin/du", "du", "/usr/bin/find"].includes(command)) {
      const target = command.endsWith("find") ? args[1] : args.at(-1)!;
      if (!within(root, target)) forbidden("external scan", target);
      if (args.some((a) => ["-delete", "-exec", "-execdir", "-ok", "-fprint", "-fprintf"].includes(a))) forbidden("mutating find", args);
      if (scenario.empty_measure && command.endsWith("du")) return result("");
      if (scenario.measure_error && command.endsWith("du")) return { ...result("", 1), stderr: "synthetic unavailable measurement" };
      return realSpawn(command, args, options);
    }
    if (command === process.execPath || command === "bun") {
      const script = args[0], name = basename(script);
      if (!within(source, script) || !["check_backup_freshness.ts", "fix_work_symlink_drift.ts", "sweep_build_caches.ts"].includes(name)) forbidden("unexpected child", script);
      if (args.includes("--apply")) forbidden("weekly forwarded apply", script);
      if (name === "check_backup_freshness.ts" && scenario.backup) {
        if (scenario.backup === "malformed") return result("not json");
        if (scenario.backup === "failure") return result("", 2);
        if (scenario.backup === "timeout") return { ...result("", null as any), error: new Error("synthetic timeout") };
        return result(JSON.stringify({ status: scenario.backup }));
      }
      if (scenario.malformed_child === name) return result("not json", scenario.child_exit ?? 0);
      if (scenario.child_error === name) return result("", 2);
      return realSpawn(process.execPath, ["--preload", import.meta.path, ...args], { ...options, env: process.env });
    }
    return forbidden("subprocess", command + " " + args.join(" "));
  },
  spawn: (...args: any[]) => forbidden("spawn", args[0]), exec: (...args: any[]) => forbidden("exec", args[0]),
  execSync: (...args: any[]) => forbidden("execSync", args[0]), execFileSync: (...args: any[]) => forbidden("execFileSync", args[0]),
}));
