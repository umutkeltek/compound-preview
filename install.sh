#!/usr/bin/env sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
cd "$repo_root"

"$repo_root/scripts/sync-ceremony-overlays.sh" "$repo_root"

if [ -n "${GOBIN:-}" ]; then
  install_dir=$GOBIN
else
  if [ -z "${HOME:-}" ]; then
    echo "install: HOME is required when GOBIN is not set" >&2
    exit 1
  fi
  install_dir=$HOME/.local/bin
fi

if [ -z "${COMPOUND_HOME:-}" ]; then
  if [ -z "${HOME:-}" ]; then
    echo "install: COMPOUND_HOME or HOME is required" >&2
    exit 1
  fi
  compound_home=$HOME/.compound
else
  compound_home=$COMPOUND_HOME
fi

mkdir -p "$install_dir"

os_alias=$install_dir/os
install_os_alias=${COMPOUND_INSTALL_OS_ALIAS:-0}
case "$install_os_alias" in
  0|1) ;;
  *)
    echo "install: COMPOUND_INSTALL_OS_ALIAS must be 0 or 1" >&2
    exit 1
    ;;
esac
install_autonomy_lab=${COMPOUND_INSTALL_AUTONOMY_LAB:-0}
case "$install_autonomy_lab" in
  0|1) ;;
  *)
    echo "install: COMPOUND_INSTALL_AUTONOMY_LAB must be 0 or 1" >&2
    exit 1
    ;;
esac
if [ "$install_os_alias" = 1 ]; then
  if [ -L "$os_alias" ]; then
    os_alias_target=$(readlink "$os_alias")
    case "$os_alias_target" in
      compound|"$install_dir/compound") ;;
      *)
        echo "install: refusing to replace foreign os symlink: $os_alias -> $os_alias_target" >&2
        exit 1
        ;;
    esac
  elif [ -e "$os_alias" ]; then
    if [ ! -f "$os_alias" ]; then
      echo "install: refusing to replace non-file os path: $os_alias" >&2
      exit 1
    fi
    os_alias_body=$(cat "$os_alias")
    case "$os_alias_body" in
      '#!/bin/sh'*'shim only'*'operating-system/umut-os/exec-lane/os.ts'*) ;;
      *)
        echo "install: refusing to replace foreign os file: $os_alias" >&2
        exit 1
        ;;
    esac
  fi
fi
build_sha=$(git -C "$repo_root" rev-parse --short HEAD 2>/dev/null || echo "")
install_dir=$(CDPATH='' cd -- "$install_dir" && pwd -P)
case "$compound_home" in /*) ;; *) compound_home=$repo_root/$compound_home ;; esac
# Recompute the alias after resolving a relative GOBIN.
os_alias=$install_dir/os
umask 077

fail() { echo "install: $*" >&2; exit 1; }
[ -n "${HOME:-}" ] || fail "HOME is required for installed profile refresh"

# Share the immutable release installer's persistent kernel lock. Source installs
# never interpret or recover its journal; only that existing owner may do so.
state_root=$install_dir/.compound-install-state
case "$(uname -s)" in
  Darwin) stat_mode() { stat -f '%Lp' "$1"; }; stat_uid() { stat -f '%u' "$1"; }; stat_links() { stat -f '%l' "$1"; } ;;
  Linux) stat_mode() { stat -c '%a' "$1"; }; stat_uid() { stat -c '%u' "$1"; }; stat_links() { stat -c '%h' "$1"; } ;;
  *) fail "source installation requires Darwin or Linux install locking" ;;
esac
if [ ! -e "$state_root" ] && [ ! -L "$state_root" ]; then mkdir -m 0700 "$state_root"; fi
[ -d "$state_root" ] && [ ! -L "$state_root" ] &&
  [ "$(stat_mode "$state_root")" = 700 ] &&
  [ "$(stat_uid "$state_root")" = "$(id -u)" ] || fail "install state is not a private owned directory"
lock_file=$state_root/lock
if [ ! -e "$lock_file" ] && [ ! -L "$lock_file" ]; then
  for entry in "$state_root"/* "$state_root"/.[!.]* "$state_root"/..?*; do
    if [ -e "$entry" ] || [ -L "$entry" ]; then fail "release recovery state has no lock; use the release installer"; fi
  done
  (set -C; : > "$lock_file") 2>/dev/null || :
fi
[ -f "$lock_file" ] && [ ! -L "$lock_file" ] &&
  [ "$(stat_mode "$lock_file")" = 600 ] &&
  [ "$(stat_uid "$lock_file")" = "$(id -u)" ] &&
  [ "$(stat_links "$lock_file")" = 1 ] || fail "install lock is not a private owned regular file"
exec 9>"$lock_file"
case "$(uname -s)" in
  Darwin) lockf -s -t 0 9 || fail "another installer holds the install lock" ;;
  Linux) flock -n 9 || fail "another installer holds the install lock" ;;
esac
for entry in "$state_root"/* "$state_root"/.[!.]* "$state_root"/..?*; do
  [ "$entry" = "$lock_file" ] && continue
  if [ -e "$entry" ] || [ -L "$entry" ]; then fail "pending release recovery state; run the immutable release installer first"; fi
done
# A killed process cannot run a trap. Preserve its retained bytes and refuse a
# new install until the operator resolves that interrupted source installation.
for entry in "$install_dir"/.compound-source-stage.*; do
  if [ -e "$entry" ] || [ -L "$entry" ]; then fail "interrupted source installation retained at $entry; inspect before retrying"; fi
done

artifacts="compound breadth"
if [ "$install_autonomy_lab" = 1 ]; then artifacts="$artifacts heartbeat_lib heartbeat attention"; fi
if [ "$install_os_alias" = 1 ]; then artifacts="$artifacts alias"; fi
artifact_target() {
  case "$1" in
    compound) target=$install_dir/compound ;;
    breadth) target=$install_dir/compound-breadth-engine ;;
    heartbeat_lib) target=$install_dir/_lib/heartbeat-promote-stage.sh ;;
    heartbeat) target=$install_dir/compound-autonomy-heartbeat.sh ;;
    attention) target=$compound_home/scripts/attention-letter.py ;;
    alias) target=$os_alias ;;
    *) return 1 ;;
  esac
  incoming=$target.incoming.$$
}

stage=$(mktemp -d "$install_dir/.compound-source-stage.XXXXXX")
published=
prepared=
activation_started=0
completed=0
cleanup() {
  status=$?
  trap - 0 HUP INT TERM
  set +e
  rollback_failed=0
  if [ "$completed" != 1 ]; then
    for key in $published; do
      artifact_target "$key"
      if ! rm -f "$incoming"; then rollback_failed=1; continue; fi
      if [ -e "$stage/previous/$key" ] || [ -L "$stage/previous/$key" ]; then
        if ! cp -pP "$stage/previous/$key" "$incoming" || ! mv -f "$incoming" "$target"; then rollback_failed=1; fi
      elif ! rm -f "$target"; then
        rollback_failed=1
      fi
    done
    if [ -n "$published" ]; then
      if [ "$rollback_failed" = 0 ]; then
        echo "install: publication rolled back; previous artifact bytes, modes and absence restored" >&2
      else
        echo "install: rollback INCOMPLETE; retained previous/new artifacts at $stage" >&2
      fi
    fi
    if [ "$activation_started" = 1 ]; then
      echo "install: activation incomplete; home/profile changes may remain. Inspect binding state before retrying." >&2
    fi
  fi
  for key in $prepared; do artifact_target "$key"; rm -f "$incoming"; done
  if [ "$rollback_failed" = 0 ]; then rm -rf "$stage"; fi
  if [ "$completed" != 1 ] && [ "$status" = 0 ]; then status=1; fi
  exit "$status"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM
mkdir "$stage/new" "$stage/previous"

# Build and validate the entire enabled file set before replacing any target.
go build -ldflags "-X main.buildSHA=$build_sha" -o "$stage/new/compound" ./cmd/compound
chmod 0755 "$stage/new/compound"
go run ./cmd/compound-install-verify --binary "$stage/new/compound" --build-sha "$build_sha"
go build -o "$stage/new/breadth" ./cmd/compound-breadth-engine
chmod 0755 "$stage/new/breadth"
breadth_nonce=source-install-$$
breadth_probe=$(printf '{"schema_version":1,"request_id":"%s","op":"capabilities"}\n' "$breadth_nonce" |
  COMPOUND_HOME='' COMPOUND_BREADTH_DISABLE_VECTOR=1 "$stage/new/breadth") || fail "breadth engine capabilities probe failed"
case "$breadth_probe" in
  '{"ok":true,"schema_version":1,"request_id":"'"$breadth_nonce"'","name":"compound-breadth","breadth":true,'*) ;;
  *) fail "breadth engine returned invalid capabilities" ;;
esac
if [ "$install_os_alias" = 1 ]; then ln -s compound "$stage/new/alias"; fi
if [ "$install_autonomy_lab" = 1 ]; then
  cp "$repo_root/scripts/_lib/heartbeat-promote-stage.sh" "$stage/new/heartbeat_lib"
  chmod 0644 "$stage/new/heartbeat_lib"
  sh -n "$stage/new/heartbeat_lib"
  sh -c '. "$1"; command -v promote_driver_stage_decide >/dev/null' _ "$stage/new/heartbeat_lib"
  cp "$repo_root/scripts/compound-autonomy-heartbeat.sh" "$stage/new/heartbeat"
  chmod 0755 "$stage/new/heartbeat"
  sh -n "$stage/new/heartbeat"
  cp "$repo_root/scripts/attention-letter.py" "$stage/new/attention"
  chmod 0755 "$stage/new/attention"
  python3 -c 'import ast,sys; ast.parse(open(sys.argv[1]).read())' "$stage/new/attention"
fi

# Existing-home health remains a refusal gate, not a cleanup mechanism.
if [ -d "$compound_home/config" ]; then
  if ! PATH="$install_dir:$PATH" COMPOUND_HOME="$compound_home" "$stage/new/compound" home doctor --home "$compound_home" >"$stage/precheck" 2>&1; then
    sed 's/^/  /' "$stage/precheck" >&2
    fail "pre-publication home doctor FAILED; nothing was published"
  fi
fi

# Refuse unsupported required adapter owners before any home/profile activation
# or publication. A successful preflight proves eligibility, not activation.
if ! PATH="$install_dir:$PATH" COMPOUND_HOME="$compound_home" "$stage/new/compound" harness refresh-lane --preflight --base "$HOME" --binary "$install_dir/compound" >"$stage/preflight" 2>&1; then
  cat "$stage/preflight" >&2
  fail "pre-publication refresh preflight FAILED; no home/profile activation or artifacts published"
fi

# Initialize through the verified staged CLI before adding anything to a fresh
# home: home init correctly refuses a populated path without its manifest.
# This is an activation effect, reported honestly even if later publication fails.
activation_started=1
PATH="$install_dir:$PATH" COMPOUND_HOME="$compound_home" "$stage/new/compound" home init --home "$compound_home" --global || fail "activation failed at home_init"

# Retain the complete preimage, then prepare a sibling temporary file at EVERY
# target. Cross-filesystem helper copies and invalid target types fail here.
for key in $artifacts; do
  artifact_target "$key"
  if [ -e "$target" ] || [ -L "$target" ]; then
    if [ "$key" != alias ] && [ -L "$target" ]; then fail "refusing symlink artifact: $target"; fi
    [ -f "$target" ] || [ -L "$target" ] || fail "refusing non-file artifact: $target"
    cp -pP "$target" "$stage/previous/$key"
  fi
  mkdir -p "$(dirname "$target")"
  [ ! -e "$incoming" ] && [ ! -L "$incoming" ] || fail "refusing existing staging path: $incoming"
  prepared="$key $prepared"
  cp -pP "$stage/new/$key" "$incoming"
done
# The shared lock excludes cooperating source/release installers. Consumers must
# be quiesced: the file set still uses sequential renames, never an atomic switch.
for key in $artifacts; do
  artifact_target "$key"
  published="$key $published"
  mv -f "$incoming" "$target"
done
echo "install: publication: complete"

PATH="$install_dir:$PATH" COMPOUND_HOME="$compound_home" "$install_dir/compound" home doctor --home "$compound_home" || fail "activation failed at home_doctor"
PATH="$install_dir:$PATH" COMPOUND_HOME="$compound_home" "$install_dir/compound" runtime doctor "$repo_root/fixtures/runtime/synthetic-service.yaml" || fail "activation failed at runtime_doctor"
# Preserve the refresh command's status; a pipeline ending in sed hid failures.
if ! PATH="$install_dir:$PATH" COMPOUND_HOME="$compound_home" "$install_dir/compound" harness refresh-lane --base "$HOME" --binary "$install_dir/compound" >"$stage/refresh" 2>&1; then
  cat "$stage/refresh" >&2
  fail "activation failed at harness_refresh"
fi
cat "$stage/refresh"
completed=1
cat <<EOF

compound installed
- publication: complete
- activation: complete
- binary: $install_dir/compound
- breadth_engine: $install_dir/compound-breadth-engine
- os_alias: opt-in via COMPOUND_INSTALL_OS_ALIAS=1 (existing path left unchanged otherwise)
- autonomy_lab: opt-in via COMPOUND_INSTALL_AUTONOMY_LAB=1
- autonomy_heartbeat: $install_dir/compound-autonomy-heartbeat.sh (when opted in)
- attention_letter: $compound_home/scripts/attention-letter.py (when opted in; max 3 decisions per cycle)
- home: $compound_home

next:
  compound workbench
  compound proof-check --json
  cd console && bun install
  compound console --operator
EOF
