# Go test profiling

The release gate remains `compound proof-check`. This profiler is diagnostic
only: it neither changes that gate nor mints a verification reference.

Before a costly run, and again after replaying onto a changed base, reject
cheap formatting failures from the candidate root (including inherited files):

```bash
bash -euo pipefail <<'CHECK'
git diff --check
unformatted=$(git ls-files -z -- '*.go' | xargs -0 gofmt -l)
if [ -n "$unformatted" ]; then
  printf '%s\n' "$unformatted"
  exit 1
fi
CHECK
```

A listed file means stop and format it before spending time on the full suite.
This preflight is diagnostic only: it preserves the canonical gate inventory
and never substitutes for fresh certification of the exact committed candidate.

Capture fresh, per-test events outside the source tree. Preserve Go's exit
status before reading the profile; a successful summarizer is not proof that
the producer succeeded:

```bash
evidence_dir=$(mktemp -d)
go build -o "$evidence_dir/testprofile" ./scripts/testprofile || exit "$?"
go test -json -count=1 -timeout=40m ./... > "$evidence_dir/events.jsonl"
test_status=$?
"$evidence_dir/testprofile" "$evidence_dir/events.jsonl"
profile_status=$?
printf 'go-test exit=%s; profiler exit=%s; evidence=%s\n' "$test_status" "$profile_status" "$evidence_dir"
test "$test_status" -eq 0 && test "$profile_status" -eq 0
```

For a focused comparison, replace `./...` with the same package and `-run`
filter on both revisions, retaining `-count=1` (or the same repeat count).
Use matching Go versions, machine load, cache warmth and package parallelism;
record source SHAs and the original process exits beside the private evidence.
Do not run competing full suites while measuring latency.

The JSON ranks package durations and top-level test durations separately.
Nested test times overlap their parents; package durations overlap each other.
**Never sum these rows into total wall time or a promised speedup.** Repeated
runs remain separate samples. All failed subtests are retained, even when
short. Skips are counted; running/paused tests at EOF remain incomplete.
Malformed input still emits partial measurements and a parse error. Exit
codes are 0 for a complete passing input, 1 for failed/incomplete input, and
2 for input/output errors. These are diagnostic results, not certification.
The example builds the profiler because `go run` maps nonzero program exits
to shell status 1; use the executable when distinguishing status 1 from 2.

The profiler's own behavior is covered by `go test ./scripts/testprofile`
and therefore by the complete package inventory in the canonical Go-test gate.

## Canonical package scheduling

The `go-test` gate runs `go run ./scripts/testbattery general-release` and the
`go-test-home` gate runs `go run ./scripts/testbattery home`. The driver discovers
Git-owned package directories and resolves them with `go list -e -json`.
Execution has three phases: general packages, the complete release package alone,
and the complete home package alone. Standalone `go run ./scripts/testbattery test`
runs all three. Every package runs once with `-count=1 -timeout=40m`; there are no
test-name filters, short-mode exclusions or blanket `-p 1`.

Every invocation names its mode explicitly. The compiled driver exits 2 when no
mode is supplied, without inventorying packages or starting tests. Through
`go run`, Go reports `exit status 2` and returns shell status 1. `-h` and
`--help` return 0. Mode-less, flag-only and unknown-mode invocations do not
start tests; an explicit help request takes precedence over mode validation.

This isolates archive scale deadlines from contention with other packages without
changing any archive limit or production durability check. An ordinary first-phase
test failure still runs home and remains a failure. Invalid, duplicate, incomplete
or empty inventory, cancellation, child signals and start errors stop later
launches and fail closed. Inventory stderr is forwarded separately from package
names. Both test phases forward stdout and stderr.

Children inherit the proof runner's process group, execution barrier, Go cache
and toolchain environment. The driver adds no timeout or child-killing mechanism;
the canonical proof runner owns group cancellation and joining. Standalone
interruption likewise relies on process-group signals. The development profile
is unchanged and remains non-certifying. The canonical gate retains its name,
`go-test-count-1-all` proof ref and all 48 gates; certification still requires
a fresh committed-source canonical battery on the landed tip (the presubmit runs before the
land, the canonical battery after it).

## Turn measurements into cheaper execution

The operational archive scale tests retain unconditional elapsed-time, byte,
append-only, and heap bounds. Their `scaleSeamsOff` helper disables the three
durability seams only for scale measurements; the crash-recovery tests retain
real syncs. Profile an unexpectedly slow run before attributing it to fsync or
host load. Load-based assertion skipping is not a repair. Cross-compile test
code as well as production code when touching platform-sensitive helpers:

```bash
probe_dir=$(mktemp -d)
GOOS=linux GOARCH=amd64 go test -c -o "$probe_dir/home.test" ./internal/home
```

Choose the next change from observed cost: repeated build/setup, process
startup, storage work, polling, or contention. Reuse immutable binaries;
clone initialized homes into isolated mutable homes. Keep real-process tests
where process lifetime, exit codes, permissions or cancellation are the
behavior under test. Preserve failure-path assertions, not just happy paths.

The hook-enforcement tests use the existing suite-wide `compoundTestBinary`;
they do not share hook configs, captured responses, nonces or runtime homes.
Build reuse must not remove the genuine-response replay falsifier.

The fixture-only-route falsifier also proves the wrapper's fixture route still
works before rejecting its broken live stdin route. Canonical installed-hook
binding fixtures clear the ambient retrieval engine, memory profile and panel
endpoint; a poisoned engine records invocation if that isolation is removed.
These tests exercise the real hook commands, not a native harness consuming them.

Work CLI tests that only require preinitialized synthetic state reuse
`initializedTestHome(t)`. Each consumer still gets independent mutable files;
initialization, distinct identity, missing-home and clock-sensitive tests keep
their explicit setup. Run the clone-isolation tests with any such migration.

Further scheduling changes or exact-input proof reuse need a separately reviewed
verification contract. A diagnostic profile cannot authorize either.

## Source qualification vs the full architecture doctor

`internal/arch` exposes two entry points over ONE check inventory:

- `arch.Doctor(repoRoot, strict)` is the canonical FULL doctor. It runs the
  source checks plus three runtime audits: the live proveable-pair law (which
  resolves the ambient `COMPOUND_HOME`), the installed build freshness and the
  installed sidecar freshness. `compound architecture doctor --strict` and the
  proof-check architecture gate call this one; their behaviour is unchanged.
- `arch.DoctorSource(repoRoot, strict)` is the SOURCE-ONLY entry point. It runs
  the candidate map and every repository source/contract/leak/budget/
  classification check, the mint ratchet and the convergence ratchet, and it
  never resolves a Compound home, inspects installed artifacts or executes an
  installed Compound binary. The runtime audits are not filtered out of a
  finished report — they are not run.

Use `DoctorSource` for tests that qualify SOURCE (ownership, classification,
doc leak, budgets, contracts). Use `Doctor` for tests that qualify RUNTIME
(live pair law, installed drift). A source qualification that calls the full
doctor inherits the operator's mutable runtime records and installation, so its
result is not a property of the source under test.

Runtime paths in a source qualification are still confined: point `HOME`,
`GOBIN` and `COMPOUND_HOME` at disposable fixtures, and own any installation
fixture the test needs — never resolve the operator's real one. The separation
controls live in `internal/arch/source_qualification_test.go`.

## The source inventory does not read ambient Git state

The source inventory is `git ls-files` over the repo the doctor was asked to
certify. The child Git process runs under a fixed contract
(`gitSourceEnumerationEnv`), so the operator's environment cannot change WHICH
SOURCE FILES the doctor enumerates:

- every inherited `GIT_*` variable is filtered out — `GIT_DIR`, `GIT_WORK_TREE`,
  `GIT_INDEX_FILE`, the object directories and the `GIT_CONFIG_*` family must
  not redirect the doctor away from the checkout under test;
- the config files Git reads OUTSIDE the checkout are neutralized:
  `GIT_CONFIG_NOSYSTEM=1` plus `GIT_CONFIG_SYSTEM` and `GIT_CONFIG_GLOBAL`
  pointing at `os.DevNull`. A malformed `~/.gitconfig` or XDG config otherwise
  made `git ls-files` fail, which moved the doctor's findings — the doc-leak
  scan failed closed and the private-body contract fell back to a filesystem
  walk;
- `core.fsmonitor` is disabled for the enumeration with
  `-c core.fsmonitor=false`. The fsmonitor hook is an EXTERNAL PROGRAM Git runs
  while listing the index, so an ambient (or repo-local) `core.fsmonitor` made
  the enumeration execute whatever helper it named.

The repo's OWN `.git/config` is deliberately still consulted: the local index,
worktree and submodule semantics Git owns there are unchanged, and the tracked
inventory stays byte-identical. The runtime build-freshness audit is a separate
Git seam and keeps its own behaviour.

The controls live in `internal/arch/git_enumeration_test.go`. Each hostile
input is a disposable fixture (temp repo, temp home, temp XDG config home, temp
system/global config files, temp helper script), and each one carries a POSITIVE
CONTROL — a plain `git ls-files` that proves the hostility was live — so a
passing control can never mean "the attack never worked".

The raw controls run under an EXPLICITLY OWNED environment: every inherited
`GIT_*` variable is excluded and the system config is confined to a disposable
file, so an exported `GIT_CONFIG_GLOBAL` can neither redirect the fixture's own
Git commands nor mask the hostile globals under test. The hostile inputs
themselves are concrete — a malformed disposable system config, a malformed
disposable global config, and a config INJECTED through the environment
(`GIT_CONFIG_COUNT`) — and the enumeration child's own environment is asserted
to be exactly `GIT_CONFIG_NOSYSTEM=1` plus `GIT_CONFIG_SYSTEM` and
`GIT_CONFIG_GLOBAL` at `os.DevNull`, with the repo's own `.git/config` still
consulted.

## Own cooperative fixture lifetimes

A fixture that acknowledges a concurrent operation must stay alive through
that operation's preparation, not expire on an earlier independent deadline.
The proof-takeover fixtures start their observer before reservation, cancel it
when reservation returns, and join it on every exit path. They still exercise
real holder transfer, rejected predecessor completion, terminal RunAttempts,
and source-bound governance; production termination grace is unchanged.

Keep the controlled preparation-delay and cancellation/join falsifiers in
`TestProofTakeoverObserver*`. A passing retry alone does not explain an earlier
suite failure. Record the failing mechanism and retain the original red evidence.
