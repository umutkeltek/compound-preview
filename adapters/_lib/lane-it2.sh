#!/usr/bin/env sh
# adapters/_lib/lane-it2.sh
#
# iTerm2 visuals for Claude lane sessions.
#
# Public API (after `. ./adapters/_lib/lane-it2.sh`):
#
#   it2_apply_lane <lane_or_alias> [project_basename]
#       Sets tab color, tab title, and badge for the active iTerm2 session.
#       Uses lanes.sh to resolve alias -> canonical id and look up tab_rgb.
#       No-ops if $TERM_PROGRAM != "iTerm.app".
#
#   it2_set_tab_color <r> <g> <b>
#       Emits iTerm2 OSC 6 escape for the active tab. 0-255 ints.
#
#   it2_set_title <title>
#       OSC 0 escape.
#
#   it2_set_badge <text>
#       OSC 1337 ; SetBadgeFormat=<base64> escape.
#
#   it2_clear
#       Reset tab color (transient flag only), clear title, clear badge.
#
# Pattern is BEL (\007) terminated to match the user's existing zshrc hook.

set -eu

# This file is meant to be sourced with its absolute path, OR with LANE_IT2_FILE
# set, OR with LANES_SH already pointing to lanes.sh. The user's zshrc
# launchers should (path = wherever you keep the compound checkout):
#     LANE_IT2_FILE="$HOME/path/to/compound/adapters/_lib/lane-it2.sh"
#     . "$LANE_IT2_FILE"
#
# We deliberately use `dirname` (no `cd`) because the user's zshrc has a
# chpwd hook (`_iterm_visuals_update`) that prints OSC escapes to stdout when
# we `cd`. Command substitutions like `$(cd ... && pwd)` would capture those
# escapes and corrupt the path.
if [ -z "${LANES_SH:-}" ]; then
  if [ -n "${LANE_IT2_FILE:-}" ]; then
    case "$LANE_IT2_FILE" in
      /*) _LANE_IT2_DIR=$(dirname -- "$LANE_IT2_FILE") ;;
      *)  _LANE_IT2_DIR=$(dirname -- "$LANE_IT2_FILE") ;;
    esac
    LANES_SH="$_LANE_IT2_DIR/lanes.sh"
    unset _LANE_IT2_DIR
  else
    echo "lane-it2.sh: set LANE_IT2_FILE=<abs path> or LANES_SH=<abs path>" >&2
    return 1 2>/dev/null || exit 1
  fi
fi
. "$LANES_SH"

# --- it2_set_tab_color ----------------------------------------------------
# iTerm2 proprietary OSC 6 escape:
#   OSC 6 ; 1 ; bg ; red ; brightness BEL
#   OSC 6 ; 1 ; bg ; green ; brightness BEL
#   OSC 6 ; 1 ; bg ; blue ; brightness BEL
#   OSC 6 ; 1 ; bg ; BEL                  (commit / reset to default)
#
# We set each channel in the current tab to (r,g,b) and then re-emit the
# commit-only OSC 6 with no params to apply the change. This pattern works
# in iTerm2 3.4+ and is the documented form in
# https://iterm2.com/documentation-escape-codes.html

it2_set_tab_color() {
  _r=${1:-0}; _g=${2:-0}; _b=${3:-0}
  printf '\033]6;1;bg;red;brightness\007%s\033]6;1;bg;green;brightness\007%s\033]6;1;bg;blue;brightness\007%s\033]6;1;bg;\007' \
    "$_r" "$_g" "$_b"
  unset _r _g _b
}

it2_clear_tab_color() {
  printf '\033]6;1;bg;default\007'
}

it2_set_title() {
  printf '\033]0;%s\007' "$1"
}

# Transparent badge when text is empty.
it2_set_badge() {
  _text="$1"
  if [ -z "$_text" ]; then
    printf '\033]1337;SetBadgeFormat=\007'
  else
    _enc=$(printf '%s' "$_text" | base64 | tr -d '\r\n')
    printf '\033]1337;SetBadgeFormat=%s\007' "$_enc"
  fi
  unset _text _enc
}

it2_apply_lane() {
  [ "${TERM_PROGRAM:-}" = "iTerm.app" ] || return 0
  _lane="$1"
  _project="${2:-${PWD##*/}}"
  _canonical=$(lane_resolve "$_lane")
  if [ -z "$_canonical" ]; then
    echo "it2_apply_lane: unknown lane: $_lane" >&2
    unset _lane _project _canonical
    return 1
  fi
  eval "$(lane_emit_sh "$_canonical")"

  # Skip if no tab_rgb was defined for this lane.
  if [ -n "$LANE_TAB_R" ] && [ -n "$LANE_TAB_G" ] && [ -n "$LANE_TAB_B" ]; then
    it2_set_tab_color "$LANE_TAB_R" "$LANE_TAB_G" "$LANE_TAB_B"
  fi

  # Title: "<lane>:<project>" — short enough for tab pills.
  it2_set_title "${LANE_LABEL}:${_project}"

  # Badge: "<icon> <lane> · <project>" — visible inside the terminal,
  # too, when running outside the Claude Code TUI (e.g. plain sh inside the tab).
  _badge="${LANE_ICON} ${LANE_LABEL} · ${_project}"
  it2_set_badge "$_badge"
  unset _badge _lane _project _canonical LANE_ID LANE_LABEL LANE_ICON LANE_FAMILY LANE_MODEL \
                LANE_ANSI_HEX LANE_TAB_R LANE_TAB_G LANE_TAB_B \
                LANE_PROFILE_DIR_VAR LANE_TAP_BIND LANE_PROFILE_DEFAULT LANE_TITLE_SUFFIX
}

it2_clear() {
  [ "${TERM_PROGRAM:-}" = "iTerm.app" ] || return 0
  it2_clear_tab_color
  it2_set_title ""
  it2_set_badge ""
}
