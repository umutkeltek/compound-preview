package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// failingOriginStore wraps a real *runattempt.Store but forces SetOrigin to
// fail, simulating a store-write fault on origin stamping immediately after
// a successful Mint. Every OTHER write (Mint, SetProofRefs, SetTranscript,
// Complete, ...) delegates to the embedded real store, so the terminalizing
// writes the runner performs on the failure path still land on disk. That
// isolation is the whole point: a blunt read-only-dir fault would also break
// the terminalizing Complete, so the test could never observe the attempt
// reaching terminal `failed`. mintedID captures the id Mint produced so the
// test can prove the returned RunAttempt IS the freshly minted one.
type failingOriginStore struct {
	*runattempt.Store
	mintedID string
}

type executionPostureStore struct {
	*runattempt.Store
	mintedID    string
	postureSet  bool
	failPosture bool
}

func (s *executionPostureStore) MintWithActionIntent(workID, actor, harness, actionIntentID string, now time.Time) (runattempt.RunAttempt, error) {
	ra, err := s.Store.MintWithActionIntent(workID, actor, harness, actionIntentID, now)
	s.mintedID = ra.ID
	return ra, err
}

func (s *executionPostureStore) SetExecutionPosture(id string, posture runattempt.ExecutionPosture) (runattempt.RunAttempt, error) {
	s.postureSet = true
	if s.failPosture {
		return runattempt.RunAttempt{}, errors.New("posture store unavailable")
	}
	return s.Store.SetExecutionPosture(id, posture)
}

func TestRunnerPersistsExecutionPostureBeforeCommandConstruction(t *testing.T) {
	home := t.TempDir()
	store := &executionPostureStore{Store: runattempt.Open(home)}
	buildCalled := false
	build := func(ctx context.Context, in RunInput) *exec.Cmd {
		buildCalled = true
		if !store.postureSet {
			t.Fatal("command constructed before execution posture was persisted")
		}
		got, err := store.Store.Get(store.mintedID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExecutionPosture == nil || got.ExecutionPosture.PolicyDecisionID != "pol_running" {
			t.Fatalf("posture at command construction = %+v", got.ExecutionPosture)
		}
		return exec.CommandContext(ctx, in.Command, in.Args...)
	}
	posture := &runattempt.ExecutionPosture{PermissionMode: "plan", PolicyDecisionID: "pol_running"}
	out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints", RunInput{
		WorkID: "work_posture_order", Harness: "os-harness-entrypoints", Workspace: home,
		Command: "echo", Args: []string{"ok"}, ActionIntentID: "act_posture_order",
		ExecutionPosture: posture, Now: time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC),
	}, build, defaultHeartbeatInterval)
	if err != nil {
		t.Fatal(err)
	}
	if !buildCalled || out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("build_called=%t attempt=%+v", buildCalled, out.RunAttempt)
	}
}

func TestRunnerExecutionPostureFailureTerminalizesBeforeCommandConstruction(t *testing.T) {
	home := t.TempDir()
	store := &executionPostureStore{Store: runattempt.Open(home), failPosture: true}
	buildCalled := false
	build := func(ctx context.Context, in RunInput) *exec.Cmd {
		buildCalled = true
		return exec.CommandContext(ctx, in.Command, in.Args...)
	}
	out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints", RunInput{
		WorkID: "work_posture_fail", Harness: "os-harness-entrypoints", Workspace: home,
		Command: "echo", Args: []string{"must-not-run"}, ActionIntentID: "act_posture_fail",
		ExecutionPosture: &runattempt.ExecutionPosture{PermissionMode: "plan", PolicyDecisionID: "pol_running"},
		ProofRefs:        []string{"capability-route-plan:crp_fixture"},
		Now:              time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC),
	}, build, defaultHeartbeatInterval)
	if err == nil || !strings.Contains(err.Error(), "set execution posture") {
		t.Fatalf("posture failure error = %v", err)
	}
	if buildCalled {
		t.Fatal("posture failure constructed or started the subprocess")
	}
	if out.RunAttempt.ID != store.mintedID || out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("posture failure did not terminalize minted attempt: out=%+v minted=%s", out.RunAttempt, store.mintedID)
	}
	persisted, err := store.Store.Get(store.mintedID)
	if err != nil {
		t.Fatal(err)
	}
	joinedProofs := strings.Join(persisted.ProofRefs, "\n")
	if !strings.Contains(joinedProofs, "capability-route-plan:crp_fixture") || !strings.Contains(joinedProofs, "infra_error:set execution posture") {
		t.Fatalf("pre-transcript failure replaced dispatch authority instead of appending its failure: %v", persisted.ProofRefs)
	}
}

// A governed launch guard runs at the last possible boundary: command and
// pipes may be prepared, but cmd.Start has not happened. If current authority
// no longer matches the posture recorded on the RunAttempt, the attempt must
// terminalize failed and the external command must never observe a side
// effect.
func TestRunnerStartGuardFailureTerminalizesWithoutStarting(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	marker := filepath.Join(home, "started")
	build := func(ctx context.Context, in RunInput) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "printf started > \"$1\"", "sh", marker)
	}
	out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints", RunInput{
		WorkID: "work_start_guard", Harness: "os-harness-entrypoints", Workspace: home,
		Command: "sh",
		Now:     time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC),
		StartGuard: func(_ context.Context, _ func() error) error {
			return errors.New("launch authority revision changed")
		},
	}, build, defaultHeartbeatInterval)
	if err == nil || !strings.Contains(err.Error(), "launch authority revision changed") {
		t.Fatalf("start guard error = %v", err)
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("guard-refused attempt status = %q, want failed", out.RunAttempt.Status)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("guard-refused command started; marker stat err=%v", statErr)
	}
}

func TestRunnerFinalEvidenceAttachesBeforeTerminalization(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	finalized := false
	out, err := runWithSubprocessOptions(context.Background(), store, "os-harness-entrypoints", RunInput{
		WorkID:    "work_final_evidence",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "sh",
		Now:       time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC),
	}, func(ctx context.Context, _ RunInput) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "printf native-proof")
	}, defaultHeartbeatInterval, subprocessRunOptions{
		FinalizeEvidence: func(got subprocessEvidenceContext) ([]string, error) {
			if got.RunAttemptID == "" || got.ExitCode != 0 || got.TimedOut {
				t.Fatalf("finalizer context = %+v", got)
			}
			if _, err := os.ReadFile(got.TranscriptPath); err != nil {
				t.Fatalf("finalizer saw non-durable transcript: %v", err)
			}
			if _, err := store.Get(got.RunAttemptID); err != nil {
				t.Fatalf("finalizer cannot read mutable attempt: %v", err)
			}
			finalized = true
			return []string{"artifact:sha256:finalizer"}, nil
		},
	})
	if err != nil {
		t.Fatalf("runWithSubprocessOptions: %v", err)
	}
	if !finalized || out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("finalizer/status = %v/%q", finalized, out.RunAttempt.Status)
	}
	persisted, err := store.Get(out.RunAttempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(persisted.ProofRefs, "\n"), "artifact:sha256:finalizer") {
		t.Fatalf("final evidence missing from terminal attempt: %v", persisted.ProofRefs)
	}
}

func TestRunnerPrepareEvidenceFailureTerminalizesBeforeBuildStartOrFinalize(t *testing.T) {
	store := runattempt.Open(t.TempDir())
	built, finalized := false, false
	out, err := runWithSubprocessOptions(context.Background(), store, "os-harness-entrypoints", RunInput{WorkID: "work_prepare_evidence", Harness: "os-harness-entrypoints", Workspace: t.TempDir(), Command: "sh", Now: time.Now().UTC()}, func(context.Context, RunInput) *exec.Cmd {
		built = true
		return exec.Command("sh", "-c", "exit 0")
	}, defaultHeartbeatInterval, subprocessRunOptions{
		PrepareEvidence:  func() error { return errors.New("private preparation failed") },
		FinalizeEvidence: func(subprocessEvidenceContext) ([]string, error) { finalized = true; return nil, nil },
	})
	if err == nil || built || finalized || out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("err=%v built=%v finalized=%v status=%q", err, built, finalized, out.RunAttempt.Status)
	}
}

func TestRunnerDoesNotFinalizeFailedOrTimedOutChild(t *testing.T) {
	for name, in := range map[string]RunInput{
		"failed":    {WorkID: "work_finalizer_failed", Harness: "os-harness-entrypoints", Workspace: t.TempDir(), Command: "sh", Args: []string{"-c", "exit 42"}, Now: time.Now().UTC()},
		"timed out": {WorkID: "work_finalizer_timeout", Harness: "os-harness-entrypoints", Workspace: t.TempDir(), Command: "sh", Args: []string{"-c", "sleep 1"}, Timeout: 20 * time.Millisecond, Now: time.Now().UTC()},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			out, err := runWithSubprocessOptions(context.Background(), runattempt.Open(t.TempDir()), "os-harness-entrypoints", in, func(ctx context.Context, in RunInput) *exec.Cmd {
				return exec.CommandContext(ctx, in.Command, in.Args...)
			}, defaultHeartbeatInterval, subprocessRunOptions{FinalizeEvidence: func(subprocessEvidenceContext) ([]string, error) {
				called = true
				return nil, nil
			}})
			if err != nil || called || out.RunAttempt.Status != runattempt.StatusFailed {
				t.Fatalf("err=%v finalized=%v status=%q", err, called, out.RunAttempt.Status)
			}
		})
	}
}

func TestRunnerFinalizerFailureTerminalizesAndPreservesOutcome(t *testing.T) {
	home := t.TempDir()
	out, err := runWithSubprocessOptions(context.Background(), runattempt.Open(home), "os-harness-entrypoints", RunInput{
		WorkID: "work_finalizer_error", Harness: "os-harness-entrypoints", Workspace: home, Command: "sh", Args: []string{"-c", "exit 0"}, Now: time.Now().UTC(),
	}, func(ctx context.Context, in RunInput) *exec.Cmd {
		return exec.CommandContext(ctx, in.Command, in.Args...)
	}, defaultHeartbeatInterval, subprocessRunOptions{FinalizeEvidence: func(subprocessEvidenceContext) ([]string, error) {
		return nil, errors.New("proof rejected")
	}})
	if err == nil || out.RunAttempt.Status != runattempt.StatusFailed || out.ExitCode != 0 || out.Duration <= 0 {
		t.Fatalf("err=%v out=%+v", err, out)
	}
}

func TestRunnerSweptWhileStartGuardWaitsDoesNotLaunch(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	marker := filepath.Join(home, "started-after-sweep")
	guardEntered := make(chan struct{})
	releaseGuard := make(chan struct{})
	type runResult struct {
		out RunOutput
		err error
	}
	result := make(chan runResult, 1)
	startedAt := time.Now().UTC().Add(-time.Hour)
	go func() {
		out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints", RunInput{
			WorkID: "work_swept_start_guard", Harness: "os-harness-entrypoints", Workspace: home,
			Command: "sh", Now: startedAt,
			StartGuard: func(_ context.Context, start func() error) error {
				close(guardEntered)
				<-releaseGuard
				return start()
			},
		}, func(ctx context.Context, _ RunInput) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "printf started > \"$1\"", "sh", marker)
		}, defaultHeartbeatInterval)
		result <- runResult{out: out, err: err}
	}()

	select {
	case <-guardEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not reach the blocked start guard")
	}
	report, err := store.SweepStale(time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.Swept != 1 || report.Escalated != 1 || report.AutoClosed != 0 {
		t.Fatalf("blocked pre-start harness attempt was not swept conservatively ambiguous: %+v", report)
	}
	close(releaseGuard)

	var got runResult
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not return after releasing the start guard")
	}
	if got.err == nil || !strings.Contains(got.err.Error(), "runattempt is terminal") {
		t.Fatalf("swept start error = %v", got.err)
	}
	if got.out.RunAttempt.Status != runattempt.StatusAbandoned {
		t.Fatalf("swept attempt status = %q, want abandoned", got.out.RunAttempt.Status)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("swept attempt launched the child; marker stat err=%v", statErr)
	}
}

func (s *failingOriginStore) MintWithActionIntent(workID, actor, harness, actionIntentID string, now time.Time) (runattempt.RunAttempt, error) {
	ra, err := s.Store.MintWithActionIntent(workID, actor, harness, actionIntentID, now)
	s.mintedID = ra.ID
	return ra, err
}

func (s *failingOriginStore) SetOrigin(_, _ string) (runattempt.RunAttempt, error) {
	return runattempt.RunAttempt{}, errors.New("disk full")
}

// TestRunnerSetOriginFailureTerminalizes (audit E14): a store.SetOrigin
// failure immediately after a successful store.Mint MUST terminalize the
// freshly minted RunAttempt as `failed` and return it in
// RunOutput.RunAttempt -- honoring the Adapter contract "terminalize the
// RunAttempt on EVERY return path" (adapter.go) so callers can forward-link
// the failed attempt's id. Falsifier: the old code did
// `return RunOutput{}, fmt.Errorf(...)`, handing back a nil-id RunAttempt
// and leaving a `pending` ratt_ record stranded forever.
func TestRunnerSetOriginFailureTerminalizes(t *testing.T) {
	home := t.TempDir()
	real := runattempt.Open(home)
	store := &failingOriginStore{Store: real}

	build := func(ctx context.Context, in RunInput) *exec.Cmd {
		return exec.CommandContext(ctx, in.Command, in.Args...)
	}
	out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints",
		RunInput{
			WorkID:         "work_setorigin_fail",
			Harness:        "os-harness-entrypoints",
			Workspace:      home,
			Command:        "echo",
			Args:           []string{"ok"},
			ActionIntentID: "act_setorigin_fail",
			Now:            time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
		}, build, defaultHeartbeatInterval)

	// (1) A non-nil error is still returned: an origin-stamp failure is a
	// real infra failure, not a swallowed one.
	if err == nil {
		t.Fatalf("SetOrigin failure must still return a non-nil error")
	}
	// (2) RunOutput.RunAttempt.ID is non-empty AND equals the minted attempt
	// -- so a caller (work-advance) can append it to allRunAttempts. The old
	// code returned RunOutput{}, stranding the attempt.
	if out.RunAttempt.ID == "" {
		t.Fatalf("SetOrigin-failure RunOutput.RunAttempt.ID is empty; the minted attempt was stranded")
	}
	if out.RunAttempt.ID != store.mintedID {
		t.Fatalf("returned RunAttempt.ID = %q, want the minted id %q", out.RunAttempt.ID, store.mintedID)
	}
	// (3) The persisted ratt_ record is terminal `failed`, NOT `pending`.
	got, gerr := real.Get(store.mintedID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != runattempt.StatusFailed {
		t.Fatalf("persisted status = %q, want failed (SetOrigin failure must terminalize, not leave pending)", got.Status)
	}
	if got.ActionIntentID != "act_setorigin_fail" {
		t.Fatalf("persisted action_intent_id = %q, want atomic mint-time bind", got.ActionIntentID)
	}
	// (4) The terminal record names the origin-stamp failure. No transcript
	// exists yet at the SetOrigin stage, so the reason lands as a proof_ref.
	body, rerr := os.ReadFile(storePath(real, got))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(body), "stamp origin") {
		t.Fatalf("persisted RunAttempt must record a reason naming the origin-stamp failure; got:\n%s", body)
	}
}

// TestRunnerStdinReachesSubprocess: RunInput.Stdin is piped to the
// subprocess (cat) and its echoed stdout lands in the transcript. Pinned:
// the old runner ran io.Copy(io.Discard, in.Stdin), silently dropping the
// caller's input.
func TestRunnerStdinReachesSubprocess(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_stdin",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "cat",
		Stdin:     strings.NewReader("hello-stdin-payload\n"),
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, rerr := os.ReadFile(out.TranscriptPath)
	if rerr != nil {
		t.Fatalf("read transcript: %v", rerr)
	}
	if !strings.Contains(string(body), "stdout: hello-stdin-payload") {
		t.Fatalf("transcript missing echoed stdin; got:\n%s", body)
	}
}

// TestRunnerActionIntentBindsBeforeTerminal: when RunInput.ActionIntentID
// is supplied, the terminal RunAttempt carries the binding. Pinned: the
// old runner called AttachActionIntent AFTER Complete, and
// AttachActionIntent refuses a terminal attempt -- so the binding was a
// guaranteed silent no-op and the attempt->intent->policy-decision chain
// was broken for every subprocess adapter.
func TestRunnerActionIntentBindsBeforeTerminal(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:         "work_intent",
		Harness:        "os-harness-entrypoints",
		Workspace:      home,
		Command:        "echo",
		Args:           []string{"ok"},
		ActionIntentID: "act_test_binding_123",
		Now:            time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.RunAttempt.ActionIntentID != "act_test_binding_123" {
		t.Fatalf("terminal RunAttempt action_intent_id = %q, want act_test_binding_123 (must bind before Complete)", out.RunAttempt.ActionIntentID)
	}
	// Re-Get from disk to prove the binding persisted through the
	// terminal transition, not just on the in-memory return value.
	got, gerr := store.Get(out.RunAttempt.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.ActionIntentID != "act_test_binding_123" {
		t.Fatalf("persisted action_intent_id = %q, want act_test_binding_123", got.ActionIntentID)
	}
}

// TestRunnerStampsLaneIDPreTerminal: when RunInput.LaneID is set, the
// terminal RunAttempt carries lane_id = that value, stamped via
// SetAttribution BEFORE Complete (a post-Complete stamp is refused by the
// terminal guard, so it would be a silent no-op). Re-Get from disk proves
// the stamp persisted through the terminal transition. Complements the
// CLI-level TestWorkAdvanceExecuteStampsLaneIDOnExecAttempt at the rail
// level -- no CLI or work-advance machinery in the path.
func TestRunnerStampsLaneIDPreTerminal(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_lane",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "echo",
		Args:      []string{"ok"},
		LaneID:    "lane-a:worker",
		Now:       time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.RunAttempt.LaneID != "lane-a:worker" {
		t.Fatalf("terminal RunAttempt lane_id = %q, want lane-a:worker (must stamp before Complete)", out.RunAttempt.LaneID)
	}
	got, gerr := store.Get(out.RunAttempt.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.LaneID != "lane-a:worker" {
		t.Fatalf("persisted lane_id = %q, want lane-a:worker", got.LaneID)
	}
}

// TestRunnerStampsHarnessExecutionOrigin (F2): every subprocess-adapter mint
// is a real harness execution, never a claim-time placeholder or a post-hoc
// observation -- the runner must stamp Origin=harness_execution right after
// Mint, before any other side effect, so F2's supersede-at-attach (in
// internal/work) can identify it when it attaches to a work envelope that
// already carries a claim_reservation attempt.
func TestRunnerStampsHarnessExecutionOrigin(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_origin",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "echo",
		Args:      []string{"ok"},
		Now:       time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.RunAttempt.Origin != runattempt.OriginHarnessExecution {
		t.Fatalf("terminal RunAttempt origin = %q, want %q", out.RunAttempt.Origin, runattempt.OriginHarnessExecution)
	}
	got, gerr := store.Get(out.RunAttempt.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Origin != runattempt.OriginHarnessExecution {
		t.Fatalf("persisted origin = %q, want %q", got.Origin, runattempt.OriginHarnessExecution)
	}
}

// TestRunnerEmptyLaneIDOmitsStamp: an empty LaneID stamps NOTHING -- the
// runner skips SetAttribution entirely, so the persisted RunAttempt JSON
// has NO lane_id key (omitempty at the rail level). Falsifier: if the
// runner called SetAttribution unconditionally, the field would still
// round-trip; asserting on the raw JSON bytes (not just the struct) pins
// that an unattributed run leaves no lane_id trace.
func TestRunnerEmptyLaneIDOmitsStamp(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_no_lane",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "echo",
		Args:      []string{"ok"},
		// LaneID intentionally empty: unattributed run.
		Now: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, rerr := os.ReadFile(storePath(store, out.RunAttempt))
	if rerr != nil {
		t.Fatalf("read RunAttempt JSON: %v", rerr)
	}
	if strings.Contains(string(body), "lane_id") {
		t.Fatalf("unattributed RunAttempt JSON must omit lane_id (empty LaneID stamps nothing); got:\n%s", body)
	}
}

// TestRunnerInfraFailureTerminalizes: a Start failure (bad command path)
// terminalizes the RunAttempt failed and returns BOTH the RunAttempt and
// an error. Pinned: the old runner returned RunOutput{} + err on every
// post-Mint infra failure, leaving a permanent status=pending record.
func TestRunnerInfraFailureTerminalizes(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_infra",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "/this/binary/does/not/exist/anywhere",
		ProofRefs: []string{"capability-route-plan:crp_fixture"},
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatalf("expected infrastructure error for a bad command path, got nil")
	}
	if out.RunAttempt.ID == "" {
		t.Fatalf("infra-failure RunOutput must still carry the minted RunAttempt id, got empty")
	}
	// The persisted record must be terminal (failed), never pending.
	got, gerr := store.Get(out.RunAttempt.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != runattempt.StatusFailed {
		t.Fatalf("persisted status = %q, want failed (infra failure must terminalize, not leave pending)", got.Status)
	}
	if !strings.Contains(strings.Join(got.ProofRefs, "\n"), "capability-route-plan:crp_fixture") {
		t.Fatalf("start failure replaced dispatch authority: %v", got.ProofRefs)
	}
	transcript, err := os.ReadFile(out.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), "infra_error:") {
		t.Fatalf("start failure transcript omitted infra evidence:\n%s", transcript)
	}
}

// TestRunnerConcurrentTranscriptWrites: a subprocess that writes many
// interleaved stdout and stderr lines produces a well-formed transcript
// -- every line is exactly "stdout: out<N>" or "stderr: err<N>". Pinned:
// the old runner had two capture goroutines writing one bufio.Writer with
// no lock, which can interleave or corrupt under contention.
func TestRunnerConcurrentTranscriptWrites(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	a := NewOSEntrypointsAdapter(store)

	const lines = 300
	out, err := a.Run(context.Background(), RunInput{
		WorkID:    "work_concurrent",
		Harness:   "os-harness-entrypoints",
		Workspace: home,
		Command:   "sh",
		Args:      []string{"-c", "for i in $(seq 1 " + itoa(lines) + "); do echo \"out$i\"; echo \"err$i\" 1>&2; done"},
		Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, rerr := os.ReadFile(out.TranscriptPath)
	if rerr != nil {
		t.Fatalf("read transcript: %v", rerr)
	}
	lineRE := regexp.MustCompile(`^(stdout|stderr): (out|err)[0-9]+$`)
	var stdoutCount, stderrCount int
	for _, ln := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if ln == "" {
			continue
		}
		if !lineRE.MatchString(ln) {
			t.Fatalf("garbled transcript line (concurrent write corruption?): %q", ln)
		}
		if strings.HasPrefix(ln, "stdout:") {
			stdoutCount++
		} else {
			stderrCount++
		}
	}
	if stdoutCount != lines || stderrCount != lines {
		t.Fatalf("transcript line counts = stdout:%d stderr:%d, want %d each (lost lines = dropped capture)", stdoutCount, stderrCount, lines)
	}
}

// TestRunnerTimeoutOwnedByRunner: the shared runner enforces
// RunInput.Timeout even when buildCmd does NOT use exec.CommandContext.
// Pinned: the old runner delegated timeout to buildCmd and ran
// capWG.Wait() before waitWithTimeout, so a future adapter that forgets
// CommandContext would hang forever inside capWG.Wait.
func TestRunnerTimeoutOwnedByRunner(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	// Deliberately build with exec.Command (no context) to simulate an
	// adapter that forgot CommandContext. The runner must still kill it.
	naiveBuild := func(_ context.Context, in RunInput) *exec.Cmd {
		return exec.Command(in.Command, in.Args...)
	}
	start := time.Now()
	out, err := runWithSubprocess(context.Background(), store, "naive",
		RunInput{
			WorkID:    "work_timeout_owner",
			Harness:   "naive",
			Workspace: home,
			Command:   "sleep",
			Args:      []string{"30"},
			Timeout:   300 * time.Millisecond,
			Now:       time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		}, naiveBuild, defaultHeartbeatInterval)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("timeout must NOT surface as a Run-level error: %v", err)
	}
	if !out.TimedOut {
		t.Fatalf("out.TimedOut = false, want true (runner must enforce timeout even without CommandContext)")
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed", out.RunAttempt.Status)
	}
	// Sanity: 30s sleep killed at 300ms should return well under 5s.
	if elapsed > 5*time.Second {
		t.Fatalf("runner did not enforce timeout: elapsed = %v (process was not killed)", elapsed)
	}
}

// itoa is a strconv-free int->string for test arg assembly (matches the
// style of intToStr in claude_adapter_test.go, kept local to avoid an
// import for a single call site).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// failOnceCompleteStore wraps a real *runattempt.Store but fails the FIRST
// Complete call, simulating a transient store fault at terminalization.
// Every later call delegates, so the runner's retry can still land.
type failOnceCompleteStore struct {
	*runattempt.Store
	calls int
}

func (s *failOnceCompleteStore) Complete(id string, status string, now time.Time) (runattempt.RunAttempt, error) {
	s.calls++
	if s.calls == 1 {
		return runattempt.RunAttempt{}, errors.New("transient write fault")
	}
	return s.Store.Complete(id, status, now)
}

// alwaysFailCompleteStore fails EVERY Complete call, simulating a store that
// refuses all terminal writes. calls counts attempts so the test can prove
// the retry and the best-effort failTerm Complete both fired.
type alwaysFailCompleteStore struct {
	*runattempt.Store
	calls int
}

func (s *alwaysFailCompleteStore) Complete(_, _ string, _ time.Time) (runattempt.RunAttempt, error) {
	s.calls++
	return runattempt.RunAttempt{}, errors.New("disk full")
}

// TestRunnerCompleteRetriedOnce: a transient store.Complete fault is retried
// once, so the run still terminalizes succeeded instead of returning a
// pending attempt the caller would forward-link onto the envelope.
// Falsifier: the old code returned the first Complete error to the caller and
// left the persisted record pending.
func TestRunnerCompleteRetriedOnce(t *testing.T) {
	home := t.TempDir()
	real := runattempt.Open(home)
	store := &failOnceCompleteStore{Store: real}

	build := func(ctx context.Context, in RunInput) *exec.Cmd {
		return exec.CommandContext(ctx, in.Command, in.Args...)
	}
	out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints",
		RunInput{
			WorkID:    "work_complete_retry",
			Harness:   "os-harness-entrypoints",
			Workspace: home,
			Command:   "echo",
			Args:      []string{"ok"},
			Now:       time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
		}, build, defaultHeartbeatInterval)
	if err != nil {
		t.Fatalf("a transient Complete fault must be retried, got error: %v", err)
	}
	if store.calls != 2 {
		t.Fatalf("Complete calls = %d, want 2 (initial + one retry)", store.calls)
	}
	if out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded after the retry", out.RunAttempt.Status)
	}
	got, gerr := real.Get(out.RunAttempt.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != runattempt.StatusSucceeded {
		t.Fatalf("persisted status = %q, want succeeded (the retry must land the terminal write)", got.Status)
	}
}

// TestRunnerCompleteFailureTerminalizesBestEffort: a persistent
// store.Complete failure still (a) retries once, (b) records the reason on
// the transcript, and (c) attempts a best-effort failed Complete -- then
// returns the error alongside the REAL run result (exit code, transcript),
// never a silently-pending record. A store that refuses every terminal write
// cannot be terminalized; the transcript line and the returned error are the
// audit trail that remains.
func TestRunnerCompleteFailureTerminalizesBestEffort(t *testing.T) {
	home := t.TempDir()
	real := runattempt.Open(home)
	store := &alwaysFailCompleteStore{Store: real}

	build := func(ctx context.Context, in RunInput) *exec.Cmd {
		return exec.CommandContext(ctx, in.Command, in.Args...)
	}
	out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints",
		RunInput{
			WorkID:    "work_complete_fail",
			Harness:   "os-harness-entrypoints",
			Workspace: home,
			Command:   "echo",
			Args:      []string{"ok"},
			Now:       time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
		}, build, defaultHeartbeatInterval)
	if err == nil {
		t.Fatalf("a persistent Complete failure must still return a non-nil error")
	}
	// The minted attempt id is returned so the caller can forward-link it.
	if out.RunAttempt.ID == "" {
		t.Fatalf("RunOutput.RunAttempt.ID is empty; the minted attempt was stranded")
	}
	// The real run result is preserved on top of failTerm's infra defaults:
	// the echo succeeded (exit 0); only terminalization failed.
	if out.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (the run itself succeeded)", out.ExitCode)
	}
	// Three Complete attempts: the initial call, the retry, and failTerm's
	// best-effort failed Complete.
	if store.calls != 3 {
		t.Fatalf("Complete calls = %d, want 3 (initial + retry + failTerm)", store.calls)
	}
	// The reason landed on the transcript even though the store refused
	// every terminal write.
	body, rerr := os.ReadFile(out.TranscriptPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(body), "infra_error: complete run attempt") {
		t.Fatalf("transcript must record the terminalization failure reason; got:\n%s", body)
	}
}

// TestRunnerClampsNonPositiveHeartbeatInterval: a non-positive heartbeat
// interval panics time.NewTicker inside heartbeatLoop. runWithSubprocess
// clamps it to the default cadence, so a misconfigured adapter degrades
// instead of crashing the run after the RunAttempt was minted.
// Falsifier: before the clamp, interval 0 crashed the process with
// "non-positive interval for NewTicker".
func TestRunnerClampsNonPositiveHeartbeatInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		home := t.TempDir()
		store := runattempt.Open(home)
		build := func(ctx context.Context, in RunInput) *exec.Cmd {
			return exec.CommandContext(ctx, in.Command, in.Args...)
		}
		out, err := runWithSubprocess(context.Background(), store, "os-harness-entrypoints",
			RunInput{
				WorkID:    "work_hb_clamp",
				Harness:   "os-harness-entrypoints",
				Workspace: home,
				Command:   "echo",
				Args:      []string{"ok"},
				Now:       time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
			}, build, interval)
		if err != nil {
			t.Fatalf("interval %v: run must succeed on the clamped cadence: %v", interval, err)
		}
		if out.RunAttempt.Status != runattempt.StatusSucceeded {
			t.Fatalf("interval %v: status = %q, want succeeded", interval, out.RunAttempt.Status)
		}
	}
}

// TestRunnerTimeoutKillsProcessGroup: the timeout kill targets the whole
// process group, not just the direct child. The fixture spawns a background
// grandchild that inherits (holds open) the capture pipes; a direct-child-
// only kill leaves the grandchild alive, capture never sees EOF, and
// capWG.Wait blocks until the grandchild exits (~30s) -- with heartbeats
// keeping the attempt fresh, so SweepStale would never collect it either.
// Falsifier: with a direct-child-only kill this test takes the full 30s
// sleep and trips the elapsed bound.
func TestRunnerTimeoutKillsProcessGroup(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	// Deliberately build with exec.Command (no context), same as
	// TestRunnerTimeoutOwnedByRunner: the runner owns the kill.
	naiveBuild := func(_ context.Context, in RunInput) *exec.Cmd {
		return exec.Command(in.Command, in.Args...)
	}
	start := time.Now()
	out, err := runWithSubprocess(context.Background(), store, "naive",
		RunInput{
			WorkID:    "work_group_kill",
			Harness:   "naive",
			Workspace: home,
			// The background sleep inherits the capture pipes; the foreground
			// sleep keeps the direct child alive until the kill fires. Only a
			// process-group kill reaps BOTH.
			Command: "sh",
			Args:    []string{"-c", "sleep 30 & sleep 30"},
			Timeout: 300 * time.Millisecond,
			Now:     time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC),
		}, naiveBuild, defaultHeartbeatInterval)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("timeout must NOT surface as a Run-level error: %v", err)
	}
	if !out.TimedOut {
		t.Fatalf("out.TimedOut = false, want true")
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed", out.RunAttempt.Status)
	}
	// Killed at 300ms; a direct-child-only kill returns only after the
	// orphaned grandchild's 30s sleep closes the pipes.
	if elapsed > 5*time.Second {
		t.Fatalf("process group was not killed: elapsed = %v (an orphaned grandchild held the pipes open)", elapsed)
	}
}

// TestRunnerTimeoutSurvivesEarlyPipeClose pins the liveness half of timeout
// ownership: draining capture must NOT disarm the kill timer.
//
// The child closes stdout and stderr immediately and then lingers. Both
// capture goroutines EOF and return within milliseconds, so a runner that
// closes stopCh (cancelling the timeout) before cmd.Wait has handed the
// child unbounded life -- Wait blocks until the process exits on its own,
// with no remaining authority to kill it. A governed executor that can be
// made to outlive its timeout by closing two file descriptors is not
// governed, so the timer must stay armed until the process is actually
// reaped.
func TestRunnerTimeoutSurvivesEarlyPipeClose(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	// Deliberately build without a context, same as the sibling timeout
	// tests: the runner owns the kill, not exec.CommandContext.
	naiveBuild := func(_ context.Context, in RunInput) *exec.Cmd {
		return exec.Command(in.Command, in.Args...)
	}
	start := time.Now()
	out, err := runWithSubprocess(context.Background(), store, "pipeclose",
		RunInput{
			WorkID:    "work_timeout_pipe_close",
			Harness:   "pipeclose",
			Workspace: home,
			Command:   "sh",
			// Close fd 1 and fd 2, then linger far past the timeout.
			Args:    []string{"-c", "exec 1>&- 2>&-; sleep 45"},
			Timeout: 300 * time.Millisecond,
			Now:     time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
		}, naiveBuild, defaultHeartbeatInterval)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("timeout must NOT surface as a Run-level error: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("runner outlived its timeout: elapsed = %v, want < 10s "+
			"(child closed stdout/stderr, so capture drained early and the "+
			"kill timer was disarmed before cmd.Wait)", elapsed)
	}
	if !out.TimedOut {
		t.Fatalf("out.TimedOut = false, want true (child lingered past Timeout)")
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed", out.RunAttempt.Status)
	}
}

// TestRunnerContextCancelKillsProcessGroup (DEFECT 4 RED -> GREEN):
//
// The shared subprocess runner's ctx-cancel goroutine watches ctx.Done()
// and triggers a process-group kill — pinning the contract that the
// unattended launch wire relies on when it forwards the heartbeat's
// cancellable ctx (pre-fix the wire passed context.Background, so this
// goroutine never had a deadline to honor; the heartbeat's outer
// `timeout 600` would SIGKILL the parent with the subprocess tree
// still alive, the very failure this fix prevents).
//
// The fixture is the canonical "descendant holds the pipes open" shape
// from TestRunnerTimeoutKillsProcessGroup: a shell background sleep that
// inherits the capture pipes, plus a foreground sleep. Cancelling
// ctx while both are alive MUST kill the whole group, not just the
// foreground child — the orphaned grandchild closing its pipes is what
// capWG.Wait waits on.
//
// RED before fix: the runner ignored ctx for kill authority; cancel()
// does nothing; the fixture lingers for its full 30s sleep and trips the
// elapsed bound.
//
// The fixture sleeps 30s, matching TestRunnerTimeoutKillsProcessGroup, so
// that "kill worked" (~0.2s) and "kill failed" (30s) are 25s apart. An
// earlier 5s fixture put the two outcomes on either side of the same 5s
// bound with no margin, and a working kill read as broken whenever the
// package ran under load — it failed inside the certifying battery, where
// this package takes 89s instead of 18s, while passing 20/20 standalone.
// The assertion bound is deliberately unchanged; only the separation grew.
func TestRunnerContextCancelKillsProcessGroup(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	// Deliberately build without a context (the runner owns the
	// process-group kill via its ctx goroutine, not via
	// exec.CommandContext).
	naiveBuild := func(_ context.Context, in RunInput) *exec.Cmd {
		return exec.Command(in.Command, in.Args...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Schedule the cancel BEFORE runWithSubprocess so the cancel goroutine
	// can run concurrently with the runner — main blocks inside
	// runWithSubprocess (cmd.Wait) and would otherwise never reach cancel.
	//
	// The cancel waits for the child to PROVE it is running (it touches
	// readyPath) instead of firing on a blind timer. A fixed 200ms delay
	// races process start: when the package runs together rather than under
	// -run isolation, start can lose that race, ctx is already cancelled at
	// Start(), and the runner surfaces "start: context canceled" — which is
	// not the behaviour under test. That race made this test fail in a full
	// package run and inside the certifying battery while passing 20/20 alone.
	readyPath := filepath.Join(home, "child-ready")
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(readyPath); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	out, err := runWithSubprocess(ctx, store, "defect4-cancel", RunInput{
		WorkID:    "work_defect4_ctx_cancel",
		Harness:   "defect4-cancel",
		Workspace: home,
		// Background sleep inherits capture pipes; foreground sleep keeps
		// the direct child alive. A process-group kill reaps BOTH; a
		// direct-child-only kill leaves the grandchild holding the
		// pipes open for the full sleep. The touch between them is the
		// readiness marker the cancel goroutine waits on.
		Command: "sh",
		Args:    []string{"-c", "sleep 30 & touch " + readyPath + "; sleep 30"},
		// No Timeout: the ctx goroutine is the only authority.
		Now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
	}, naiveBuild, defaultHeartbeatInterval)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("cancel must NOT surface as a Run-level error: %v", err)
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed (ctx cancel must terminalize the attempt)", out.RunAttempt.Status)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("ctx cancellation did not kill the tree: elapsed = %v (the orphaned grandchild held the pipes open)", elapsed)
	}
	// Cancellation evidence MUST be persisted on the RunAttempt's
	// proof_refs — the audit answers "why did this die early?" with
	// the real reason rather than a generic non-zero exit.
	got, gerr := runattempt.Open(home).Get(out.RunAttempt.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	var hit bool
	for _, ref := range got.ProofRefs {
		if strings.HasPrefix(ref, "cancel:context_cancelled") {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatalf("RunAttempt.ProofRefs %v must include cancel:context_cancelled evidence", got.ProofRefs)
	}
}

// TestRunnerTimeoutPersistsEvidence (DEFECT 4 RED -> GREEN):
//
// The shared subprocess runner persists `timeout:exceeded:<duration>`
// as a proof_ref on the RunAttempt when the deadline kills the tree,
// so the audit answers "why did this die early?" with the real
// reason rather than a generic non-zero exit.
//
// Pinned alongside TestRunnerTimeoutOwnedByRunner (which verifies the
// kill happens): the proof_ref is the durable half of the same
// contract.
func TestRunnerTimeoutPersistsEvidence(t *testing.T) {
	home := t.TempDir()
	store := runattempt.Open(home)
	naiveBuild := func(_ context.Context, in RunInput) *exec.Cmd {
		return exec.Command(in.Command, in.Args...)
	}
	out, err := runWithSubprocess(context.Background(), store, "defect4-evidence", RunInput{
		WorkID:    "work_defect4_timeout_evidence",
		Harness:   "defect4-evidence",
		Workspace: home,
		Command:   "sleep",
		Args:      []string{"30"},
		Timeout:   200 * time.Millisecond,
		Now:       time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
	}, naiveBuild, defaultHeartbeatInterval)
	if err != nil {
		t.Fatalf("timeout must NOT surface as a Run-level error: %v", err)
	}
	if !out.TimedOut {
		t.Fatal("out.TimedOut = false, want true")
	}
	if out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("status = %q, want failed", out.RunAttempt.Status)
	}
	got, gerr := runattempt.Open(home).Get(out.RunAttempt.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	var hit bool
	for _, ref := range got.ProofRefs {
		if strings.HasPrefix(ref, "timeout:exceeded:") {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatalf("RunAttempt.ProofRefs %v must include timeout:exceeded evidence", got.ProofRefs)
	}
}
