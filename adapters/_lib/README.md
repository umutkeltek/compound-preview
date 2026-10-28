# adapters/_lib — Lane visual + install system

Cross-lane infrastructure for Claude Code profile adapters. Solves the
"all my lane tabs look the same in iTerm2" problem by giving every lane:

- A distinct **iTerm2 tab color** (set on session start via OSC 6 escape)
- A **tab title** showing `<lane>:<project>`
- A **terminal badge** showing `<icon> <lane> · <project>`
- A **Claude Code statusLine hook** that prints a colored, width-aware
  banner inside the TUI every turn — greeting → git → context bar → quote

Architecture is loosely based on PAI's `~/.claude/statusline-command.sh`,
parameterized by lane.

## Add a new lane

1. Append a block to `lanes.json`. Required keys: `id`, `label`, `icon`,
   `family` ("claude_code" or "codex"), `tab_rgb`, `ansi_hex`,
   `profile_dir_var`, `tap_bind` (or empty for native), `profile_default`
   (default profile dir), `title_suffix`, `model`. Optional: `aliases`.
2. Drop a directory `adapters/claude-<id>/` with `install.sh` (a 5-line
   shim that calls `../_lib/install-lane.sh --lane <id>`) and a
   `hook-registration.ts` mirroring the shape in `claude-zai/`.
3. Run `adapters/claude-<id>/install.sh`.
4. Source `lane-launcher.sh` from your zshrc — you get `claude-<id>`
   automatically with the right tab color, badge, tap URL, and isolated
   profile dir.

That is the entire changelog per new lane.

## One-line zshrc snippet

```sh
# path = wherever you keep the compound checkout
[ -r "$HOME/path/to/compound/adapters/_lib/zshrc-snippet.zsh" ] \
  && . "$HOME/path/to/compound/adapters/_lib/zshrc-snippet.zsh"
```

The snippet self-locates: every sibling default derives from the sourced
file's own directory, so no absolute install path is baked in.

After sourcing:

```sh
claude-zai            # blue ⬢ — z.ai GLM-5.2 via Switchback tap
claude-zai-full       # same as above (alias)
claude-zai-direct     # cyan ⬢ — z.ai GLM-5.2 direct (no headroom)
claude-codex          # green ⬡ — codex / gpt-5.6
claude-minimax        # orange ◦ — minimax / M3
claude-qwen           # purple ❖ — qwen / 3.8-max
codex-native          # red ▲ — codex vendor (uses existing shell launcher)

apply-lane minimax    # repaint the current pane's iTerm2 tab/badge by lane
paint-lane qwen       # set lane env vars (LANE_LABEL etc.) + apply
lane-use native       # alias that swaps lanes without launching a binary
```

The snippet also enables `zshrc-overlay.zsh`, which adds a `chpwd/precmd`
hook that paints your iTerm2 tab **color** + a lane-augmented title and
badge on every directory change. Your existing `_iterm_visuals_update`
hook (project + pane_role visuals) stays intact — the overlay runs
alongside it.

## Adding a new lane takes ~1 minute

A typical new block to `lanes.json`:

```json
{
  "id": "deepseek",
  "label": "deepseek",
  "aliases": ["claude-deepseek"],
  "icon": "⬢",
  "family": "claude_code",
  "model": "deepseek/v4-pro",
  "tab_rgb": [120, 80, 200],
  "ansi_hex": "7850C8",
  "profile_dir_var": "CLAUDE_DEEPSEEK_PROFILE_DIR",
  "tap_bind": "127.0.0.1:18800",
  "profile_default": "${HOME}/.switchback/config/claude/deepseek",
  "title_suffix": "v4-pro"
}
```

That's it. `lane-launcher.sh` will generate `claude-deepseek`, the install
shim will pick up the right hook registration, and the statusline gets the
purple color automatically.

## How the three layers interact

1. **iTerm2 chrome (per-session, set instantly on launch)**
   `lane-launcher.sh` calls `it2_apply_lane <lane>` which emits:
   - `\033]6;1;bg;red;brightness\007<n>\033]6;1;bg;green;...\033]6;1;bg;blue;...\033]6;1;bg;\007` → tab color
   - `\033]0;<lane>:<project>\007` → tab title
   - `\033]1337;SetBadgeFormat=<base64>\007` → terminal badge

2. **Claude Code statusLine (per-turn, set via profile settings.json)**
   `install-lane.sh` merges `statusLine` into the profile's settings.json:
   ```json
   {
     "statusLine": {
       "type": "command",
       "command": "/…/lane-statusline.sh --lane zai"
     }
   }
   ```
   Claude Code pipes session JSON to that command on every turn. The script
   renders a width-aware banner in the lane's truecolor.

3. **Tap routing (per-process, env vars on the wrapper)**
   Each wrapper sets `ANTHROPIC_BASE_URL=http://127.0.0.1:<tap_bind>` so
   `claude` (Claude Code CLI) routes traffic through the matching Switchback
   tap. `CLAUDE_CONFIG_DIR` keeps the profile isolated.

The three layers are independent — disable any one and the other two still
work.

## Verifying

```sh
# Run the shell test suite (63 assertions, ~1s on a modern machine).
sh adapters/_lib/install-lane.test.sh

# Manual smoke:
. adapters/_lib/lane-launcher.sh
type claude-zai             # → function
type claude-zai-full        # → function (alias)

# iTerm2 escapes (visible only inside iTerm2)
it2_apply_lane zai compound  # sets tab color, title, badge

# Statusline rendering (Claude Code pipes session JSON to this on every turn)
COLUMNS=120 adapters/_lib/lane-statusline.sh --lane zai < session.json
```

## Files

```
adapters/_lib/
├── lanes.json                 # single source of truth (one block per lane)
├── lanes.sh                   # shell loader (lane_field, lane_resolve, lane_emit_sh)
├── lane-it2.sh                # iTerm2 OSC 6/0/1337 escapes
├── lane-launcher.sh           # generates shell functions (claude-zai, etc.)
├── lane-statusline.sh         # Claude Code statusLine hook (runs per turn)
├── statusline-install.sh      # merges statusLine into profile settings.json
├── install-lane.sh            # runs compound hook install + statusline + symlinks
├── zshrc-snippet.zsh          # one-line drop-in for ~/.zshrc
├── zshrc-overlay.zsh          # paints iTerm2 tab color on chpwd/precmd
├── install-lane.test.sh       # 38-test shell test suite
└── README.md                  # this file
```
