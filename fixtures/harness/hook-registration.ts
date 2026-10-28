export interface HookReg {
  harness: "claude" | "codex";
  event: "SessionStart" | "UserPromptSubmit" | "PreToolUse" | "Stop";
  file: string;
  command: string;
  matcher?: string;
}

export const HOOK_REGISTRATIONS: HookReg[] = [
  { harness: "claude", event: "SessionStart", file: "RecallContext.hook.ts", command: `"$HOME/.claude/hooks/RecallContext.hook.ts"` },
  { harness: "codex", event: "SessionStart", file: "RecallContext.hook.ts", command: `${HOME}/.bun/bin/bun ${HOME}/.codex/hooks/RecallContext.hook.ts` },
  { harness: "claude", event: "UserPromptSubmit", file: "IntentRecall.hook.ts", command: `"$HOME/.claude/hooks/IntentRecall.hook.ts"` },
  { harness: "codex", event: "UserPromptSubmit", file: "IntentRecall.hook.ts", command: `${HOME}/.bun/bin/bun ${HOME}/.codex/hooks/IntentRecall.hook.ts` },
  { harness: "claude", event: "PreToolUse", matcher: "*", file: "PlacementGuard.hook.ts", command: `"$HOME/.claude/hooks/PlacementGuard.hook.ts"` },
  { harness: "codex", event: "PreToolUse", matcher: "*", file: "compound", command: `${HOME}/.local/bin/compound hook pre-tool-use --live --harness codex` },
  { harness: "codex", event: "Stop", file: "compound", command: `${HOME}/.local/bin/compound hook stop --live --harness codex` },
];
