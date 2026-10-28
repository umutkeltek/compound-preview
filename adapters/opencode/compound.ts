import type { Plugin } from "@opencode-ai/plugin"
import type { Part } from "@opencode-ai/sdk"

type CompoundHookName = "session-start" | "user-prompt-submit" | "post-tool-use"

type HookInput = {
  hook_event_name: "SessionStart" | "UserPromptSubmit" | "PostToolUse"
  session_id: string
  cwd: string
  prompt?: string
  tool_name?: string
  tool_input?: Record<string, unknown>
  tool_response?: string
}

type HookResponse = {
  additional_context?: string
  hookSpecificOutput?: {
    additionalContext?: string
  }
}

export type HookRunner = (name: CompoundHookName, input: HookInput) => Promise<string>

// Budgets are per hook because the hooks have different jobs, and one shared
// budget silently defeated the most important of them. Measured on a real
// control-plane repo: `compound hook session-start --live` takes 10-13s (it
// builds the whole ContextPack), so a single 2s budget made session-start time
// out and fail open on EVERY start — opencode ran with zero Compound context
// while the plugin, the hook, and every status surface all reported success.
// Session start happens once and its entire purpose is to block until context
// exists, so it gets room; the per-turn and per-tool hooks stay tight so a slow
// control plane can never stall a turn.
const DEFAULT_TIMEOUT_MS: Record<CompoundHookName, number> = {
  "session-start": 30_000,
  "user-prompt-submit": 10_000,
  "post-tool-use": 2_000,
}

function oneLine(error: unknown): string {
  const text = error instanceof Error ? error.message : String(error)
  return text.replace(/\s+/g, " ").trim() || "unknown error"
}

function timeoutMS(name: CompoundHookName): number {
  const configured = Number.parseInt(process.env.COMPOUND_OPENCODE_HOOK_TIMEOUT_MS ?? "", 10)
  return Number.isFinite(configured) && configured > 0 ? configured : DEFAULT_TIMEOUT_MS[name]
}

function compoundBinary(): string {
  return process.env.COMPOUND_BIN || Bun.which("compound") || "compound"
}

const runCompoundHook: HookRunner = async (name, input) => {
  const proc = Bun.spawn(
    [compoundBinary(), "hook", name, "--live", "--harness", "opencode"],
    {
      stdin: new TextEncoder().encode(JSON.stringify(input)),
      stdout: "pipe",
      stderr: "pipe",
    },
  )

  let timedOut = false
  const budgetMS = timeoutMS(name)
  const timer = setTimeout(() => {
    timedOut = true
    proc.kill()
  }, budgetMS)

  const [exitCode, stdout, stderr] = await Promise.all([
    proc.exited,
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
  ]).finally(() => clearTimeout(timer))

  if (timedOut) throw new Error("timed out after " + budgetMS + "ms")
  if (exitCode !== 0) throw new Error("exit " + exitCode + ": " + oneLine(stderr))

  let response: HookResponse
  try {
    response = JSON.parse(stdout) as HookResponse
  } catch {
    throw new Error("returned invalid JSON")
  }
  return (
    response.additional_context ??
    response.hookSpecificOutput?.additionalContext ??
    ""
  ).trim()
}

function userPrompt(parts: Part[]): string {
  return parts
    .filter(
      (part): part is Extract<Part, { type: "text" }> =>
        part.type === "text" && !part.synthetic && !part.ignored,
    )
    .map((part) => part.text)
    .join("\n")
    .trim()
}

function contextBlock(source: string, context: string): string {
  return "<compound-context source=\"" + source + "\">\n" + context + "\n</compound-context>"
}

function appendSystemContext(existing: string | undefined, blocks: string[]): string | undefined {
  const additions = blocks.filter(Boolean)
  if (additions.length === 0) return existing
  return [existing?.trim(), ...additions].filter(Boolean).join("\n\n")
}

function createCompoundPlugin(runHook: HookRunner = runCompoundHook): Plugin {
  return async ({ directory }) => {
    const sessionStarts = new Map<string, Promise<string>>()
    const sessionContextInjected = new Set<string>()

    const invokeOpen = async (
      name: CompoundHookName,
      input: HookInput,
    ): Promise<string> => {
      try {
        return await runHook(name, input)
      } catch (error) {
        console.error("[compound/opencode] " + name + " failed open: " + oneLine(error))
        return ""
      }
    }

    const ensureSessionStart = (sessionID: string, cwd = directory): Promise<string> => {
      const existing = sessionStarts.get(sessionID)
      if (existing) return existing
      const pending = invokeOpen("session-start", {
        hook_event_name: "SessionStart",
        session_id: sessionID,
        cwd,
      })
      sessionStarts.set(sessionID, pending)
      return pending
    }

    return {
      event: async ({ event }) => {
        if (event.type === "session.created") {
          void ensureSessionStart(event.properties.info.id, event.properties.info.directory)
        }
        if (event.type === "session.deleted") {
          sessionStarts.delete(event.properties.info.id)
          sessionContextInjected.delete(event.properties.info.id)
        }
      },

      "chat.message": async (input, output) => {
        const blocks: string[] = []
        if (!sessionContextInjected.has(input.sessionID)) {
          const context = await ensureSessionStart(input.sessionID)
          if (context) blocks.push(contextBlock("session-start", context))
          sessionContextInjected.add(input.sessionID)
        }

        const prompt = userPrompt(output.parts)
        const promptContext = await invokeOpen("user-prompt-submit", {
          hook_event_name: "UserPromptSubmit",
          session_id: input.sessionID,
          cwd: directory,
          prompt,
        })
        if (promptContext) blocks.push(contextBlock("user-prompt-submit", promptContext))

        output.message.system = appendSystemContext(output.message.system, blocks)
      },

      "tool.execute.after": async (input, output) => {
        void invokeOpen("post-tool-use", {
          hook_event_name: "PostToolUse",
          session_id: input.sessionID,
          cwd: directory,
          tool_name: input.tool,
          tool_input: input.args ?? {},
          tool_response: output.output,
        })
      },
    }
  }
}

type TestablePlugin = Plugin & {
  createWithRunner: (runHook: HookRunner) => Plugin
  timeoutMS: (name: CompoundHookName) => number
}

// opencode initializes every function exported by a plugin module. Keep the default plugin as the
// only runtime function export; properties preserve test access without becoming module exports.
const CompoundPlugin = Object.assign(createCompoundPlugin(), {
  createWithRunner: createCompoundPlugin,
  timeoutMS,
}) as TestablePlugin

export default CompoundPlugin
