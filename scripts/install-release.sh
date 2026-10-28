#!/usr/bin/env sh
# Install Compound from one exact public-release candidate.
#
# SHA256SUMS provides release-set consistency and corruption detection. It is
# not an independent publisher signature; see docs/getting-started.md for the
# optional GitHub attestation check that authenticates the release origin.
set -eu
umask 077

die() {
  printf 'install-release: %s\n' "$1" >&2
  exit 1
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
    return
  fi
  die "sha256sum or shasum is required for release verification"
}

sha256_text() {
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "$1" | sha256sum | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    printf '%s' "$1" | shasum -a 256 | awk '{print $1}'
    return
  fi
  die "sha256sum or shasum is required for release verification"
}

is_sha256() {
  [ "${#1}" -eq 64 ] || return 1
  case "$1" in
    *[!0-9a-f]*) return 1 ;;
  esac
}

is_commit() {
  [ "${#1}" -eq 40 ] || return 1
  case "$1" in
    *[!0-9a-f]*) return 1 ;;
  esac
}

is_nonce() {
  [ "${#1}" -eq 32 ] || return 1
  case "$1" in
    *[!0-9a-f]*) return 1 ;;
  esac
}

new_nonce() {
  command -v od >/dev/null 2>&1 || die "od is required for install transaction identity"
  nonce_value=$(od -An -N16 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n')
  is_nonce "$nonce_value" || die "cannot create install transaction nonce"
  printf '%s\n' "$nonce_value"
}

path_mode() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then
    stat -f '%Lp' "$1"
  else
    stat -c '%a' "$1"
  fi
}

path_uid() {
  if stat -f '%u' "$1" >/dev/null 2>&1; then
    stat -f '%u' "$1"
  else
    stat -c '%u' "$1"
  fi
}

path_links() {
  if stat -f '%l' "$1" >/dev/null 2>&1; then
    stat -f '%l' "$1"
  else
    stat -c '%h' "$1"
  fi
}

private_regular_file() {
  private_path=$1
  private_mode=$2
  [ -f "$private_path" ] && [ ! -L "$private_path" ] || return 1
  [ "$(path_mode "$private_path")" = "$private_mode" ] || return 1
  [ "$(path_uid "$private_path")" = "$current_uid" ] || return 1
  [ "$(path_links "$private_path")" = 1 ] || return 1
}

is_repository_identity() {
  repository=$1
  case "$repository" in
    */*) ;;
    *) return 1 ;;
  esac
  repository_owner=${repository%%/*}
  repository_name=${repository#*/}
  [ -n "$repository_owner" ] && [ -n "$repository_name" ] || return 1
  [ "$repository" = "$repository_owner/$repository_name" ] || return 1
  case "$repository_owner$repository_name" in
    *[!A-Za-z0-9_.-]*) return 1 ;;
  esac
}

crash_at() {
  if [ "${COMPOUND_INSTALL_CRASH_AT:-}" = "$1" ]; then
    kill -9 "$$"
  fi
}

is_stable_semver() {
  printf '%s\n' "$1" | awk '
    $0 ~ /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ { ok = 1 }
    END { if (!ok) exit 1 }
  '
}

checksum_for() {
  sums=$1
  target=$2
  value=$(awk -v target="$target" '
    NF == 0 { next }
    NF != 2 { exit 2 }
    {
      name = $2
      sub(/^\*/, "", name)
      if (name == target) {
        count++
        digest = $1
      }
    }
    END {
      if (count != 1) exit 1
      print digest
    }
  ' "$sums") || return 1
  is_sha256 "$value" || return 1
  printf '%s\n' "$value"
}

# The installer verifies itself before detecting a platform, downloading a
# manifest, or touching an installation target. Piped/stdin execution has no
# immutable file to verify and is intentionally refused.
self_path=$0
if [ ! -f "$self_path" ] || [ -L "$self_path" ]; then
  die "installer must be executed from a regular install-release.sh file, not stdin or a symlink"
fi
self_name=$(basename "$self_path")
[ "$self_name" = "install-release.sh" ] || die "installer filename must be install-release.sh"
self_dir=$(unset CDPATH; cd "$(dirname "$self_path")" && pwd -P) || die "cannot resolve installer directory"
self_path=$self_dir/install-release.sh
source_sums=$self_dir/SHA256SUMS
if [ ! -f "$source_sums" ] || [ -L "$source_sums" ]; then
  die "SHA256SUMS is required beside install-release.sh"
fi
self_expected=$(checksum_for "$source_sums" install-release.sh) || die "SHA256SUMS has no unique valid installer entry"
self_actual=$(sha256_file "$self_path")
[ "$self_expected" = "$self_actual" ] || die "installer checksum mismatch"

invocation_dir=$(pwd -P) || die "cannot resolve invocation directory"
if [ -n "${COMPOUND_RELEASE_TARBALL:-}" ]; then
  case "$COMPOUND_RELEASE_TARBALL" in
    /*) : ;;
    *) COMPOUND_RELEASE_TARBALL=$invocation_dir/$COMPOUND_RELEASE_TARBALL ;;
  esac
fi
if [ -n "${COMPOUND_HOME:-}" ]; then
  case "$COMPOUND_HOME" in
    /*) compound_home=$COMPOUND_HOME ;;
    *) compound_home=$invocation_dir/$COMPOUND_HOME ;;
  esac
elif [ -n "${HOME:-}" ]; then
  compound_home=$HOME/.compound
else
  die "COMPOUND_HOME or HOME is required"
fi

requested_version=${COMPOUND_VERSION:-}
if [ -n "$requested_version" ] && ! is_stable_semver "$requested_version"; then
  die "COMPOUND_VERSION must be strict stable SemVer"
fi
release_url=
if [ -z "${COMPOUND_RELEASE_TARBALL:-}" ]; then
  [ -n "$requested_version" ] || die "COMPOUND_VERSION is required for an exact tagged download"
  if [ -n "${COMPOUND_RELEASE_URL:-}" ]; then
    release_url=$COMPOUND_RELEASE_URL
  else
    release_repository=${COMPOUND_RELEASE_REPOSITORY:-}
    is_repository_identity "$release_repository" || die "COMPOUND_RELEASE_REPOSITORY must be an explicit owner/repository identity"
    release_url="https://github.com/${release_repository}/releases/download/v${requested_version}/"
  fi
fi

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "unsupported OS (public alpha supports darwin/arm64 on macOS 15 or newer and linux/amd64)" ;;
esac
case $(uname -m) in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) die "unsupported architecture (public alpha supports darwin/arm64 on macOS 15 or newer and linux/amd64)" ;;
esac
case "$os/$arch" in
  darwin/arm64|linux/amd64) ;;
  *) die "unsupported platform $os/$arch (public alpha supports darwin/arm64 on macOS 15 or newer and linux/amd64)" ;;
esac
if [ "$os" = darwin ]; then
  command -v sw_vers >/dev/null 2>&1 || die "sw_vers is required to verify the macOS support floor"
  macos_version=$(sw_vers -productVersion) || die "cannot determine macOS version"
  macos_major=${macos_version%%.*}
  case "$macos_major" in ''|*[!0-9]*) die "cannot determine macOS major version: $macos_version" ;; esac
  [ "$macos_major" -ge 15 ] || die "unsupported macOS $macos_version: prebuilt installs require macOS 15 or newer"
fi

if [ -n "${GOBIN:-}" ]; then
  requested_install_dir=$GOBIN
else
  [ -n "${HOME:-}" ] || die "HOME is required when GOBIN is not set"
  requested_install_dir=$HOME/.local/bin
fi
case "$requested_install_dir" in
  /*) requested_install_path=$requested_install_dir ;;
  *) requested_install_path=$invocation_dir/$requested_install_dir ;;
esac
mkdir -p "$requested_install_path" || die "cannot create install directory"
[ -d "$requested_install_path" ] || die "install root must be a directory"
install_dir=$(unset CDPATH; cd "$requested_install_path" && pwd -P) || die "cannot resolve canonical install root"
install_root_sha256=$(sha256_text "$install_dir")
is_sha256 "$install_root_sha256" || die "cannot identify canonical install root"
cd "$install_dir" || die "cannot enter physical install root: $install_dir"

state_root=./.compound-install-state
state_root_display=$install_dir/.compound-install-state
current_uid=$(id -u) || die "cannot identify install transaction owner"
state_root_created=0
if [ -e "$state_root" ] || [ -L "$state_root" ]; then
  [ -d "$state_root" ] && [ ! -L "$state_root" ] || die "install transaction state root is not a private directory: $state_root"
  [ "$(path_mode "$state_root")" = 700 ] || die "install transaction state root is not mode 0700: $state_root"
  [ "$(path_uid "$state_root")" = "$current_uid" ] || die "install transaction state root has a foreign owner: $state_root"
else
  mkdir "$state_root" || die "cannot create install transaction state root"
  chmod 0700 "$state_root" || die "cannot secure install transaction state root"
  state_root_created=1
fi
lock_file=$state_root/lock
if [ ! -e "$lock_file" ] && [ ! -L "$lock_file" ]; then
  if [ "$state_root_created" != 1 ]; then
    for unlocked_entry in "$state_root"/* "$state_root"/.[!.]* "$state_root"/..?*; do
      if [ -e "$unlocked_entry" ] || [ -L "$unlocked_entry" ]; then
        die "install transaction state root has entries but no persistent lock: $state_root_display"
      fi
    done
  fi
  (set -C; : > "$lock_file") 2>/dev/null || :
fi
private_regular_file "$lock_file" 600 || die "install transaction lock is not a private regular file: $lock_file"

# A stable lock inode is retained permanently. The current shell opens it once
# on FD 9, acquires the platform kernel lock on that exact open file
# description, and keeps the descriptor alive for the complete install. There
# is no re-exec marker a caller can forge while another process owns the lock.
exec 9>"$lock_file" || die "cannot open install transaction lock: $lock_file"
lock_status=0
case "$os" in
  darwin)
    command -v lockf >/dev/null 2>&1 || die "FD-mode lockf -s -t 0 9 is required for concurrent install exclusion on macOS 15 or newer"
    if lockf -s -t 0 9; then
      :
    else
      lock_status=$?
    fi
    ;;
  linux)
    command -v flock >/dev/null 2>&1 || die "BusyBox-compatible flock -n FD support is required for concurrent install exclusion on Linux"
    if flock -n 9; then
      :
    else
      lock_status=$?
    fi
    ;;
esac
if [ "$lock_status" -ne 0 ]; then
  die "another install transaction is active or lock acquisition failed at $state_root_display (status $lock_status)"
fi
unset _COMPOUND_INSTALL_LOCK_NONCE
lock_nonce=$(new_nonce)

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/compound-install.XXXXXX") || die "cannot create private work directory"
chmod 0700 "$work_dir" || die "cannot secure private work directory"
transaction_active=0
transaction_prepared=0
transaction_preserved=0
transaction_dir=$state_root/transaction
transaction_next_dir=$state_root/transaction.next
owner_file=$state_root/owner
owner_next_file=$state_root/owner.next
recovered_install_state=0
compound_target=./compound
breadth_target=./compound-breadth-engine

validate_physical_install_root() {
  anchored_install_dir=$(pwd -P) || return 1
  [ "$anchored_install_dir" = "$install_dir" ]
}

target_has_digest() {
  digest_target=$1
  digest_expected=$2
  [ -f "$digest_target" ] && [ ! -L "$digest_target" ] || return 1
  digest_actual=$(sha256_file "$digest_target")
  [ "$digest_actual" = "$digest_expected" ]
}

durability_barrier() {
  barrier_phase=$1
  if ! COMPOUND_INSTALL_SYNC_PHASE="$barrier_phase" sync; then
    transaction_active=0
    transaction_preserved=1
    die "durability barrier failed at $barrier_phase; transaction state preserved at $state_root_display"
  fi
}

private_directory() {
  private_directory_path=$1
  [ -d "$private_directory_path" ] && [ ! -L "$private_directory_path" ] || return 1
  [ "$(path_mode "$private_directory_path")" = 700 ] || return 1
  [ "$(path_uid "$private_directory_path")" = "$current_uid" ] || return 1
}

private_staged_file() {
  private_staged_path=$1
  [ -f "$private_staged_path" ] && [ ! -L "$private_staged_path" ] || return 1
  private_staged_mode=$(path_mode "$private_staged_path")
  case "$private_staged_mode" in 600|700|755) ;; *) return 1 ;; esac
  [ "$(path_uid "$private_staged_path")" = "$current_uid" ] || return 1
  [ "$(path_links "$private_staged_path")" = 1 ] || return 1
}

validate_owner_record_at() {
  owner_record_path=$1
  private_regular_file "$owner_record_path" 600 || return 1
  owner_record_values=$(awk '
    NR == 1 { if (NF != 2 || $1 != "schema" || $2 != "compound.install.owner.v0.1") { bad = 1; exit }; next }
    NR == 2 { if (NF != 2 || $1 != "pid" || $2 !~ /^[1-9][0-9]*$/) { bad = 1; exit }; pid = $2; next }
    NR == 3 { if (NF != 2 || $1 != "nonce" || length($2) != 32 || $2 ~ /[^0-9a-f]/) { bad = 1; exit }; nonce = $2; next }
    { bad = 1; exit }
    END { if (bad || NR != 3) exit 1; print pid, nonce }
  ' "$owner_record_path") || return 1
  set -- $owner_record_values
  [ "$#" -eq 2 ] || return 1
  recorded_owner_pid=$1
  recorded_owner_nonce=$2
  private_regular_file "$owner_record_path" 600 || return 1
  return 0
}

validate_owner_record() {
  validate_owner_record_at "$owner_file"
}

validate_journal_record_at() {
  journal_path=$1
  private_regular_file "$journal_path" 600 || return 1
  journal_values=$(awk -v root="$install_root_sha256" '
    function sha(value) { return length(value) == 64 && value !~ /[^0-9a-f]/ }
    NR == 1 { if (NF != 2 || $1 != "schema" || $2 != "compound.install.transaction.v0.1") { bad = 1; exit }; next }
    NR == 2 { if (NF != 2 || $1 != "owner_pid" || $2 !~ /^[1-9][0-9]*$/) { bad = 1; exit }; owner_pid = $2; next }
    NR == 3 { if (NF != 2 || $1 != "owner_nonce" || length($2) != 32 || $2 ~ /[^0-9a-f]/) { bad = 1; exit }; owner_nonce = $2; next }
    NR == 4 { if (NF != 2 || $1 != "install_root_sha256" || $2 != root) { bad = 1; exit }; next }
    NR == 5 { if (NF != 2 || $1 != "compound_target" || $2 != "compound") { bad = 1; exit }; next }
    NR == 6 { if (NF != 2 || $1 != "breadth_target" || $2 != "compound-breadth-engine") { bad = 1; exit }; next }
    NR == 7 { if (NF != 2 || $1 != "compound_prior_present" || $2 !~ /^[01]$/) { bad = 1; exit }; compound_present = $2; next }
    NR == 8 { if (NF != 2 || $1 != "compound_prior_type" || ($2 != "absent" && $2 != "regular")) { bad = 1; exit }; compound_type = $2; next }
    NR == 9 { if (NF != 2 || $1 != "compound_prior_sha256" || ($2 != "-" && !sha($2))) { bad = 1; exit }; compound_prior = $2; next }
    NR == 10 { if (NF != 2 || $1 != "breadth_prior_present" || $2 !~ /^[01]$/) { bad = 1; exit }; breadth_present = $2; next }
    NR == 11 { if (NF != 2 || $1 != "breadth_prior_type" || ($2 != "absent" && $2 != "regular")) { bad = 1; exit }; breadth_type = $2; next }
    NR == 12 { if (NF != 2 || $1 != "breadth_prior_sha256" || ($2 != "-" && !sha($2))) { bad = 1; exit }; breadth_prior = $2; next }
    NR == 13 { if (NF != 2 || $1 != "compound_new_sha256" || !sha($2)) { bad = 1; exit }; compound_new = $2; next }
    NR == 14 { if (NF != 2 || $1 != "breadth_new_sha256" || !sha($2)) { bad = 1; exit }; breadth_new = $2; next }
    NR == 15 { if (NF != 2 || $1 != "phase" || ($2 != "prepared" && $2 != "committed")) { bad = 1; exit }; phase = $2; next }
    { bad = 1; exit }
    END {
      if (bad || NR != 15) exit 1
      if (compound_present == 0 && (compound_type != "absent" || compound_prior != "-")) exit 1
      if (compound_present == 1 && (compound_type != "regular" || !sha(compound_prior))) exit 1
      if (breadth_present == 0 && (breadth_type != "absent" || breadth_prior != "-")) exit 1
      if (breadth_present == 1 && (breadth_type != "regular" || !sha(breadth_prior))) exit 1
      print owner_pid, owner_nonce, compound_present, compound_prior, breadth_present, breadth_prior, compound_new, breadth_new, phase
    }
  ' "$journal_path") || return 1
  set -- $journal_values
  [ "$#" -eq 9 ] || return 1
  journal_owner_pid=$1
  journal_owner_nonce=$2
  journal_compound_prior_present=$3
  journal_compound_prior_digest=$4
  journal_breadth_prior_present=$5
  journal_breadth_prior_digest=$6
  journal_compound_new_digest=$7
  journal_breadth_new_digest=$8
  journal_phase=$9
  private_regular_file "$journal_path" 600 || return 1
  return 0
}

validate_journal_record() {
  validate_journal_record_at "$transaction_dir/journal"
}

remember_expected_journal() {
  expected_journal_owner_pid=$journal_owner_pid
  expected_journal_owner_nonce=$journal_owner_nonce
  expected_journal_compound_prior_present=$journal_compound_prior_present
  expected_journal_compound_prior_digest=$journal_compound_prior_digest
  expected_journal_breadth_prior_present=$journal_breadth_prior_present
  expected_journal_breadth_prior_digest=$journal_breadth_prior_digest
  expected_journal_compound_new_digest=$journal_compound_new_digest
  expected_journal_breadth_new_digest=$journal_breadth_new_digest
  expected_journal_phase=$journal_phase
}

journal_matches_expected_phase() {
  expected_phase=$1
  [ "$journal_owner_pid" = "$expected_journal_owner_pid" ] &&
    [ "$journal_owner_nonce" = "$expected_journal_owner_nonce" ] &&
    [ "$journal_compound_prior_present" = "$expected_journal_compound_prior_present" ] &&
    [ "$journal_compound_prior_digest" = "$expected_journal_compound_prior_digest" ] &&
    [ "$journal_breadth_prior_present" = "$expected_journal_breadth_prior_present" ] &&
    [ "$journal_breadth_prior_digest" = "$expected_journal_breadth_prior_digest" ] &&
    [ "$journal_compound_new_digest" = "$expected_journal_compound_new_digest" ] &&
    [ "$journal_breadth_new_digest" = "$expected_journal_breadth_new_digest" ] &&
    [ "$journal_phase" = "$expected_phase" ]
}

validate_state_root_inventory() {
  state_lock_present=0
  state_owner_present=0
  state_owner_next_present=0
  state_transaction_present=0
  state_transaction_next_present=0
  state_retired_transaction_path=
  state_retired_owner_path=
  for state_entry in "$state_root"/* "$state_root"/.[!.]* "$state_root"/..?*; do
    if [ ! -e "$state_entry" ] && [ ! -L "$state_entry" ]; then
      continue
    fi
    state_name=${state_entry##*/}
    case "$state_name" in
      lock)
        private_regular_file "$state_entry" 600 || return 1
        [ "$state_lock_present" = 0 ] || return 1
        state_lock_present=1
        ;;
      owner)
        private_regular_file "$state_entry" 600 || return 1
        [ "$state_owner_present" = 0 ] || return 1
        state_owner_present=1
        ;;
      owner.next)
        private_staged_file "$state_entry" || return 1
        [ "$state_owner_next_present" = 0 ] || return 1
        state_owner_next_present=1
        ;;
      transaction)
        private_directory "$state_entry" || return 1
        [ "$state_transaction_present" = 0 ] || return 1
        state_transaction_present=1
        ;;
      transaction.next)
        private_directory "$state_entry" || return 1
        [ "$state_transaction_next_present" = 0 ] || return 1
        state_transaction_next_present=1
        ;;
      transaction.retired.*)
        retired_transaction_nonce=${state_name#transaction.retired.}
        is_nonce "$retired_transaction_nonce" || return 1
        private_directory "$state_entry" || return 1
        [ -z "$state_retired_transaction_path" ] || return 1
        state_retired_transaction_path=$state_entry
        ;;
      owner.retired.*)
        retired_owner_nonce=${state_name#owner.retired.}
        is_nonce "$retired_owner_nonce" || return 1
        private_regular_file "$state_entry" 600 || return 1
        [ -z "$state_retired_owner_path" ] || return 1
        state_retired_owner_path=$state_entry
        ;;
      *) return 1 ;;
    esac
  done
  [ "$state_lock_present" = 1 ] || return 1
  return 0
}

validate_transaction_inventory_at() {
  inventory_dir=$1
  inventory_allow_journal_next=${2:-0}
  inventory_allow_unfinished_scratch=${3:-0}
  private_directory "$inventory_dir" || return 1
  target_has_digest "$inventory_dir/compound.new" "$journal_compound_new_digest" || return 1
  target_has_digest "$inventory_dir/compound-breadth-engine.new" "$journal_breadth_new_digest" || return 1
  if [ "$journal_compound_prior_present" = 1 ]; then
    target_has_digest "$inventory_dir/compound.previous" "$journal_compound_prior_digest" || return 1
  elif [ -e "$inventory_dir/compound.previous" ] || [ -L "$inventory_dir/compound.previous" ]; then
    return 1
  fi
  if [ "$journal_breadth_prior_present" = 1 ]; then
    target_has_digest "$inventory_dir/compound-breadth-engine.previous" "$journal_breadth_prior_digest" || return 1
  elif [ -e "$inventory_dir/compound-breadth-engine.previous" ] || [ -L "$inventory_dir/compound-breadth-engine.previous" ]; then
    return 1
  fi
  for transaction_entry in "$inventory_dir"/* "$inventory_dir"/.[!.]* "$inventory_dir"/..?*; do
    if [ ! -e "$transaction_entry" ] && [ ! -L "$transaction_entry" ]; then
      continue
    fi
    transaction_name=${transaction_entry##*/}
    case "$transaction_name" in
      journal)
        private_regular_file "$transaction_entry" 600 || return 1
        ;;
      journal.next)
        [ "$inventory_allow_journal_next" = 1 ] || return 1
        private_regular_file "$transaction_entry" 600 || return 1
        ;;
      compound.new|compound-breadth-engine.new)
        private_regular_file "$transaction_entry" 755 || return 1
        ;;
      compound.previous)
        private_regular_file "$transaction_entry" 755 || return 1
        [ "$journal_compound_prior_present" = 1 ] || return 1
        ;;
      compound-breadth-engine.previous)
        private_regular_file "$transaction_entry" 755 || return 1
        [ "$journal_breadth_prior_present" = 1 ] || return 1
        ;;
      compound.publish)
        if [ "$inventory_allow_unfinished_scratch" = 1 ]; then
          private_staged_file "$transaction_entry" || return 1
        else
          private_regular_file "$transaction_entry" 755 || return 1
          target_has_digest "$transaction_entry" "$journal_compound_new_digest" || return 1
        fi
        ;;
      compound-breadth-engine.publish)
        if [ "$inventory_allow_unfinished_scratch" = 1 ]; then
          private_staged_file "$transaction_entry" || return 1
        else
          private_regular_file "$transaction_entry" 755 || return 1
          target_has_digest "$transaction_entry" "$journal_breadth_new_digest" || return 1
        fi
        ;;
      compound.restore)
        if [ "$inventory_allow_unfinished_scratch" = 1 ]; then
          private_staged_file "$transaction_entry" || return 1
        else
          private_regular_file "$transaction_entry" 755 || return 1
          [ "$journal_compound_prior_present" = 1 ] && target_has_digest "$transaction_entry" "$journal_compound_prior_digest" || return 1
        fi
        ;;
      compound-breadth-engine.restore)
        if [ "$inventory_allow_unfinished_scratch" = 1 ]; then
          private_staged_file "$transaction_entry" || return 1
        else
          private_regular_file "$transaction_entry" 755 || return 1
          [ "$journal_breadth_prior_present" = 1 ] && target_has_digest "$transaction_entry" "$journal_breadth_prior_digest" || return 1
        fi
        ;;
      *) return 1 ;;
    esac
  done
  return 0
}

validate_transaction_inventory() {
  validate_transaction_inventory_at "$transaction_dir" "${1:-0}" "${2:-0}"
}

discard_unpublished_prepared_scratch() {
  [ "$expected_journal_phase" = prepared ] || return 1
  # These exact names are unpublished staging, never recovery authority. The
  # authoritative generation and target classifications must be exact before
  # an incomplete stage can be removed.
  validate_transaction_inventory 1 1 || return 1
  validate_owner_record || return 1
  [ "$recorded_owner_pid" = "$expected_journal_owner_pid" ] || return 1
  [ "$recorded_owner_nonce" = "$expected_journal_owner_nonce" ] || return 1
  validate_journal_record || return 1
  journal_matches_expected_phase prepared || return 1
  observed_target_state "$compound_target" "$expected_journal_compound_prior_present" "$expected_journal_compound_prior_digest" "$expected_journal_compound_new_digest" >/dev/null || return 1
  observed_target_state "$breadth_target" "$expected_journal_breadth_prior_present" "$expected_journal_breadth_prior_digest" "$expected_journal_breadth_new_digest" >/dev/null || return 1

  scratch_discarded=0
  for scratch_name in journal.next compound.publish compound-breadth-engine.publish compound.restore compound-breadth-engine.restore; do
    scratch_path=$transaction_dir/$scratch_name
    if [ -e "$scratch_path" ] || [ -L "$scratch_path" ]; then
      rm -f "$scratch_path" || return 1
      scratch_discarded=1
    fi
  done
  if [ "$scratch_discarded" = 1 ]; then
    durability_barrier unpublished_scratch_discarded
    validate_owner_record || return 1
    [ "$recorded_owner_pid" = "$expected_journal_owner_pid" ] || return 1
    [ "$recorded_owner_nonce" = "$expected_journal_owner_nonce" ] || return 1
    validate_journal_record || return 1
    journal_matches_expected_phase prepared || return 1
  fi
  return 0
}

validate_staged_transaction_inventory() {
  staged_dir=$1
  private_directory "$staged_dir" || return 1
  for partial_entry in "$staged_dir"/* "$staged_dir"/.[!.]* "$staged_dir"/..?*; do
    if [ ! -e "$partial_entry" ] && [ ! -L "$partial_entry" ]; then
      continue
    fi
    partial_name=${partial_entry##*/}
    case "$partial_name" in
      journal|journal.next)
        private_regular_file "$partial_entry" 600 || return 1
        ;;
      compound.new|compound-breadth-engine.new|compound.previous|compound-breadth-engine.previous)
        private_staged_file "$partial_entry" || return 1
        ;;
      *) return 1 ;;
    esac
  done
  return 0
}

remove_staged_transaction() {
  staged_dir=$1
  validate_staged_transaction_inventory "$staged_dir" || return 1
  for partial_name in journal journal.next compound.new compound-breadth-engine.new compound.previous compound-breadth-engine.previous; do
    partial_path=$staged_dir/$partial_name
    if [ -e "$partial_path" ] || [ -L "$partial_path" ]; then
      rm -f "$partial_path" || return 1
    fi
  done
  rmdir "$staged_dir" || return 1
  return 0
}

validate_retired_transaction_inventory() {
  retired_inventory_dir=$1
  private_directory "$retired_inventory_dir" || return 1
  for retired_entry in "$retired_inventory_dir"/* "$retired_inventory_dir"/.[!.]* "$retired_inventory_dir"/..?*; do
    if [ ! -e "$retired_entry" ] && [ ! -L "$retired_entry" ]; then
      continue
    fi
    retired_name=${retired_entry##*/}
    case "$retired_name" in
      journal|journal.next)
        private_regular_file "$retired_entry" 600 || return 1
        ;;
      compound.new|compound-breadth-engine.new|compound.previous|compound-breadth-engine.previous|compound.publish|compound-breadth-engine.publish|compound.restore|compound-breadth-engine.restore)
        private_regular_file "$retired_entry" 755 || return 1
        ;;
      *) return 1 ;;
    esac
  done
  return 0
}

gc_retired_transaction() {
  gc_transaction_dir=$1
  gc_transaction_nonce=${gc_transaction_dir##*.}
  is_nonce "$gc_transaction_nonce" || return 1
  validate_retired_transaction_inventory "$gc_transaction_dir" || return 1
  if [ -e "$gc_transaction_dir/journal" ] || [ -L "$gc_transaction_dir/journal" ]; then
    validate_journal_record_at "$gc_transaction_dir/journal" || return 1
    [ "$journal_owner_nonce" = "$gc_transaction_nonce" ] || return 1
  fi
  for cleanup_name in compound.publish compound-breadth-engine.publish compound.restore compound-breadth-engine.restore; do
    cleanup_path=$gc_transaction_dir/$cleanup_name
    if [ -e "$cleanup_path" ] || [ -L "$cleanup_path" ]; then
      rm -f "$cleanup_path" || return 1
    fi
  done
  for cleanup_spec in \
    'compound.new:cleanup_after_compound_new' \
    'compound-breadth-engine.new:cleanup_after_breadth_new' \
    'compound.previous:cleanup_after_compound_previous' \
    'compound-breadth-engine.previous:cleanup_after_breadth_previous'; do
    cleanup_name=${cleanup_spec%%:*}
    cleanup_hook=${cleanup_spec#*:}
    cleanup_path=$gc_transaction_dir/$cleanup_name
    if [ -e "$cleanup_path" ] || [ -L "$cleanup_path" ]; then
      rm -f "$cleanup_path" || return 1
    fi
    crash_at "$cleanup_hook"
  done
  if [ -e "$gc_transaction_dir/journal.next" ] || [ -L "$gc_transaction_dir/journal.next" ]; then
    rm -f "$gc_transaction_dir/journal.next" || return 1
  fi
  if [ -e "$gc_transaction_dir/journal" ] || [ -L "$gc_transaction_dir/journal" ]; then
    rm -f "$gc_transaction_dir/journal" || return 1
  fi
  crash_at cleanup_after_journal
  rmdir "$gc_transaction_dir" || return 1
  durability_barrier transaction_cleaned
  crash_at cleanup_after_transaction_dir
  return 0
}

validate_expected_target_pair() {
  expected_target_pair=$1
  case "$expected_target_pair" in
    new)
      target_has_digest "$compound_target" "$expected_journal_compound_new_digest" || return 1
      target_has_digest "$breadth_target" "$expected_journal_breadth_new_digest" || return 1
      ;;
    prior)
      if [ "$expected_journal_compound_prior_present" = 1 ]; then
        target_has_digest "$compound_target" "$expected_journal_compound_prior_digest" || return 1
      elif [ -e "$compound_target" ] || [ -L "$compound_target" ]; then
        return 1
      fi
      if [ "$expected_journal_breadth_prior_present" = 1 ]; then
        target_has_digest "$breadth_target" "$expected_journal_breadth_prior_digest" || return 1
      elif [ -e "$breadth_target" ] || [ -L "$breadth_target" ]; then
        return 1
      fi
      ;;
    *) return 1 ;;
  esac
}

retire_transaction_generation() {
  retire_phase=$1
  retire_target_pair=$2
  validate_state_root_inventory || return 1
  [ "$state_owner_present" = 1 ] && [ "$state_transaction_present" = 1 ] || return 1
  [ "$state_owner_next_present" = 0 ] && [ "$state_transaction_next_present" = 0 ] || return 1
  [ -z "$state_retired_transaction_path" ] && [ -z "$state_retired_owner_path" ] || return 1
  validate_journal_record || return 1
  journal_matches_expected_phase "$retire_phase" || return 1
  validate_transaction_inventory || return 1
  validate_expected_target_pair "$retire_target_pair" || return 1
  validate_owner_record || return 1
  [ "$recorded_owner_pid" = "$expected_journal_owner_pid" ] || return 1
  [ "$recorded_owner_nonce" = "$expected_journal_owner_nonce" ] || return 1
  validate_journal_record || return 1
  journal_matches_expected_phase "$retire_phase" || return 1
  validate_expected_target_pair "$retire_target_pair" || return 1
  retired_transaction_path=$state_root/transaction.retired.$expected_journal_owner_nonce
  [ ! -e "$retired_transaction_path" ] && [ ! -L "$retired_transaction_path" ] || return 1
  mv "$transaction_dir" "$retired_transaction_path" || return 1
  transaction_active=0
  transaction_prepared=0
  durability_barrier transaction_retired
  crash_at after_transaction_retired
  validate_journal_record_at "$retired_transaction_path/journal" || return 1
  journal_matches_expected_phase "$retire_phase" || return 1
  validate_retired_transaction_inventory "$retired_transaction_path" || return 1
  gc_retired_transaction "$retired_transaction_path" || return 1
  return 0
}

gc_retired_owner() {
  gc_owner_path=$1
  gc_owner_nonce=${gc_owner_path##*.}
  is_nonce "$gc_owner_nonce" || return 1
  validate_owner_record_at "$gc_owner_path" || return 1
  [ "$recorded_owner_nonce" = "$gc_owner_nonce" ] || return 1
  rm -f "$gc_owner_path" || return 1
  durability_barrier owner_cleaned
  return 0
}

retire_owner_record() {
  retire_owner_pid=$1
  retire_owner_nonce=$2
  validate_state_root_inventory || return 1
  [ "$state_owner_present" = 1 ] || return 1
  [ "$state_owner_next_present" = 0 ] && [ "$state_transaction_present" = 0 ] && [ "$state_transaction_next_present" = 0 ] || return 1
  [ -z "$state_retired_transaction_path" ] && [ -z "$state_retired_owner_path" ] || return 1
  validate_owner_record || return 1
  [ "$recorded_owner_pid" = "$retire_owner_pid" ] || return 1
  [ "$recorded_owner_nonce" = "$retire_owner_nonce" ] || return 1
  durability_barrier owner_retiring
  validate_state_root_inventory || return 1
  [ "$state_owner_present" = 1 ] || return 1
  [ "$state_owner_next_present" = 0 ] && [ "$state_transaction_present" = 0 ] && [ "$state_transaction_next_present" = 0 ] || return 1
  [ -z "$state_retired_transaction_path" ] && [ -z "$state_retired_owner_path" ] || return 1
  validate_owner_record || return 1
  [ "$recorded_owner_pid" = "$retire_owner_pid" ] || return 1
  [ "$recorded_owner_nonce" = "$retire_owner_nonce" ] || return 1
  retired_owner_path=$state_root/owner.retired.$retire_owner_nonce
  [ ! -e "$retired_owner_path" ] && [ ! -L "$retired_owner_path" ] || return 1
  mv "$owner_file" "$retired_owner_path" || return 1
  durability_barrier owner_retired
  validate_owner_record_at "$retired_owner_path" || return 1
  [ "$recorded_owner_pid" = "$retire_owner_pid" ] || return 1
  [ "$recorded_owner_nonce" = "$retire_owner_nonce" ] || return 1
  gc_retired_owner "$retired_owner_path" || return 1
  return 0
}

observed_target_state() {
  observed_target=$1
  observed_prior_present=$2
  observed_prior_digest=$3
  observed_new_digest=$4
  if [ ! -e "$observed_target" ] && [ ! -L "$observed_target" ]; then
    printf 'absent\n'
    return 0
  fi
  [ -f "$observed_target" ] && [ ! -L "$observed_target" ] || return 1
  if target_has_digest "$observed_target" "$observed_new_digest"; then
    printf 'new\n'
    return 0
  fi
  if [ "$observed_prior_present" = 1 ] && target_has_digest "$observed_target" "$observed_prior_digest"; then
    printf 'prior\n'
    return 0
  fi
  return 1
}

write_owner_record() {
  validate_state_root_inventory || return 1
  [ "$state_owner_present" = 0 ] && [ "$state_owner_next_present" = 0 ] || return 1
  [ "$state_transaction_present" = 0 ] && [ "$state_transaction_next_present" = 0 ] || return 1
  [ -z "$state_retired_transaction_path" ] && [ -z "$state_retired_owner_path" ] || return 1
  if [ -e "$owner_next_file" ] || [ -L "$owner_next_file" ]; then
    return 1
  fi
  printf 'schema compound.install.owner.v0.1\npid %s\nnonce %s\n' "$$" "$lock_nonce" > "$owner_next_file" || return 1
  chmod 0600 "$owner_next_file" || return 1
  durability_barrier owner_staged
  validate_owner_record_at "$owner_next_file" || return 1
  [ "$recorded_owner_pid" = "$$" ] && [ "$recorded_owner_nonce" = "$lock_nonce" ] || return 1
  mv "$owner_next_file" "$owner_file" || return 1
  durability_barrier owner
  validate_owner_record || return 1
  [ "$recorded_owner_pid" = "$$" ] && [ "$recorded_owner_nonce" = "$lock_nonce" ] || return 1
  return 0
}

write_journal_record() {
  write_phase=$1
  write_path=$2
  if [ "$write_phase" = committed ] && [ "${COMPOUND_INSTALL_CRASH_AT:-}" = during_committed_journal_write ]; then
    printf '%s\n' 'schema compound.install.transaction.v0.1' > "$write_path" || return 1
    crash_at during_committed_journal_write
  fi
  printf '%s\n' \
    'schema compound.install.transaction.v0.1' \
    "owner_pid $$" \
    "owner_nonce $lock_nonce" \
    "install_root_sha256 $install_root_sha256" \
    'compound_target compound' \
    'breadth_target compound-breadth-engine' \
    "compound_prior_present $compound_prior_present" \
    "compound_prior_type $compound_prior_type" \
    "compound_prior_sha256 $compound_prior_digest" \
    "breadth_prior_present $breadth_prior_present" \
    "breadth_prior_type $breadth_prior_type" \
    "breadth_prior_sha256 $breadth_prior_digest" \
    "compound_new_sha256 $compound_expected" \
    "breadth_new_sha256 $breadth_expected" \
    "phase $write_phase" > "$write_path" || return 1
  chmod 0600 "$write_path" || return 1
  return 0
}

recover_transaction() {
  validate_state_root_inventory || return 1
  validate_physical_install_root || return 1

  if [ -n "$state_retired_owner_path" ]; then
    [ "$state_owner_present" = 0 ] && [ "$state_owner_next_present" = 0 ] || return 1
    [ "$state_transaction_present" = 0 ] && [ "$state_transaction_next_present" = 0 ] || return 1
    [ -z "$state_retired_transaction_path" ] || return 1
    gc_retired_owner "$state_retired_owner_path" || return 1
    recovered_install_state=1
    return 0
  fi

  if [ -n "$state_retired_transaction_path" ]; then
    [ "$state_owner_present" = 1 ] && [ "$state_owner_next_present" = 0 ] || return 1
    [ "$state_transaction_present" = 0 ] && [ "$state_transaction_next_present" = 0 ] || return 1
    validate_owner_record || return 1
    recovery_owner_pid=$recorded_owner_pid
    recovery_owner_nonce=$recorded_owner_nonce
    retired_transaction_nonce=${state_retired_transaction_path##*.}
    [ "$recovery_owner_nonce" = "$retired_transaction_nonce" ] || return 1
    gc_retired_transaction "$state_retired_transaction_path" || return 1
    retire_owner_record "$recovery_owner_pid" "$recovery_owner_nonce" || return 1
    recovered_install_state=1
    return 0
  fi

  if [ "$state_transaction_next_present" = 1 ]; then
    [ "$state_owner_present" = 1 ] && [ "$state_owner_next_present" = 0 ] || return 1
    [ "$state_transaction_present" = 0 ] || return 1
    validate_owner_record || return 1
    recovery_owner_pid=$recorded_owner_pid
    recovery_owner_nonce=$recorded_owner_nonce
    remove_staged_transaction "$transaction_next_dir" || return 1
    transaction_active=0
    durability_barrier staged_transaction_cleaned
    retire_owner_record "$recovery_owner_pid" "$recovery_owner_nonce" || return 1
    recovered_install_state=1
    return 0
  fi

  if [ "$state_owner_next_present" = 1 ]; then
    [ "$state_owner_present" = 0 ] && [ "$state_transaction_present" = 0 ] || return 1
    rm -f "$owner_next_file" || return 1
    durability_barrier owner_stage_cleaned
    recovered_install_state=1
    return 0
  fi

  if [ "$state_transaction_present" = 0 ]; then
    if [ "$state_owner_present" = 1 ]; then
      validate_owner_record || return 1
      recovery_owner_pid=$recorded_owner_pid
      recovery_owner_nonce=$recorded_owner_nonce
      retire_owner_record "$recovery_owner_pid" "$recovery_owner_nonce" || return 1
      recovered_install_state=1
    fi
    return 0
  fi

  [ "$state_owner_present" = 1 ] && [ "$state_owner_next_present" = 0 ] || return 1
  transaction_preserved=1
  validate_owner_record || return 1
  validate_journal_record || return 1
  [ "$recorded_owner_pid" = "$journal_owner_pid" ] || return 1
  [ "$recorded_owner_nonce" = "$journal_owner_nonce" ] || return 1
  remember_expected_journal

  if [ "$expected_journal_phase" = prepared ]; then
    discard_unpublished_prepared_scratch || return 1
  elif [ -e "$transaction_dir/journal.next" ] || [ -L "$transaction_dir/journal.next" ]; then
    return 1
  fi
  validate_transaction_inventory || return 1

  # This is the mutation firewall: classify both targets and the complete
  # retained state before the first recovery copy, rename, or removal.
  recover_compound_state=$(observed_target_state "$compound_target" "$expected_journal_compound_prior_present" "$expected_journal_compound_prior_digest" "$expected_journal_compound_new_digest") || return 1
  recover_breadth_state=$(observed_target_state "$breadth_target" "$expected_journal_breadth_prior_present" "$expected_journal_breadth_prior_digest" "$expected_journal_breadth_new_digest") || return 1

  if [ "$expected_journal_phase" = committed ]; then
    [ "$recover_compound_state" = new ] || return 1
    [ "$recover_breadth_state" = new ] || return 1
    recovered_target_pair=new
  else
    rm -f "$transaction_dir/compound.publish" "$transaction_dir/compound-breadth-engine.publish" \
      "$transaction_dir/compound.restore" "$transaction_dir/compound-breadth-engine.restore" || return 1
    if [ "$expected_journal_compound_prior_present" = 1 ] && [ "$recover_compound_state" != prior ]; then
      cp "$transaction_dir/compound.previous" "$transaction_dir/compound.restore" || return 1
      crash_at after_compound_restore_copy
      chmod 0755 "$transaction_dir/compound.restore" || return 1
      target_has_digest "$transaction_dir/compound.restore" "$expected_journal_compound_prior_digest" || return 1
    fi
    if [ "$expected_journal_breadth_prior_present" = 1 ] && [ "$recover_breadth_state" != prior ]; then
      cp "$transaction_dir/compound-breadth-engine.previous" "$transaction_dir/compound-breadth-engine.restore" || return 1
      crash_at after_breadth_restore_copy
      chmod 0755 "$transaction_dir/compound-breadth-engine.restore" || return 1
      target_has_digest "$transaction_dir/compound-breadth-engine.restore" "$expected_journal_breadth_prior_digest" || return 1
    fi
    durability_barrier recovery_staged
    validate_owner_record || return 1
    [ "$recorded_owner_pid" = "$expected_journal_owner_pid" ] || return 1
    [ "$recorded_owner_nonce" = "$expected_journal_owner_nonce" ] || return 1
    validate_journal_record || return 1
    journal_matches_expected_phase prepared || return 1
    validate_transaction_inventory || return 1
    current_compound_state=$(observed_target_state "$compound_target" "$expected_journal_compound_prior_present" "$expected_journal_compound_prior_digest" "$expected_journal_compound_new_digest") || return 1
    current_breadth_state=$(observed_target_state "$breadth_target" "$expected_journal_breadth_prior_present" "$expected_journal_breadth_prior_digest" "$expected_journal_breadth_new_digest") || return 1
    [ "$current_compound_state" = "$recover_compound_state" ] || return 1
    [ "$current_breadth_state" = "$recover_breadth_state" ] || return 1
    if [ "${COMPOUND_INSTALL_FAIL_RESTORE_COMPOUND:-0}" = 1 ] && [ "$recover_compound_state" != prior ]; then
      return 1
    fi
    if [ "$expected_journal_compound_prior_present" = 1 ]; then
      if [ "$recover_compound_state" != prior ]; then
        mv "$transaction_dir/compound.restore" "$compound_target" || return 1
      fi
    elif [ "$recover_compound_state" != absent ]; then
      rm -f "$compound_target" || return 1
    fi
    if [ "$expected_journal_breadth_prior_present" = 1 ]; then
      if [ "$recover_breadth_state" != prior ]; then
        mv "$transaction_dir/compound-breadth-engine.restore" "$breadth_target" || return 1
      fi
    elif [ "$recover_breadth_state" != absent ]; then
      rm -f "$breadth_target" || return 1
    fi
    durability_barrier recovery_targets
    validate_owner_record || return 1
    [ "$recorded_owner_pid" = "$expected_journal_owner_pid" ] || return 1
    [ "$recorded_owner_nonce" = "$expected_journal_owner_nonce" ] || return 1
    validate_journal_record || return 1
    journal_matches_expected_phase prepared || return 1
    validate_transaction_inventory || return 1
    validate_expected_target_pair prior || return 1
    recovered_target_pair=prior
  fi

  retire_transaction_generation "$expected_journal_phase" "$recovered_target_pair" || return 1
  retire_owner_record "$expected_journal_owner_pid" "$expected_journal_owner_nonce" || return 1
  transaction_active=0
  transaction_prepared=0
  transaction_preserved=0
  recovered_install_state=1
  return 0
}

rollback_install() {
  if [ "$transaction_active" != 1 ]; then
    return 0
  fi
  transaction_preserved=1
  if ! recover_transaction; then
    transaction_active=0
    printf 'install-release: rollback restoration failed; recovery transaction preserved at %s\n' "$transaction_dir" >&2
    return 1
  fi
  transaction_active=0
  transaction_preserved=0
  return 0
}

cleanup() {
  if [ "$transaction_active" = 1 ]; then
    rollback_install || :
  fi
  rm -rf "$work_dir" || :
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

cp "$source_sums" "$work_dir/SHA256SUMS" || die "cannot pin SHA256SUMS"
chmod 0600 "$work_dir/SHA256SUMS" || die "cannot secure SHA256SUMS"

provided_name=
if [ -n "${COMPOUND_RELEASE_TARBALL:-}" ]; then
  if [ ! -f "$COMPOUND_RELEASE_TARBALL" ] || [ -L "$COMPOUND_RELEASE_TARBALL" ]; then
    die "offline tarball must be a regular non-symlink file"
  fi
  provided_name=$(basename "$COMPOUND_RELEASE_TARBALL")
  release_source=$(dirname "$COMPOUND_RELEASE_TARBALL")/RELEASE
  if [ ! -f "$release_source" ] || [ -L "$release_source" ]; then
    die "RELEASE is required beside the offline tarball"
  fi
  cp "$release_source" "$work_dir/RELEASE" || die "cannot pin offline RELEASE"
else
  command -v curl >/dev/null 2>&1 || die "curl is required for downloads (or set COMPOUND_RELEASE_TARBALL)"
  curl -fsSL -o "$work_dir/RELEASE" "${release_url}RELEASE" || die "RELEASE is required for release installation"
fi
chmod 0600 "$work_dir/RELEASE" || die "cannot secure RELEASE"

release_expected=$(checksum_for "$work_dir/SHA256SUMS" RELEASE) || die "SHA256SUMS has no unique valid RELEASE entry"
release_actual=$(sha256_file "$work_dir/RELEASE")
[ "$release_expected" = "$release_actual" ] || die "checksum mismatch for RELEASE"

# The manifest grammar is intentionally closed: exactly one installer, two
# supported platform archives, and two application SBOMs per archive.
if ! awk '
  NR == 1 { if (NF != 2 || $1 != "schema" || $2 != "compound.release.v0.2") exit 1; next }
  NR == 2 {
    if (NF != 2 || $1 != "version" || $2 !~ /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/) exit 1
    version = $2; next
  }
  NR == 3 { if (NF != 2 || $1 != "commit" || length($2) != 40 || $2 ~ /[^0-9a-f]/) exit 1; next }
  NR == 4 { if (NF != 2 || $1 != "installer" || $2 != "install-release.sh") exit 1; next }
  NR == 5 {
    a = "compound_" version "_darwin_arm64.tar.gz"
    s1 = "compound_" version "_darwin_arm64.cdx.json"
    s2 = "compound_" version "_darwin_arm64_compound-breadth-engine.cdx.json"
    if (NF != 6 || $1 != "artifact" || $2 != "darwin" || $3 != "arm64" || $4 != a || $5 != s1 || $6 != s2) exit 1
    next
  }
  NR == 6 {
    a = "compound_" version "_linux_amd64.tar.gz"
    s1 = "compound_" version "_linux_amd64.cdx.json"
    s2 = "compound_" version "_linux_amd64_compound-breadth-engine.cdx.json"
    if (NF != 6 || $1 != "artifact" || $2 != "linux" || $3 != "amd64" || $4 != a || $5 != s1 || $6 != s2) exit 1
    next
  }
  { exit 1 }
  END { if (NR != 6) exit 1 }
' "$work_dir/RELEASE"; then
  die "RELEASE manifest is invalid"
fi

manifest_version=$(awk 'NR == 2 { print $2 }' "$work_dir/RELEASE")
manifest_commit=$(awk 'NR == 3 { print $2 }' "$work_dir/RELEASE")
is_stable_semver "$manifest_version" || die "RELEASE manifest is invalid"
is_commit "$manifest_commit" || die "RELEASE manifest is invalid"
if [ -n "$requested_version" ] && [ "$manifest_version" != "$requested_version" ]; then
  die "release manifest version does not match requested version"
fi
tarball="compound_${manifest_version}_${os}_${arch}.tar.gz"

# SHA256SUMS is also a closed inventory. No stale or undeclared release object
# can hitch a ride through a broad upload or local directory.
if ! awk -v version="$manifest_version" '
  function expect(name) { wanted[name] = 1 }
  BEGIN {
    expect("RELEASE")
    expect("install-release.sh")
    expect("compound_" version "_darwin_arm64.tar.gz")
    expect("compound_" version "_darwin_arm64.cdx.json")
    expect("compound_" version "_darwin_arm64_compound-breadth-engine.cdx.json")
    expect("compound_" version "_linux_amd64.tar.gz")
    expect("compound_" version "_linux_amd64.cdx.json")
    expect("compound_" version "_linux_amd64_compound-breadth-engine.cdx.json")
  }
  NF != 2 { exit 1 }
  {
    name = $2
    sub(/^\*/, "", name)
    if (!(name in wanted) || seen[name]++ || length($1) != 64 || $1 ~ /[^0-9a-f]/) exit 1
  }
  END {
    if (NR != 8) exit 1
    for (name in wanted) if (seen[name] != 1) exit 1
  }
' "$work_dir/SHA256SUMS"; then
  die "SHA256SUMS inventory is invalid"
fi

if [ -n "${COMPOUND_RELEASE_TARBALL:-}" ]; then
  [ "$provided_name" = "$tarball" ] || die "offline tarball name does not match verified release asset $tarball"
  cp "$COMPOUND_RELEASE_TARBALL" "$work_dir/archive.tar.gz" || die "cannot pin offline tarball"
else
  curl -fsSL -o "$work_dir/archive.tar.gz" "${release_url}${tarball}" || die "download failed: ${release_url}${tarball}"
fi
chmod 0600 "$work_dir/archive.tar.gz" || die "cannot secure release archive"
archive_expected=$(checksum_for "$work_dir/SHA256SUMS" "$tarball") || die "no unique checksum entry for $tarball"
archive_actual=$(sha256_file "$work_dir/archive.tar.gz")
[ "$archive_expected" = "$archive_actual" ] || die "checksum mismatch for $tarball"

command -v gzip >/dev/null 2>&1 || die "gzip is required for release archive validation"
gzip -dc "$work_dir/archive.tar.gz" > "$work_dir/archive.tar" || die "release archive is not valid gzip"
chmod 0600 "$work_dir/archive.tar" || die "cannot secure release tar"

tar_field() {
  file=$1
  offset=$2
  count=$3
  dd if="$file" bs=1 skip="$offset" count="$count" 2>/dev/null | tr -d '\000'
}

tar_octal() {
  value=$(tar_field "$1" "$2" "$3" | tr -d ' ')
  [ -n "$value" ] || value=0
  case "$value" in *[!0-7]*) return 1 ;; esac
  printf '%s\n' "$value" | awk '
    {
      value = 0
      for (i = 1; i <= length($0); i++) value = value * 8 + substr($0, i, 1)
      print value
    }
  '
}

tar_type_code() {
  dd if="$1" bs=1 skip="$2" count=1 2>/dev/null | od -An -tu1 | awk 'NF { print $1 + 0; exit }'
}

validate_tar_header() {
  file=$1
  block=$2
  expected_name=$3
  expected_type=$4
  expected_mode=$5
  base=$((block * 512))
  name=$(tar_field "$file" "$base" 100)
  mode=$(tar_octal "$file" $((base + 100)) 8) || return 1
  size=$(tar_octal "$file" $((base + 124)) 12) || return 1
  type=$(tar_type_code "$file" $((base + 156)))
  link=$(tar_field "$file" $((base + 157)) 100)
  magic=$(tar_field "$file" $((base + 257)) 6)
  format_version=$(tar_field "$file" $((base + 263)) 2)
  prefix=$(tar_field "$file" $((base + 345)) 155)
  [ "$name" = "$expected_name" ] || return 1
  [ "$type" = "$expected_type" ] || return 1
  [ "$mode" = "$expected_mode" ] || return 1
  [ -z "$link" ] || return 1
  [ "$magic" = "ustar" ] || return 1
  [ "$format_version" = "00" ] || return 1
  [ -z "$prefix" ] || return 1
  printf '%s\n' "$size"
}

bundle="compound_${manifest_version}_${os}_${arch}"
block=0
dir_size=$(validate_tar_header "$work_dir/archive.tar" "$block" "$bundle/" 53 493) || die "archive structure is invalid"
[ "$dir_size" = 0 ] || die "archive structure is invalid"
block=$((block + 1))
compound_size=$(validate_tar_header "$work_dir/archive.tar" "$block" "$bundle/compound" 48 493) || die "archive structure is invalid"
[ "$compound_size" -gt 0 ] || die "archive structure is invalid"
block=$((block + 1 + (compound_size + 511) / 512))
breadth_size=$(validate_tar_header "$work_dir/archive.tar" "$block" "$bundle/compound-breadth-engine" 48 493) || die "archive structure is invalid"
[ "$breadth_size" -gt 0 ] || die "archive structure is invalid"
block=$((block + 1 + (breadth_size + 511) / 512))
identity_size=$(validate_tar_header "$work_dir/archive.tar" "$block" "$bundle/RELEASE-IDENTITY" 48 420) || die "archive structure is invalid"
[ "$identity_size" -gt 0 ] || die "archive structure is invalid"
block=$((block + 1 + (identity_size + 511) / 512))
tar_bytes=$(wc -c < "$work_dir/archive.tar" | tr -d ' ')
expected_bytes=$(((block + 2) * 512))
[ "$tar_bytes" = "$expected_bytes" ] || die "archive structure is invalid"
for zero_block in "$block" $((block + 1)); do
  nonzero=$(dd if="$work_dir/archive.tar" bs=512 skip="$zero_block" count=1 2>/dev/null | tr -d '\000')
  [ -z "$nonzero" ] || die "archive structure is invalid"
done

command -v tar >/dev/null 2>&1 || die "tar is required for release extraction"
mkdir "$work_dir/extracted" || die "cannot create extraction directory"
tar -xf "$work_dir/archive.tar" -C "$work_dir/extracted" || die "extract failed after archive validation"
bin_dir=$work_dir/extracted/$bundle
for required in compound compound-breadth-engine; do
  path=$bin_dir/$required
  if [ ! -f "$path" ] || [ -L "$path" ] || [ ! -x "$path" ]; then
    die "archive is missing executable regular file $required"
  fi
done
identity_path=$bin_dir/RELEASE-IDENTITY
if [ ! -f "$identity_path" ] || [ -L "$identity_path" ]; then
  die "archive is missing RELEASE-IDENTITY"
fi

if ! awk -v version="$manifest_version" -v commit="$manifest_commit" -v os="$os" -v arch="$arch" '
  NR == 1 { if (NF != 2 || $1 != "schema" || $2 != "compound.release_archive.v0.1") exit 1; next }
  NR == 2 { if (NF != 2 || $1 != "version" || $2 != version) exit 1; next }
  NR == 3 { if (NF != 2 || $1 != "commit" || $2 != commit) exit 1; next }
  NR == 4 { if (NF != 3 || $1 != "platform" || $2 != os || $3 != arch) exit 1; next }
  NR == 5 { if (NF != 3 || $1 != "binary" || $2 != "compound" || length($3) != 64 || $3 ~ /[^0-9a-f]/) exit 1; next }
  NR == 6 { if (NF != 3 || $1 != "binary" || $2 != "compound-breadth-engine" || length($3) != 64 || $3 ~ /[^0-9a-f]/) exit 1; next }
  { exit 1 }
  END { if (NR != 6) exit 1 }
' "$identity_path"; then
  die "release archive identity is invalid"
fi
compound_expected=$(awk 'NR == 5 { print $3 }' "$identity_path")
breadth_expected=$(awk 'NR == 6 { print $3 }' "$identity_path")
compound_actual=$(sha256_file "$bin_dir/compound")
breadth_actual=$(sha256_file "$bin_dir/compound-breadth-engine")
[ "$compound_expected" = "$compound_actual" ] || die "compound checksum does not match release identity"
[ "$breadth_expected" = "$breadth_actual" ] || die "breadth-engine checksum does not match release identity"
binary_identity=$("$bin_dir/compound" version) || die "release binary version probe failed"
[ "$binary_identity" = "compound $manifest_version (build $manifest_commit)" ] || die "compound version/full-SHA identity does not match RELEASE"

# The canonical install root is the main shell's cwd for the entire critical
# section. A caller-facing symlink may retarget without redirecting mutations;
# renaming the physical directory itself is detected and fails closed.
validate_physical_install_root || die "physical install root changed while transaction lock was held"
crash_at after_lock
transaction_preserved=1
if ! recover_transaction; then
  die "interrupted install state is unsafe; recovery transaction preserved at $state_root"
fi
transaction_preserved=0
if [ "$recovered_install_state" = 1 ]; then
  printf 'install-release: recovered interrupted install state\n' >&2
  crash_at after_recovery
fi

write_owner_record || die "cannot publish install transaction owner"
transaction_active=1
crash_at after_owner

# Snapshot both prior targets completely before creating a durable prepared
# journal. The retained copies, not mutable live paths, are recovery authority.
if [ -e "$compound_target" ] || [ -L "$compound_target" ]; then
  [ -f "$compound_target" ] && [ ! -L "$compound_target" ] || die "existing compound target must be a regular non-symlink file"
  compound_prior_present=1
  compound_prior_type=regular
  compound_prior_digest=$(sha256_file "$compound_target")
else
  compound_prior_present=0
  compound_prior_type=absent
  compound_prior_digest=-
fi
if [ -e "$breadth_target" ] || [ -L "$breadth_target" ]; then
  [ -f "$breadth_target" ] && [ ! -L "$breadth_target" ] || die "existing breadth target must be a regular non-symlink file"
  breadth_prior_present=1
  breadth_prior_type=regular
  breadth_prior_digest=$(sha256_file "$breadth_target")
else
  breadth_prior_present=0
  breadth_prior_type=absent
  breadth_prior_digest=-
fi

mkdir "$transaction_next_dir" || die "cannot create staged binary transaction"
chmod 0700 "$transaction_next_dir" || die "cannot secure staged binary transaction"
cp "$bin_dir/compound" "$transaction_next_dir/compound.new" || die "cannot stage compound"
cp "$bin_dir/compound-breadth-engine" "$transaction_next_dir/compound-breadth-engine.new" || die "cannot stage breadth engine"
chmod 0755 "$transaction_next_dir/compound.new" "$transaction_next_dir/compound-breadth-engine.new" || die "cannot mark staged binaries executable"
target_has_digest "$transaction_next_dir/compound.new" "$compound_expected" || die "staged compound identity changed"
target_has_digest "$transaction_next_dir/compound-breadth-engine.new" "$breadth_expected" || die "staged breadth identity changed"
if [ "$compound_prior_present" = 1 ]; then
  cp "$compound_target" "$transaction_next_dir/compound.previous" || die "cannot retain prior compound"
  chmod 0755 "$transaction_next_dir/compound.previous" || die "cannot secure prior compound"
  target_has_digest "$transaction_next_dir/compound.previous" "$compound_prior_digest" || die "prior compound changed while retained"
fi
if [ "$breadth_prior_present" = 1 ]; then
  cp "$breadth_target" "$transaction_next_dir/compound-breadth-engine.previous" || die "cannot retain prior breadth engine"
  chmod 0755 "$transaction_next_dir/compound-breadth-engine.previous" || die "cannot secure prior breadth engine"
  target_has_digest "$transaction_next_dir/compound-breadth-engine.previous" "$breadth_prior_digest" || die "prior breadth engine changed while retained"
fi
write_journal_record prepared "$transaction_next_dir/journal" || die "cannot prepare transaction journal"
durability_barrier prepared_staged
validate_owner_record || die "prepared owner record changed"
[ "$recorded_owner_pid" = "$$" ] && [ "$recorded_owner_nonce" = "$lock_nonce" ] || die "prepared owner identity changed"
validate_journal_record_at "$transaction_next_dir/journal" || die "staged prepared transaction journal changed"
[ "$journal_owner_pid" = "$$" ] && [ "$journal_owner_nonce" = "$lock_nonce" ] && [ "$journal_phase" = prepared ] || die "staged prepared journal owner identity changed"
validate_transaction_inventory_at "$transaction_next_dir" || die "staged prepared transaction inventory changed"
remember_expected_journal
crash_at after_prepared_staged
mv "$transaction_next_dir" "$transaction_dir" || die "cannot publish prepared transaction namespace"
transaction_prepared=1
durability_barrier prepared
validate_physical_install_root || die "physical install root changed after preparation"
crash_at after_transaction_ready
crash_at after_prepared

# Re-validate the whole prepared state and both prior targets before the first
# publication rename. This is a second defence against caller-side races.
validate_owner_record || die "prepared owner record changed"
validate_journal_record || die "prepared transaction journal changed"
[ "$recorded_owner_pid" = "$$" ] && [ "$recorded_owner_nonce" = "$lock_nonce" ] || die "prepared owner identity changed"
[ "$journal_owner_pid" = "$$" ] && [ "$journal_owner_nonce" = "$lock_nonce" ] || die "prepared journal owner identity changed"
journal_matches_expected_phase prepared || die "prepared transaction generation changed"
validate_transaction_inventory || die "prepared transaction inventory changed"
if [ "$compound_prior_present" = 1 ]; then
  target_has_digest "$compound_target" "$compound_prior_digest" || die "compound target changed after preparation"
else
  [ ! -e "$compound_target" ] && [ ! -L "$compound_target" ] || die "compound target appeared after preparation"
fi
if [ "$breadth_prior_present" = 1 ]; then
  target_has_digest "$breadth_target" "$breadth_prior_digest" || die "breadth target changed after preparation"
else
  [ ! -e "$breadth_target" ] && [ ! -L "$breadth_target" ] || die "breadth target appeared after preparation"
fi

cp "$transaction_dir/compound.new" "$transaction_dir/compound.publish" || die "cannot stage compound publication"
crash_at after_compound_publish_copy
cp "$transaction_dir/compound-breadth-engine.new" "$transaction_dir/compound-breadth-engine.publish" || die "cannot stage breadth publication"
crash_at after_breadth_publish_copy
chmod 0755 "$transaction_dir/compound.publish" "$transaction_dir/compound-breadth-engine.publish" || die "cannot secure publication staging"
target_has_digest "$transaction_dir/compound.publish" "$compound_expected" || die "compound publication staging changed"
target_has_digest "$transaction_dir/compound-breadth-engine.publish" "$breadth_expected" || die "breadth publication staging changed"
durability_barrier publish_staged
crash_at after_compound_backup
crash_at after_breadth_backup

validate_physical_install_root || die "physical install root changed before publication"
mv "$transaction_dir/compound.publish" "$compound_target" || die "cannot publish compound"
crash_at after_compound_publish
if [ "${COMPOUND_INSTALL_FAIL_AFTER_FIRST_RENAME:-0}" = 1 ]; then
  die "injected failure after first binary rename"
fi
validate_physical_install_root || die "physical install root changed during publication"
mv "$transaction_dir/compound-breadth-engine.publish" "$breadth_target" || die "cannot publish breadth engine"
crash_at after_breadth_publish
durability_barrier targets
crash_at after_targets_synced

validate_physical_install_root || die "physical install root changed before home initialization"
if ! PATH="$install_dir:$PATH" COMPOUND_HOME="$compound_home" "$compound_target" home init --home "$compound_home" --global; then
  die "home init failed"
fi
if ! PATH="$install_dir:$PATH" COMPOUND_HOME="$compound_home" "$compound_target" home doctor --home "$compound_home"; then
  die "home doctor failed"
fi

target_has_digest "$compound_target" "$compound_expected" || die "published compound identity changed before commit"
target_has_digest "$breadth_target" "$breadth_expected" || die "published breadth identity changed before commit"
validate_physical_install_root || die "physical install root changed before commit"
write_journal_record committed "$transaction_dir/journal.next" || die "cannot prepare committed transaction journal"
durability_barrier committed_staged
crash_at after_committed_staged
validate_owner_record || die "transaction owner changed before commit publication"
[ "$recorded_owner_pid" = "$$" ] && [ "$recorded_owner_nonce" = "$lock_nonce" ] || die "transaction owner changed before commit publication"
validate_journal_record || die "prepared journal changed before commit publication"
journal_matches_expected_phase prepared || die "prepared journal changed before commit publication"
validate_transaction_inventory 1 || die "committed staging inventory changed"
validate_journal_record_at "$transaction_dir/journal.next" || die "committed staging journal changed"
journal_matches_expected_phase committed || die "committed staging generation changed"
validate_journal_record || die "prepared journal changed before commit publication"
journal_matches_expected_phase prepared || die "prepared journal changed before commit publication"
validate_expected_target_pair new || die "published targets changed before commit publication"
mv "$transaction_dir/journal.next" "$transaction_dir/journal" || die "cannot publish committed transaction journal"
durability_barrier committed
crash_at after_commit
crash_at after_committed

transaction_preserved=1
validate_physical_install_root || die "physical install root changed before cleanup"
validate_owner_record || die "installed binaries but transaction owner changed before cleanup"
[ "$recorded_owner_pid" = "$$" ] && [ "$recorded_owner_nonce" = "$lock_nonce" ] || die "installed binaries but transaction owner changed before cleanup"
validate_journal_record || die "installed binaries but journal changed before cleanup"
journal_matches_expected_phase committed || die "installed binaries but journal changed before cleanup"
validate_transaction_inventory || die "installed binaries but transaction inventory changed before cleanup"
validate_expected_target_pair new || die "installed binaries changed before cleanup"
retire_transaction_generation committed new || die "installed binaries but journal changed before cleanup"
retire_owner_record "$$" "$lock_nonce" || die "installed binaries but transaction owner changed before cleanup"
transaction_preserved=0
validate_physical_install_root || die "physical install root changed before install completion"

cat <<EOM

compound installed
- binary: $install_dir/compound
- breadth_engine: $install_dir/compound-breadth-engine
- home: $compound_home

next:
  export PATH="$install_dir:\$PATH"
  compound version
  compound workbench
EOM
