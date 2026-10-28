# Getting Started

Start with one useful outcome in a project you already know. Install Compound,
connect one supported harness, then ask that harness to pursue the goal within
your boundaries. Keep advanced memory setup and unattended operation for later.

**This is a developer preview, not yet a no-terminal installer.** The steps below
are explicit so an agent or technical helper can assist without guessing. There
is no accepted setup-time promise or evidence yet from a nontechnical-user trial.

Your first checkpoint is simple: the installed version is identifiable, home
health is known, your harness receives context, and you can inspect what ran.
Durable history is not the same as automatic memory promotion or measured benefit.

| You want to… | Start here |
|---|---|
| Install a published release | Option A below; no Go required. **No stable release is published yet**, so this path fails on purpose today |
| Build an authenticated release from source | Option B below |
| Try unreleased source | Reviewed contributor checkout below |
| Connect your existing agent after installation | [First useful session](#first-useful-session) |
| See progress and health | [Console setup](console.md); requires a source checkout |
| Learn the internal lifecycle | [Explicit demonstration](#2-orient-on-your-repo) |

## Prerequisites

- **Git** (Compound uses git for delivery proof).
- **Go 1.27.1+** (the version in `go.mod`) — only required if you build from source (the prebuilt binary
  path needs no Go).
- For a prebuilt release: `curl`, `gh`, `sha256sum` or `shasum`, `gzip`, and
  `tar`. The installer also requires FD-mode `lockf -s -t 0 9` on macOS 15 or
  newer, or BusyBox-compatible `flock -n 9` on Linux. Authenticate `gh` before
  using the provenance check below. Compound itself needs no cloud account,
  API key, or external service at runtime.

Compound's public alpha is a strictly scoped, file-backed, single-user
system: file backend only, one trusted OS user on local disk, and explicit
home authority. The full support and security boundary — including the
current hardening gaps the docs name rather than paper over:
[`security/supported-scope.md`](./security/supported-scope.md).

Actor and operator labels record provenance; they do not independently
authenticate a person. Autonomous grant creation is outside this supported
journey. Interactive profiles remain observation-only unless their exact native
launch profile has accepted behavioral enforcement proof.

Optional (for the console): **Bun** and **Node 20+**.
Optional (for semantic memory): a local **Ollama** server — see
[`memory-quickstart.md`](./memory-quickstart.md).

## 1. Install

Release acceptance uses the shipped nine-file bundle, absolute installed binary
paths and explicit disposable homes. The owned Woodpecker release pipeline runs
the native Linux/amd64 drill; the Mac publisher runs it natively on Darwin/arm64
before creating a draft. It covers initialized first use, goal continuity, owned-process
interruption and fresh resume, rejected result acceptance, installer rollback,
and a restored-home product/decision consumer with the original home unavailable.
The report is candidate-bound evidence, not authorization to publish:

```sh
node scripts/installed-release-acceptance.mjs \
  /absolute/candidate-bundle FULL_CANDIDATE_COMMIT \
  /absolute/absent-evidence-root /absolute/prior-bundle FULL_PRIOR_COMMIT
```

The runner requires Node 20+ and a source checkout for synthetic fixtures; it
executes only installed Compound binaries. It never connects providers or real
accounts, starts schedulers, mints authority, tags, or publishes a release.

Saved continuity uses a synthetic local Codex-compatible worker, not model
inference. The drill verifies the selected executable, model and provider tuple,
interruption, and exact resume. A separate unsupported-adapter control must refuse
before launch without changing the work envelope or minting an attempt.

The historical `scripts/release-smoke.sh` entrypoint is retired and exits without
effects. It is not an installer or a substitute for governed `proof-check`.

Pick one:

### Option A — prebuilt binary (no Go required)

These are release-consumer instructions, not evidence that a release exists.
Use only a stable version that is already listed on the repository's Releases
page; the numeric value below is an example, not a claim that its assets have
been published. If no stable release is listed, the release URLs intentionally
fail—use the unreleased contributor-checkout path below instead.

Prebuilt public-alpha releases support exactly `darwin/arm64` on macOS 15 or newer
and `linux/amd64`. On another platform, build from source.

Choose an explicit stable release version and download its exact nine-file
inventory into a fresh directory. Do not pipe an installer from the mutable
`main` branch into a shell. Set `COMPOUND_RELEASE_REPOSITORY` to the exact
`owner/repository` identity shown on the public release page; the same value
binds downloads, the signing key, and the source remote below.

```sh
set -eu

VERSION=0.1.0
REPOSITORY=${COMPOUND_RELEASE_REPOSITORY:?set the public owner/repository identity}
if ! printf '%s\n' "$VERSION" | awk '
  $0 ~ /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ { ok = 1 }
  END { if (!ok) exit 1 }
'; then
  echo "VERSION must be strict stable SemVer" >&2
  exit 1
fi
TAG="v${VERSION}"
RELEASE_DIR="compound-release-${VERSION}"
BASE_URL="https://github.com/${REPOSITORY}/releases/download/${TAG}"

mkdir "$RELEASE_DIR"
ASSETS="
RELEASE
SHA256SUMS
install-release.sh
compound_${VERSION}_darwin_arm64.tar.gz
compound_${VERSION}_darwin_arm64.cdx.json
compound_${VERSION}_darwin_arm64_compound-breadth-engine.cdx.json
compound_${VERSION}_linux_amd64.tar.gz
compound_${VERSION}_linux_amd64.cdx.json
compound_${VERSION}_linux_amd64_compound-breadth-engine.cdx.json
"
for ASSET in $ASSETS; do
  curl --fail --location \
    --output "$RELEASE_DIR/$ASSET" \
    "$BASE_URL/$ASSET"
done
```

`SHA256SUMS` detects corruption and proves that the manifest, installer, and
archives are internally consistent with one release set. Because the checksum
file comes from the same release location as those files, checksums alone do
**not** authenticate who published them.

Before executing the installer, download the detached SSH signature and the trust
anchor from the same release tag. Compare its fingerprint with `SECURITY.md`:
`SHA256:iAb0Eaq9vv9CrfNRy/YbitZVbaSpwqE0wY8uf5jftRE`.

```sh
curl --fail --location --output "$RELEASE_DIR/SHA256SUMS.sig" "$BASE_URL/SHA256SUMS.sig"
curl --fail --location --output "$RELEASE_DIR/allowed_signers" \
  "https://raw.githubusercontent.com/${REPOSITORY}/${TAG}/release/allowed_signers"
FINGERPRINT=$(awk '{print $3, $4}' "$RELEASE_DIR/allowed_signers" | ssh-keygen -lf - | awk '{print $2}')
test "$FINGERPRINT" = 'SHA256:iAb0Eaq9vv9CrfNRy/YbitZVbaSpwqE0wY8uf5jftRE'
ssh-keygen -Y verify -f "$RELEASE_DIR/allowed_signers" -I compound-release \
  -n compound-release -s "$RELEASE_DIR/SHA256SUMS.sig" < "$RELEASE_DIR/SHA256SUMS"
(cd "$RELEASE_DIR" && if command -v sha256sum >/dev/null 2>&1; then
  sha256sum --check SHA256SUMS
else
  shasum -a 256 --check SHA256SUMS
fi)
```

This proves that the checksum bytes were signed by the key the repository
publishes, and that the downloaded bytes match those checksums. It does not
independently establish who controls that key beyond the repository. The
installer does not enforce this SSH signature check. Neither the signature nor
checksums make a Git tag or GitHub release immutable.

Select the archive for the current supported platform and run the verified
installer from its regular file. Defaults install to `~/.local/bin` and
initialize `~/.compound`; override those locations with `GOBIN` and
`COMPOUND_HOME`.

```sh
case "$(uname -s)/$(uname -m)" in
  Darwin/arm64) PLATFORM=darwin_arm64 ;;
  Linux/x86_64|Linux/amd64) PLATFORM=linux_amd64 ;;
  *) echo "unsupported prebuilt platform" >&2; exit 1 ;;
esac

COMPOUND_VERSION="$VERSION" \
COMPOUND_RELEASE_TARBALL="$RELEASE_DIR/compound_${VERSION}_${PLATFORM}.tar.gz" \
  sh "$RELEASE_DIR/install-release.sh"
```

The installer first verifies its own bytes against the colocated
`SHA256SUMS`, then validates `RELEASE`, the selected archive's checksum and
closed member layout, both binary identities, and the `compound` version/full
commit identity. It publishes both binaries under one install lock, restores
the previous pair on failure when possible, preserves recovery state if a
restore fails, and runs `compound home init` plus `home doctor`. If the
installer process is interrupted after preparing publication, the next
invocation acquires the persistent kernel lock, validates the rooted private
owner record, exact journal, retained old/new bytes, and both target hashes,
then either restores the pre-commit pair or finishes cleanup for an exact pair
already marked committed. Unexpected or mismatched recovery state is preserved
and future installs fail closed pending manual inspection; it is never guessed
away. Exact private `journal.next`, `.publish`, and `.restore` files are
unpublished scratch, not authority: only a validated prepared generation may
discard them, sync that cleanup, reparse the owner and main journal, and re-enter
strict inventory validation before recovery continues.

Target publication uses two sequential renames, not an atomic pair switch. A
crash can therefore leave a durable partial pair visible until the next installer
invocation acquires the lock and deterministically restores or reconciles the
journaled generation. Consumers that cannot tolerate that bounded interval must
stop invoking either binary until recovery completes.

For an offline or air-gapped installation, verify the SSH signature and checksums
on a connected machine before transfer, then carry the unchanged nine-file bundle
plus `SHA256SUMS.sig` and the tag-bound `allowed_signers` verification evidence.
Keep all names unchanged and all files colocated. The
offline installer reads these four files directly:

- `install-release.sh`
- `SHA256SUMS`
- `RELEASE`
- `compound_<version>_<supported-platform>.tar.gz`

The remaining four SBOM files and the other supported platform archive stay in
the exact release directory as the release's provenance and inventory
evidence. On the offline machine, run the same platform-selection block above,
or point to the selected archive explicitly:

```sh
COMPOUND_VERSION=0.1.0 \
COMPOUND_RELEASE_TARBALL=/path/to/compound-release-0.1.0/compound_0.1.0_linux_amd64.tar.gz \
  sh /path/to/compound-release-0.1.0/install-release.sh
```

### Option B — build from source

Select the same stable tag and authenticate its `RELEASE` manifest before
trusting the full commit recorded inside it. This path fetches only that tag;
it never builds the repository's mutable default branch.

```sh
set -eu

VERSION=0.1.0
REPOSITORY=${COMPOUND_RELEASE_REPOSITORY:?set the public owner/repository identity}
TAG="v${VERSION}"
SOURCE_EVIDENCE="compound-${VERSION}-source-evidence"
mkdir "$SOURCE_EVIDENCE"
BASE_URL="https://github.com/${REPOSITORY}/releases/download/${TAG}"
for ASSET in RELEASE SHA256SUMS SHA256SUMS.sig; do
  curl --fail --location --output "$SOURCE_EVIDENCE/$ASSET" "$BASE_URL/$ASSET"
done
curl --fail --location --output "$SOURCE_EVIDENCE/allowed_signers" \
  "https://raw.githubusercontent.com/${REPOSITORY}/${TAG}/release/allowed_signers"
# Compare this fingerprint with SECURITY.md before accepting the signature.
FINGERPRINT=$(awk '{print $3, $4}' "$SOURCE_EVIDENCE/allowed_signers" | ssh-keygen -lf - | awk '{print $2}')
test "$FINGERPRINT" = 'SHA256:iAb0Eaq9vv9CrfNRy/YbitZVbaSpwqE0wY8uf5jftRE'
ssh-keygen -Y verify -f "$SOURCE_EVIDENCE/allowed_signers" -I compound-release \
  -n compound-release -s "$SOURCE_EVIDENCE/SHA256SUMS.sig" < "$SOURCE_EVIDENCE/SHA256SUMS"
# Only RELEASE is needed for a source build; verify its one signed checksum.
awk '$2 == "RELEASE" { print; n++ } END { if (n != 1) exit 1 }' \
  "$SOURCE_EVIDENCE/SHA256SUMS" > "$SOURCE_EVIDENCE/RELEASE.sha256"
(cd "$SOURCE_EVIDENCE" && if command -v sha256sum >/dev/null 2>&1; then
  sha256sum --check RELEASE.sha256
else
  shasum -a 256 --check RELEASE.sha256
fi)
COMMIT=$(awk '
  NR == 1 { if (NF != 2 || $1 != "schema" || $2 != "compound.release.v0.2") exit 1; next }
  NR == 2 { if (NF != 2 || $1 != "version" || $2 != version) exit 1; next }
  NR == 3 { if (NF != 2 || $1 != "commit" || length($2) != 40 || $2 ~ /[^0-9a-f]/) exit 1; print $2; next }
  NR == 4 { if (NF != 2 || $1 != "installer" || $2 != "install-release.sh") exit 1; next }
  NR == 5 { if (NF != 6 || $1 != "artifact" || $2 != "darwin" || $3 != "arm64") exit 1; next }
  NR == 6 { if (NF != 6 || $1 != "artifact" || $2 != "linux" || $3 != "amd64") exit 1; next }
  { exit 1 }
  END { if (NR != 6) exit 1 }
' version="$VERSION" "$SOURCE_EVIDENCE/RELEASE")

mkdir compound
git init compound
cd compound
git remote add origin "https://github.com/${REPOSITORY}.git"
git fetch --no-tags --depth=1 origin "refs/tags/$TAG:refs/tags/$TAG"
test "$(git rev-parse "refs/tags/$TAG^{commit}")" = "$COMMIT"
git checkout --detach "$COMMIT"
test "$(git rev-parse HEAD)" = "$COMMIT"
make install
```

This stages and checks both `compound` and `compound-breadth-engine`, plus any
explicitly enabled alias and autonomy helpers, before replacing installed files.
It initializes `COMPOUND_HOME` at `~/.compound` and runs home/runtime doctors and
the required installed-lane binding refresh. Publication and activation are
reported separately; a failed refresh fails installation.

Before initializing the home or publishing artifacts, the staged CLI runs
`compound harness refresh-lane --preflight`. This read-only check validates every
required native hook writer before any profile is refreshed. It prints nothing
and exits 0 when every writer is available; a nonzero exit carries the reason.
It does not certify activation or provider acceptance.
An absent home needs no initialization for this check; an existing home must
have a compatible manifest. Ordinary refresh retains mutation admission.
OpenCode registration belongs to `adapters/opencode/install.ts`; oh-my-pi shim
installation belongs to `compound hook install --harness oh-my-pi` and does not
publish settings-style hook authority. Entries for either adapter are reported
as `external` with their owner's command named, and do not stop source
installation: neither records an `invocation_revision`, and each reaches the
binary through a stable path rather than a pinned identity, so publication can
leave neither half-activated. Refreshing them stays that owner's job — run the
named command to pick up a new contract. A harness with no known owner at all is
still a hard refusal, and removing manifest entries is never a repair.

Source installation shares the release installer's kernel lock. Stop consumers
of the installed binaries/helpers during installation: the set is published by
sequential renames. On ordinary failure or a handled signal, the source installer
restores previous artifact bytes, modes, symlinks and absent files. Home or profile
changes made during activation are not undone; inspect binding state before
retrying. If restoration fails, retained old/new files remain in the named
`.compound-source-stage.*` directory and another source install refuses to run.
SIGKILL, power loss and filesystem durability are not covered by this trap-based
rollback. An interrupted source installation requires inspection of its retained
files; it has no automatic crash replay. Immutable tagged releases retain the
journaled recovery protocol described above. Source installation refuses pending
release recovery state and never repairs or deletes that owner's journal.
The release workflow proves the tag commit is contained in remote `main` before
building and publishing. Repository administrators must still activate tag
immutability, protected-branch rules, and the private vulnerability-reporting
channel before cutting the first tag; those external controls are pre-tag gates,
not properties this source checkout can create.

```sh
# Make sure it's on your PATH:
export PATH="$HOME/.local/bin:$PATH"
compound version
```

### Unreleased contributor checkout

Before the first stable tag—or when testing a change that has not been
released—work only from a source checkout already pinned to a commit you have
reviewed:

```sh
test -z "$(git status --short)"
git rev-parse HEAD
make install
```

This produces an unreleased development build, not a release artifact. Record
the printed commit before testing, and do not present that installation as a
published Compound version.

## First useful session

After installation, keep the same explicit runtime home across terminals.
Run these checks before starting work:

```sh
compound version
compound home doctor
```

If you built with `go build` instead of `make install`, no runtime home exists yet: run
`compound home init` once first, or every command above reports a missing manifest.

A failed doctor is not a clean home. Follow its reported issue before recovery
or upgrade; do not delete runtime data to clear a warning. The source-setup
section can report missing Go on a binary-only installation: Go is required
for building, not for using the prebuilt CLI.

Choose **one** managed interactive adapter:

```sh
# Claude Code
compound hook install --harness claude-code --scope global
compound hook install --harness claude-code --scope global --check --json

# OR Codex
compound hook install --harness codex --scope global
compound hook install --harness codex --scope global --check --json
```

These commands change the chosen harness configuration. Keep its existing
permission/sandbox settings; a fresh process is needed to load updated hooks.
The managed interactive default observes actions, not blocks them.
[Harness Wiring](harness-wiring.md) covers OpenCode, isolated guarded profiles
and adapter limitations. Listed or installed does not mean execution-ready.

The hook check verifies registration, not model-executor readiness. Governed
model execution also needs your chosen harness installed and authenticated,
Compound's model-route metadata for that executor, and an authorized executor
route for the work. Inspect the route setup without launching a model:

```sh
compound route model-capabilities --json
compound route lane-projection --harness codex --json
```

Use `--harness claude-code` instead if that is your chosen harness. A fresh home
can report missing `registries/model-capabilities.yaml`; repeating hook
installation will not create it. Follow [First model route](harness-wiring.md#first-model-route)
to enroll your existing executor configuration. If it is not configured yet,
the [local-command demonstration](#2-orient-on-your-repo) remains available;
it does not run a model.

Start your harness in your project and give it a bounded goal, for example:

> Inspect this project's tests and identify one reproducible maintenance issue.
> Create and claim one bounded Compound task. Record its owner and acceptance
> predicate, then save an authorized executor selection before execution.
> Keep changes local, run the relevant checks, and report what is verified
> and what remains unknown.
> Do not publish, spend, or change permissions.

The harness must establish a work item, owner, acceptance predicate and authorized
selection before execution. Compound does not promise that every harness turns
arbitrary prose into an admitted task. The [explicit lifecycle demonstration](#2-orient-on-your-repo)
below shows those mechanics for integrators; it is not a request to make users
copy IDs every session.

Before launching, have the harness or setup helper run the work-bound
[readiness check](harness-wiring.md#first-model-route) for the claimed work,
chosen harness and real project directory. It does not launch a model or prove
that a model request will succeed.

## Daily use and recovery

Use the same actor that holds the work. With exactly one held selected task and
a saved executor, Continue resolves the task and preserves its goal and selection:

```sh
compound work continue --selected --actor <your-existing-actor> --json
```

Replace the actor placeholder with the identity already holding your work.
Initial creation/claim is separate. More than one held task is an ambiguity to
resolve, not permission to pick whichever is convenient.

- **See progress:** `compound workbench` and `compound coordination`, or the
  local dashboard's Today, Live and Work views.
- **Interrupted or verifying:** inspect the reported attempt first. When the
  selected path reports a recoverable, settled attempt, the same command with
  `--resume` requests a new authorized run. Never blindly repeat an uncertain
  external effect.
- **Worker finished:** a successful process is not acceptance. Supply the
  workflow's actual verification before closing; do not manufacture proof
  strings just to mark work done.
- **Needs you:** changed goals, missing authority, new spending and genuine
  quality decisions remain explicit. There is no silent provider fallback.
- **Fresh session:** use the same home and actor. Saved work remains available;
  absence of a memory citation does not prove absence of reuse.

For backups and upgrades, follow [Recovery](operations/recovery.md).
A verified archive and a tested restore into a new target are distinct checks.
For branch hygiene, inspect `compound executors worktrees --repo /path/to/project --json`.
Landed, dirty, live, kept and unknown worktrees are different states; “landed”
does not authorize deleting a checkout. Retirement uses exact reviewed plans.

## Explicit lifecycle demonstration

The remaining walkthrough teaches the underlying objects using a local command,
without a model call. Its outcome is inspectable history, not proof of learning.
Completing it does not enroll a model route or prove that an agent can execute
the same goal.

## 2. Orient on YOUR repo

Point Compound at any repo you work on. This produces a **ContextPack** — the
frozen context an agent would consume, including what was included **and
excluded** and why.

```sh
cd /path/to/your/project
compound orient --harness claude-code --cwd . --task "describe what this project does" --json
```

Orientation is bound to real identities before it writes anything. Compound
resolves the harness through its registry (`claude-code` becomes the canonical
`claude_code` identity), resolves `--cwd` to an absolute symlink-free existing
directory, and refuses unknown harnesses, missing paths, or files used as a
workspace. A successful ContextPack therefore describes a registered harness
and a real project directory rather than a caller-supplied fiction.

You'll see a JSON ContextPack. Copy its `id` (`ctx_...`) into
`<context_pack_id>` in the next step. Replace all angle-bracket placeholders in
this guide before running their commands. Key fields:

- `summary_md` — plain-language orientation summary
- `included[]` — what context was served (and why)
- `excluded[]` — what was deliberately left out (and why)
- `id` — the briefing identity to attach to your work

This is the spine's first move: **discover reality before acting.** The agent
gets task-scoped context, not a wall of everything.

## 3. Create a work envelope

A WorkEnvelope is one unit of work with a stable identity (`work_id`) that
links intent → context → action → receipt → transition.

```sh
compound work create \
  --owner "getting-started" \
  --predicate "the harness prints hello and records proof" \
  --context-pack <context_pack_id> \
  --state ready \
  --by "operator:you"
```

Note the returned `work_id` (e.g., `work_a1b2c3d4e5f6`).

```sh
# See it on the board:
compound workbench

# See the full envelope:
compound work show <work_id>
```

## 4. Claim and execute

```sh
# Claim it (single-holder lease):
compound work claim <work_id> --by "agent:claude-code:session-1"

# Run a bounded command through the governed execution path:
compound work advance <work_id> \
  --to running \
  --by "agent:claude-code:session-1" \
  --execute \
  --harness os-harness-entrypoints \
  --workspace "$PWD" \
  -- echo "hello from compound"

# Inspect the WorkEnvelope and the persisted RunAttempt:
compound work show <work_id> --json
compound run-attempt list --json
```

A successful command prints a `run_attempt_id`, captures a transcript, and
moves the work to `verifying`. The WorkEnvelope forward-links that `ratt_...`
record, so the run remains reachable from the work on both success and failure.

You can stop after claiming and return in a later terminal. Use
`compound workbench` and `compound work show <work_id> --json` to recover the
claimed work and its `ids.context_pack_id`, then run the execution command
with the same `--by` identity. This is resumption before launch, not automatic
recovery of an interrupted subprocess; inspect the RunAttempt before retrying
a command that may already have produced side effects.

## 5. Close with proof

The successful RunAttempt is already attached to the work. Close the verifying
work to mint a **Receipt** and a **Transition** recording the demonstration's
outcome. This is an audit of a local command, so use `audit_only`; printing hello
does not establish a product improvement or learned memory.

```sh
compound work close <work_id> \
  --by "agent:claude-code:session-1" \
  --outcome completed \
  --move-type audit_only \
  --summary "the local demonstration produced inspectable execution evidence"
```

The close output includes both `receipt` and `transition` IDs.

```sh
# See the proof chain:
compound work show <work_id> --json

# See the transition:
compound transition show <transition_id>
```

The Transition records the work's state change and supporting evidence. The
receipt is available for later inspection; it does not by itself promote memory
or guarantee inclusion in a later orientation.

## 6. Wire your agent (hooks)

Compound integrates with agent harnesses through **lifecycle hooks**. See
[`harness-wiring.md`](./harness-wiring.md) for the full guide. The short
version for Claude Code:

```sh
compound hook install --harness claude-code --scope global
compound hook install --harness claude-code --scope global --check --json
```

The first command installs the managed interactive posture; the second checks
it without changing settings. This posture observes actions. See the wiring
guide for the separately guarded executor profiles.

When hooks are wired:
- **Session start** → agent receives a ContextPack (task-scoped orientation)
- **Pre-tool-use** → records ActionIntent + PolicyDecision evidence; the
  interactive default does not block tool calls
- **Session end** → the bounded adapter records session outcome evidence

## 7. Verify the loop

```sh
# The workbench shows your closed work with its receipt:
compound workbench

# Check the local runtime home:
compound home doctor

# Check that the recorded work's proof references resolve:
compound work proof-chain-doctor
```

## What just happened

```text
orient           → the briefing (ContextPack: task-scoped context, not a preamble)
work create      → the job at the door (WorkEnvelope: one stable identity)
claim/--execute  → the edge at work (RunAttempt + transcript, linked to the work)
close            → the receipt (Receipt + Transition: recorded execution outcome)
hooks            → policy evidence; enforced postures can gate before mutation
```

Open a new terminal or agent session and run `compound workbench`, then
`compound work show <work_id> --json`. The completed work still links to the
original briefing and execution evidence, so the next session can inspect what
happened without reconstructing this demonstration.

Automatic recall is a separate step: Compound selects relevant adjudicated
memory for an orientation. This walkthrough proves durable execution history,
not automatic learning from every receipt. Continue with the
[Memory Quickstart](memory-quickstart.md) to set up memory deliberately.

## Next steps

| I want to... | Read |
|---|---|
| Read the support and security boundary | [`security/supported-scope.md`](./security/supported-scope.md) |
| Wire Compound into my agent harness | [`harness-wiring.md`](./harness-wiring.md) |
| Set up semantic memory (search over my docs) | [`memory-quickstart.md`](./memory-quickstart.md) |
| See the visual console | [`console.md`](./console.md) |
| Understand the architecture | [`LAYERS.md`](./LAYERS.md) |
| Contribute to Compound itself | [`CONTRIBUTING.md`](../CONTRIBUTING.md) |
