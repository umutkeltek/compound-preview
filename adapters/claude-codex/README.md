# adapters/claude-codex

Installs an isolated Claude Code profile for the **codex / gpt-5.6** lane.

This adapter is a thin wrapper over the shared installer in
`adapters/_lib/install-lane.sh`. Add a new lane by appending one block to
`adapters/_lib/lanes.json` and copying this directory.

## Install

```sh
adapters/claude-codex/install.sh \
  --profile-dir "$HOME/.switchback/config/claude/codex" \
  --compound "$HOME/.local/bin/compound" \
  --steering-source "$HOME/.claude/CLAUDE.md"
```

## Run

```sh
claude-codex
```

Visual identity: **⬡** (green), iTerm2 tab colored to match. Lane
identity and color come from `adapters/_lib/lanes.json`.
