import { describe, expect, test } from "bun:test"
import type { PluginInput } from "@opencode-ai/plugin"
import type { Part, UserMessage } from "@opencode-ai/sdk"

import CompoundPlugin, { type HookRunner } from "./compound.ts"

function pluginInput(): PluginInput {
  return {
    directory: "/repo",
    worktree: "/repo",
  } as unknown as PluginInput
}

function userMessage(sessionID: string, system?: string): UserMessage {
  return {
    id: "message-1",
    sessionID,
    role: "user",
    time: { created: 1 },
    agent: "build",
    model: { providerID: "fixture", modelID: "fixture" },
    system,
  }
}

function textPart(sessionID: string, text: string): Part {
  return {
    id: "part-1",
    sessionID,
    messageID: "message-1",
    type: "text",
    text,
  }
}

describe("CompoundPlugin", () => {
  test("exports only the default runtime plugin factory", async () => {
    const module = await import("./compound.ts")
    const runtimeFactories = Object.entries(module)
      .filter(([, value]) => typeof value === "function")
      .map(([name]) => name)

    expect(runtimeFactories).toEqual(["default"])
  })

  test("injects session context once and prompt context on every user message", async () => {
    const calls: string[] = []
    const runner: HookRunner = async (name, input) => {
      calls.push(name + ":" + (input.prompt ?? ""))
      if (name === "session-start") return "SESSION CONTEXT"
      if (name === "user-prompt-submit") return "PROMPT CONTEXT"
      return ""
    }
    const hooks = await CompoundPlugin.createWithRunner(runner)(pluginInput())
    const firstMessage = userMessage("session-1", "EXISTING")

    await hooks["chat.message"]?.(
      { sessionID: "session-1" },
      { message: firstMessage, parts: [textPart("session-1", "hello")] },
    )

    expect(firstMessage.system).toContain("EXISTING")
    expect(firstMessage.system).toContain('<compound-context source="session-start">')
    expect(firstMessage.system).toContain("SESSION CONTEXT")
    expect(firstMessage.system).toContain('<compound-context source="user-prompt-submit">')
    expect(firstMessage.system).toContain("PROMPT CONTEXT")

    const secondMessage = userMessage("session-1")
    await hooks["chat.message"]?.(
      { sessionID: "session-1" },
      { message: secondMessage, parts: [textPart("session-1", "again")] },
    )

    expect(secondMessage.system).not.toContain("SESSION CONTEXT")
    expect(secondMessage.system).toContain("PROMPT CONTEXT")
    expect(calls).toEqual([
      "session-start:",
      "user-prompt-submit:hello",
      "user-prompt-submit:again",
    ])
  })

  test("observes completed tools without waiting on the observation result", async () => {
    const calls: string[] = []
    const runner: HookRunner = async (name, input) => {
      calls.push(name + ":" + (input.tool_name ?? ""))
      return ""
    }
    const hooks = await CompoundPlugin.createWithRunner(runner)(pluginInput())

    await hooks["tool.execute.after"]?.(
      { tool: "read", sessionID: "session-2", callID: "call-1", args: { path: "README.md" } },
      { title: "read", output: "contents", metadata: {} },
    )

    expect(calls).toEqual(["post-tool-use:read"])
  })

  test("hook failures leave the opencode callback successful", async () => {
    const messages: string[] = []
    const original = console.error
    console.error = (...args: unknown[]) => messages.push(args.join(" "))
    try {
      const hooks = await CompoundPlugin.createWithRunner(async () => {
        throw new Error("fixture failure\nwith detail")
      })(pluginInput())
      const message = userMessage("session-3")

      await hooks["chat.message"]?.(
        { sessionID: "session-3" },
        { message, parts: [textPart("session-3", "hello")] },
      )

      expect(message.system).toBeUndefined()
      expect(messages).toEqual([
        "[compound/opencode] session-start failed open: fixture failure with detail",
        "[compound/opencode] user-prompt-submit failed open: fixture failure with detail",
      ])
    } finally {
      console.error = original
    }
  })
})

describe("hook timeout budgets", () => {
  // These budgets are the difference between opencode having Compound context
  // and silently running blind. On 2026-08-07 one shared 2s budget was measured
  // against a real `compound hook session-start --live` run: 10.3s and 12.9s on
  // two consecutive probes. Session start therefore timed out and failed open on
  // every start, while the hook itself succeeded and every surface read healthy.
  test("session start outlives a real ContextPack build", () => {
    // 13s was the slower observed build; the budget must clear it with headroom,
    // or this regression returns as a silent one.
    expect(CompoundPlugin.timeoutMS("session-start")).toBeGreaterThanOrEqual(20_000)
  })

  test("per-turn and per-tool hooks stay tight enough not to stall a turn", () => {
    expect(CompoundPlugin.timeoutMS("post-tool-use")).toBeLessThanOrEqual(2_000)
    expect(CompoundPlugin.timeoutMS("user-prompt-submit")).toBeLessThanOrEqual(10_000)
  })

  test("session start is not held to the per-tool budget", () => {
    // The falsifier for the original defect: a single shared budget makes these
    // equal. They must not be.
    expect(CompoundPlugin.timeoutMS("session-start")).toBeGreaterThan(
      CompoundPlugin.timeoutMS("post-tool-use"),
    )
  })

  test("an explicit env override still wins for every hook", () => {
    const previous = process.env.COMPOUND_OPENCODE_HOOK_TIMEOUT_MS
    process.env.COMPOUND_OPENCODE_HOOK_TIMEOUT_MS = "1234"
    try {
      expect(CompoundPlugin.timeoutMS("session-start")).toBe(1234)
      expect(CompoundPlugin.timeoutMS("post-tool-use")).toBe(1234)
    } finally {
      if (previous === undefined) delete process.env.COMPOUND_OPENCODE_HOOK_TIMEOUT_MS
      else process.env.COMPOUND_OPENCODE_HOOK_TIMEOUT_MS = previous
    }
  })
})
