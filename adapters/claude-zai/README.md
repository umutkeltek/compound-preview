# claude-zai adapter

This adapter enrolls an isolated Claude Code-compatible profile as the distinct `claude_zai`
Compound harness. The profile keeps its own settings and history, while Compound events,
EffectiveContracts, receipts, and doctor rows retain the lane identity instead of being attributed
to native `claude_code`.

The installer no-clobber merges SessionStart, UserPromptSubmit, PreToolUse, and PostToolUse into the
profile's `settings.json`. Those native rows are compiled from Compound's semantic
`prepare_context`/`guard_mutation`/`observe_outcome` posture; inspect the exact projection with
`compound harness posture-plan --harness claude-zai`. Both tool hooks use the universal `*`
matcher; PostToolUse is the
session-ownership signal that lets the next PreToolUse distinguish this session's dirt from foreign
dirt. It also links the profile's `CLAUDE.md` to the global Claude steering source. Repository
`CLAUDE.md` files are already discovered from the shared working directory, so only global steering
needs this profile-local link.

Run against an explicit profile directory:

```sh
adapters/claude-zai/install.sh --profile-dir "${HOME}/.switchback/config/claude/zai-direct"
```

The installer refuses to replace a hand-written profile `CLAUDE.md`. Re-running it is idempotent.

The registration source is a parser-tested fragment containing the four exact rows the spine owner
must add to its full registration. `contract import-hooks` replaces managed rows rather than
additively merging them, so do not import this fragment over a populated live authority. The
orchestrator merges these rows into the spine source and then imports that complete source. A
temporary-home proof of the fragment is safe:

```sh
COMPOUND_HOME="$(mktemp -d)" compound contract import-hooks \
  --hook-registration-ts adapters/claude-zai/hook-registration.ts
```

The private profile guide remains the provider/cost owner. Its `claude-zai` lane must use
`harness: "claude-zai"` (not `"api"`) and route `sb run claude --with zai-direct`; its note should
state that repository steering is shared and the adapter links global steering. Apply the same row
to the generated overlay. The tracked importer test proves that a `claude` role binds this variant
without collapsing the resulting lane node back into native Claude.
