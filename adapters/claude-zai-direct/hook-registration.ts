const HOME = "${HOME}"

export interface HookReg {
  harness: "claude-zai-direct"
  event: "SessionStart" | "UserPromptSubmit" | "PreToolUse" | "PostToolUse"
  file: "compound"
  command: string
  matcher?: "*"
}

export const HOOK_REGISTRATIONS: HookReg[] = [
  { harness: "claude-zai-direct", event: "SessionStart",       file: "compound", command: `${HOME}/.local/bin/compound hook session-start --live --harness claude-zai-direct` },
  { harness: "claude-zai-direct", event: "UserPromptSubmit",   file: "compound", command: `${HOME}/.local/bin/compound hook user-prompt-submit --live --harness claude-zai-direct` },
  { harness: "claude-zai-direct", event: "PreToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook pre-tool-use --live --harness claude-zai-direct --enforce` },
  { harness: "claude-zai-direct", event: "PostToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook post-tool-use --live --harness claude-zai-direct` },
]
