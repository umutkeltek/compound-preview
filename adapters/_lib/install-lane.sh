#!/usr/bin/env sh
# adapters/_lib/install-lane.sh
#
# Unified installer for any Claude-flavored lane. Calls `compound hook install`
# to write the harness-authority hooks, then merges the lane-themed statusline
# into the profile's settings.json, then symlinks global steering.
#
# Usage:
#   install-lane.sh --lane <id> [--profile-dir DIR] [--compound PATH]
#                          [--steering-source FILE] [--hook-registration-ts FILE]
#
# Defaults:
#   --profile-dir        $CLAUDE_CONFIG_DIR OR
#                        <lanes.json profile_default> for the lane
#   --compound           $HOME/.local/bin/compound
#   --steering-source    $HOME/.claude/CLAUDE.md
#   --hook-registration-ts   sibling ../hook-registration.ts if present
#
# Idempotent: re-running for the same lane is a no-op; re-running with a
# different lane rewires the statusline command.

set -eu

lane=""
profile_dir=""
compound_bin=""
steering_source=""
hook_reg=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --lane)                lane=$2; shift 2 ;;
    --profile-dir)         profile_dir=$2; shift 2 ;;
    --compound)            compound_bin=$2; shift 2 ;;
    --steering-source)     steering_source=$2; shift 2 ;;
    --hook-registration-ts) hook_reg=$2; shift 2 ;;
    -h|--help)
      echo "usage: install-lane.sh --lane ID [--profile-dir DIR] [--compound PATH] [--steering-source FILE] [--hook-registration-ts FILE]" >&2
      exit 0 ;;
    *) echo "install-lane: unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$lane" ]; then
  echo "install-lane: --lane is required" >&2
  exit 2
fi
if [ -z "${HOME:-}" ]; then echo "install-lane: HOME is required" >&2; exit 1; fi

_lib_dir=$(CDPATH= dirname -- "${INSTALL_LANE_FILE:-$0}")
LANES_SH_FILE="$_lib_dir/lanes.sh"
. "$LANES_SH_FILE"

# Resolve lane metadata.
eval "$(lane_emit_sh "$lane")"
if [ -z "${LANE_ID:-}" ]; then
  echo "install-lane: unknown lane: $lane" >&2
  exit 2
fi

if [ -z "$profile_dir" ]; then
  if [ -n "${CLAUDE_CONFIG_DIR:-}" ]; then
    profile_dir=$CLAUDE_CONFIG_DIR
  else
    eval "profile_dir=\"$LANE_PROFILE_DEFAULT\""
  fi
fi
if [ -z "$compound_bin" ]; then
  compound_bin=$HOME/.local/bin/compound
fi
if [ -z "$steering_source" ]; then
  steering_source=$HOME/.claude/CLAUDE.md
fi
if [ -z "$hook_reg" ]; then
  for _candidate in "$_lib_dir/../$LANE_ID" "$_lib_dir/../claude-$LANE_ID"; do
    if [ -f "$_candidate/hook-registration.ts" ]; then
      hook_reg="$_candidate/hook-registration.ts"
      break
    fi
  done
fi

if [ ! -x "$compound_bin" ]; then
  echo "install-lane: compound binary not executable: $compound_bin" >&2
  exit 1
fi
if [ ! -f "$steering_source" ]; then
  echo "install-lane: global steering source missing: $steering_source" >&2
  exit 1
fi
if [ -z "$hook_reg" ] || [ ! -f "$hook_reg" ]; then
  echo "install-lane: hook-registration.ts not found for lane '$lane' (looked near $_lib_dir/../)" >&2
  exit 1
fi

mkdir -p "$profile_dir"

# Resolve THIS lane's hook harness from lanes.json. Every lane that reaches
# install-lane.sh is a claude_code-family isolated profile (its own
# --config-dir), but the hook HARNESS is lane-specific: core `compound hook
# install` enrolls each managed harness separately and gates --config-dir to
# claude-zai, claude-minimax, and claude-qwen. So we must install the lane's
# REAL harness:
#
#   zai / zai-direct -> claude-zai     (the Claude Code hook roster)
#   codex            -> claude-zai     (claude_code-family profile; its hook
#                                       ROSTER is the Claude Code set, exposed
#                                       by core as claude-zai. Lane identity is
#                                       still carried by --config-dir and the
#                                       statusline. Intentional + documented.)
#   minimax          -> claude-minimax (its own enrolled harness)
#   qwen             -> claude-qwen    (its own enrolled harness)
#   others           -> "" => FAIL     (core has no such harness with
#                                       --config-dir yet; do NOT silently
#                                       collapse it into claude-zai)
#
# Collapsing every lane into claude-zai breaks executor identity, hook
# attribution, capability floors, and RunAttempt actor/lane routing.
hook_harness=$(lane_field "$lane" hook_harness)
if [ -z "$hook_harness" ]; then
  echo "install-lane: lane '$lane' has no supported hook harness (core 'compound hook install' does not enroll one with --config-dir yet); refusing to install" >&2
  exit 1
fi
"$compound_bin" hook install --harness "$hook_harness" --config-dir "$profile_dir" --binary "$compound_bin"

LANES_SH_FILE="$_lib_dir/lanes.sh" \
LANE_STATUSLINE_FILE="$_lib_dir/lane-statusline.sh" \
  sh "$_lib_dir/statusline-install.sh" --profile-dir "$profile_dir" --lane "$lane"

_steering_target="$profile_dir/CLAUDE.md"
if [ -e "$_steering_target" ] && [ ! -L "$_steering_target" ]; then
  echo "install-lane: refusing to replace non-symlink steering: $_steering_target" >&2
  exit 1
fi
if [ -L "$_steering_target" ]; then
  _curr=$(readlink "$_steering_target")
  if [ "$_curr" != "$steering_source" ]; then
    ln -sfn "$steering_source" "$_steering_target"
  fi
else
  ln -s "$steering_source" "$_steering_target"
fi
unset _steering_target _curr _candidate

echo "lane '$lane' profile installed"
echo "- settings: $profile_dir/settings.json"
echo "- steering: symlinked to $steering_source"
echo "- authority: $hook_reg"
echo "- statusline: $_lib_dir/lane-statusline.sh --lane $lane"

