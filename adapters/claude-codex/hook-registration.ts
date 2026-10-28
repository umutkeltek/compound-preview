const HOME = "${HOME}"

export interface HookReg {
  harness: "claude-codex"
  event: "SessionStart" | "UserPromptSubmit" | "PreToolUse" | "PostToolUse"
  file: "compound"
  command: string
  matcher?: "*"
}

export const HOOK_REGISTRATIONS: HookReg[] = [
  { harness: "claude-codex", event: "SessionStart",       file: "compound", command: `${HOME}/.local/bin/compound hook session-start --live --harness claude-codex` },
  { harness: "claude-codex", event: "UserPromptSubmit",   file: "compound", command: `${HOME}/.local/bin/compound hook user-prompt-submit --live --harness claude-codex` },
  { harness: "claude-codex", event: "PreToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook pre-tool-use --live --harness claude-codex --enforce` },
  { harness: "claude-codex", event: "PostToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook post-tool-use --live --harness claude-codex` },
]
