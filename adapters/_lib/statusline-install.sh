#!/usr/bin/env sh
# adapters/_lib/statusline-install.sh
#
# Merge a `statusLine` entry into a Claude Code profile's settings.json.
#
# Usage:
#   statusline-install.sh --profile-dir DIR --lane ID [--command-path PATH]
#
# Defaults --command-path to the sibling lane-statusline.sh if LANES_SH_FILE
# (via lanes.sh) is configured. The merge is idempotent: re-running with the
# same lane/no-op keeps the existing entry verbatim.

set -eu

profile_dir=""
lane=""
command_path=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --profile-dir)  profile_dir=$2;  shift 2 ;;
    --lane)         lane=$2;         shift 2 ;;
    --command-path) command_path=$2; shift 2 ;;
    *)              echo "usage: statusline-install.sh --profile-dir DIR --lane ID [--command-path PATH]" >&2; exit 2 ;;
  esac
done

if [ -z "$profile_dir" ] || [ -z "$lane" ]; then
  echo "statusline-install: --profile-dir and --lane are required" >&2
  exit 2
fi

# Resolve command_path if not provided.
if [ -z "$command_path" ]; then
  if [ -z "${LANES_SH_FILE:-}" ]; then
    echo "statusline-install: LANES_SH_FILE or --command-path is required" >&2
    exit 2
  fi
  _lib_dir=$(CDPATH= dirname -- "$LANES_SH_FILE")
  command_path="$_lib_dir/lane-statusline.sh"
  unset _lib_dir
fi

if [ ! -x "$command_path" ] && [ ! -f "$command_path" ]; then
  echo "statusline-install: command does not exist: $command_path" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "statusline-install: jq is required" >&2
  exit 1
fi

settings="$profile_dir/settings.json"
if [ ! -f "$settings" ]; then
  # Profile never had settings.json (no compound hook install yet). Initialize.
  mkdir -p "$profile_dir"
  printf '{}' > "$settings"
fi

# Atomic merge with jq. Idempotent in the strict sense: re-running with the
# same --lane produces a byte-identical settings.json. Re-running with a
# different --lane fully replaces the statusLine entry.
tmp=$settings.compound-statusline-merge.tmp
new_command="$command_path --lane $lane"
jq --arg cmd "$new_command" '
  def is_statusline:
    .statusLine
    and (.statusLine | type) == "object"
    and (.statusLine.type // "command") == "command"
    and (.statusLine.command // "") == $cmd;
  if is_statusline then .
  else . + { statusLine: { type: "command", command: $cmd } }
  end
' "$settings" > "$tmp"
mv "$tmp" "$settings"

# Make sure the command is executable.
chmod +x "$command_path" 2>/dev/null || true

echo "statusline installed (lane=$lane, command=$command_path)"
