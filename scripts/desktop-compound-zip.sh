#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
usage: scripts/desktop-compound-zip.sh [options]

Create a Desktop snapshot zip from the current compound checkout:
  1. remove existing ~/Desktop/compound and ~/Desktop/compound.zip
  2. copy the compound repo to ~/Desktop/compound
  3. remove top-level external memory projects except the keep list
  4. write AUDIT-NOTES.md describing package-time git state
  5. zip ~/Desktop/compound.zip

Default mode is for a ChatGPT Pro source audit:
  - keeps scripts, tools, console source, private docs, .git,
    root binaries, and other local audit material
  - always excludes .claude/ (harness session state and agent worktrees —
    the worktrees are duplicate checkouts of this same tree and were the
    reason snapshots ballooned past the source folder)
  - always excludes .codex/, .agents/, /state/, .pytest_cache/, .ua/,
    /.hermes/, /body/ — same rationale as .claude/ (harness/runtime or generated
    analysis state, duplicate checkouts, not part of the audited source).
    /body/ is the switchback body-store sidecar (live SQLite + WAL + lock
    files at the repo root); it is runtime state, never source truth.
    Skipping these keeps rsync from walking ~10 GB of agent worktrees on
    every run.
  - excludes dependency/build artifacts: node_modules, .next
  - removes Finder/system junk: .DS_Store, __MACOSX
  - enforces a hard zip budget (--max-zip-mb, default 480): an oversized
    zip is deleted and the run fails with the largest staged files listed.
    Default is 480 (not 500) to leave headroom under common 512 MB upload
    caps used by ChatGPT Pro and similar services.
  - removes top-level memory/* external clones except names in the keep list
  - writes AUDIT-NOTES.md at the top of the staged copy so an auditor sees
    package-time HEAD/branch/dirty-state/ahead-behind before reading the tree

Use --upload-safe for the smaller public-audit bundle:
  - also excludes .git, node_modules, .next, root binary, bin/, home/,
    docs_private/, .compound-private/, Plans/, and *.private.md

Options:
  --repo PATH              Source repo. Default: git root for this script.
  --desktop PATH           Destination desktop directory. Default: ~/Desktop.
  --name NAME              Staged folder name. Default: compound.
  --zip NAME               Zip filename. Default: compound.zip.
  --upload-safe            Produce the smaller public-audit bundle.
  --delta                  Produce the small delta-review bundle: source tree
                           without .git, .compound-private/, memory/, and
                           docs_private/plan/packets/ bulk, so external
                           reviewers with constrained extraction sandboxes can
                           open it. Pair it with a git bundle of the candidate
                           branches plus a COMPOUND_DELTA_PACKET.md (documented
                           in the audit workflow); those carry the actual delta.
  --include-node-modules   Keep dependency/build artifacts in the local snapshot.
  --include-private        With --upload-safe, keep private docs; harness/runtime directories remain excluded.
  --keep-memory NAME       Keep top-level memory/NAME. Can be repeated.
                           Default keep list: compound.
  --no-default-memory      Clear the default keep-memory list.
  --exclude PATTERN        Extra rsync exclude pattern (repeatable). Applied in
                           addition to the built-in excludes; use it to trim
                           staged bulk (e.g. release-rehearsal binaries) when the
                           zip budget trips. Patterns are rsync exclude rules:
                           a leading / anchors to the transfer root, * does not
                           cross /, no leading / matches at any depth.
  --max-zip-mb N           Hard budget for the finished zip in MB. Over budget:
                           list largest staged files, delete the zip, exit 1.
                           0 disables the check. Default: 480.
  --dry-run                Print actions without deleting, copying, or zipping.
  -h, --help               Show this help.

Environment:
  COMPOUND_KEEP_MEMORY_PROJECTS="name other-name" overrides the default keep list.

Notes:
  Memory pruning only applies to top-level memory/* external clones. It does not
  remove Compound source surfaces like internal/mem, docs/memory-architecture.md,
  or console memory pages.

  AUDIT-NOTES.md is generated fresh on every run from the SOURCE checkout's git
  state (HEAD, branch, worktree cleanliness, ahead/behind vs upstream) at the
  moment of packaging. Extractors that drop unix exec bits (e.g. Python zipfile)
  make an extracted copy look dirty even when the source repo was clean at
  package time, and push/branch state moves on after the snapshot is taken —
  read AUDIT-NOTES.md before flagging either as a finding.
USAGE
}

die() {
  printf 'desktop-compound-zip: %s\n' "$*" >&2
  exit 1
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(git -C "$script_dir/.." rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -z "$repo_root" ]]; then
  repo_root="$(cd "$script_dir/.." && pwd -P)"
fi

repo_path="$repo_root"
desktop_path="${COMPOUND_DESKTOP:-$HOME/Desktop}"
stage_name="${COMPOUND_STAGE_NAME:-compound}"
zip_name="${COMPOUND_ZIP_NAME:-compound.zip}"
upload_safe=0
delta=0
include_private=1
include_node_modules=0
dry_run=0
max_zip_mb=480

extra_excludes=()

keep_memory=()
if [[ -n "${COMPOUND_KEEP_MEMORY_PROJECTS:-}" ]]; then
  # shellcheck disable=SC2206
  keep_memory=(${COMPOUND_KEEP_MEMORY_PROJECTS})
else
  keep_memory=("compound")
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      [[ $# -ge 2 ]] || die "--repo requires a path"
      repo_path="$2"
      shift 2
      ;;
    --desktop)
      [[ $# -ge 2 ]] || die "--desktop requires a path"
      desktop_path="$2"
      shift 2
      ;;
    --name)
      [[ $# -ge 2 ]] || die "--name requires a folder name"
      stage_name="$2"
      shift 2
      ;;
    --zip)
      [[ $# -ge 2 ]] || die "--zip requires a file name"
      zip_name="$2"
      shift 2
      ;;
    --upload-safe)
      upload_safe=1
      include_private=0
      shift
      ;;
    --delta)
      delta=1
      shift
      ;;
    --include-node-modules)
      include_node_modules=1
      shift
      ;;
    --include-private)
      include_private=1
      shift
      ;;
    --keep-memory)
      [[ $# -ge 2 ]] || die "--keep-memory requires a top-level memory directory name"
      keep_memory+=("$2")
      shift 2
      ;;
    --no-default-memory)
      keep_memory=()
      shift
      ;;
    --exclude)
      [[ $# -ge 2 ]] || die "--exclude requires an rsync exclude pattern"
      extra_excludes+=("$2")
      shift 2
      ;;
    --max-zip-mb)
      [[ $# -ge 2 ]] || die "--max-zip-mb requires a number"
      [[ "$2" =~ ^[0-9]+$ ]] || die "--max-zip-mb must be a non-negative integer"
      max_zip_mb="$2"
      shift 2
      ;;
    --dry-run)
      dry_run=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

[[ "$stage_name" != "" ]] || die "--name cannot be empty"
[[ "$zip_name" != "" ]] || die "--zip cannot be empty"
[[ "$stage_name" != "." && "$stage_name" != ".." && "$stage_name" != */* ]] || die "--name must be a simple folder name"
[[ "$zip_name" != "." && "$zip_name" != ".." && "$zip_name" != */* ]] || die "--zip must be a simple file name"
[[ -d "$repo_path" ]] || die "repo path does not exist: $repo_path"

command -v rsync >/dev/null 2>&1 || die "rsync is required"
command -v zip >/dev/null 2>&1 || die "zip is required"

repo_real="$(cd "$repo_path" && pwd -P)"
mkdir -p "$desktop_path"
desktop_real="$(cd "$desktop_path" && pwd -P)"
stage_path="$desktop_real/$stage_name"
zip_path="$desktop_real/$zip_name"

[[ "$repo_real" != "$stage_path" ]] || die "source repo and Desktop stage resolve to the same path"
[[ "$stage_path" == "$desktop_real/"* ]] || die "stage path must stay under desktop"
[[ "$zip_path" == "$desktop_real/"* ]] || die "zip path must stay under desktop"

run() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  if [[ "$dry_run" -eq 0 ]]; then
    "$@"
  fi
}

# Renders AUDIT-NOTES.md content describing the SOURCE checkout's git state
# at package time (never the staged copy, never $PWD). Read-only: only runs
# git queries, never mutates the source repo.
generate_audit_notes() {
  local snapshot_utc head_sha branch status_lines status_verdict
  local upstream ahead behind ahead_behind_line

  snapshot_utc="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  if git -C "$repo_real" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    head_sha="$(git -C "$repo_real" rev-parse HEAD 2>/dev/null || printf unknown)"
    branch="$(git -C "$repo_real" rev-parse --abbrev-ref HEAD 2>/dev/null || printf unknown)"
    status_lines="$(git -C "$repo_real" status --porcelain 2>/dev/null || true)"
    if [[ -z "$status_lines" ]]; then
      status_verdict="worktree CLEAN at package time (git status --porcelain reported no changes)."
    else
      status_verdict="worktree DIRTY at package time. git status --porcelain reported:
$status_lines"
    fi
    if upstream="$(git -C "$repo_real" rev-parse --abbrev-ref --symbolic-full-name @{upstream} 2>/dev/null)"; then
      read -r behind ahead <<<"$(git -C "$repo_real" rev-list --left-right --count @{upstream}...HEAD 2>/dev/null)"
      behind="${behind:-0}"
      ahead="${ahead:-0}"
      ahead_behind_line="ahead $ahead, behind $behind, vs upstream $upstream (package time only)."
    else
      ahead_behind_line="no upstream configured for branch '$branch'; ahead/behind not applicable."
    fi
  else
    head_sha="unknown (source path is not inside a git work tree)"
    branch="unknown"
    status_verdict="unknown (source path is not inside a git work tree)"
    ahead_behind_line="unknown (source path is not inside a git work tree)"
  fi

  cat <<NOTES
# AUDIT-NOTES.md

Generated by scripts/desktop-compound-zip.sh at package time. This file
describes the SOURCE repo's own git state at the moment this snapshot was
zipped — it does not describe the extracted copy you are reading now.

## Snapshot
- snapshot_utc: $snapshot_utc
- head_sha: $head_sha
- branch: $branch

## Package-time worktree status
$status_verdict

## Package-time ahead/behind
$ahead_behind_line

## Caveats
1. Many extractors (e.g. Python zipfile) drop unix exec bits. File-mode diffs
   (100755 -> 100644) observed in an extracted copy are packaging artifacts,
   NOT repo state. Judge modes against this note, not the extracted tree.
2. Push/branch state advances after snapshot; "ahead N" above reflects
   package time only.
3. .claude/, .codex/, .agents/, state/, .pytest_cache/, .ua/, .hermes/,
   body/, and graphify-out/ are always excluded because they contain
   harness/runtime state, caches, generated analysis output, duplicate
   agent checkouts, or live body-store sidecar databases (body/ is the
   switchback body-store SQLite/WAL/lock state).
   Their absence is a packaging rule, not repo state.

Cross-check findings against this note before flagging repo hygiene.
NOTES
}

rsync_args=(
  -a
  --delete
  --exclude ".DS_Store"
  --exclude "__MACOSX/"
  --exclude "/.claude/"
  --exclude "/.codex/"
  --exclude "/.agents/"
  --exclude "/state/"
  --exclude "/.pytest_cache/"
  --exclude "/.ua/"
  --exclude "/.hermes/"
  --exclude "/body/"
  --exclude "/graphify-out/"
)

if [[ "$delta" -eq 0 ]]; then
  rsync_args+=(--filter "+ /memory/")
  for name in ${keep_memory+"${keep_memory[@]}"}; do
    [[ -n "$name" && "$name" != "." && "$name" != ".." && "$name" != */* ]] || die "invalid keep-memory name: $name"
    rsync_args+=(--filter "+ /memory/$name/***")
  done
  rsync_args+=(--filter "- /memory/***")
else
  rsync_args+=(
    --exclude "/.git/"
    --exclude "/.compound-private/"
    --exclude "/memory/"
    --exclude "/docs_private/plan/packets/"
  )
fi

for pattern in ${extra_excludes+"${extra_excludes[@]}"}; do
  rsync_args+=(--exclude "$pattern")
done

if [[ "$include_node_modules" -eq 0 ]]; then
  rsync_args+=(
    --exclude "node_modules/"
    --exclude ".next/"
  )
fi

if [[ "$upload_safe" -eq 1 ]]; then
  rsync_args+=(
    --exclude ".git/"
    --exclude "/compound"
    --exclude "/bin/"
    --exclude "/home/"
  )
  if [[ "$include_private" -eq 0 ]]; then
    rsync_args+=(
      --exclude "/docs_private/"
      --exclude "/.compound-private/"
      --exclude "/Plans/"
      --exclude "*.private.md"
    )
  fi
fi

printf '# compound Desktop snapshot zip\n'
printf -- '- source: %s\n' "$repo_real"
printf -- '- stage: %s\n' "$stage_path"
printf -- '- zip: %s\n' "$zip_path"
printf -- '- mode: %s\n' "$([[ "$delta" -eq 1 ]] && printf delta || { [[ "$upload_safe" -eq 1 ]] && printf upload-safe || printf full-local; })"
printf -- '- include_private: %s\n' "$include_private"
printf -- '- include_node_modules: %s\n' "$include_node_modules"
printf -- '- keep_memory: %s\n' "${keep_memory[*]:-(none)}"
if [[ "${#extra_excludes[@]}" -gt 0 ]]; then
  printf -- '- extra_excludes:\n'
  for pattern in "${extra_excludes[@]}"; do
    printf -- '    %s\n' "$pattern"
  done
fi

run rm -rf -- "$stage_path" "$zip_path"
run mkdir -p -- "$stage_path"
run rsync "${rsync_args[@]}" "$repo_real/" "$stage_path/"

if [[ "$dry_run" -eq 0 && -d "$stage_path/memory" ]]; then
  if ! find "$stage_path/memory" -mindepth 1 -print -quit | grep -q .; then
    rmdir "$stage_path/memory"
  fi
fi

audit_notes_path="$stage_path/AUDIT-NOTES.md"
run sh -c 'cat > "$1"' _ "$audit_notes_path" <<<"$(generate_audit_notes)"

if [[ "$dry_run" -eq 0 ]]; then
  find "$stage_path" -name ".DS_Store" -delete
  (
    cd "$desktop_real"
    COPYFILE_DISABLE=1 zip -qr "$zip_path" "$stage_name"
  )
  file_count="$(find "$stage_path" -type f | wc -l | tr -d ' ')"
  zip_bytes="$(wc -c < "$zip_path" | tr -d ' ')"
  if [[ "$max_zip_mb" -gt 0 && "$zip_bytes" -gt $((max_zip_mb * 1024 * 1024)) ]]; then
    printf 'desktop-compound-zip: zip is %s MB, over the %s MB budget. Largest staged files:\n' \
      "$((zip_bytes / 1024 / 1024))" "$max_zip_mb" >&2
    find "$stage_path" -type f -size +1024k -exec du -k {} + 2>/dev/null \
      | sort -rn | head -15 | awk '{printf "  %8.1f MB  %s\n", $1/1024, $2}' >&2
    rm -f -- "$zip_path"
    die "oversized zip deleted; exclude the offenders above (or raise --max-zip-mb) and re-run"
  fi
  printf -- '- files: %s\n' "$file_count"
  printf -- '- zip_bytes: %s (budget: %s MB)\n' "$zip_bytes" "$max_zip_mb"
  printf -- '- audit_notes: %s\n' "$audit_notes_path"
  printf -- '- done: %s\n' "$zip_path"
else
  printf -- '- dry_run: no files changed\n'
fi
