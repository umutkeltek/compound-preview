#!/usr/bin/env sh
# adapters/_lib/lanes.sh
#
# Single-line loader for the lane registry.
#
# Public API (after `. ./adapters/_lib/lanes.sh`):
#   lane_field <lane_or_alias> <field>           # prints a lane field
#   lane_resolve <lane_or_alias>                 # prints canonical lane id
#   lane_list                                    # newline-separated canonical ids
#   lane_emit_sh <caller_lane> [extra_env ...]   # prints shell-ready snippets
#                                                # LANE_ID=... LANE_TAB_R=... LANE_TAB_G=... LANE_TAB_B=...
#                                                # LANE_ANSI_HEX=... LANE_MODEL=... LANE_ICON=...
#
# Requires: jq. On macOS: brew install jq (or use the system jq present at /usr/bin/jq).

set -eu

# Source this file with its absolute path OR set LANES_SH to the file's path.
# LANES_SH is preferred; LANES_SH_FILE is accepted as an alias.
if [ -z "${LANES_SH:-}" ]; then
  if [ -n "${LANES_SH_FILE:-}" ]; then
    LANES_SH="$LANES_SH_FILE"
    unset LANES_SH_FILE
  else
    echo "lanes.sh: set LANES_SH=<absolute path to lanes.sh> before sourcing" >&2
    return 1 2>/dev/null || exit 1
  fi
fi
_LANES_SH_DIR=$(CDPATH= dirname -- "$LANES_SH")
# Keep LANES_SH in environment so subsequent sourced files inherit the path.
LANES_SH="$_LANES_SH_DIR/lanes.sh"
LANES_JSON="${LANES_JSON:-$_LANES_SH_DIR/lanes.json}"

if [ ! -f "$LANES_JSON" ]; then
  echo "lanes.sh: lanes.json not found at $LANES_JSON" >&2
  return 1 2>/dev/null || exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "lanes.sh: jq is required (brew install jq)" >&2
  return 1 2>/dev/null || exit 1
fi
unset _LANES_SH_DIR

# Resolve a lane or alias to its canonical id. Empty on miss.
lane_resolve() {
  _lr_q="$1"
  jq -r --arg q "$_lr_q" '
    (.lanes[] | select(.id == $q).id) // empty,
    (.lanes[] | .aliases[] | select(. == $q)) as $a
      | (.lanes[] | select(.aliases[] == $a).id) // empty
  ' "$LANES_JSON" 2>/dev/null | head -n1 | grep -v '^$' | head -n1
  unset _lr_q
}

# Print a single field for a lane id OR alias. Empty on miss.
lane_field() {
  _lf_lane="$1"
  _lf_field="$2"
  _lf_canonical=$(lane_resolve "$_lf_lane")
  [ -n "$_lf_canonical" ] || return 1
  jq -r --arg id "$_lf_canonical" --arg f "$_lf_field" '
    (.lanes[] | select(.id == $id)) as $lane
      | if ($lane[$f] | type) == "array" then ($lane[$f] | join(",")) else ($lane[$f] // "" | tostring) end
  ' "$LANES_JSON" 2>/dev/null | head -n1
  unset _lf_lane _lf_field _lf_canonical
}

lane_list() {
  jq -r '.lanes[].id' "$LANES_JSON"
}

# Emit shell variable assignments for a lane (consumed by lane-launcher.sh and
# install.sh scripts). Values are shell-quoted (@sh) so evaling them is safe.
#
# Usage: eval "$(lane_emit_sh zai)"
lane_emit_sh() {
  _le_lane="$1"
  _le_canonical=$(lane_resolve "$_le_lane")
  if [ -z "$_le_canonical" ]; then
    echo "lane_emit_sh: unknown lane: $_le_lane" >&2
    unset _le_lane _le_canonical
    return 1
  fi
  shift
  jq -r --arg id "$_le_canonical" '
    (.lanes[] | select(.id == $id)) as $lane
      | "LANE_ID=" + ($lane.id        | @sh),
        "LANE_LABEL=" + ($lane.label  | @sh),
        "LANE_ICON=" + ($lane.icon    | @sh),
        "LANE_FAMILY=" + ($lane.family | @sh),
        "LANE_MODEL=" + ($lane.model   | @sh),
        "LANE_ANSI_HEX=" + ($lane.ansi_hex | @sh),
        "LANE_TAB_R=" + ($lane.tab_rgb[0] | tostring),
        "LANE_TAB_G=" + ($lane.tab_rgb[1] | tostring),
        "LANE_TAB_B=" + ($lane.tab_rgb[2] | tostring),
        "LANE_PROFILE_DIR_VAR=" + ($lane.profile_dir_var    | @sh),
        "LANE_TAP_BIND=" + ($lane.tap_bind                | @sh),
        "LANE_PROFILE_DEFAULT=" + ($lane.profile_default   | @sh),
        "LANE_TITLE_SUFFIX=" + ($lane.title_suffix         | @sh)
  ' "$LANES_JSON" 2>/dev/null
  # Pass through any extra caller env assignments (already meant to be raw key=value).
  for _le_arg in "$@"; do
    printf 'export %s\n' "$_le_arg"
  done
  unset _le_lane _le_canonical _le_arg
}
