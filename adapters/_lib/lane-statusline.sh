#!/usr/bin/env sh
# adapters/_lib/lane-statusline.sh
#
# Claude Code statusLine hook — rendered once per turn.
#
# Reads session JSON from stdin (Claude Code's documented contract). Renders a
# lane-themed, terminal-width-aware banner:
#
#   <lane icon> <lane label>                   PAI-style header
#   ─────────────────────────────────────────────────────────────
#   <dir> @ <branch>   *N    ±N      <age>     git status
#   <bar>  N% (Nk/Nk)   <duration>             context usage, gradient bar
#   ✦ "<short quote>"  — <author>              (only in 'normal' width)
#
# Lane identity comes from `--lane <id>` (looked up via lanes.json) so each
# profile is visually distinct. Architecture is loosely based on the PAI
# statusline (greeting + git + context); it deliberately stays smaller than
# PAI's full version because we want this to be cheap to re-render every turn.
#
# Usage:
#   ~/.local/bin/lane-statusline.sh --lane zai     < feed_of_claude_json
#   OR
#   cat session.json | lane-statusline.sh --lane minimax
#
# Width modes (cols):
#   nano  < 40    dir only, context bar 4 cells
#   micro < 60    dir + branch, context bar 6 cells
#   mini  < 90    plus counts and age
#   normal >= 90  full banner with quote

set -eu

# --- arg parsing -----------------------------------------------------------
LANE_ID=""
LANES_SH_FILE_DEFAULT=""
ARG_SKIPPED=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --lane)        LANE_ID="${2:-}"; shift 2 ;;
    --lanes-json)  LANES_JSON="${2:-}"; shift 2 ;;
    -h|--help)     sed -n '2,30p' "$0"; exit 0 ;;
    *)             shift ;;
  esac
done

# Locate this file's directory (works when invoked from any cwd because the
# adapters tree is self-contained). Avoid `cd && pwd` because the user's zshrc
# has a chpwd hook that prints OSC escapes; capturing that into $(...) corrupts
# the captured path.
SELF="${LANE_STATUSLINE_FILE:-$(command -v lane-statusline.sh 2>/dev/null || echo "$0")}"
case "$SELF" in
  /*) SELF_DIR=$(dirname -- "$SELF") ;;
  *)  SELF_DIR=$(dirname -- "./$SELF") ;;
esac
unset SELF

LANES_SH="${LANES_SH:-$SELF_DIR/lanes.sh}"
. "$LANES_SH"

if [ -z "$LANE_ID" ]; then
  echo "lane-statusline.sh: --lane <id> is required" >&2
  exit 1
fi
eval "$(lane_emit_sh "$LANE_ID")"
LANE_RGB_HEX="#$LANE_ANSI_HEX"
LANE_RGB_RGB=$(printf '%d;%d;%d' "$LANE_TAB_R" "$LANE_TAB_G" "$LANE_TAB_B")

# --- terminal width --------------------------------------------------------
_term_width() {
  if [ -n "${KITTY_WINDOW_ID:-}" ] && command -v kitty >/dev/null 2>&1; then
    kitty @ ls | jq -r --arg id "$KITTY_WINDOW_ID" '
      .[].tabs[].windows[] | select(.id == ($id | tonumber)) | .columns
    ' 2>/dev/null
    return
  fi
  if [ -n "${COLUMNS:-}" ]; then printf '%s\n' "$COLUMNS"; return; fi
  tput cols 2>/dev/null || echo 80
}
WIDTH=$(_term_width)
[ "$WIDTH" -lt 40 ] 2>/dev/null && MODE=nano  || true
[ "$WIDTH" -ge 40 ] && [ "$WIDTH" -lt 60 ] && MODE=micro
[ "$WIDTH" -ge 60 ] && [ "$WIDTH" -lt 90 ] && MODE=mini
[ "$WIDTH" -ge 90 ] && MODE=normal

# --- read input JSON -------------------------------------------------------
INPUT=$(cat)

DIR=$(echo "$INPUT" | jq -r '.workspace.current_dir // .cwd // ""')
MODEL=$(echo "$INPUT" | jq -r '.model.display_name // ""')
DURATION_MS=$(echo "$INPUT" | jq -r '.cost.total_duration_ms // 0')
CACHE_READ=$(echo "$INPUT" | jq -r '.context_window.current_usage.cache_read_input_tokens // 0')
INPUT_TOK=$(echo "$INPUT" | jq -r '.context_window.current_usage.input_tokens // 0')
CACHE_CREATE=$(echo "$INPUT" | jq -r '.context_window.current_usage.cache_creation_input_tokens // 0')
OUTPUT_TOK=$(echo "$INPUT" | jq -r '.context_window.current_usage.output_tokens // 0')
CTX_MAX=$(echo "$INPUT" | jq -r '.context_window.context_window_size // 200000')

# Sanitize to ints (jq returns number type; coerce)
DIR=$(printf '%s' "$DIR")
MODEL=$(printf '%s' "$MODEL")
DURATION_MS=$(printf '%d' "$DURATION_MS" 2>/dev/null || echo 0)
CACHE_READ=$(printf '%d' "$CACHE_READ" 2>/dev/null || echo 0)
INPUT_TOK=$(printf '%d' "$INPUT_TOK" 2>/dev/null || echo 0)
CACHE_CREATE=$(printf '%d' "$CACHE_CREATE" 2>/dev/null || echo 0)
OUTPUT_TOK=$(printf '%d' "$OUTPUT_TOK" 2>/dev/null || echo 0)
CTX_MAX=$(printf '%d' "$CTX_MAX" 2>/dev/null || echo 200000)

# --- palette ---------------------------------------------------------------
RESET='\033[0m'
SLATE_400='\033[38;2;148;163;184m'
SLATE_500='\033[38;2;100;116;139m'
SLATE_600='\033[38;2;71;85;105m'
LANE="\033[38;2;${LANE_RGB_RGB}m"
LANE_BG="\033[48;2;${LANE_RGB_RGB}m"
LANE_DIM="\033[38;2;$((LANE_TAB_R*30/100));$((LANE_TAB_G*30/100));$((LANE_TAB_B*30/100))m"
EMERALD='\033[38;2;74;222;128m'
AMBER='\033[38;2;251;191;36m'
ROSE='\033[38;2;251;113;133m'

# Bar gradient: emerald -> amber -> rose as we cross 33 / 66.
_bar_color() {
  pct=$1
  if [ "$pct" -le 33 ] 2>/dev/null; then echo "$EMERALD"
  elif [ "$pct" -le 66 ] 2>/dev/null; then echo "$AMBER"
  else echo "$ROSE"
  fi
}

_render_bar() {
  width=$1; pct=$2
  filled=$((pct * width / 100))
  if [ "$filled" -lt 0 ] 2>/dev/null; then filled=0; fi
  if [ "$filled" -gt "$width" ] 2>/dev/null; then filled="$width"; fi
  i=1
  out=""
  while [ "$i" -le "$width" ]; do
    if [ "$i" -le "$filled" ]; then
      c=$(_bar_color "$pct")
    else
      c="$SLATE_600"
    fi
    out="${out}${c}▮${RESET}"
    i=$((i + 1))
  done
  printf '%b' "$out"
}

# --- duration formatting ---------------------------------------------------
_fmt_dur() {
  s=$((DURATION_MS / 1000))
  if [ "$s" -ge 3600 ]; then printf '%dh%dm' $((s / 3600)) $(((s % 3600) / 60))
  elif [ "$s" -ge 60 ]; then printf '%dm%ds' $((s / 60)) $((s % 60))
  else printf '%ds' "$s"
  fi
}

# --- git status (best effort) ---------------------------------------------
DIR_BASENAME=$(basename "$DIR")
BRANCH=""
MODIFIED=0
STAGED=0
UNTRACKED=0
STASH=0
if [ -d "$DIR/.git" ] || (cd "$DIR" 2>/dev/null && git rev-parse --git-dir >/dev/null 2>&1); then
  cd "$DIR" 2>/dev/null || true
  BRANCH=$(git branch --show-current 2>/dev/null || echo "")
  MODIFIED=$(git diff --name-only 2>/dev/null | wc -l | tr -d ' ')
  STAGED=$(git diff --cached --name-only 2>/dev/null | wc -l | tr -d ' ')
  UNTRACKED=$(git ls-files --others --exclude-standard 2>/dev/null | wc -l | tr -d ' ')
  STASH=$(git stash list 2>/dev/null | wc -l | tr -d ' ')
fi
cd "${OLDPWD:-}" 2>/dev/null || cd / 2>/dev/null || true

# --- render header (always) ------------------------------------------------
printf "%b" "${LANE}${LANE_ICON}${RESET} ${LANE}${LANE_LABEL}${RESET}  ${SLATE_500}·${RESET} ${SLATE_400}${DIR_BASENAME}${RESET}"
if [ -n "$BRANCH" ]; then
  printf "%b" "  ${SLATE_500}@${RESET} ${SLATE_400}${BRANCH}${RESET}"
fi
if [ -n "$MODEL" ]; then
  printf "%b" "  ${SLATE_500}·${RESET} ${SLATE_500}${MODEL}${RESET}"
fi
printf "\n"

# --- render context line ---------------------------------------------------
TOTAL=$((CACHE_READ + INPUT_TOK + CACHE_CREATE + OUTPUT_TOK))
# Add a small offset (~22k) for system prompt/tools; PAI uses this. Tunable.
BASELINE=22000
USED=$((TOTAL + BASELINE))
if [ "$CTX_MAX" -gt 0 ] && [ "$USED" -gt 0 ]; then
  PCT=$((USED * 100 / CTX_MAX))
  if [ "$PCT" -gt 100 ] 2>/dev/null; then PCT=100; fi
  if [ "$PCT" -lt 0 ] 2>/dev/null; then PCT=0; fi
else
  PCT=0
fi
USED_K=$((USED / 1000))
MAX_K=$((CTX_MAX / 1000))

# Choose bar width per mode
case "$MODE" in
  nano)        BAR=4 ;;
  micro|mini)  BAR=8 ;;
  *)           BAR=12 ;;
esac

DURATION=$(_fmt_dur)

if [ "$MODE" = "nano" ]; then
  printf "%b\n" "  ${SLATE_500}◉${RESET} $(_render_bar "$BAR" "$PCT")$(_bar_color "$PCT")${PCT}%%${RESET}  ${SLATE_500}⏱${RESET} ${SLATE_400}${DURATION}${RESET}"
elif [ "$MODE" = "micro" ]; then
  printf "%b\n" "  ${SLATE_500}◉${RESET} $(_render_bar "$BAR" "$PCT")$(_bar_color "$PCT")${PCT}%%${RESET}  ${SLATE_500}(${USED_K}k)${RESET}  ${SLATE_500}⏱${RESET} ${SLATE_400}${DURATION}${RESET}"
elif [ "$MODE" = "mini" ]; then
  printf "%b\n" "  ${SLATE_500}◉${RESET} ${SLATE_400}CONTEXT${RESET} $(_render_bar "$BAR" "$PCT")$(_bar_color "$PCT")${PCT}%%${RESET}  ${SLATE_500}(${USED_K}k/${MAX_K}k)${RESET}  ${SLATE_500}⏱${RESET} ${SLATE_400}${DURATION}${RESET}"
else
  printf "%b\n" "  ${SLATE_500}◉${RESET} ${SLATE_400}CONTEXT${RESET} $(_render_bar "$BAR" "$PCT")$(_bar_color "$PCT")${PCT}%%${RESET}  ${SLATE_500}(${USED_K}k / ${MAX_K}k)${RESET}  ${SLATE_500}⏱${RESET} ${SLATE_400}${DURATION}${RESET}"
fi

# --- git detail (mini and normal) ----------------------------------------
if [ "$MODE" = "mini" ] || [ "$MODE" = "normal" ]; then
  bits=""
  if [ "$MODIFIED" -gt 0 ] 2>/dev/null; then bits="${bits}  ${LANE_DIM}±${MODIFIED}${RESET}"; fi
  if [ "$STAGED" -gt 0 ]    2>/dev/null; then bits="${bits}  ${LANE_DIM}+${STAGED}${RESET}"; fi
  if [ "$UNTRACKED" -gt 0 ] 2>/dev/null; then bits="${bits}  ${LANE_DIM}?${UNTRACKED}${RESET}"; fi
  if [ "$STASH" -gt 0 ]     2>/dev/null; then bits="${bits}  ${LANE_DIM}≡${STASH}${RESET}"; fi
  if [ -n "$bits" ]; then
    printf "%b\n" "  ${SLATE_500}◆${RESET} ${SLATE_400}git${RESET}${bits}"
  fi
fi

# --- quote (normal only, short circuit) -----------------------------------
if [ "$MODE" = "normal" ]; then
  QUOTES="$HOME/.claude/PAI/MEMORY/STATE/quotes.jsonl"
  if [ -f "$QUOTES" ]; then
    Q=$(tail -n 1 "$QUOTES" 2>/dev/null)
    QT=$(echo "$Q" | jq -r '.text // ""' 2>/dev/null)
    QA=$(echo "$Q" | jq -r '.author // ""' 2>/dev/null)
    if [ -n "$QT" ]; then
      printf "%b\n" "  ${LANE_DIM}✦${RESET} ${SLATE_400}\"${QT}\"${RESET} ${LANE_DIM}—${QA}${RESET}"
    fi
  fi
fi
