# adapters/_lib/zshrc-snippet.zsh
#
# Drop ONE line near the bottom of your ~/.zshrc to wire up the lane system
# (path = wherever you keep the compound checkout):
#
#     [ -r "$HOME/path/to/compound/adapters/_lib/zshrc-snippet.zsh" ] \
#       && . "$HOME/path/to/compound/adapters/_lib/zshrc-snippet.zsh"
#
# Or, if you already have it, just source adapters/_lib/zshrc-overlay.zsh for
# only the visual overlay (without the launcher functions).
#
# What this does:
#   1. Sources lane-launcher.sh — generates claude-zai, claude-minimax, etc.
#   2. Sources zshrc-overlay.zsh — adds chpwd/precmd hook that paints the
#      lane's iTerm2 tab color + lane-augmented title and badge.
#
# Both files are re-source safe. Order matters: launcher first, overlay second.
# The user's existing `_iterm_visuals_update` is preserved untouched.

# zsh passes through LANE_* env vars automatically. We don't need to export them.
# Sibling files default to THIS sourced file's own directory (zsh sets $0 to the
# sourced path), so no absolute install location is baked in.
_SNIPPET_DIR=${0:A:h}
LANE_LAUNCHER_FILE="$_SNIPPET_DIR/lane-launcher.sh"
[ -r "$LANE_LAUNCHER_FILE" ] && . "$LANE_LAUNCHER_FILE"

LANE_IT2_FILE="$_SNIPPET_DIR/lane-it2.sh"
[ -r "$_SNIPPET_DIR/zshrc-overlay.zsh" ] \
  && . "$_SNIPPET_DIR/zshrc-overlay.zsh"
unset _SNIPPET_DIR

