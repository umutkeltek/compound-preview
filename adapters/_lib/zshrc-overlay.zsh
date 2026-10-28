# adapters/_lib/zshrc-overlay.zsh
#
# Lane-aware overlay for the user's existing `_iterm_visuals_update` hook.
# Drop ONE line near the bottom of your ~/.zshrc (path = wherever you keep
# the compound checkout):
#
#     [ -r "$HOME/path/to/compound/adapters/_lib/zshrc-overlay.zsh" ] \
#       && . "$HOME/path/to/compound/adapters/_lib/zshrc-overlay.zsh"
#
# This file ADDS its own chpwd/precmd hook (`_iterm_lane_paint`) — does NOT
# touch the user's. Painted second, so when a lane is selected our tab color
# + lane-augmented title/badge win. With no lane selected, this hook is a
# no-op and the user's visuals stay exactly as they were.

# Locate lane-it2.sh. Default: the sibling of THIS sourced file, so no
# absolute install location is baked in (zsh sets $0 to the sourced path).
_OVERLAY_DIR=${0:A:h}
: "${LANE_IT2_FILE:=$_OVERLAY_DIR/lane-it2.sh}"
if [ ! -f "$LANE_IT2_FILE" ]; then
  echo "lane overlay: missing $LANE_IT2_FILE (set LANE_IT2_FILE in zshrc to override)" >&2
  unset _OVERLAY_DIR
  return 1 2>/dev/null || exit 1
fi
# lane-it2.sh expects either LANE_IT2_FILE or LANES_SH to find lanes.sh. We
# give it a second look-up hint for non-default install locations.
if [ -z "${LANES_SH:-}" ]; then
  : "${LANES_SH:=$_OVERLAY_DIR/lanes.sh}"
  export LANES_SH
fi
unset _OVERLAY_DIR
. "$LANE_IT2_FILE"

# Lane paint hook. Idempotent registration (add-zsh-hook -d clears any prior).
_iterm_lane_paint() {
  [ "${TERM_PROGRAM:-}" = "iTerm.app" ] || return 0
  [ -n "${LANE_LABEL:-}" ] || return 0

  local r="${LANE_TAB_R:-200}" g="${LANE_TAB_G:-200}" b="${LANE_TAB_B:-200}"
  local icon="${LANE_ICON:-◆}"
  local title="${LANE_LABEL}:${PWD##*/}"
  local badge="${icon} ${LANE_LABEL} · ${PWD##*/}"

  printf '\033]6;1;bg;red;brightness\007%s\033]6;1;bg;green;brightness\007%s\033]6;1;bg;blue;brightness\007%s\033]6;1;bg;\007' \
    "$r" "$g" "$b"
  printf '\033]0;%s\007' "$title"
  printf '\033]1337;SetBadgeFormat=%s\007' \
    "$(printf '%s' "$badge" | base64 | tr -d '\r\n')"
}

if [ -n "${ZSH_VERSION:-}" ]; then
  autoload -Uz add-zsh-hook 2>/dev/null
  add-zsh-hook -d chpwd  _iterm_lane_paint 2>/dev/null
  add-zsh-hook -d precmd _iterm_lane_paint 2>/dev/null
  add-zsh-hook    chpwd  _iterm_lane_paint
  add-zsh-hook    precmd _iterm_lane_paint
fi

# Ad-hoc recolor for bare shells (no binary launch).
paint-lane() {
  if [ "$#" -eq 0 ]; then
    echo "usage: paint-lane <lane>   # examples: zai, minimax, qwen, codex, native" >&2
    return 2
  fi
  local canonical
  canonical=$(lane_resolve "$1")
  if [ -z "$canonical" ]; then
    echo "paint-lane: unknown lane: $1" >&2
    return 1
  fi
  eval "$(lane_emit_sh "$canonical")"
  export LANE_ID LANE_LABEL LANE_MODEL LANE_ICON LANE_ANSI_HEX \
         LANE_TAB_R LANE_TAB_G LANE_TAB_B
  _iterm_lane_paint
  unset canonical
}

apply-lane() {
  if [ "$#" -eq 0 ]; then
    echo "usage: apply-lane <lane> [project_basename]" >&2
    return 2
  fi
  it2_apply_lane "$@"
}
