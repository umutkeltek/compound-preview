import { mock } from "bun:test";
import * as nativeFs from "node:fs";
const originalReadFile = nativeFs.readFileSync;
const originalRead = nativeFs.readSync;
let boundedReads = 0;
let maximumRead = 0;
let wholeTranscriptReads = 0;
const largeTranscript = () => homedir() + "/native/large.jsonl";
mock.module("node:fs", () => ({
  ...nativeFs,
  readFileSync: (...args: any[]) => {
    if (args[0] === largeTranscript()) {
      wholeTranscriptReads++;
      throw new Error("whole growing transcript read");
    }
    return (originalReadFile as any)(...args);
  },
  readSync: (...args: any[]) => {
    boundedReads++;
    maximumRead = Math.max(maximumRead, args[3] ?? args[1].byteLength);
    return (originalRead as any)(...args);
  },
}));
const { default: compoundHook } = await import("../omp_hook_shim.ts");
import { mkdirSync, readFileSync, writeFileSync, rmSync, truncateSync } from "node:fs";
import { homedir } from "node:os";

type Fixture = { session_id: string; cwd: string; transcript_path?: string };
const handlers = new Map<string, (payload?: unknown, ctx?: any) => unknown>();
compoundHook({ on: (event, handler) => handlers.set(event, handler) });
const failures: string[] = [];
function check(ok: boolean, reason: string): void { if (!ok) failures.push(reason); }
function calls(): Fixture[] {
  return readFileSync(homedir() + "/fixture-calls.jsonl", "utf8").trim().split("\n").filter(Boolean).map((s) => JSON.parse(s));
}
async function emit(event: string, ctx?: any, payload: unknown = {}): Promise<any> {
  const handler = handlers.get(event);
  if (!handler) throw new Error("missing handler " + event);
  return await handler(payload, ctx);
}
function context(id: string, path: string | undefined, cwd = "/srv/projects/fixture"): any {
  return { cwd, sessionManager: { getSessionId: () => id, getSessionFile: () => path } };
}
function last(): Fixture { return calls().at(-1)!; }
function session(path: string, id: string, cwd: string): void {
  mkdirSync(path.slice(0, path.lastIndexOf("/")), { recursive: true });
  writeFileSync(path, JSON.stringify({ type: "session", version: 3, id, cwd }) + "\n");
}
const cwd = "/srv/projects/fixture";
const a = homedir() + "/native/a.jsonl";
const b = homedir() + "/native/b.jsonl";

// A native ID remains authoritative before its lazily created file exists.
await emit("session_start", context("native-a", a));
check(last().session_id === "native-a" && last().cwd === cwd, "lazy native identity/cwd lost");
check(last().transcript_path === undefined, "lazy native file invented a transcript");
session(a, "native-a", cwd);
await emit("before_agent_start", context("native-a", a), { prompt: "first" });
check(last().session_id === "native-a" && last().transcript_path === a, "late native file was not picked up");

// A workspace change can preserve a native ID but must not carry old context.
await emit("session_start", context("native-a", a));
const changedCwd = "/srv/projects/changed-fixture";
const moved: any = await emit("before_agent_start", context("native-a", a, changedCwd), { prompt: "moved" });
check(last().session_id === "native-a" && last().cwd === changedCwd && last().transcript_path === undefined, "boundary cwd was cached or old transcript reused");
check(moved?.message === "boundary-pack", "same-session cwd switch carried old workspace start context");

// Concurrent same-cwd sessions must never borrow the newest neighbouring file.
session(b, "native-b", cwd);
await emit("tool_call", context("native-a", a), { toolName: "Read", toolCallId: "a", input: {} });
check(last().session_id === "native-a" && last().transcript_path === a, "same-cwd neighbour replaced session a");
await emit("tool_result", context("native-b", b), { toolName: "Read", toolCallId: "b" });
check(last().session_id === "native-b" && last().transcript_path === b, "session b result used session a");

// The same callback context can switch/fork; no identity is cached at import.
let id = "native-a";
let file: string | undefined = a;
const mutable = { cwd, sessionManager: { getSessionId: () => id, getSessionFile: () => file } };
await emit("session_start", mutable);
id = "native-fork";
file = homedir() + "/native/lazy-fork.jsonl";
const switched: any = await emit("before_agent_start", mutable, { prompt: "switched" });
check(last().session_id === id && last().transcript_path === undefined, "switch/fork retained old native identity/path");
check(switched?.message === "boundary-pack", "switch/fork carried another session's start context");
await emit("session_shutdown", mutable);
check(last().session_id === id, "shutdown retained the original session");

// A context whose native manager is not ready must not scan another session.
const unready = context("", undefined);
await emit("session_start", unready);
check(last().transcript_path === undefined, "unready native manager borrowed a transcript");
const unreadyId = last().session_id;
await emit("before_agent_start", unready, { prompt: "still unready" });
check(last().session_id === unreadyId && last().transcript_path === undefined, "unready native fallback changed between boundaries");
unready.sessionManager.getSessionId = () => "native-ready";
await emit("before_agent_start", unready, { prompt: "ready" });
check(last().session_id === "native-ready", "late native initialization remained synthetic");

// An explicit path with another header is not a transcript for this native ID.
await emit("tool_call", context("native-a", b), { toolName: "Read" });
check(last().session_id === "native-a" && last().transcript_path === undefined, "wrong native transcript header was borrowed");

// A growing transcript must be header-checked with bounded I/O, not read then sliced.
session(largeTranscript(), "native-large", cwd);
truncateSync(largeTranscript(), 16 * 1024 * 1024);
const readsBefore = boundedReads;
await emit("tool_call", context("native-large", largeTranscript()), { toolName: "Read" });
check(last().session_id === "native-large" && last().transcript_path === largeTranscript(), "large native transcript was not validated");
check(wholeTranscriptReads === 0 && boundedReads > readsBefore && maximumRead <= 4096, "native header validation did not bound reads before allocation");

// Compatibility callers without native context still discover the matching legacy file.
const legacyCwd = process.cwd();
const slug = "--" + legacyCwd.replace(/^\/+/, "").replace(/\//g, "-").replace(/[^A-Za-z0-9._-]/g, "-") + "--";
const legacy = homedir() + "/.omp/agent/sessions/" + slug + "/legacy.jsonl";
session(legacy, "legacy-only", legacyCwd);
await emit("tool_call", { cwd: legacyCwd, sessionManager: {} }, { toolName: "Read" });
check(last().session_id !== "legacy-only" && last().transcript_path === undefined, "present unready native manager fell through to another disk session");
await emit("tool_call", undefined, { toolName: "Read" });
check(last().session_id === "legacy-only" && last().transcript_path === legacy, "no-context compatibility discovery lost");
rmSync(legacy);
await emit("tool_call", undefined, { toolName: "Read" });
const fallback = last().session_id;
await emit("tool_result", undefined, { toolName: "Read" });
check(last().session_id === fallback && last().transcript_path === undefined, "no-context fallback identity is not stable");
if (failures.length) throw new Error(failures.join("\n"));
