const HOME = "${HOME}"

export interface HookReg {
  harness: "claude-minimax"
  event: "SessionStart" | "UserPromptSubmit" | "PreToolUse" | "PostToolUse" | "SessionEnd"
  file: "compound"
  command: string
  matcher?: "*"
}

export const HOOK_REGISTRATIONS: HookReg[] = [
  { harness: "claude-minimax", event: "SessionStart",       file: "compound", command: `${HOME}/.local/bin/compound hook session-start --live --harness claude_minimax` },
  { harness: "claude-minimax", event: "UserPromptSubmit",   file: "compound", command: `${HOME}/.local/bin/compound hook user-prompt-submit --live --harness claude_minimax` },
  { harness: "claude-minimax", event: "PreToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook pre-tool-use --live --harness claude_minimax --enforce` },
  { harness: "claude-minimax", event: "PostToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook post-tool-use --live --harness claude_minimax` },
  { harness: "claude-minimax", event: "SessionEnd", file: "compound", command: `${HOME}/.local/bin/compound hook session-end-adapter --live --harness claude_minimax` },
]
