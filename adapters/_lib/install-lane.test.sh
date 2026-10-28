#!/usr/bin/env sh
# adapters/_lib/install-lane.test.sh
#
# Shell-level tests for the lane installer + registry. Run with `sh` or
# any POSIX shell. Exits 0 on success, non-zero on first failure.
#
# Tests use isolated /tmp directories and a fake `compound` binary (a shell
# stub that mimics just enough of the real command to keep install-lane.sh
# happy). No network. No filesystem outside /tmp.

set -eu

# --- fixtures ------------------------------------------------------------

LIB_DIR=$(CDPATH= dirname -- "$0")
# Compute absolute path without `cd && pwd` (we avoid that pattern because
# it's a known trap when sourced under a chpwd hook).
case "$LIB_DIR" in
  /*) ;;
  *)  LIB_DIR="$(pwd)/$LIB_DIR"
      [ -d "$LIB_DIR" ] || LIB_DIR="$(pwd)"
      ;;
esac

TMPDIR=$(mktemp -d 2>/dev/null) || TMPDIR=/tmp/install-lane.test.$$
mkdir -p "$TMPDIR"
trap 'rm -rf "$TMPDIR"' EXIT

# Fake `compound` binary: stub that always exits 0.
FAKE_BIN="$TMPDIR/fake-bin"
mkdir -p "$FAKE_BIN"
cat > "$FAKE_BIN/compound" <<'STUB'
#!/usr/bin/env sh
# Minimal stand-in for `compound`. Records `hook install` invocations to
# $HOOK_INSTALL_LOG (when set) so tests can assert the harness arg, then
# exits 0.
if [ "${1:-}" = "hook" ] && [ "${2:-}" = "install" ] && [ -n "${HOOK_INSTALL_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$HOOK_INSTALL_LOG"
fi
exit 0
STUB
chmod +x "$FAKE_BIN/compound"

# --- helpers -------------------------------------------------------------

# Assert two strings are equal.
assert_eq() {
  _expect="$1"
  _actual="$2"
  _msg="${3:-assertion}"
  if [ "$_expect" != "$_actual" ]; then
    echo "FAIL: $_msg"
    echo "  expected: $_expect"
    echo "  actual:   $_actual"
    exit 1
  fi
  printf "ok: %s\n" "$_msg"
}

# Assert a JSON file has a given jq expression.
assert_jq() {
  _file="$1"
  _expr="$2"
  _expect="$3"
  _msg="$4"
  _actual=$(jq -r "$_expr" "$_file" 2>/dev/null)
  if [ "$_actual" != "$_expect" ]; then
    echo "FAIL: $_msg"
    echo "  jq: $_expr"
    echo "  expected: $_expect"
    echo "  actual:   $_actual"
    exit 1
  fi
  printf "ok: %s\n" "$_msg"
}

# --- tests ---------------------------------------------------------------

echo "==> lane registry: lanes.sh + lanes.json"

LANES_SH_FILE="$LIB_DIR/lanes.sh" . "$LIB_DIR/lanes.sh"

# Canonical lane ids resolve to themselves.
for _id in zai zai-direct codex minimax qwen native codex-native; do
  _got=$(lane_resolve "$_id")
  assert_eq "$_id" "$_got" "lane_resolve($_id) = $_id"
done

# Aliases resolve to canonical ids.
assert_eq zai       "$(lane_resolve zai-full)"       "lane_resolve(zai-full) = zai"
assert_eq zai       "$(lane_resolve claude-zai)"     "lane_resolve(claude-zai) = zai"
assert_eq zai       "$(lane_resolve claude-zai-full)" "lane_resolve(claude-zai-full) = zai"
assert_eq codex     "$(lane_resolve claude-codex)"   "lane_resolve(claude-codex) = codex"
assert_eq codex     "$(lane_resolve gpt56-ultra)"    "lane_resolve(gpt56-ultra) = codex"
assert_eq minimax   "$(lane_resolve claude-minimax)" "lane_resolve(claude-minimax) = minimax"
assert_eq minimax   "$(lane_resolve 'MiniMax')"      "lane_resolve(MiniMax) = minimax"
assert_eq qwen      "$(lane_resolve claude-qwen)"    "lane_resolve(claude-qwen) = qwen"

# Unknown lanes resolve to empty.
if [ -n "$(lane_resolve no-such-lane 2>/dev/null)" ]; then
  echo "FAIL: lane_resolve(no-such-lane) should be empty"
  exit 1
fi
printf "ok: lane_resolve(unknown) is empty\n"

# lane_field returns expected colors.
assert_eq "0,180,255"  "$(lane_field zai tab_rgb)"     "zai tab_rgb"
assert_eq "5DB4FF"     "$(lane_field zai ansi_hex)"    "zai ansi_hex"
assert_eq "255,145,50" "$(lane_field minimax tab_rgb)" "minimax tab_rgb"
assert_eq "FF9132"     "$(lane_field minimax ansi_hex)" "minimax ansi_hex"

# lane_emit_sh returns valid shell variable assignments.
_eval_out=$(eval "$(lane_emit_sh minimax)" && printf '%s\n' "label=$LANE_LABEL r=$LANE_TAB_R icon=$LANE_ICON")
case "$_eval_out" in
  "label=minimax r=255 icon=◦") printf "ok: lane_emit_sh(minimax) sets LANE_* vars\n" ;;
  *) echo "FAIL: lane_emit_sh output: $_eval_out"; exit 1 ;;
esac

echo ""
echo "==> statusline-install.sh: idempotency"

PROFILE="$TMPDIR/profile-zai"
mkdir -p "$PROFILE"
printf '{"hooks":{}}' > "$PROFILE/settings.json"

LANES_SH_FILE="$LIB_DIR/lanes.sh" \
LANE_STATUSLINE_FILE="$LIB_DIR/lane-statusline.sh" \
  sh "$LIB_DIR/statusline-install.sh" \
    --profile-dir "$PROFILE" --lane zai >/dev/null

# StatusLine entry exists with the right command.
assert_jq "$PROFILE/settings.json" '(.statusLine.type)'      "command"                      "statusLine.type=command"
assert_jq "$PROFILE/settings.json" '(.statusLine.command)'   "$LIB_DIR/lane-statusline.sh --lane zai" "statusLine.command contains script"

# Save a checksum; rerun and verify byte-identical.
_md_before=$( (cd "$PROFILE" && md5sum settings.json | awk '{print $1}') )
LANES_SH_FILE="$LIB_DIR/lanes.sh" \
LANE_STATUSLINE_FILE="$LIB_DIR/lane-statusline.sh" \
  sh "$LIB_DIR/statusline-install.sh" \
    --profile-dir "$PROFILE" --lane zai >/dev/null
_md_after=$( (cd "$PROFILE" && md5sum settings.json | awk '{print $1}') )
assert_eq "$_md_before" "$_md_after" "rerun with same lane is byte-identical"

# Switch lanes → statusLine entry fully replaced.
LANES_SH_FILE="$LIB_DIR/lanes.sh" \
LANE_STATUSLINE_FILE="$LIB_DIR/lane-statusline.sh" \
  sh "$LIB_DIR/statusline-install.sh" \
    --profile-dir "$PROFILE" --lane minimax >/dev/null
assert_jq "$PROFILE/settings.json" '(.statusLine.command)'   "$LIB_DIR/lane-statusline.sh --lane minimax" "statusline replaced on lane switch"

# Switch back to original lane → byte-identical to first install.
LANES_SH_FILE="$LIB_DIR/lanes.sh" \
LANE_STATUSLINE_FILE="$LIB_DIR/lane-statusline.sh" \
  sh "$LIB_DIR/statusline-install.sh" \
    --profile-dir "$PROFILE" --lane zai >/dev/null
assert_jq "$PROFILE/settings.json" '(.statusLine.command)'   "$LIB_DIR/lane-statusline.sh --lane zai" "statusline replaced on lane switch back"

echo ""
echo "==> install-lane.sh: end-to-end with stub compound"

# Pick a lane with a stable profile default. Use 'zai-direct' so we don't
# disturb the user's existing claude-zai profile.
PROFILE2="$TMPDIR/profile-direct"
mkdir -p "$PROFILE2"

# Need a settings.json with the harness authority row that compound hook install
# would normally add. We don't actually run real compound — we just check our
# command sequences.
echo '{"hooks":{}}' > "$PROFILE2/settings.json"
# Capture the hook install invocation so we can assert the harness arg.
# install-lane.sh reads the lane's REAL hook harness from lanes.json
# (hook_harness field). For zai-direct that is claude-zai.
HOOK_INSTALL_LOG="$TMPDIR/hook-install.log"; export HOOK_INSTALL_LOG
: > "$HOOK_INSTALL_LOG"
LANES_SH_FILE="$LIB_DIR/lanes.sh" \
LANE_STATUSLINE_FILE="$LIB_DIR/lane-statusline.sh" \
  sh "$LIB_DIR/install-lane.sh" \
    --lane zai-direct \
    --profile-dir "$PROFILE2" \
    --compound "$FAKE_BIN/compound" \
    --steering-source "$LIB_DIR/README.md" 2>&1 | sed 's/\o033/<ESC>/g' >/dev/null

# The hook install call must target the lane's own hook harness (claude-zai for
# zai-direct) with the lane's own profile dir.
_hook_line=$(cat "$HOOK_INSTALL_LOG")
case "$_hook_line" in
  *"--harness claude-zai"*) printf "ok: zai-direct hook install targets claude-zai\n" ;;
  *) echo "FAIL: hook install harness arg wrong: $_hook_line"; exit 1 ;;
esac
case "$_hook_line" in
  *"--config-dir $PROFILE2"*) printf "ok: hook install uses the lane profile dir\n" ;;
  *) echo "FAIL: hook install config-dir arg wrong: $_hook_line"; exit 1 ;;
esac

# Statusline entry installed.
assert_jq "$PROFILE2/settings.json" '(.statusLine.command)' \
  "$LIB_DIR/lane-statusline.sh --lane zai-direct" \
  "install-lane wires statusline for zai-direct"

# CLAUDE.md symlink created.
[ -L "$PROFILE2/CLAUDE.md" ] || { echo "FAIL: CLAUDE.md not a symlink"; exit 1; }
printf "ok: CLAUDE.md symlinked\n"

# Idempotency: rerun with same lane → byte-identical settings.json.
_md1=$(md5sum "$PROFILE2/settings.json" | awk '{print $1}')
LANES_SH_FILE="$LIB_DIR/lanes.sh" \
LANE_STATUSLINE_FILE="$LIB_DIR/lane-statusline.sh" \
  sh "$LIB_DIR/install-lane.sh" \
    --lane zai-direct --profile-dir "$PROFILE2" \
    --compound "$FAKE_BIN/compound" --steering-source "$LIB_DIR/README.md" >/dev/null 2>&1
_md2=$(md5sum "$PROFILE2/settings.json" | awk '{print $1}')
assert_eq "$_md1" "$_md2" "install-lane rerun is byte-identical"

echo ""
echo "==> install-lane.sh: each lane installs its OWN hook harness"

# minimax must install --harness claude-minimax, NOT collapse into claude-zai.
# This is the core regression the adapter fix targets: a minimax lane that
# records as claude-zai breaks executor identity / hook attribution.
PROFILE_MM="$TMPDIR/profile-minimax"
mkdir -p "$PROFILE_MM"
echo '{"hooks":{}}' > "$PROFILE_MM/settings.json"
: > "$HOOK_INSTALL_LOG"
LANES_SH_FILE="$LIB_DIR/lanes.sh" \
LANE_STATUSLINE_FILE="$LIB_DIR/lane-statusline.sh" \
  sh "$LIB_DIR/install-lane.sh" \
    --lane minimax \
    --profile-dir "$PROFILE_MM" \
    --compound "$FAKE_BIN/compound" \
    --steering-source "$LIB_DIR/README.md" >/dev/null 2>&1
_mm_line=$(cat "$HOOK_INSTALL_LOG")
case "$_mm_line" in
  *"--harness claude-minimax"*) printf "ok: minimax hook install targets claude-minimax\n" ;;
  *) echo "FAIL: minimax must install claude-minimax, got: $_mm_line"; exit 1 ;;
esac
case "$_mm_line" in
  *"--harness claude-zai"*) echo "FAIL: minimax must NOT collapse into claude-zai: $_mm_line"; exit 1 ;;
  *) printf "ok: minimax does not collapse into claude-zai\n" ;;
esac

# qwen must install --harness claude-qwen (its own enrolled harness), NOT
# collapse into claude-zai -- same identity contract as minimax.
PROFILE_QW="$TMPDIR/profile-qwen"
mkdir -p "$PROFILE_QW"
echo '{"hooks":{}}' > "$PROFILE_QW/settings.json"
: > "$HOOK_INSTALL_LOG"
LANES_SH_FILE="$LIB_DIR/lanes.sh" \
LANE_STATUSLINE_FILE="$LIB_DIR/lane-statusline.sh" \
  sh "$LIB_DIR/install-lane.sh" \
    --lane qwen \
    --profile-dir "$PROFILE_QW" \
    --compound "$FAKE_BIN/compound" \
    --steering-source "$LIB_DIR/README.md" >/dev/null 2>&1
_qw_line=$(cat "$HOOK_INSTALL_LOG")
case "$_qw_line" in
  *"--harness claude-qwen"*) printf "ok: qwen hook install targets claude-qwen\n" ;;
  *) echo "FAIL: qwen must install claude-qwen, got: $_qw_line"; exit 1 ;;
esac
case "$_qw_line" in
  *"--harness claude-zai"*) echo "FAIL: qwen must NOT collapse into claude-zai: $_qw_line"; exit 1 ;;
  *) printf "ok: qwen does not collapse into claude-zai\n" ;;
esac

echo ""
echo "==> lane-launcher.sh: per-lane functions generated (bash)"
LANE_LAUNCHER_FILE="$LIB_DIR/lane-launcher.sh" \
  bash -c '
    export LANE_LAUNCHER_FILE
    unset LANES_SH_FILE LANE_IT2_FILE
    . "$LANE_LAUNCHER_FILE"
    for c in claude-zai claude-zai-full claude-codex claude-minimax claude-qwen codex-native claude-gpt56-ultra claude-MiniMax; do
      if [ "$(type -t "$c" 2>/dev/null)" != "function" ]; then
        echo "FAIL: missing function: $c"
        exit 1
      fi
      printf "ok: %s is a function\n" "$c"
    done
  '

echo ""
echo "==> lane-launcher.sh: per-lane functions generated (zsh)"
LANE_LAUNCHER_FILE="$LIB_DIR/lane-launcher.sh" \
  zsh -c '
    export LANE_LAUNCHER_FILE LANE_IT2_FILE HOME
    source "$LANE_LAUNCHER_FILE"
    set -- claude-zai claude-zai-full claude-zai-direct claude-codex claude-minimax claude-qwen codex-native claude-native claude-MiniMax codex-vendor claude-gpt56-ultra
    for c; do
      if ! typeset -f "$c" >/dev/null 2>&1; then
        print -- "FAIL: missing function: $c"
        exit 1
      fi
      print -- "ok: $c is a function"
    done
  '

echo ""
echo "==> lane-launcher.sh: profile expansion and tap bind validation (bash)"
LANE_LAUNCHER_FILE="$LIB_DIR/lane-launcher.sh" \
  bash -c '
    export LANE_LAUNCHER_FILE HOME
    unset LANES_SH_FILE LANE_IT2_FILE
    . "$LANE_LAUNCHER_FILE"
    HOME=/tmp/lane-test-home

    # _lane_profile_dir expands ${HOME} without eval.
    _out=$(_lane_profile_dir "" "${HOME}/.config/claude")
    if [ "$_out" != "/tmp/lane-test-home/.config/claude" ]; then
      echo "FAIL: profile dir expansion: $_out"
      exit 1
    fi
    printf "ok: profile dir expands ${HOME}\n"

    # Shell metacharacters in the registry value are not executed; they pass through literally.
    _out=$(_lane_profile_dir "" "${HOME}/x; echo pwned")
    if [ "$_out" != "/tmp/lane-test-home/x; echo pwned" ]; then
      echo "FAIL: profile dir eval escape: $_out"
      exit 1
    fi
    printf "ok: profile dir does not eval shell metacharacters\n"

    # A literal ${HOME} with an embedded double-quote exercises the difference between
    # string-substitution and eval: an eval-based implementation would see the unescaped
    # quote and either terminate early or produce a syntax error.
    _evil="\${HOME}/config\"evil"
    _out=$(_lane_profile_dir "" "$_evil")
    _expected="/tmp/lane-test-home/config\"evil"
    if [ "$_out" != "$_expected" ]; then
      echo "FAIL: profile dir embedded quote escape: $_out"
      exit 1
    fi
    printf "ok: profile dir handles embedded quotes without eval\n"

    # Valid host:port bindings (including empty for native lanes).
    _lane_tap_bind_is_valid "127.0.0.1:8080" || { echo "FAIL: valid IP:port rejected"; exit 1; }
    _lane_tap_bind_is_valid "localhost:18772" || { echo "FAIL: valid hostname:port rejected"; exit 1; }
    _lane_tap_bind_is_valid "" || { echo "FAIL: empty tap bind rejected"; exit 1; }

    # Malformed or dangerous bindings must be rejected before forming a URL.
    _lane_tap_bind_is_valid "127.0.0.1" && { echo "FAIL: missing port accepted"; exit 1; }
    _lane_tap_bind_is_valid "127.0.0.1:abc" && { echo "FAIL: non-numeric port accepted"; exit 1; }
    _lane_tap_bind_is_valid "127.0.0.1:8080:extra" && { echo "FAIL: extra colon accepted"; exit 1; }
    _lane_tap_bind_is_valid ";rm -rf /:1" && { echo "FAIL: shell metacharacter bind accepted"; exit 1; }
    _lane_tap_bind_is_valid "http://evil.com:1" && { echo "FAIL: scheme-in-host accepted"; exit 1; }
    printf "ok: tap bind validation rejects malformed values\n"
  '

echo ""
echo "==> lane-launcher.sh: profile expansion and tap bind validation (zsh)"
LANE_LAUNCHER_FILE="$LIB_DIR/lane-launcher.sh" \
  zsh -c '
    export LANE_LAUNCHER_FILE HOME
    unset LANES_SH_FILE LANE_IT2_FILE
    source "$LANE_LAUNCHER_FILE"
    HOME=/tmp/lane-test-home

    _out=$(_lane_profile_dir "" "${HOME}/.config/claude")
    if [ "$_out" != "/tmp/lane-test-home/.config/claude" ]; then
      print -- "FAIL: zsh profile dir expansion: $_out"
      exit 1
    fi
    print -- "ok: zsh profile dir expands ${HOME}"

    _out=$(_lane_profile_dir "" "${HOME}/x; echo pwned")
    if [ "$_out" != "/tmp/lane-test-home/x; echo pwned" ]; then
      print -- "FAIL: zsh profile dir eval escape: $_out"
      exit 1
    fi
    print -- "ok: zsh profile dir does not eval shell metacharacters"

    _evil="\${HOME}/config\"evil"
    _out=$(_lane_profile_dir "" "$_evil")
    _expected="/tmp/lane-test-home/config\"evil"
    if [ "$_out" != "$_expected" ]; then
      print -- "FAIL: zsh profile dir embedded quote escape: $_out"
      exit 1
    fi
    print -- "ok: zsh profile dir handles embedded quotes without eval"

    _lane_tap_bind_is_valid "127.0.0.1:8080" || { print -- "FAIL: zsh valid IP:port rejected"; exit 1; }
    _lane_tap_bind_is_valid "localhost:18772" || { print -- "FAIL: zsh valid hostname:port rejected"; exit 1; }
    _lane_tap_bind_is_valid "" || { print -- "FAIL: zsh empty tap bind rejected"; exit 1; }
    _lane_tap_bind_is_valid "127.0.0.1" && { print -- "FAIL: zsh missing port accepted"; exit 1; }
    _lane_tap_bind_is_valid "127.0.0.1:abc" && { print -- "FAIL: zsh non-numeric port accepted"; exit 1; }
    _lane_tap_bind_is_valid ";rm -rf /:1" && { print -- "FAIL: zsh shell metacharacter bind accepted"; exit 1; }
    print -- "ok: zsh tap bind validation rejects malformed values"
  '

echo ""
echo "==> lane-statusline.sh: renders distinct lanes from session JSON"

INPUT='{"workspace":{"current_dir":"/srv/compound"},"model":{"display_name":"Claude 4.7 Sonnet"},"cost":{"total_duration_ms":120000},"context_window":{"current_usage":{"cache_read_input_tokens":30000,"input_tokens":10000,"cache_creation_input_tokens":2000,"output_tokens":500},"context_window_size":200000}}'

for _lane in zai zai-direct codex minimax qwen native; do
  _first=$(COLUMNS=110 sh "$LIB_DIR/lane-statusline.sh" --lane "$_lane" <<EOF
$INPUT
EOF
)
  # First line must contain the lane icon and label (lowercased ANSI rendering
  # makes this tricky — just look for the lane id in a stripped form).
  _stripped=$(printf '%s' "$_first" | tr -d '\033' | sed 's/\[[0-9;]*m//g')
  case "$_stripped" in
    *"$_lane"*) printf "ok: statusline for %s contains lane label\n" "$_lane" ;;
    *) echo "FAIL: statusline for $_lane missing label: $_stripped"; exit 1 ;;
  esac
done

echo ""
echo "==> lane-it2.sh: OSC 6 escapes contain correct RGB"

# Capture it2_apply_lane output and verify the red/green/blue brightness values.
for _lane in zai minimax qwen; do
  _expected_r=$(lane_field "$_lane" tab_rgb | cut -d, -f1)
  _expected_g=$(lane_field "$_lane" tab_rgb | cut -d, -f2)
  _expected_b=$(lane_field "$_lane" tab_rgb | cut -d, -f3)
  _out=$(TERM_PROGRAM=iTerm.app LANE_IT2_FILE="$LIB_DIR/lane-it2.sh" \
    LANES_SH="$LIB_DIR/lanes.sh" \
    sh -c '. "$LANE_IT2_FILE"; it2_apply_lane "$1" p' -- "$_lane")
  if printf '%s' "$_out" | grep -q "bg;red;brightness" && \
     printf '%s' "$_out" | grep -qE "(^|[^0-9])${_expected_r}([^0-9]|$)"; then
    printf "ok: %s OSC6 sets red to %s\n" "$_lane" "$_expected_r"
  else
    echo "FAIL: $_lane does not emit red=$_expected_r in OSC6"
    exit 1
  fi
done

echo ""
echo "==========================================="
echo "  ALL TESTS PASSED"
echo "==========================================="
