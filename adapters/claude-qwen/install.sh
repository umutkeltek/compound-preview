#!/usr/bin/env sh
# adapters/claude-qwen/install.sh — thin wrapper around the shared installer.
# All real work happens in adapters/_lib/install-lane.sh.
set -eu
lane=qwen
lib=$(CDPATH= cd -- "$(dirname -- "$0")/../_lib" && pwd)
exec sh "$lib/install-lane.sh" --lane "$lane" "$@"
