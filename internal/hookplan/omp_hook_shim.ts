// compound-owned: oh-my-pi-hook-shim@2
/**
 * compound ↔ oh-my-pi hook shim (installed by `compound hook install --harness
 * oh-my-pi` into <agentDir>/hooks/pre/compound.ts).
 *
 * omp discovers TS hook modules under <agentDir>/hooks/{pre,post}/ and wires
 * them into the HookRunner (session_start, before_agent_start, tool_call,
 * tool_result, session_shutdown). This shim makes compound the meaning spine
 * for omp sessions, matching what compound hook install does for claude-code:
 *
 *   - session_start → `compound hook session-start --live`; its context (summary
 *     + door map) is held and prepended to the first before_agent_start message.
 *   - before_agent_start → `compound hook user-prompt-submit --live` and inject
 *     the returned ContextPack (memory + contract + capability offers) into the
 *     session as a persisted custom message.
 *   - tool_call → `compound hook pre-tool-use --live` (observation + policy);
 *     a policy deny blocks the tool with compound's reason.
 *   - session_shutdown → `compound hook session-end --live` so the session
 *     receipt lands in compound instead of being derived from exit.
 *
 * Lifecycle and context calls are observational and fail open. The tool
 * boundary is fail closed: a slow, absent, or malformed Compound response
 * blocks the tool without hanging the session because spawnSync has a hard
 * timeout. Governed headless profiles disable ambient extension discovery;
 * Switchback may load this exact owned shim explicitly.
 */
import { spawnSync } from "node:child_process";
import { existsSync, readdirSync, openSync, readSync, closeSync, statSync } from "node:fs";
import { homedir } from "node:os";

/** Structural slice of OMP's (event, ctx) API; no installed-package dependency. */
interface CompoundHookContext {
	cwd?: string;
	sessionManager?: {
		getSessionId?: () => unknown;
		getSessionFile?: () => unknown;
	};
}
interface CompoundHookApi {
	on(event: string, handler: (payload?: unknown, ctx?: CompoundHookContext) => unknown): void;
}

interface ToolCallPayload {
	toolName?: string;
	toolCallId?: string;
	input?: Record<string, unknown>;
}

interface ToolResultPayload {
	toolName?: string;
	toolCallId?: string;
}

interface BeforeAgentStartPayload {
	prompt?: string;
}

interface HookSpecificOutput {
	hookEventName?: string;
	additionalContext?: string;
	permissionDecision?: string;
	permissionDecisionReason?: string;
}

interface CompoundHookDocument {
	hookSpecificOutput?: HookSpecificOutput;
	permissionDecision?: string;
	permissionDecisionReason?: string;
}

const COMPOUND = `${homedir()}/.local/bin/compound`;
const HARNESS = "oh_my_pi";
const CALL_TIMEOUT_MS = 8_000;

// Legacy discovery is only for callers without OMP's native session API.
// Its Go-side compatibility mirror lives in omp_derive.go. Native contexts
// never scan neighbouring files: a session's own transcript may still be lazy.
// Same bound as OmpDeriveSession on the Go side (internal/hookplan/omp_derive.go):
// when a cwd slug holds thousands of stale session files, only header-read
// the newest OMP_DERIVE_MAX_PROBE by mtime. The contract still returns the
// single newest matching candidate; the bound just keeps the scan cost
// proportional to a small constant.
const OMP_DERIVE_MAX_PROBE = 64;

function deriveOmpSession(homedirValue: string, cwd: string): { sessionId: string; transcriptPath: string | null } {
	const slug = slugFromCwd(cwd);
	const dir = `${homedirValue}/.omp/agent/sessions/${slug}`;
	if (!existsSync(dir)) {
		return { sessionId: `omp-${process.pid}-${Date.now().toString(36)}`, transcriptPath: null };
	}
	// Pass 1: mtime-only pre-filter. Pick the newest OMP_DERIVE_MAX_PROBE
	// .jsonl files; stat cost is already paid by readdirSync on some
	// platforms but explicitly here so the bound holds regardless.
	let top: { path: string; mtime: number }[] = [];
	try {
		const entries = readdirSync(dir);
		for (const entry of entries) {
			if (!entry.endsWith(".jsonl")) continue;
			const full = `${dir}/${entry}`;
			let st: import("node:fs").Stats;
			try { st = statSync(full); } catch { continue; }
			if (top.length < OMP_DERIVE_MAX_PROBE) {
				top.push({ path: full, mtime: st.mtimeMs });
				continue;
			}
			let idx = 0;
			for (let i = 1; i < top.length; i++) {
				if (top[i].mtime < top[idx].mtime) idx = i;
			}
			if (top[idx].mtime < st.mtimeMs) top[idx] = { path: full, mtime: st.mtimeMs };
		}
	} catch {
		return { sessionId: `omp-${process.pid}-${Date.now().toString(36)}`, transcriptPath: null };
	}
	// Pass 2: header read on the bounded set only, picking the newest match.
	let newestPath: string | null = null;
	let newestMtime = 0;
	let newestEmbeddedId = "";
	for (const c of top) {
		let headerProbe: { id: string; cwd: string } | null;
		try { headerProbe = readSessionHeader(c.path); } catch { continue; }
		if (!headerProbe) continue;
		if (headerProbe.cwd !== cwd) continue;
		if (c.mtime <= newestMtime) continue;
		newestPath = c.path;
		newestMtime = c.mtime;
		newestEmbeddedId = headerProbe.id;
	}
	if (!newestPath) {
		return { sessionId: `omp-${process.pid}-${Date.now().toString(36)}`, transcriptPath: null };
	}
	const fallbackId = `omp-${process.pid}-${Date.now().toString(36)}`;
	return { sessionId: newestEmbeddedId || fallbackId, transcriptPath: newestPath };
}

function slugFromCwd(cwd: string): string {
	// omp's on-disk convention: strip the leading "/", replace every "/" with
	// "-", then wrap in "--". Synthetic example (privacy-law scrub):
	//   /srv/projects/sample                   -> --srv-projects-sample--
	let s = cwd.replace(/^\/+/, "").replace(/\//g, "-");
	s = s.replace(/[^A-Za-z0-9._-]/g, "-");
	return `--${s}--`;
}

function parseSessionHeader(line: string): { id: string; cwd: string } | null {
	try {
		const j = JSON.parse(line) as { type?: string; id?: string; cwd?: string };
		if (j.type !== "session" || typeof j.id !== "string" || typeof j.cwd !== "string") return null;
		return { id: j.id, cwd: j.cwd };
	} catch {
		return null;
	}
}

// Bound I/O before allocation; transcripts can grow throughout a session.
function readSessionHeader(path: string): { id: string; cwd: string } | null {
	const fd = openSync(path, "r");
	try {
		const buffer = Buffer.alloc(4_096);
		const size = readSync(fd, buffer, 0, buffer.length, 0);
		for (const line of buffer.toString("utf8", 0, size).split("\n")) {
			const header = parseSessionHeader(line);
			if (header) return header;
		}
		return null;
	} finally { closeSync(fd); }
}

type SessionFixture = { session_id: string; cwd: string; transcript_path?: string };
const fallbackSessionId = "omp-" + process.pid + "-" + Date.now().toString(36);

// Resolve every boundary: native initialization, switches, and forks can all
// happen after the hook module was loaded.
function sessionFixture(ctx?: CompoundHookContext): SessionFixture {
	const cwd = typeof ctx?.cwd === "string" && ctx.cwd.trim() ? ctx.cwd : process.cwd();
	const manager = ctx?.sessionManager;
	if (manager !== undefined) {
		const fixture: SessionFixture = { session_id: fallbackSessionId, cwd };
		try {
			const id = typeof manager?.getSessionId === "function" ? manager.getSessionId() : undefined;
			if (typeof id !== "string" || !id.trim()) return fixture;
			fixture.session_id = id;
			const path = manager.getSessionFile?.();
			if (typeof path === "string" && path && existsSync(path) && statSync(path).isFile()) {
				const header = readSessionHeader(path);
				if (header?.id === id && header.cwd === cwd) fixture.transcript_path = path;
			}
		} catch {
			// A lazy/moving file does not erase an obtained native ID or
			// authorize borrowing another same-cwd session.
		}
		return fixture;
	}
	const discovered = deriveOmpSession(homedir(), cwd);
	const fixture: SessionFixture = {
		session_id: discovered.transcriptPath ? discovered.sessionId : fallbackSessionId,
		cwd,
	};
	if (discovered.transcriptPath) fixture.transcript_path = discovered.transcriptPath;
	return fixture;
}

function callCompound(verb: string, fixture: Record<string, unknown>): CompoundHookDocument | null {
	try {
		const args = ["hook", verb, "--live", "--harness", HARNESS];
		if (verb === "pre-tool-use") args.push("--enforce");
		const res = spawnSync(COMPOUND, args, {
			input: JSON.stringify(fixture),
			encoding: "utf-8",
			timeout: CALL_TIMEOUT_MS,
			maxBuffer: 8 * 1024 * 1024,
		});
    if (res.error || res.signal) return null;
		const out = (res.stdout ?? "").trim();
		// Verbs print a JSON document first; some append a human status line.
		// Parse the leading top-level object leniently.
		const start = out.indexOf("{");
		if (start < 0) return null;
		let depth = 0;
		let inString = false;
		for (let i = start; i < out.length; i++) {
			const ch = out[i];
			if (inString) {
				if (ch === "\\") i++;
				else if (ch === '"') inString = false;
				continue;
			}
			if (ch === '"') inString = true;
			else if (ch === "{") depth++;
			else if (ch === "}") {
				depth--;
				if (depth === 0) {
					try {
          const doc = JSON.parse(out.slice(start, i + 1)) as CompoundHookDocument;
          if (res.status !== 0) {
            const decision =
              doc.permissionDecision === "deny" ||
              doc.hookSpecificOutput?.permissionDecision === "deny"
                ? "deny"
                : null;
            if (
              verb !== "pre-tool-use" ||
              doc.hookSpecificOutput?.hookEventName !== "PreToolUse" ||
              decision !== "deny"
            ) {
              return null;
            }
          }
          return doc;
					} catch {
						return null;
					}
				}
			}
		}
		return null;
	} catch {
		return null;
	}
}

export default function compoundHook(pi: CompoundHookApi): void {
	// omp gives session_start no message channel, so keep its context (the
	// session summary and the door map) and prepend it to the first message.
	let startContext = "";
	let startContextSessionId = "";
	let startContextCwd = "";
	pi.on("session_start", (_payload, context) => {
		const fixture = sessionFixture(context);
		const ctx = callCompound("session-start", fixture)?.hookSpecificOutput?.additionalContext;
		startContext = typeof ctx === "string" ? ctx.trim() : "";
		startContextSessionId = fixture.session_id;
		startContextCwd = fixture.cwd;
	});

	pi.on("before_agent_start", (payload, context) => {
		const event = (payload ?? {}) as BeforeAgentStartPayload;
		const fixture = sessionFixture(context);
		const doc = callCompound("user-prompt-submit", { ...fixture, prompt: event.prompt ?? "" });
		const ctx = doc?.hookSpecificOutput?.additionalContext;
		const parts = [fixture.session_id === startContextSessionId && fixture.cwd === startContextCwd ? startContext : "", typeof ctx === "string" ? ctx : ""].filter((part) => part.length > 0);
		startContext = "";
		startContextSessionId = "";
		startContextCwd = "";
		if (parts.length === 0) return undefined;
		return { message: parts.join("\n\n") };
	});

	pi.on("tool_call", (payload, context) => {
		const event = (payload ?? {}) as ToolCallPayload;
		const doc = callCompound("pre-tool-use", {
			...sessionFixture(context),
			tool_name: event.toolName ?? "unknown",
			tool_use_id: event.toolCallId ?? "",
			tool_input: event.input ?? {},
		});
		if (!doc?.hookSpecificOutput || doc.hookSpecificOutput.hookEventName !== "PreToolUse") {
			return { block: true, reason: "compound policy gate unavailable" };
		}
		const topHasDecision = Object.prototype.hasOwnProperty.call(doc, "permissionDecision");
		const nestedHasDecision = Object.prototype.hasOwnProperty.call(doc.hookSpecificOutput, "permissionDecision");
		const topDecision: unknown = doc.permissionDecision;
		const nestedDecision: unknown = doc.hookSpecificOutput.permissionDecision;
		if (topDecision === "deny" || nestedDecision === "deny") {
			const reason = nestedDecision === "deny"
				? doc.hookSpecificOutput.permissionDecisionReason
				: doc.permissionDecisionReason;
			return { block: true, reason: reason ?? "compound policy denied this tool call" };
		}
		for (const [present, decision] of [
			[topHasDecision, topDecision],
			[nestedHasDecision, nestedDecision],
		] as const) {
			if (present && decision !== "allow") {
				return { block: true, reason: "compound policy returned an invalid permission decision" };
			}
		}
		return undefined;
	});
	pi.on("tool_result", (payload, context) => {
		const event = (payload ?? {}) as ToolResultPayload;
		callCompound("post-tool-use", {
			...sessionFixture(context),
			tool_name: event.toolName ?? "unknown",
			tool_use_id: event.toolCallId ?? "",
		});
	});

	pi.on("session_shutdown", (_payload, context) => {
		callCompound("session-end", sessionFixture(context));
	});
}
