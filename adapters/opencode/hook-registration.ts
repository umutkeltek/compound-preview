export interface HookReg {
  harness: "opencode"
  event: "SessionStart" | "UserPromptSubmit" | "PostToolUse"
  file: string
  command: string
  status: "non_blocking"
}

export const HOOK_REGISTRATIONS: HookReg[] = [
  {
    harness: "opencode",
    event: "SessionStart",
    file: "compound.ts",
    command: "opencode plugin callback session-start",
    status: "non_blocking",
  },
  {
    harness: "opencode",
    event: "UserPromptSubmit",
    file: "compound.ts",
    command: "opencode plugin callback user-prompt-submit",
    status: "non_blocking",
  },
  {
    harness: "opencode",
    event: "PostToolUse",
    file: "compound.ts",
    command: "opencode plugin callback post-tool-use",
    status: "non_blocking",
  },
]
