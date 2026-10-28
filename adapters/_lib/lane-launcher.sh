#!/usr/bin/env sh
# adapters/_lib/lane-launcher.sh
#
# Auto-generates shell wrapper functions for each lane. After sourcing,
# your shell gets one function per lane:
#
#   claude-zai()         claude-minimax()     claude-qwen()
#   claude-codex()       claude-native()      codex-native()
#
# Each wrapper:
#   1. Reads lane identity (LANE_ID, LANE_TAB_R/G/B, LANE_PROFILE_DEFAULT,
#      LANE_PROFILE_DIR_VAR, LANE_TAP_BIND, ...) from lanes.json via lanes.sh.
#   2. Sets CLAUDE_CONFIG_DIR to the per-lane profile dir (so every lane keeps
#      isolated settings/history).
#   3. Sets ANTHROPIC_BASE_URL to the matching Switchback tap, OR keeps it
#      unset for native lanes.
#   4. Emits iTerm2 tab color + title + badge via it2_apply_lane().
#   5. Launches the right binary (claude from $HOME/.local/bin/claude,
#      or /opt/homebrew/bin/codex for codex-family).
#
# To install in your zshrc, add (path = wherever you keep the compound
# checkout):
#
#     LANE_LAUNCHER_FILE="$HOME/path/to/compound/adapters/_lib/lane-launcher.sh"
#     LANE_IT2_FILE="$HOME/path/to/compound/adapters/_lib/lane-it2.sh"
#     . "$LANE_LAUNCHER_FILE"
#
# Then `claude-zai`, `claude-minimax`, `claude-qwen`, etc. become commands.

set -eu

if [ -z "${LANE_LAUNCHER_FILE:-}" ]; then
  echo "lane-launcher.sh: set LANE_LAUNCHER_FILE before sourcing" >&2
  return 1 2>/dev/null || exit 1
fi
_LL_DIR=$(CDPATH= dirname -- "$LANE_LAUNCHER_FILE")

LANES_SH="${LANES_SH:-$_LL_DIR/lanes.sh}"
LANE_IT2_SH="${LANE_IT2_SH:-$_LL_DIR/lane-it2.sh}"
. "$LANES_SH"
. "$LANE_IT2_SH"
unset _LL_DIR LANE_LAUNCHER_FILE

# Locator for the per-lane settings/profile dir. Honors a user-set env var
# before falling back to the registry's profile_default.
_lane_profile_dir() {
  _var_name="$1"
  _fallback="$2"
  if [ -n "$_var_name" ] && [ -n "${!_var_name:-}" ]; then
    printf '%s\n' "${!_var_name}"
  elif [ -n "$_fallback" ]; then
    # Expand ${HOME} placeholders in the registry value without eval. Only the literal
    # ${HOME} substring is replaced; no other shell expansion or command substitution
    # can run.
    while case "$_fallback" in *'${HOME}'*) true;; *) false;; esac; do
      _prefix=${_fallback%%'${HOME}'*}
      _suffix=${_fallback#*'${HOME}'}
      _fallback="$_prefix$HOME$_suffix"
    done
    printf '%s\n' "$_fallback"
  fi
  unset _var_name _fallback _prefix _suffix
}

# Validate that a tap bind is host:port with a numeric port. Empty is allowed (native
# lanes), but anything non-empty must be well-formed before it becomes a URL.
_lane_tap_bind_is_valid() {
  _ltb_value="$1"
  if [ -z "$_ltb_value" ]; then
    unset _ltb_value
    return 0
  fi
  if printf '%s\n' "$_ltb_value" | grep -Eq '^[A-Za-z0-9._-]+:[0-9]+$'; then
    unset _ltb_value
    return 0
  fi
  unset _ltb_value
  return 1
}

# Generic Claude-Code lane launcher.
#
# Use this for any lane whose `family` is "claude_code". The function sets the
# right tap upstream + isolated profile dir + iTerm2 visuals, then execs the
# Claude Code binary with passthrough args.
#
# `claude-zai`, `claude-minimax`, etc. are wrappers around this.
_launch_claude_lane() {
  _lane="$1"; shift
  eval "$(lane_emit_sh "$_lane")"
  it2_apply_lane "$_lane" "${PWD##*/}"

  profile_dir=$(_lane_profile_dir "$LANE_PROFILE_DIR_VAR" "$LANE_PROFILE_DEFAULT")
  if [ -n "$profile_dir" ]; then
    mkdir -p "$profile_dir"
    export CLAUDE_CONFIG_DIR="$profile_dir"
  fi

  if [ -n "$LANE_TAP_BIND" ]; then
    if ! _lane_tap_bind_is_valid "$LANE_TAP_BIND"; then
      echo "lane-launcher.sh: invalid LANE_TAP_BIND ${LANE_TAP_BIND}; expected host:port with a numeric port" >&2
      return 1
    fi
    export ANTHROPIC_BASE_URL="http://${LANE_TAP_BIND}"
  fi

  export LANE_ID LANE_LABEL LANE_MODEL
  unset LANE_FAMILY LANE_ICON LANE_ANSI_HEX LANE_TAB_R LANE_TAB_G LANE_TAB_B \
        LANE_PROFILE_DIR_VAR LANE_TAP_BIND LANE_PROFILE_DEFAULT LANE_TITLE_SUFFIX

  # Run the local Claude binary if present, else fall back to /opt/homebrew.
  if [ -x "$HOME/.local/bin/claude" ]; then
    command "$HOME/.local/bin/claude" "$@"
  elif command -v claude >/dev/null 2>&1; then
    command claude "$@"
  else
    echo "claude binary not found in PATH" >&2
    return 127
  fi
  unset _lane profile_dir
}

# codex-family launcher: keeps Codex's full TUI but applies the lane's color
# and badge so visually it doesn't collide with the Claude lanes.
#
# Per-invocation CODEX_HOME isolation: every launch gets a fresh temp
# dir under $TMPDIR (or /tmp) so concurrent codex invocations — the
# scout TUI plus a parallel exec, two parallel exec, anything — do not
# share state through ~/.codex. Mirrors the spirit of
# _launch_claude_lane's per-lane CLAUDE_CONFIG_DIR but uses mktemp for
# true per-invocation isolation rather than per-lane persistence. The
# temp dir is left for OS temp-rotation to reclaim; if mktemp fails the
# launcher proceeds without isolation (worst case: weaker, but the lane
# still runs).
_launch_codex_lane() {
  _lane="$1"; shift
  eval "$(lane_emit_sh "$_lane")"
  it2_apply_lane "$_lane" "${PWD##*/}"
  export LANE_ID LANE_LABEL

  unset LANE_FAMILY LANE_ICON LANE_ANSI_HEX LANE_TAB_R LANE_TAB_G LANE_TAB_B \
        LANE_PROFILE_DIR_VAR LANE_TAP_BIND LANE_PROFILE_DEFAULT LANE_TITLE_SUFFIX

  if _codex_home=$(mktemp -d "${TMPDIR:-/tmp}/codex.XXXXXX" 2>/dev/null) \
     && [ -n "$_codex_home" ]; then
    export CODEX_HOME="$_codex_home"
  fi
  unset _codex_home

  if command -v codex >/dev/null 2>&1; then
    command codex "$@"
  else
    echo "codex binary not found in PATH" >&2
    return 127
  fi
  unset _lane
}

# Auto-generate per-lane functions. `claude-zai`, `claude-minimax`, etc.
#
# We use the zsh `function <name>() { ... }` form (works with hyphens, which
# the lane ids require). zsh's `function` builtin accepts names that POSIX sh
# rejects, so the user-facing command names match the lane registry.
#
# Use a newline-aware iterator because in zsh, `for x in $foo` does NOT word-
# split on newlines (unlike bash). A loop reading newline-delimited lines is
# portable across both shells.
_lane_list=$(lane_list)
# In zsh, `for x in $var` does NOT split on whitespace even when IFS
# contains newline; that option is controlled by SH_WORD_SPLIT, which by
# default is OFF in zsh (and ON in bash). Enable it explicitly so this loop
# iterates one lane per iteration under both shells.
if [ -n "${ZSH_VERSION:-}" ]; then
  setopt local_options sh_word_split 2>/dev/null || true
fi
_old_IFS=${IFS:- }
IFS='
'
for _l in $_lane_list; do
  _fam=$(lane_field "$_l" family)

  # Decide (a) the launcher to use and (b) the set of command names to define.
  # For claude_code lanes, both `claude-<id>` and any aliases from the registry
  # become commands. For codex lanes, only the bare id (and aliases) — they
  # keep the `codex` family prefix-less (matches the existing shell usage).
  case "$_fam" in
    claude_code) _launcher=_launch_claude_lane; _prefix="claude-" ;;
    codex)       _launcher=_launch_codex_lane;  _prefix=""      ;;
    *)           _launcher=_launch_claude_lane; _prefix="claude-" ;;
  esac

  # Build the list of names that map to this lane.
  _aliases=$(jq -r --arg id "$_l" '.lanes[] | select(.id == $id).aliases[]' "$LANES_JSON" 2>/dev/null) || _aliases=""
  _names="${_prefix}${_l}"
  if [ -n "$_aliases" ]; then
    while IFS= read -r _a; do
      [ -z "$_a" ] && continue
      # Skip aliases that already match the canonical id.
      [ "$_a" = "$_l" ] && continue
      # If the alias already starts with the prefix, use it as-is. This
      # avoids creating things like `claude-claude-zai` from the alias
      # `claude-zai` mapped to the canonical `zai` lane.
      case "$_a" in
        "${_prefix}"*) _add_name="$_a" ;;
        *)              _add_name="${_prefix}${_a}" ;;
      esac
      _names="${_names}
${_add_name}"
    done <<EOF
$_aliases
EOF
  fi

  while IFS= read -r _gen; do
    [ -z "$_gen" ] && continue
    eval "function ${_gen}() { ${_launcher} '${_l}' \"\$@\"; }"
  done <<EOF
$_names
EOF
done
IFS=$_old_IFS
unset _l _fam _gen _launcher _lane_list _old_IFS _names _aliases _prefix _a _add_name

# Also expose `lane-use <id>` for ad-hoc switching of the current pane
# (sets env + iTerm2 visuals without launching a binary).
lane-use() {
  if [ "$#" -lt 1 ]; then
    echo "usage: lane-use <lane> [--no-it2]" >&2
    return 2
  fi
  _l="$1"; shift
  eval "$(lane_emit_sh "$_l")"
  export LANE_ID LANE_LABEL LANE_MODEL LANE_ICON LANE_ANSI_HEX \
         LANE_TAB_R LANE_TAB_G LANE_TAB_B
  case " $* " in *" --no-it2 "*) ;; *) it2_apply_lane "$_l" "${PWD##*/}" ;; esac
  unset _l
}
