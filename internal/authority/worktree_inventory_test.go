package authority

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// notLiveWindow is a runtime-relative future offset so fixture files (created
// "now" by git) read as far older than the activity window — no fixed fixture
// date, so no staleness time bomb.
func inventoryFutureNow() time.Time { return time.Now().Add(48 * time.Hour) }

// W1 + W2 + W3 + W4 + main class: one ground-truth build over a fixture that
// would misclassify under a wrong implementation. Crucially W1 asserts the
// stalled worktree is NOT landed_clean (the data-loss guard).
func TestBuildWorktreeInventoryClassifiesGroundTruth(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	live := inventoryFakeLiveness{active: map[string]bool{fix.laneLive: true}}

	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: live,
	}, fix.repo, WorktreeInventoryOptions{Now: inventoryFutureNow()})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	if inv.SchemaVersion != WorktreeInventorySchemaVersion || inv.MainTip != fix.mainTip {
		t.Fatalf("envelope header wrong: schema=%q main=%q", inv.SchemaVersion, inv.MainTip)
	}

	byBranch := map[string]WorktreeInventoryRow{}
	var mainRow, probeRow *WorktreeInventoryRow
	for i := range inv.Rows {
		row := inv.Rows[i]
		if row.IsMain {
			mainRow = &inv.Rows[i]
			continue
		}
		if row.Detached {
			probeRow = &inv.Rows[i]
			continue
		}
		byBranch[row.Branch] = row
	}

	if mainRow == nil || mainRow.Class != WorktreeClassMain {
		t.Fatalf("main row = %+v", mainRow)
	}
	// W4: detached HEAD -> probe, even when dirty.
	if probeRow == nil || probeRow.Class != WorktreeClassProbe {
		t.Fatalf("probe row = %+v", probeRow)
	}
	if probeRow.DirtyFiles == 0 {
		t.Fatalf("probe fixture should be dirty to prove class survives dirtiness: %+v", probeRow)
	}

	// W1: unique commit + dirty, dead, no recent activity -> stalled_unlanded,
	// and explicitly NOT landed_clean.
	stalled := byBranch["lane-stalled"]
	if stalled.Class != WorktreeClassStalledUnlanded {
		t.Fatalf("lane-stalled class = %q, want stalled_unlanded; row=%+v", stalled.Class, stalled)
	}
	if stalled.Class == WorktreeClassLandedClean {
		t.Fatal("data-loss guard breached: unlanded worktree read as landed_clean")
	}
	if stalled.AheadCommits < 1 || stalled.DirtyFiles < 1 {
		t.Fatalf("lane-stalled should hold unique commit + dirty file: %+v", stalled)
	}

	// W2: contained + clean + not live -> landed_clean.
	if got := byBranch["lane-landed"].Class; got != WorktreeClassLandedClean {
		t.Fatalf("lane-landed class = %q, want landed_clean; row=%+v", got, byBranch["lane-landed"])
	}

	// W3: identical containment/cleanliness as W2 but liveness observes open
	// handles -> live (liveness overrides containment).
	liveRow := byBranch["lane-live"]
	if liveRow.Class != WorktreeClassLive {
		t.Fatalf("lane-live class = %q, want live; row=%+v", liveRow.Class, liveRow)
	}
	if liveRow.Class == WorktreeClassLandedClean {
		t.Fatal("live lane read as landed_clean — liveness must override containment")
	}

	// Counts reflect the classes.
	if inv.Counts.Main != 1 || inv.Counts.Probe != 1 || inv.Counts.StalledUnlanded != 1 ||
		inv.Counts.LandedClean != 1 || inv.Counts.Live != 1 {
		t.Fatalf("counts = %+v", inv.Counts)
	}
}

// Inventory liveness is one snapshot over the complete root set. Its cost must
// not grow by launching one external probe per worktree row.
func TestBuildWorktreeInventoryTakesExactlyOneLivenessSnapshot(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	live := &inventoryRecordingSnapshotLiveness{active: map[string]bool{fix.laneLive: true}}

	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: live,
	}, fix.repo, WorktreeInventoryOptions{Now: inventoryFutureNow()})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	if live.calls != 1 {
		t.Fatalf("liveness snapshot calls = %d, want exactly 1", live.calls)
	}
	if len(live.roots) != len(inv.Rows) {
		t.Fatalf("snapshot roots = %d, inventory rows = %d", len(live.roots), len(inv.Rows))
	}
	if row := inventoryRowByBranch(t, inv, "lane-live"); row.Class != WorktreeClassLive {
		t.Fatalf("snapshot active evidence was not classified live: %+v", row)
	}
}

func TestLSOFWorktreeInventoryLivenessAttributesNestedOpenPathToLongestRoot(t *testing.T) {
	mainRoot := filepath.Join(t.TempDir(), "repo")
	childRoot := filepath.Join(mainRoot, ".claude", "worktrees", "child")
	if err := os.MkdirAll(childRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &inventoryRecordingProcessRunner{result: WorktreeRetirementProcessResult{
		Stdout: "p123\nfcwd\nn" + filepath.Join(childRoot, "nested") + "\n",
	}}

	observations := (LSOFWorktreeInventoryLiveness{Runner: runner}).Snapshot(
		context.Background(), []string{mainRoot, childRoot},
	)
	if runner.calls != 1 {
		t.Fatalf("lsof process calls = %d, want exactly 1", runner.calls)
	}
	wantArgs := []string{"-w", "-n", "-P", "-a", "-d", "cwd", "-Fn"}
	if runner.command != "lsof" || !reflect.DeepEqual(runner.args, wantArgs) {
		t.Fatalf("lsof invocation = %q %v, want %q %v", runner.command, runner.args, "lsof", wantArgs)
	}
	if got := observations[childRoot].State; got != WorktreeLivenessActive {
		t.Fatalf("nested child liveness = %q, want active; observations=%+v", got, observations)
	}
	if got := observations[childRoot].Evidence; !strings.Contains(got, "advisory CWD snapshot") || !strings.Contains(got, "governed retirement") {
		t.Fatalf("active evidence must name the advisory CWD scope and stronger retirement proof: %q", got)
	}
	if got := observations[mainRoot].State; got != WorktreeLivenessUnused {
		t.Fatalf("parent MAIN inherited child handle: state=%q, want unused; observations=%+v", got, observations)
	}
}

func TestLSOFWorktreeInventoryLivenessRejectsStringPrefixWithoutPathBoundary(t *testing.T) {
	mainRoot := filepath.Join(t.TempDir(), "repo")
	prefixSibling := mainRoot + "-archive"
	if err := os.MkdirAll(prefixSibling, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &inventoryRecordingProcessRunner{result: WorktreeRetirementProcessResult{
		Stdout: "p456\nfcwd\nn" + prefixSibling + "\n",
	}}

	observations := (LSOFWorktreeInventoryLiveness{Runner: runner}).Snapshot(
		context.Background(), []string{mainRoot},
	)
	if got := observations[mainRoot].State; got != WorktreeLivenessUnused {
		t.Fatalf("prefix sibling matched MAIN without a path boundary: state=%q, want unused; observations=%+v", got, observations)
	}
}

func TestLSOFWorktreeInventoryLivenessTimeoutPreservesPartialActiveEvidence(t *testing.T) {
	mainRoot := filepath.Join(t.TempDir(), "repo")
	childRoot := filepath.Join(mainRoot, ".claude", "worktrees", "child")
	otherRoot := filepath.Join(t.TempDir(), "other-worktree")
	runner := &inventoryDeadlineProcessRunner{partial: WorktreeRetirementProcessResult{
		Stdout: "p789\nfcwd\nn" + childRoot + "\n",
	}}

	observations := (LSOFWorktreeInventoryLiveness{Runner: runner, Timeout: time.Second}).Snapshot(
		context.Background(), []string{mainRoot, childRoot, otherRoot},
	)
	if runner.calls != 1 || !runner.sawDeadline {
		t.Fatalf("snapshot runner calls=%d saw_deadline=%t, want one bounded call", runner.calls, runner.sawDeadline)
	}
	if got := observations[childRoot].State; got != WorktreeLivenessActive {
		t.Fatalf("partial active evidence was discarded on timeout: state=%q observations=%+v", got, observations)
	}
	for _, unresolved := range []string{mainRoot, otherRoot} {
		if got := observations[unresolved].State; got != WorktreeLivenessUnknown {
			t.Fatalf("unresolved root %q state=%q, want unknown; observations=%+v", unresolved, got, observations)
		}
	}
}

func TestLSOFWorktreeInventoryLivenessHighCardinalityAttributionHonorsContextBudget(t *testing.T) {
	rootParent := t.TempDir()
	roots := make([]string, 44)
	for i := range roots {
		roots[i] = filepath.Join(rootParent, "worktree-"+strconv.Itoa(i))
		if err := os.MkdirAll(roots[i], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	activePath := filepath.Join(roots[len(roots)-1], "active.go")
	var stdout strings.Builder
	stdout.WriteString("p1\nfcwd\nn" + activePath + "\n")
	const syntheticRecords = 50_000
	for i := 0; i < syntheticRecords; i++ {
		stdout.WriteString("p2\nfcwd\nn/synthetic/outside/file-")
		stdout.WriteString(strconv.Itoa(i))
		stdout.WriteByte('\n')
	}
	stdoutText := stdout.String()
	runner := &inventoryRecordingProcessRunner{result: WorktreeRetirementProcessResult{Stdout: stdoutText}}

	observations := (LSOFWorktreeInventoryLiveness{Runner: runner, Timeout: time.Nanosecond}).Snapshot(
		context.Background(), roots,
	)
	if got := observations[roots[len(roots)-1]].State; got != WorktreeLivenessActive {
		t.Fatalf("partial active evidence was lost while bounding attribution: state=%q observations=%+v", got, observations)
	}
	if got := observations[roots[0]].State; got != WorktreeLivenessUnknown {
		t.Fatalf("unresolved root state=%q, want unknown after attribution budget expires", got)
	}

	// Deterministic replacement for the old wall-clock ("elapsed < 750ms")
	// budget assertion: prove directly, via the record counter
	// attributeWorktreeInventoryLSOFPaths reports, that an expired context
	// makes attribution bail out long before it would consume all
	// syntheticRecords+1 lsof entries. An already-cancelled context makes
	// this exact (recordsProcessed == 1, since ctx.Err() is checked right
	// after the very first record), but assert the weaker, still-conclusive
	// bound so this doesn't silently start asserting timing-shaped behavior
	// again if the attribution loop's per-record work changes.
	expiredCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, complete, recordsProcessed := attributeWorktreeInventoryLSOFPaths(expiredCtx, stdoutText, roots)
	if complete {
		t.Fatal("attribution reported complete against an already-cancelled context")
	}
	if recordsProcessed >= syntheticRecords {
		t.Fatalf("attribution processed %d records against an already-cancelled context, want it to bail long before consuming all %d records", recordsProcessed, syntheticRecords)
	}
}

func TestLSOFWorktreeInventoryLivenessCountsOnlyCWDRecords(t *testing.T) {
	mainRoot := filepath.Join(t.TempDir(), "repo")
	childRoot := filepath.Join(mainRoot, ".claude", "worktrees", "child")
	if err := os.MkdirAll(childRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &inventoryRecordingProcessRunner{result: WorktreeRetirementProcessResult{Stdout: strings.Join([]string{
		"p100", "fcwd", "n" + mainRoot,
		"p101", "f12", "n" + filepath.Join(childRoot, "open.go"),
		"",
	}, "\n")}}

	observations := (LSOFWorktreeInventoryLiveness{Runner: runner}).Snapshot(
		context.Background(), []string{mainRoot, childRoot},
	)
	if got := observations[mainRoot].State; got != WorktreeLivenessActive {
		t.Fatalf("cwd MAIN record was not counted: state=%q observations=%+v", got, observations)
	}
	if got := observations[childRoot].State; got != WorktreeLivenessUnused {
		t.Fatalf("ordinary open file must not count in a CWD-only snapshot: state=%q observations=%+v", got, observations)
	}
}

func TestLSOFWorktreeInventoryLivenessExitZeroIncompleteWarningFailsClosed(t *testing.T) {
	activeRoot := filepath.Join(t.TempDir(), "active")
	unresolvedRoot := filepath.Join(t.TempDir(), "unresolved")
	runner := &inventoryRecordingProcessRunner{result: WorktreeRetirementProcessResult{
		Stdout:   "p123\nfcwd\nn" + activeRoot + "\n",
		Stderr:   "lsof: WARNING: cannot inspect a synthetic mount; Output information may be incomplete.\n",
		ExitCode: 0,
	}}

	observations := (LSOFWorktreeInventoryLiveness{Runner: runner}).Snapshot(
		context.Background(), []string{activeRoot, unresolvedRoot},
	)
	if got := observations[activeRoot].State; got != WorktreeLivenessActive {
		t.Fatalf("active evidence from incomplete snapshot was discarded: state=%q observations=%+v", got, observations)
	}
	if got := observations[unresolvedRoot].State; got != WorktreeLivenessUnknown {
		t.Fatalf("exit-zero incomplete snapshot marked unresolved root %q, want unknown; observations=%+v", got, observations)
	}
}

func TestLSOFWorktreeInventoryLivenessEvidenceDeterministicAcrossInputOrder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	pathA := filepath.Join(root, "a.go")
	pathB := filepath.Join(root, "b.go")
	snapshot := func(paths ...string) WorktreeLivenessObservation {
		var stdout strings.Builder
		for i, path := range paths {
			stdout.WriteString("p")
			stdout.WriteString(strconv.Itoa(i + 1))
			stdout.WriteString("\nfcwd\nn")
			stdout.WriteString(path)
			stdout.WriteByte('\n')
		}
		runner := &inventoryRecordingProcessRunner{result: WorktreeRetirementProcessResult{Stdout: stdout.String()}}
		return (LSOFWorktreeInventoryLiveness{Runner: runner}).Snapshot(context.Background(), []string{root})[root]
	}

	forward := snapshot(pathA, pathB)
	reverse := snapshot(pathB, pathA)
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("liveness evidence depends on lsof record order: forward=%+v reverse=%+v", forward, reverse)
	}
}

func TestLSOFWorktreeInventoryLivenessContextCancellationKillsProcessGroupAndPreservesPartial(t *testing.T) {
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	activeRoot := filepath.Join(t.TempDir(), "active")
	unresolvedRoot := filepath.Join(t.TempDir(), "unresolved")
	fakeLSOF := filepath.Join(binDir, "lsof")
	script := `#!/bin/sh
sleep 30 >/dev/null 2>&1 &
child=$!
printf 'p%s\nfcwd\nn%s\n' "$$" "$COMPOUND_TEST_LSOF_CWD_PATH"
printf '%s\n' "$child" > "$COMPOUND_TEST_LSOF_PID_FILE"
wait "$child"
`
	if err := os.WriteFile(fakeLSOF, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPOUND_TEST_LSOF_PID_FILE", pidFile)
	t.Setenv("COMPOUND_TEST_LSOF_CWD_PATH", activeRoot)

	ctx, cancel := context.WithCancel(context.Background())
	observationsReady := make(chan map[string]WorktreeLivenessObservation, 1)
	go func() {
		observationsReady <- (LSOFWorktreeInventoryLiveness{Timeout: 5 * time.Second}).Snapshot(
			ctx, []string{activeRoot, unresolvedRoot},
		)
	}()

	// Readiness must mean "the child pid is PUBLISHED", not merely "the pid file
	// exists". The fake lsof writes it with `> "$PID_FILE"`, which CREATES the
	// file before the contents land, so an os.Stat-only check can return ready
	// while the file is still empty. Cancelling there kills the process group
	// before the shell finishes the write, so the pid is never published at all
	// and no later read can recover it. Gating readiness on a parseable pid
	// closes the window at its source.
	var childPID int
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil {
				childPID = pid
				break
			}
		} else if !os.IsNotExist(err) {
			cancel()
			t.Fatalf("inspect fake lsof readiness: %v", err)
		}
		if time.Now().After(readyDeadline) {
			cancel()
			t.Fatal("fake lsof did not publish a parseable child pid")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancelStarted := time.Now()
	cancel()
	var observations map[string]WorktreeLivenessObservation
	select {
	case observations = <-observationsReady:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking lsof exceeded cancellation budget")
	}
	if elapsed := time.Since(cancelStarted); elapsed > 2*time.Second {
		t.Fatalf("blocking lsof cancellation took %s", elapsed)
	}
	if got := observations[activeRoot].State; got != WorktreeLivenessActive {
		t.Fatalf("partial active evidence was lost on real process cancellation: state=%q observations=%+v", got, observations)
	}
	if got := observations[unresolvedRoot].State; got != WorktreeLivenessUnknown {
		t.Fatalf("unresolved root state=%q, want unknown after real process cancellation", got)
	}

	// childPID was captured by the readiness gate above, which cannot pass until
	// the fake lsof published a parseable pid.
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	deadline := time.Now().Add(time.Second)
	for inventoryTestProcessExists(childPID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if inventoryTestProcessExists(childPID) {
		t.Fatalf("lsof child process %d survived snapshot cancellation", childPID)
	}
}

func inventoryTestProcessExists(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// The activity path (not lsof) must also promote a worktree to live: a clean,
// contained worktree whose files were just touched reads live within the window.
func TestBuildWorktreeInventoryRecentActivityClassifiesLive(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	// Dead liveness everywhere: only activity mtime can make a row live here.
	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: time.Now(), ActivityWindow: 60 * time.Minute})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	landed := inventoryRowByBranch(t, inv, "lane-landed")
	if landed.Class != WorktreeClassLive {
		t.Fatalf("freshly created clean worktree should read live via activity, got %q; row=%+v", landed.Class, landed)
	}
	if !landed.ActivityRecent {
		t.Fatalf("activity_recent should be set: %+v", landed)
	}
}

// F2 clean-main-quiet: the primary checkout is still observed through the
// shared liveness seam, but a clean, unused main row carries no live signal and
// keeps its leave-alone action.
func TestBuildWorktreeInventoryF2CleanMainStaysQuiet(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Second)

	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	main := inventoryMainRow(t, inv)
	if main.Live || main.ActivityRecent || main.DirtyFiles != 0 {
		t.Fatalf("clean main must stay quiet: %+v", main)
	}
	if main.Liveness.State != WorktreeLivenessUnused {
		t.Fatalf("main liveness was not observed through the shared seam: %+v", main.Liveness)
	}
	if main.NextAction != "leave alone — primary checkout" {
		t.Fatalf("clean main next_action = %q", main.NextAction)
	}
}

func TestBuildWorktreeInventoryMainStatusFailureFailsClosed(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	rec := &inventoryRecordingGit{
		delegate:      ExecWorktreeRetirementGit{},
		failStatusFor: fix.repo,
	}

	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: rec, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: inventoryFutureNow()})
	if err != nil {
		t.Fatal(err)
	}
	main := inventoryMainRow(t, inv)
	if !main.IsMain || main.Class != WorktreeClassUnknown {
		t.Fatalf("MAIN with unreadable status must fail closed as unknown: %+v", main)
	}
	if !strings.Contains(main.Reason, "primary checkout") || !strings.Contains(main.Reason, "git status") {
		t.Fatalf("MAIN unknown reason must name the checkout and failed fact: %q", main.Reason)
	}
	if !strings.Contains(main.NextAction, "inspect") || !strings.Contains(main.NextAction, "lane visibility") {
		t.Fatalf("MAIN unknown next action must require inspection before trusting visibility: %q", main.NextAction)
	}
	if inv.Counts.Unknown == 0 {
		t.Fatalf("MAIN status failure must increment unknown count: %+v", inv.Counts)
	}
}

// F3 commit-recency-age: a clean linked worktree with an artificially old
// .git entry still reports its recent HEAD commit as the newest age evidence.
func TestBuildWorktreeInventoryF3CleanWorktreeAgeUsesHeadCommitTime(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Second)
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(fix.laneLanded, ".git"), old, old); err != nil {
		t.Fatal(err)
	}

	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: now, ActivityWindow: 60 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	landed := inventoryRowByBranch(t, inv, "lane-landed")
	if landed.AgeSeconds == nil || *landed.AgeSeconds >= int64((60*time.Minute).Seconds()) {
		t.Fatalf("clean worktree age must reflect recent HEAD commit, not old .git entry: %+v", landed)
	}
	if !landed.ActivityRecent {
		t.Fatalf("recent HEAD commit must be activity evidence: %+v", landed)
	}
}

// W5: read-only proof. A recording fake wraps real git; the full build may only
// invoke read verbs, and a `worktree` call may only be `worktree list`.
func TestBuildWorktreeInventoryIsReadOnly(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	rec := &inventoryRecordingGit{delegate: ExecWorktreeRetirementGit{}}

	beforeRefs := retirementGitOutput(t, fix.repo, "show-ref")
	beforeWorktrees := retirementGitOutput(t, fix.repo, "worktree", "list", "--porcelain")

	if _, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: rec, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: inventoryFutureNow()}); err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	readOnly := map[string]bool{"rev-parse": true, "worktree": true, "status": true, "show": true, "merge-base": true, "rev-list": true, "cherry": true}
	mutatingTokens := map[string]bool{
		"add": true, "commit": true, "rm": true, "update-ref": true, "checkout": true,
		"switch": true, "reset": true, "branch": true, "stash": true, "clean": true,
		"gc": true, "prune": true, "apply": true, "merge": true, "rebase": true, "push": true,
	}
	if len(rec.calls) == 0 {
		t.Fatal("recording git captured zero calls")
	}
	for _, call := range rec.calls {
		verb := inventoryGitVerb(call)
		if !readOnly[verb] {
			t.Fatalf("non-whitelisted git verb %q in call %v", verb, call)
		}
		if verb == "worktree" && !inventoryArgsContain(call, "list") {
			t.Fatalf("worktree call is not a read-only list: %v", call)
		}
		for _, arg := range call {
			if mutatingTokens[arg] {
				t.Fatalf("mutating git token %q appeared in call %v", arg, call)
			}
		}
	}

	if after := retirementGitOutput(t, fix.repo, "show-ref"); after != beforeRefs {
		t.Fatalf("inventory mutated refs:\nbefore:\n%s\nafter:\n%s", beforeRefs, after)
	}
	if after := retirementGitOutput(t, fix.repo, "worktree", "list", "--porcelain"); after != beforeWorktrees {
		t.Fatalf("inventory mutated worktree registry")
	}
}

// W7: an unreadable fact for one worktree isolates that row as unknown (naming
// the failing fact) while every other row still renders.
func TestBuildWorktreeInventoryUnreadableFactIsolatedAsUnknown(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	rec := &inventoryRecordingGit{delegate: ExecWorktreeRetirementGit{}, failRevListFor: fix.stalledHead}

	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: rec, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: inventoryFutureNow()})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	bad := inventoryRowByBranch(t, inv, "lane-stalled")
	if bad.Class != WorktreeClassUnknown {
		t.Fatalf("row with failing rev-list should be unknown, got %q; row=%+v", bad.Class, bad)
	}
	if !strings.Contains(bad.Reason, "rev-list") {
		t.Fatalf("unknown reason should name the failing fact (rev-list): %q", bad.Reason)
	}
	// The other clean worktree still classifies normally — one bad worktree does
	// not kill the projection.
	if got := inventoryRowByBranch(t, inv, "lane-landed").Class; got != WorktreeClassLandedClean {
		t.Fatalf("sibling worktree class = %q, want landed_clean", got)
	}
}

// A clean, contained worktree is retireable only when liveness is positively
// resolved as unused. Unknown liveness must fail closed instead of falling
// through to landed_clean and its retirement action.
func TestBuildWorktreeInventoryUnknownLivenessNeverRetireable(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)

	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryUnknownLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: inventoryFutureNow()})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	row := inventoryRowByBranch(t, inv, "lane-landed")
	if row.Class != WorktreeClassUnknown {
		t.Fatalf("unknown liveness class = %q, want unknown; row=%+v", row.Class, row)
	}
	if strings.Contains(row.NextAction, "retire") {
		t.Fatalf("unknown liveness must never offer retirement: %+v", row)
	}
}

// I1 (identity exact-evidence): a row whose branch ref matches a nonterminal
// envelope's worktree-branch: artifact gains work_id+state; a row with no
// artifact stays honestly bare, and a single match notes no ambiguity.
func TestJoinWorktreeEnvelopeIdentityExactEvidence(t *testing.T) {
	rows := []WorktreeInventoryRow{
		{BranchRef: "refs/heads/lane-a", Branch: "lane-a", Reason: "r-a"},
		{BranchRef: "refs/heads/lane-b", Branch: "lane-b", Reason: "r-b"},
	}
	joinWorktreeEnvelopeIdentity(rows, []WorktreeEnvelopeArtifact{
		{WorkID: "work_a", State: "running", BranchRef: "refs/heads/lane-a", CreatedAt: "2026-07-17T10:00:00Z"},
	})
	if rows[0].WorkID != "work_a" || rows[0].WorkState != "running" {
		t.Fatalf("lane-a should carry exact identity: %+v", rows[0])
	}
	if strings.Contains(rows[0].Reason, "ambiguous") {
		t.Fatalf("single match must not note ambiguity: %q", rows[0].Reason)
	}
	if rows[1].WorkID != "" || rows[1].WorkState != "" {
		t.Fatalf("lane-b has no artifact and must stay honestly bare: %+v", rows[1])
	}
}

// I2 (dangling tolerated): a worktree-branch: ref matching no row — including
// the known dangling refs/heads/(pending-dispatch) on work_2f9b20dc6661 — is
// silently ignored: no error, no annotation, no row touched.
func TestJoinWorktreeEnvelopeIdentityIgnoresDanglingRefs(t *testing.T) {
	rows := []WorktreeInventoryRow{{BranchRef: "refs/heads/lane-a", Reason: "r-a"}}
	before := rows[0]
	joinWorktreeEnvelopeIdentity(rows, []WorktreeEnvelopeArtifact{
		{WorkID: "work_dangling", State: "running", BranchRef: "refs/heads/(pending-dispatch)", CreatedAt: "2026-07-17T10:00:00Z"},
		{WorkID: "work_other", State: "running", BranchRef: "refs/heads/lane-z", CreatedAt: "2026-07-17T10:00:00Z"},
	})
	if !reflect.DeepEqual(rows[0], before) {
		t.Fatalf("non-matching refs (incl. the known dangling pending-dispatch) must not touch any row: %+v", rows[0])
	}
}

// I3a (main-root ambiguity): a shared canonical delivery root is exact routing
// evidence, but it is not enough to infer which of several running envelopes is
// actively editing the primary checkout. Singular identity therefore stays
// empty and the reason says that no active editor was inferred.
func TestJoinWorktreeEnvelopeIdentityMainRootAmbiguityDoesNotInferEditor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	rows := []WorktreeInventoryRow{{
		Path: root, BranchRef: "refs/heads/main", IsMain: true, Reason: "primary checkout",
	}}
	joinWorktreeEnvelopeIdentity(rows, []WorktreeEnvelopeArtifact{
		{WorkID: "work_old", State: "running", Holder: "codex:old", TargetWorktreeRoot: root, CreatedAt: "2026-07-17T09:00:00Z"},
		{WorkID: "work_new", State: "running", Holder: "codex:new", TargetWorktreeRoot: root, CreatedAt: "2026-07-17T11:00:00Z"},
	})
	if rows[0].WorkID != "" || rows[0].WorkState != "" || rows[0].WorkHolder != "" {
		t.Fatalf("ambiguous main-root evidence must not infer singular work identity: %+v", rows[0])
	}
	if !strings.Contains(rows[0].Reason, "active editor was not inferred") {
		t.Fatalf("reason must state that the active editor was not inferred: %q", rows[0].Reason)
	}
}

// Exact branch identity outranks delivery-root fallback. A branch-attached
// envelope may carry MAIN's canonical delivery root as delivery metadata, but
// it belongs only to the linked branch row; a genuinely root-only running
// envelope remains eligible to identify MAIN.
func TestJoinWorktreeEnvelopeIdentityBranchArtifactDoesNotFallBackToMainRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	rows := []WorktreeInventoryRow{
		{Path: root, BranchRef: "refs/heads/main", IsMain: true, Reason: "primary checkout"},
		{Path: filepath.Join(root, ".claude", "worktrees", "lane"), BranchRef: "refs/heads/lane", Branch: "lane", Reason: "linked lane"},
	}
	joinWorktreeEnvelopeIdentity(rows, []WorktreeEnvelopeArtifact{
		{
			WorkID: "work_branch", State: "running", Holder: "codex:branch",
			BranchRef: "refs/heads/lane", TargetWorktreeRoot: root, CreatedAt: "2026-07-18T08:00:00Z",
		},
		{
			WorkID: "work_main", State: "running", Holder: "codex:main",
			TargetWorktreeRoot: root, CreatedAt: "2026-07-18T09:00:00Z",
		},
	})
	if rows[0].WorkID != "work_main" || rows[0].WorkHolder != "codex:main" || len(rows[0].WorkCandidates) != 0 {
		t.Fatalf("MAIN must join only genuinely root-only evidence: %+v", rows[0])
	}
	if rows[1].WorkID != "work_branch" || rows[1].WorkHolder != "codex:branch" {
		t.Fatalf("linked row must retain its exact branch-attached identity: %+v", rows[1])
	}
}

// I3b (branch-artifact ambiguity): when two nonterminal envelopes name one branch, the newest
// (CreatedAt, then WorkID) wins and the row's reason notes the ambiguity naming
// the winner.
func TestJoinWorktreeEnvelopeIdentityNewestWinsOnAmbiguity(t *testing.T) {
	rows := []WorktreeInventoryRow{{BranchRef: "refs/heads/lane-a", Reason: "r-a"}}
	joinWorktreeEnvelopeIdentity(rows, []WorktreeEnvelopeArtifact{
		{WorkID: "work_old", State: "claimed", BranchRef: "refs/heads/lane-a", CreatedAt: "2026-07-17T09:00:00Z"},
		{WorkID: "work_new", State: "running", BranchRef: "refs/heads/lane-a", CreatedAt: "2026-07-17T11:00:00Z"},
	})
	if rows[0].WorkID != "work_new" || rows[0].WorkState != "running" {
		t.Fatalf("newest nonterminal envelope must win: %+v", rows[0])
	}
	if !strings.Contains(rows[0].Reason, "ambiguous") || !strings.Contains(rows[0].Reason, "work_new") {
		t.Fatalf("ambiguity must be noted in the reason naming the winner: %q", rows[0].Reason)
	}
}

// Keep annotation is a pure annotation: a kept row KEEPS its ground-truth class
// (W-regression: a kept stalled_unlanded stays stalled_unlanded) and only its
// next_action defers to the operator decision. Unkept rows are untouched.
func TestAnnotateWorktreeKeepsDefersNextActionKeepsClass(t *testing.T) {
	rows := []WorktreeInventoryRow{
		{BranchRef: "refs/heads/lane-a", Class: WorktreeClassStalledUnlanded, NextAction: "unlanded work — check lane liveness before salvage or keep"},
		{BranchRef: "refs/heads/lane-b", Class: WorktreeClassLandedClean, NextAction: "retire via compound authority worktree-retire (governed)"},
	}
	kept := annotateWorktreeKeeps(rows, map[string]WorktreeKeepAnnotation{
		"refs/heads/lane-a": {DecisionID: "keep-1", Revisit: "auth refactor lands"},
	})
	if kept != 1 {
		t.Fatalf("kept count = %d, want 1", kept)
	}
	if rows[0].Class != WorktreeClassStalledUnlanded {
		t.Fatalf("kept stalled_unlanded must stay stalled_unlanded: %+v", rows[0])
	}
	if !rows[0].Kept || rows[0].KeepDecisionID != "keep-1" || rows[0].KeepRevisit != "auth refactor lands" {
		t.Fatalf("kept row annotation wrong: %+v", rows[0])
	}
	if !strings.Contains(rows[0].NextAction, "kept by operator decision keep-1") || !strings.Contains(rows[0].NextAction, "revisit: auth refactor lands") {
		t.Fatalf("kept next_action wrong: %q", rows[0].NextAction)
	}
	if rows[1].Kept || rows[1].NextAction == "" || strings.Contains(rows[1].NextAction, "kept by operator") {
		t.Fatalf("unkept row must be untouched: %+v", rows[1])
	}
}

// End-to-end through BuildWorktreeInventory over a real fixture: keeps annotate
// (class unchanged, counts.kept incremented, next_action deferred), identity
// joins the exact envelope, the dangling ref is ignored, and readable keeps omit
// the keeps_readable field.
func TestBuildWorktreeInventoryAppliesLedgerJoins(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	readable := true
	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryFakeLiveness{active: map[string]bool{fix.laneLive: true}},
	}, fix.repo, WorktreeInventoryOptions{
		Now: inventoryFutureNow(),
		Keeps: map[string]WorktreeKeepAnnotation{
			"refs/heads/lane-stalled": {DecisionID: "keep-stalled", Revisit: "salvage decided"},
		},
		KeepsReadable: &readable,
		Envelopes: []WorktreeEnvelopeArtifact{
			{WorkID: "work_live", State: "running", BranchRef: "refs/heads/lane-live", CreatedAt: "2026-07-17T10:00:00Z"},
			{WorkID: "work_dangling", State: "running", BranchRef: "refs/heads/(pending-dispatch)", CreatedAt: "2026-07-17T10:00:00Z"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stalled := inventoryRowByBranch(t, inv, "lane-stalled")
	if stalled.Class != WorktreeClassStalledUnlanded {
		t.Fatalf("kept annotation must not change class, got %q: %+v", stalled.Class, stalled)
	}
	if !stalled.Kept || stalled.KeepDecisionID != "keep-stalled" || !strings.Contains(stalled.NextAction, "kept by operator decision keep-stalled") {
		t.Fatalf("lane-stalled should be annotated kept: %+v", stalled)
	}
	if inv.Counts.Kept != 1 {
		t.Fatalf("counts.kept = %d, want 1", inv.Counts.Kept)
	}
	live := inventoryRowByBranch(t, inv, "lane-live")
	if live.WorkID != "work_live" || live.WorkState != "running" {
		t.Fatalf("lane-live should carry exact identity: %+v", live)
	}
	landed := inventoryRowByBranch(t, inv, "lane-landed")
	if landed.WorkID != "" || landed.Kept {
		t.Fatalf("lane-landed has no envelope and no keep — must be bare: %+v", landed)
	}
	if inv.KeepsReadable != nil {
		t.Fatalf("readable keeps must omit keeps_readable, got %v", *inv.KeepsReadable)
	}
}

// K3 projection: unreadable keeps surface keeps_readable:false and annotate
// nothing — honest, not guessed.
func TestBuildWorktreeInventoryUnreadableKeepsFailClosed(t *testing.T) {
	fix := newWorktreeInventoryFixture(t)
	unreadable := false
	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{
		Now:           inventoryFutureNow(),
		Keeps:         map[string]WorktreeKeepAnnotation{"refs/heads/lane-landed": {DecisionID: "keep-x", Revisit: "x"}},
		KeepsReadable: &unreadable,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.KeepsReadable == nil || *inv.KeepsReadable {
		t.Fatalf("unreadable keeps must surface keeps_readable:false: %v", inv.KeepsReadable)
	}
	if inv.Counts.Kept != 0 {
		t.Fatalf("unreadable keeps must annotate nothing: kept=%d", inv.Counts.Kept)
	}
	if inventoryRowByBranch(t, inv, "lane-landed").Kept {
		t.Fatalf("unreadable keeps must not annotate any row")
	}
}

// F-DEBT (work_984e63b68945): the scratch-worktree-debt watchdog flags
// contained+clean+older-than-48h (landed_clean past the threshold) rows and emits
// the exact per-row sweep suggestion, reported (advisory, non-fatal — the inventory
// is read-only and never retires anything). A dirty tree or an unmerged tree must
// NOT qualify even when old, so the sweep never targets unlanded work; and a
// contained+clean tree under the 48h threshold must NOT qualify (still cooling down
// from its lane). This is the falsifier the task demanded: a synthetic repo/worktree
// fixture where contained+clean+old yields the debt row and dirty or unmerged does NOT.
func TestBuildWorktreeInventoryScratchWorktreeDebt(t *testing.T) {
	fix := newScratchWorktreeDebtFixture(t)

	// 72h future: every fixture row is comfortably past the 48h debt threshold and
	// well outside the 60m activity window. Freshened at runtime (time.Now()), not a
	// fixed fixture date, so this falsifier cannot red days after a green land.
	now := time.Now().Add(72 * time.Hour)
	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: now})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}

	// POSITIVE: contained + clean + old → debt-eligible.
	debt := inventoryRowByBranch(t, inv, "lane-debt")
	if debt.Class != WorktreeClassLandedClean {
		t.Fatalf("lane-debt precondition: class = %q, want landed_clean; row=%+v", debt.Class, debt)
	}
	if !debt.DebtEligible {
		t.Fatalf("lane-debt (contained+clean+old) must be debt-eligible: %+v", debt)
	}

	// NEGATIVE: contained but dirty → NOT debt-eligible (data-loss guard: a dirty
	// tree holds uncommitted work the sweep would lose).
	dirty := inventoryRowByBranch(t, inv, "lane-dirty")
	if dirty.DebtEligible {
		t.Fatalf("lane-dirty (dirty working tree) must NOT be debt-eligible: %+v", dirty)
	}

	// NEGATIVE: clean but unmerged (one commit ahead of main) → NOT debt-eligible
	// (data-loss guard: HEAD not contained in main, sweep would lose the commit).
	unmerged := inventoryRowByBranch(t, inv, "lane-unmerged")
	if unmerged.DebtEligible {
		t.Fatalf("lane-unmerged (HEAD not contained in main) must NOT be debt-eligible: %+v", unmerged)
	}

	if inv.Counts.ScratchWorktreeDebt != 1 {
		t.Fatalf("counts.scratch_worktree_debt = %d, want 1: %+v", inv.Counts.ScratchWorktreeDebt, inv.Counts)
	}
	if len(inv.ScratchWorktreeDebtSweep) != 1 {
		t.Fatalf("sweep suggestions = %d, want 1: %+v", len(inv.ScratchWorktreeDebtSweep), inv.ScratchWorktreeDebtSweep)
	}
	suggestion := inv.ScratchWorktreeDebtSweep[0]
	if !strings.Contains(suggestion, "git worktree remove") {
		t.Fatalf("sweep suggestion must name `git worktree remove`: %q", suggestion)
	}
	if !strings.Contains(suggestion, debt.Path) {
		t.Fatalf("sweep suggestion must name the debt worktree path (%q): %q", debt.Path, suggestion)
	}

	// Threshold falsifier: the SAME fixture re-run with Now only 24h in the future
	// reads every row as under the 48h threshold. lane-debt is still landed_clean
	// (contained+clean+not-live) but must NOT be debt-eligible and the sweep must
	// be empty — a freshly-landed lane is cooling down, not debt.
	nowFresh := time.Now().Add(24 * time.Hour)
	invFresh, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{Now: nowFresh})
	if err != nil {
		t.Fatalf("build fresh inventory: %v", err)
	}
	freshDebt := inventoryRowByBranch(t, invFresh, "lane-debt")
	if freshDebt.Class != WorktreeClassLandedClean {
		t.Fatalf("fresh lane-debt precondition: class = %q, want landed_clean; row=%+v", freshDebt.Class, freshDebt)
	}
	if freshDebt.DebtEligible {
		t.Fatalf("lane-debt under 48h must NOT be debt-eligible: %+v", freshDebt)
	}
	if invFresh.Counts.ScratchWorktreeDebt != 0 || len(invFresh.ScratchWorktreeDebtSweep) != 0 {
		t.Fatalf("under-48h inventory must report zero debt and no sweep: counts=%+v sweep=%+v", invFresh.Counts, invFresh.ScratchWorktreeDebtSweep)
	}
}

// K3+kept-debt: an operator-kept landed_clean row must NOT carry a sweep suggestion,
// even when old — the keep defers retirement to the operator decision.
func TestBuildWorktreeInventoryScratchWorktreeDebtKeptRowExcluded(t *testing.T) {
	fix := newScratchWorktreeDebtFixture(t)
	now := time.Now().Add(72 * time.Hour)
	readable := true
	inv, err := BuildWorktreeInventory(context.Background(), WorktreeInventoryDependencies{
		Git: ExecWorktreeRetirementGit{}, Liveness: inventoryFakeLiveness{},
	}, fix.repo, WorktreeInventoryOptions{
		Now: now,
		Keeps: map[string]WorktreeKeepAnnotation{
			"refs/heads/lane-debt": {DecisionID: "keep-debt", Revisit: "rehearsing rebase"},
		},
		KeepsReadable: &readable,
	})
	if err != nil {
		t.Fatalf("build inventory: %v", err)
	}
	debt := inventoryRowByBranch(t, inv, "lane-debt")
	if !debt.Kept {
		t.Fatalf("lane-debt should be annotated kept: %+v", debt)
	}
	if debt.DebtEligible {
		t.Fatalf("kept lane-debt must NOT be debt-eligible: %+v", debt)
	}
	if inv.Counts.ScratchWorktreeDebt != 0 || len(inv.ScratchWorktreeDebtSweep) != 0 {
		t.Fatalf("kept row must not contribute to debt: counts=%+v sweep=%+v", inv.Counts, inv.ScratchWorktreeDebtSweep)
	}
}

// --- fixtures & fakes ---

type scratchWorktreeDebtFixture struct {
	repo         string
	mainTip      string
	laneDebt     string
	laneDirty    string
	laneUnmerged string
}

// newScratchWorktreeDebtFixture builds a minimal synthetic repo with three linked
// worktrees that isolate the scratch-worktree-debt classifier: lane-debt is the
// POSITIVE (contained+clean), lane-dirty and lane-unmerged are the two data-loss
// NEGATIVES. Every row's age is controlled by the caller's inventory Now, not a
// fixture date.
func newScratchWorktreeDebtFixture(t *testing.T) scratchWorktreeDebtFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	retirementRunGit(t, repo, "init", "-b", "main")
	retirementRunGit(t, repo, "config", "user.email", "test@example.invalid")
	retirementRunGit(t, repo, "config", "user.name", "Compound Test")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retirementRunGit(t, repo, "add", "seed.txt")
	retirementRunGit(t, repo, "commit", "-m", "seed")
	mainTip := retirementGitOutput(t, repo, "rev-parse", "refs/heads/main")

	// lane-debt: branch at main, clean working tree → contained + clean.
	laneDebt := filepath.Join(root, "wt-debt")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-debt", laneDebt, "main")

	// lane-dirty: branch at main (contained) but with an uncommitted file → dirty.
	laneDirty := filepath.Join(root, "wt-dirty")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-dirty", laneDirty, "main")
	if err := os.WriteFile(filepath.Join(laneDirty, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// lane-unmerged: one commit ahead of main, working tree clean → not contained.
	laneUnmerged := filepath.Join(root, "wt-unmerged")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-unmerged", laneUnmerged, "main")
	if err := os.WriteFile(filepath.Join(laneUnmerged, "unique.txt"), []byte("unique\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retirementRunGit(t, laneUnmerged, "add", "unique.txt")
	retirementRunGit(t, laneUnmerged, "commit", "-m", "unmerged work")

	return scratchWorktreeDebtFixture{
		repo:         evalInventoryPath(t, repo),
		mainTip:      mainTip,
		laneDebt:     evalInventoryPath(t, laneDebt),
		laneDirty:    evalInventoryPath(t, laneDirty),
		laneUnmerged: evalInventoryPath(t, laneUnmerged),
	}
}

type worktreeInventoryFixture struct {
	repo        string
	mainTip     string
	laneStalled string
	laneLanded  string
	laneLive    string
	probe       string
	stalledHead string
}

func newWorktreeInventoryFixture(t *testing.T) worktreeInventoryFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	retirementRunGit(t, repo, "init", "-b", "main")
	retirementRunGit(t, repo, "config", "user.email", "test@example.invalid")
	retirementRunGit(t, repo, "config", "user.name", "Compound Test")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retirementRunGit(t, repo, "add", "seed.txt")
	retirementRunGit(t, repo, "commit", "-m", "seed")
	mainTip := retirementGitOutput(t, repo, "rev-parse", "refs/heads/main")

	// lane-stalled: one unique commit + a dirty file (would misclassify as
	// landed_clean under a wrong implementation).
	laneStalled := filepath.Join(root, "wt-stalled")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-stalled", laneStalled, "main")
	if err := os.WriteFile(filepath.Join(laneStalled, "unique.txt"), []byte("unique\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retirementRunGit(t, laneStalled, "add", "unique.txt")
	retirementRunGit(t, laneStalled, "commit", "-m", "unlanded work")
	stalledHead := retirementGitOutput(t, laneStalled, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(laneStalled, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// lane-landed: branch at main, clean -> contained + clean.
	laneLanded := filepath.Join(root, "wt-landed")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-landed", laneLanded, "main")

	// lane-live: identical to lane-landed; only liveness will differ.
	laneLive := filepath.Join(root, "wt-live")
	retirementRunGit(t, repo, "worktree", "add", "-b", "lane-live", laneLive, "main")

	// probe: detached HEAD, dirtied.
	probe := filepath.Join(root, "wt-probe")
	retirementRunGit(t, repo, "worktree", "add", "--detach", probe, "main")
	if err := os.WriteFile(filepath.Join(probe, "scratch.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return worktreeInventoryFixture{
		repo:        evalInventoryPath(t, repo),
		mainTip:     mainTip,
		laneStalled: evalInventoryPath(t, laneStalled),
		laneLanded:  evalInventoryPath(t, laneLanded),
		laneLive:    evalInventoryPath(t, laneLive),
		probe:       evalInventoryPath(t, probe),
		stalledHead: stalledHead,
	}
}

func evalInventoryPath(t *testing.T, path string) string {
	t.Helper()
	if c, err := filepath.EvalSymlinks(path); err == nil {
		return c
	}
	return path
}

func inventoryRowByBranch(t *testing.T, inv WorktreeInventory, branch string) WorktreeInventoryRow {
	t.Helper()
	for _, row := range inv.Rows {
		if row.Branch == branch {
			return row
		}
	}
	t.Fatalf("no row for branch %q", branch)
	return WorktreeInventoryRow{}
}

func inventoryMainRow(t *testing.T, inv WorktreeInventory) WorktreeInventoryRow {
	t.Helper()
	for _, row := range inv.Rows {
		if row.IsMain {
			return row
		}
	}
	t.Fatal("no main row")
	return WorktreeInventoryRow{}
}

type inventoryFakeLiveness struct {
	active map[string]bool
}

func (f inventoryFakeLiveness) Snapshot(_ context.Context, roots []string) map[string]WorktreeLivenessObservation {
	observations := make(map[string]WorktreeLivenessObservation, len(roots))
	for _, root := range roots {
		if f.active[evalInventoryFakePath(root)] {
			observations[root] = WorktreeLivenessObservation{State: WorktreeLivenessActive, Evidence: "fake: open handles"}
		} else {
			observations[root] = WorktreeLivenessObservation{State: WorktreeLivenessUnused, Evidence: "fake: no handles"}
		}
	}
	return observations
}

type inventoryUnknownLiveness struct{}

func (inventoryUnknownLiveness) Snapshot(_ context.Context, roots []string) map[string]WorktreeLivenessObservation {
	observations := make(map[string]WorktreeLivenessObservation, len(roots))
	for _, root := range roots {
		observations[root] = WorktreeLivenessObservation{State: WorktreeLivenessUnknown, Evidence: "fake: snapshot unresolved"}
	}
	return observations
}

type inventoryRecordingSnapshotLiveness struct {
	calls  int
	roots  []string
	active map[string]bool
}

type inventoryRecordingProcessRunner struct {
	result  WorktreeRetirementProcessResult
	err     error
	calls   int
	command string
	args    []string
}

func (r *inventoryRecordingProcessRunner) Run(_ context.Context, command string, args ...string) (WorktreeRetirementProcessResult, error) {
	r.calls++
	r.command = command
	r.args = append([]string(nil), args...)
	return r.result, r.err
}

type inventoryDeadlineProcessRunner struct {
	partial     WorktreeRetirementProcessResult
	calls       int
	sawDeadline bool
}

func (r *inventoryDeadlineProcessRunner) Run(ctx context.Context, _ string, _ ...string) (WorktreeRetirementProcessResult, error) {
	r.calls++
	_, r.sawDeadline = ctx.Deadline()
	return r.partial, context.DeadlineExceeded
}

func (f *inventoryRecordingSnapshotLiveness) Snapshot(_ context.Context, roots []string) map[string]WorktreeLivenessObservation {
	f.calls++
	f.roots = append([]string(nil), roots...)
	observations := make(map[string]WorktreeLivenessObservation, len(roots))
	for _, root := range roots {
		state := WorktreeLivenessUnused
		if f.active[evalInventoryFakePath(root)] {
			state = WorktreeLivenessActive
		}
		observations[root] = WorktreeLivenessObservation{State: state, Evidence: "fake: inventory snapshot"}
	}
	return observations
}

func evalInventoryFakePath(path string) string {
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		return canonical
	}
	return path
}

type inventoryRecordingGit struct {
	delegate       WorktreeRetirementGit
	mu             sync.Mutex
	calls          [][]string
	failRevListFor string
	failStatusFor  string
}

func (g *inventoryRecordingGit) Run(ctx context.Context, repo string, args ...string) (string, error) {
	g.mu.Lock()
	g.calls = append(g.calls, append([]string{}, args...))
	g.mu.Unlock()
	if g.failStatusFor != "" && filepath.Clean(repo) == filepath.Clean(g.failStatusFor) && inventoryGitVerb(args) == "status" {
		return "", &WorktreeRetirementCommandError{
			Argv: append([]string{"git"}, args...), ExitCode: 128, Output: "fatal: status unavailable (injected)",
		}
	}
	if g.failRevListFor != "" && len(args) > 0 && args[0] == "rev-list" {
		for _, a := range args {
			if strings.Contains(a, g.failRevListFor) {
				return "", &WorktreeRetirementCommandError{
					Argv: append([]string{"git", "rev-list"}, args...), ExitCode: 128, Output: "fatal: bad revision (injected)",
				}
			}
		}
	}
	return g.delegate.Run(ctx, repo, args...)
}

func inventoryGitVerb(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

func inventoryArgsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
