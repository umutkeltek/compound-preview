#!/usr/bin/env sh
set -eu

if [ "$#" -gt 1 ]; then
  echo "usage: scripts/sync-ceremony-overlays.sh [repo-root]" >&2
  exit 2
fi

if [ "$#" -eq 1 ]; then
  repo_root=$(CDPATH= cd -- "$1" && pwd)
else
  repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fi

ceremonies="compound-execute compound-land compound-verify extract-approach"
pending=

cleanup_pending() {
  if [ -n "${pending:-}" ]; then
    rm -f "$pending"
  fi
}

trap cleanup_pending 0
trap 'exit 1' HUP INT TERM

for ceremony in $ceremonies; do
  owner="$repo_root/skills/$ceremony/SKILL.md"
  if [ ! -f "$owner" ]; then
    echo "sync-ceremony-overlays: missing tracked owner $owner" >&2
    exit 1
  fi
done

for overlay in .agents .claude; do
  for ceremony in $ceremonies; do
    adapter_dir="$repo_root/$overlay/skills/$ceremony"
    adapter="$adapter_dir/SKILL.md"
    target="../../../skills/$ceremony/SKILL.md"
    mkdir -p "$adapter_dir"
    pending="$adapter.pending.$$"
    rm -f "$pending"
    ln -s "$target" "$pending"
    mv -f "$pending" "$adapter"
    pending=
  done
done
