# adapters/claude-qwen

Installs an isolated Claude Code profile for the **qwen / 3.8-max** lane.

This adapter is a thin wrapper over the shared installer in
`adapters/_lib/install-lane.sh`. Add a new lane by appending one block to
`adapters/_lib/lanes.json` and copying this directory.

## Install

```sh
adapters/claude-qwen/install.sh \
  --profile-dir "$HOME/.switchback/config/claude/qwen" \
  --compound "$HOME/.local/bin/compound" \
  --steering-source "$HOME/.claude/CLAUDE.md"
```

## Run

```sh
claude-qwen
```

Visual identity: **❖** (purple), iTerm2 tab colored to match. Lane
identity and color come from `adapters/_lib/lanes.json`.
