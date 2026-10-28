const HOME = "${HOME}"

export interface HookReg {
  harness: "claude-zai"
  event: "SessionStart" | "UserPromptSubmit" | "PreToolUse" | "PostToolUse" | "SessionEnd"
  file: "compound"
  command: string
  matcher?: "*"
}

export const HOOK_REGISTRATIONS: HookReg[] = [
  { harness: "claude-zai", event: "SessionStart", file: "compound", command: `${HOME}/.local/bin/compound hook session-start --live --harness claude_zai` },
  { harness: "claude-zai", event: "UserPromptSubmit", file: "compound", command: `${HOME}/.local/bin/compound hook user-prompt-submit --live --harness claude_zai` },
  { harness: "claude-zai", event: "PreToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook pre-tool-use --live --harness claude_zai --enforce` },
  { harness: "claude-zai", event: "PostToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook post-tool-use --live --harness claude_zai` },
  { harness: "claude-zai", event: "SessionEnd", file: "compound", command: `${HOME}/.local/bin/compound hook session-end-adapter --live --harness claude_zai` },
]
