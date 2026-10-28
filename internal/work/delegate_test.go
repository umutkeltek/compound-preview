// Package-level falsifiers for slice 1 of the governed execution substrate:
// `compound work delegate|steer|attach`. These tests are red-without and
// green-with -- each red proof mutates a single line in the orchestrator
// (e.g. deletes the guard call, swallows the confirm, returns the wrong
// refusal reason) to demonstrate teeth, then reverts before commit. The
// orchestrator is pure (no real tmux, no real wrap resolution) and is
// exercised through the injected `fakeSessionRunner` so the tests never
// touch a real tmux server, the operator's $PATH, or a real wrapper binary.
//
// What the tests cover:
//
//	D1: walled lane is REFUSED even before spawn -- fake runner records zero
//	    spawns.
//	D2a: walled beats --confirm.
//	D2b: confirmation_required + --confirm PROCEEDS with confirmation stamped.
//	D3: nil guard refuses (reason=gate_absent) and never spawns.
//	D4: a live bound session (Has returns true) refuses a second spawn.
//	D5: steer on a dead session (Has returns false) records no send-keys;
//	    on a live session records exactly one send-keys.
//	D6: binding roundtrip -- steer and attach both read the SAME session name.
//	D7: each verb path emits the documented structured event.
//	D8: missing tmux runner records no spawn and emits refused{tmux_absent}.
//	dry-run: prints the decision without spawning, no event.
//
// premise-validation kept honest: D6 binding roundtrip without a status verb
// resolves steer/attach against the durable binding (NOT against a non-
// existent status verb). This packet explicitly excludes a `compound work
// status` verb from slice 1; the contract here never names one.
package work

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// fakeSessionRunner is the test double for the SessionRunner seam. It records
// every Has/Create/SendKeys/Kill call and answers Has from `live` so the
// "single live session" envelope stays observable from the test.
type fakeSessionRunner struct {
	mu sync.Mutex

	live     map[string]bool
	creates  []fakeCreateCall
	keys     []fakeSendKeysCall
	kills    []string
	avail    bool
	availErr error

	// createErr lets a test inject a Create failure (e.g. tmux-absent simulating
	// by treating `available=false` separately).
	createErr error
	// partialCreate simulates tmux returning an error after a session with
	// the requested name became observable. The orchestrator must classify
	// this as ambiguous and perform zero kills.
	partialCreate bool
	// duplicateNameErr models real tmux's refusal to create a second session
	// under a name the server already has ("duplicate session"). Opt-in so
	// the tests that only care about the spawn decision keep a Create that
	// always succeeds; the teardown falsifiers need it because a surviving
	// orphan pane is precisely what turns the next spawn into
	// spawn_ambiguous.
	duplicateNameErr error
	onCreate         func()
	killErr          error
	statusErr        error

	// panes holds explicit SessionStatus observations per session name.
	// When unset the runner falls back to "live -> exists/ExitCode=-1"
	// or "gone -> exists=false" so existing tests don't need to
	// populate it.
	panes map[string]SessionStatus
}

type fakeCreateCall struct {
	Name        string
	Dir         string
	Command     string
	Args        []string
	Environment map[string]string
}

type fakeSendKeysCall struct {
	Name  string
	Text  string
	Enter bool
}

func newFakeRunner() *fakeSessionRunner {
	return &fakeSessionRunner{
		live:  map[string]bool{},
		avail: true,
	}
}

func (f *fakeSessionRunner) Available() error { return f.availErr }

func (f *fakeSessionRunner) Has(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[name], nil
}

// SessionStatus is the in-test SessionRunner pane probe. Tests inject the
// shape they want each delegate verb to observe: a live pane returns
// {Exists: true, ExitCode: -1}; a dead pane returns observed exit (and
// optional signal); an unknown pane returns Exists=false and an
// UnknownReason so the orchestrator must classify as abandoned. The
// `pane` field overrides the default "Exists/PaneDead false" so a test
// can hand-call the runner with an explicit observation.
func (f *fakeSessionRunner) SessionStatus(name string) (SessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return SessionStatus{UnknownReason: "injected status uncertainty", ExitCode: -1}, f.statusErr
	}
	if ps, ok := f.panes[name]; ok {
		return ps, nil
	}
	if f.live[name] {
		return SessionStatus{Exists: true, ExitCode: -1}, nil
	}
	return SessionStatus{Exists: false}, nil
}

func (f *fakeSessionRunner) Create(name, dir, command string, args []string) error {
	return f.create(name, dir, command, args, nil)
}

func (f *fakeSessionRunner) CreateWithEnvironment(name, dir, command string, args []string, environment map[string]string) error {
	return f.create(name, dir, command, args, environment)
}

func (f *fakeSessionRunner) create(name, dir, command string, args []string, environment map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil && !f.partialCreate {
		return f.createErr
	}
	if f.duplicateNameErr != nil && f.live[name] {
		return f.duplicateNameErr
	}
	copiedEnvironment := make(map[string]string, len(environment))
	for key, value := range environment {
		copiedEnvironment[key] = value
	}
	f.creates = append(f.creates, fakeCreateCall{
		Name:        name,
		Dir:         dir,
		Command:     command,
		Args:        append([]string{}, args...),
		Environment: copiedEnvironment,
	})
	f.live[name] = true
	if f.onCreate != nil {
		f.onCreate()
	}
	if f.createErr != nil {
		return f.createErr
	}
	return nil
}

func (f *fakeSessionRunner) SendKeys(name, text string, enter bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.live[name] {
		return fmt.Errorf("session %q is not live", name)
	}
	f.keys = append(f.keys, fakeSendKeysCall{Name: name, Text: text, Enter: enter})
	return nil
}

func (f *fakeSessionRunner) Kill(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, name)
	if f.killErr != nil {
		return f.killErr
	}
	delete(f.live, name)
	delete(f.panes, name)
	return nil
}

func (f *fakeSessionRunner) AttachArgv(name string) []string {
	return []string{"tmux", "attach", "-t", name}
}

// capturedEvents scans the eventstore for compound.work_delegate_event.* ids
// and unmarshals them into a slice of generic JSON values for assertion. The
// scanner ignores the work lifecycle stream (work.jsonl) -- delegate events
// are dedicated records under <COMPOUND_HOME>/events/.
func capturedDelegateEvents(t *testing.T, s *Store) []map[string]any {
	t.Helper()
	entries, err := s.events.Scan(scanDelegateEvents())
	if err != nil {
		t.Fatalf("scan delegate events: %v", err)
	}
	out := []map[string]any{}
	for _, e := range entries {
		rec, err := e.Read()
		if err != nil {
			t.Fatalf("read delegate event %s: %v", e.ID, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Bytes, &payload); err != nil {
			t.Fatalf("unmarshal delegate event %s: %v", e.ID, err)
		}
		out = append(out, payload)
	}
	return out
}

func scanDelegateEvents() scanOptionsForDelegate {
	return scanOptionsForDelegate{Prefixes: []string{"wde_"}}
}

// scanOptionsForDelegate aliases eventstore.ScanOptions so the test does not
// need a separate import line at the top of the file.
type scanOptionsForDelegate = struct {
	Prefixes    []string
	RegularOnly bool
	Cursor      string
	// RecentSegments: see eventstore.ScanOptions; tests use the zero value.
	RecentSegments int
}

// requireEventKind fails if the captured slice does not contain exactly one
// event with the given kind.
func requireEventKind(t *testing.T, events []map[string]any, kind string) map[string]any {
	t.Helper()
	var match map[string]any
	for _, e := range events {
		if k, _ := e["kind"].(string); k == kind {
			if match != nil {
				t.Fatalf("more than one %q event found: %+v", kind, events)
			}
			match = e
		}
	}
	if match == nil {
		t.Fatalf("no %q event found among %d events: %+v", kind, len(events), events)
	}
	return match
}

// fakeGuard returns a LaneCapacityGuardFunc with the canned result.
func fakeGuard(refused, confirmationRequired bool, msg, state string) LaneCapacityGuardFunc {
	return func(req LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		return LaneCapacityGuardResult{
			Capacity: DispatchLaneCapacity{
				Remaining: 0.5,
				State:     state,
			},
			Refused:              refused,
			ConfirmationRequired: confirmationRequired,
			Message:              msg,
		}, nil
	}
}

func TestDelegateUsesInjectedTupleResolutionAndStampsAttribution(t *testing.T) {
	origGuard := LaneCapacityGuard
	origResolver := DelegateHarnessResolver
	LaneCapacityGuard = fakeGuard(false, false, "alive", "alive")
	DelegateHarnessResolver = func(req DelegateHarnessResolveRequest) (DelegateHarnessResolution, error) {
		if req.HomePath == "" || req.Harness != "claude_minimax" || req.Now.IsZero() || req.CommandOverride != "" {
			t.Fatalf("resolver input = %+v", req)
		}
		return DelegateHarnessResolution{
			Harness:                    "claude_minimax",
			Command:                    "resolved-claude-minimax",
			LaneID:                     "claude_minimax",
			RequestedModelID:           "claude-minimax (token plan)",
			RequestedProviderRoute:     "minimax",
			RequestedProviderRouteKind: "provider_class",
			CapacityLane:               "minimax",
			TupleBacked:                true,
		}, nil
	}
	t.Cleanup(func() {
		LaneCapacityGuard = origGuard
		DelegateHarnessResolver = origResolver
	})

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude_minimax", Actor: "codex", Now: fixedNow,
	}, runner)
	if err != nil || result.Status != "spawned" {
		t.Fatalf("delegate: err=%v result=%+v", err, result)
	}
	if len(runner.creates) != 1 || runner.creates[0].Command != "resolved-claude-minimax" {
		t.Fatalf("delegate did not launch resolved command: %+v", runner.creates)
	}
	ra, err := runattempt.Open(s.home).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if ra.LaneID != "claude_minimax" || ra.RequestedModelID != "claude-minimax (token plan)" || ra.RequestedProviderRoute != "minimax" {
		t.Fatalf("delegated dispatch intent = lane:%q model:%q provider:%q", ra.LaneID, ra.RequestedModelID, ra.RequestedProviderRoute)
	}
	if ra.ModelID != "" || ra.ProviderRoute != "" {
		t.Fatalf("delegated run guessed observed identity: model=%q provider=%q", ra.ModelID, ra.ProviderRoute)
	}
}

func TestDelegateTupleResolutionFailureCreatesNoSessionOrAttempt(t *testing.T) {
	origGuard := LaneCapacityGuard
	origResolver := DelegateHarnessResolver
	LaneCapacityGuard = fakeGuard(false, false, "alive", "alive")
	DelegateHarnessResolver = func(DelegateHarnessResolveRequest) (DelegateHarnessResolution, error) {
		return DelegateHarnessResolution{}, errors.New("no Compound lane tuple")
	}
	t.Cleanup(func() {
		LaneCapacityGuard = origGuard
		DelegateHarnessResolver = origResolver
	})

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude_qwen", Actor: "codex", Command: "raw-worker-tool", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "refused" || result.Refusal == nil || result.Refusal.Reason != "harness_unknown" {
		t.Fatalf("resolution failure result = %+v", result)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("resolution failure created session: %+v", runner.creates)
	}
	files, err := filepath.Glob(filepath.Join(s.home, "run_attempts", "ratt_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("resolution failure minted execution attempt; files=%v", files)
	}
}

func TestDelegateNilReadinessFailsClosedBeforeCapacityOrSpawn(t *testing.T) {
	originalReadiness := DelegateHarnessReadiness
	originalGuard := LaneCapacityGuard
	capacityCalls := 0
	DelegateHarnessReadiness = nil
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ok"}}, nil
	}
	t.Cleanup(func() {
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalGuard
	})

	s := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, s, "codex", "", "")
	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "refused" || result.Refusal == nil || result.Refusal.Reason != "readiness_unmet" ||
		result.Readiness == nil || result.Readiness.Ready {
		t.Fatalf("nil readiness result = %+v", result)
	}
	if capacityCalls != 0 || len(runner.creates) != 0 {
		t.Fatalf("nil readiness reached side effects: capacity_calls=%d creates=%+v", capacityCalls, runner.creates)
	}
	if events := capturedDelegateEvents(t, s); len(events) != 0 {
		t.Fatalf("nil readiness persisted delegate events: %+v", events)
	}
}

func TestDelegateValidatesWorkAndLeaseBeforeReadiness(t *testing.T) {
	originalReadiness := DelegateHarnessReadiness
	readinessCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() { DelegateHarnessReadiness = originalReadiness })

	s := newExperimentalDelegateStore(t)
	runner := newFakeRunner()
	if _, err := s.Delegate(DelegateOptions{
		WorkID: "work_missing", Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner); err == nil || !strings.Contains(err.Error(), "load work") {
		t.Fatalf("missing work error = %v", err)
	}
	if readinessCalls != 0 {
		t.Fatalf("missing work reached readiness %d time(s)", readinessCalls)
	}

	workID := prepareClaimedWork(t, s, "lease-owner", "", "")
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude", Actor: "other-actor", Command: "claude", DryRun: true, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "lease_held" {
		t.Fatalf("wrong-holder result = %+v", result)
	}
	if readinessCalls != 0 {
		t.Fatalf("wrong holder reached readiness %d time(s)", readinessCalls)
	}
	if events := capturedDelegateEvents(t, s); len(events) != 0 {
		t.Fatalf("wrong-holder dry-run persisted delegate events: %+v", events)
	}
}

func TestDelegateRevalidatesWorkImmediatelyBeforeMint(t *testing.T) {
	assertDelegateConcurrentAuthorityMutationRefused(t, func(s *Store, workID, holder string) error {
		_, err := s.Advance(workID, AdvanceInput{
			To:    "running",
			Actor: holder,
			Note:  "concurrent lifecycle mutation during delegate admission",
		}, fixedNow.Add(time.Second))
		return err
	})
}

func TestDelegateRevalidatesLeaseImmediatelyBeforeMint(t *testing.T) {
	assertDelegateConcurrentAuthorityMutationRefused(t, func(s *Store, workID, _ string) error {
		return s.recordDeclaredLaneOnLease(workID, "agent-lane://changed-during-admission")
	})
}

func TestDelegateRevalidatesReadinessSnapshotAfterCapacityAdmission(t *testing.T) {
	originalReadiness := DelegateHarnessReadiness
	originalGuard := LaneCapacityGuard
	t.Cleanup(func() {
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalGuard
	})

	s := newExperimentalDelegateStore(t)
	const holder = "orchestrator:test"
	workID := prepareClaimedWork(t, s, holder, "", "")
	beforeAttempts := runAttemptsByID(t, s)
	readinessRevision := "sha256:before-capacity"
	readinessCalls := 0
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		readinessCalls++
		return DelegateHarnessReadinessResult{
			Ready: true,
			Evidence: []DelegateHarnessReadinessEvidence{{
				Kind: "permission_posture", Ref: "harness-posture://claude-zai", Revision: readinessRevision,
			}},
		}, nil
	}
	capacityCalls := 0
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		if capacityCalls == 1 {
			readinessRevision = "sha256:after-capacity"
		}
		return LaneCapacityGuardResult{
			Capacity: DispatchLaneCapacity{State: "ok", Remaining: 0.8, DataRef: "test://capacity/stable"},
		}, nil
	}

	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Actor: holder, Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("readiness mutation during capacity admission result = %+v", result)
	}
	if readinessCalls != 2 {
		t.Fatalf("readiness calls = %d, want initial check + immediate pre-mint revalidation", readinessCalls)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("changed readiness reached Create: %+v", runner.creates)
	}
	assertNoNewDelegateAttempt(t, beforeAttempts, runAttemptsByID(t, s))
}

func TestDelegateRevalidatesCapacityImmediatelyBeforeMint(t *testing.T) {
	originalReadiness := DelegateHarnessReadiness
	originalGuard := LaneCapacityGuard
	t.Cleanup(func() {
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalGuard
	})

	s := newExperimentalDelegateStore(t)
	const holder = "orchestrator:test"
	workID := prepareClaimedWork(t, s, holder, "", "")
	beforeAttempts := runAttemptsByID(t, s)
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{
			Ready: true,
			Evidence: []DelegateHarnessReadinessEvidence{{
				Kind: "permission_posture", Ref: "harness-posture://claude-zai", Revision: "sha256:stable",
			}},
		}, nil
	}
	capacityCalls := 0
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		capacityCalls++
		if capacityCalls == 1 {
			return LaneCapacityGuardResult{
				Capacity: DispatchLaneCapacity{State: "ok", Remaining: 0.8, DataRef: "test://capacity/open"},
			}, nil
		}
		return LaneCapacityGuardResult{
			Capacity: DispatchLaneCapacity{State: "walled", Remaining: 0, DataRef: "test://capacity/walled"},
			Refused:  true,
			Message:  "capacity walled during admission",
		}, nil
	}

	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Actor: holder, Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("capacity mutation before mint result = %+v", result)
	}
	if capacityCalls != 2 {
		t.Fatalf("capacity calls = %d, want initial admission + immediate pre-mint revalidation", capacityCalls)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("changed capacity reached Create: %+v", runner.creates)
	}
	assertNoNewDelegateAttempt(t, beforeAttempts, runAttemptsByID(t, s))
}

func assertNoNewDelegateAttempt(t *testing.T, before, after map[string]runattempt.RunAttempt) {
	t.Helper()
	for id, attempt := range after {
		if _, existed := before[id]; !existed && attempt.Origin == runattempt.OriginHarnessExecution {
			t.Fatalf("admission race minted delegate attempt %s: %+v", id, attempt)
		}
	}
}

func assertDelegateConcurrentAuthorityMutationRefused(t *testing.T, mutate func(*Store, string, string) error) {
	t.Helper()
	originalGuard := LaneCapacityGuard
	s := newExperimentalDelegateStore(t)
	const holder = "orchestrator:test"
	workID := prepareClaimedWork(t, s, holder, "", "")
	beforeAttempts := runAttemptsByID(t, s)
	LaneCapacityGuard = func(LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		if err := mutate(s, workID, holder); err != nil {
			return LaneCapacityGuardResult{}, err
		}
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ok", Remaining: 0.8}}, nil
	}
	t.Cleanup(func() {
		LaneCapacityGuard = originalGuard
	})

	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude-zai",
		Actor:     holder,
		Command:   "claude-zai",
		Workspace: t.TempDir(),
		Now:       fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatalf("concurrent authority mutation should produce a structured refusal before mint: %v", err)
	}
	if result.Status != "refused" || result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("concurrent authority mutation result = %+v", result)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("concurrent authority mutation reached Create: %+v", runner.creates)
	}
	afterAttempts := runAttemptsByID(t, s)
	for id, attempt := range afterAttempts {
		if _, existed := beforeAttempts[id]; !existed && attempt.Origin == runattempt.OriginHarnessExecution {
			t.Fatalf("concurrent authority mutation minted delegate attempt %s: %+v", id, attempt)
		}
	}
}

// prepareClaimedWork returns a WorkEnvelope already in "claimed" state under
// byActor, with a real lease; the test pool then runs Delegate/Steer against
// it. The lease carries the canonical lane id (set via ClaimWithLane) so the
// orchestrator resolves the same input the dispatch verb uses.
func prepareClaimedWork(t *testing.T, s *Store, byActor, laneID, predicate string) string {
	t.Helper()
	if predicate == "" {
		predicate = "delegate spawn is audited as a work lifecycle event"
	}
	w, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: predicate,
		State:            "ready",
	}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ClaimWithLane(w.ID, byActor, laneID, fixedNow); err != nil {
		t.Fatalf("claim-with-lane: %v", err)
	}
	// The orchestrator accepts claimed OR running work. Seed at claimed so the
	// gate path matches the expected lifecycle step.
	return w.ID
}

// D1: walled lane is REFUSED, no spawn, refused{reason=walled}, the runner
// records zero Create calls. We prove teeth by mutating the orchestrator to
// skip the guard and observing a spawn (the targeted revert lives in the
// in-test scratch; persistence does not see it).
func TestDelegateGateHonored_RefusedWalled(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(true, false, "walled: lane at 0% for 4d", "walled")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID:  wID,
		Harness: "claude",
		Actor:   "codex",
		Session: "compound-" + wID,
		Command: "claude",
		Args:    []string{"--print"},
		Now:     fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "refused" || res.Refusal == nil || res.Refusal.Reason != "walled" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("expected zero spawns, got %d (%+v)", len(runner.creates), runner.creates)
	}
	if res.Binding != nil {
		t.Fatalf("refused path must not synthesize a binding, got %+v", res.Binding)
	}
	// Event store contract: one .refused event with reason=walled.
	events := capturedDelegateEvents(t, s)
	refused := requireEventKind(t, events, "work.delegate.refused")
	if got, _ := refused["reason"].(string); got != "walled" {
		t.Fatalf("refused reason = %q, want %q", got, "walled")
	}
	if got, _ := refused["work_id"].(string); got != wID {
		t.Fatalf("refused work_id = %q, want %q", got, wID)
	}
	// Work lifecycle tail must carry the delegate event id.
	life, lerr := s.eventsFor(wID)
	if lerr != nil {
		t.Fatalf("events for %s: %v", wID, lerr)
	}
	if !lifeHasAttached(life, firstEventID(events, "work.delegate.refused")) {
		t.Fatalf("lifecycle events do not attach the refused delegate event id: %+v", life)
	}
}

// D2a: walled beats --confirm. --confirm is forwarded into the guard but a
// walled refusal still returns refused{walled} without spawning.
func TestDelegateWalledBeatsConfirm(t *testing.T) {
	called := false
	orig := LaneCapacityGuard
	LaneCapacityGuard = func(req LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		called = true
		if !req.Confirm {
			t.Fatalf("guard must receive --confirm=true on walled+confirm path")
		}
		return LaneCapacityGuardResult{
			Capacity: DispatchLaneCapacity{State: "walled"},
			Refused:  true,
			Message:  "walled: capacity=0",
		}, nil
	}
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID:  wID,
		Harness: "claude",
		Actor:   "codex",
		Command: "claude",
		Confirm: true,
		Now:     fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if !called {
		t.Fatalf("guard was not invoked -- orchestrator skipped the gate")
	}
	if res.Status != "refused" || res.Refusal == nil || res.Refusal.Reason != "walled" {
		t.Fatalf("expected refused{walled}, got %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("walled+confirm must not spawn")
	}
}

// D2b: expiring + --confirm PROCEEDS with confirmation stamped on the spawned
// event; without --confirm, the orchestrator returns refused{expiring_unconfirmed}.
func TestDelegateConfirmationRequired_NeedsConfirm(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, true, "expiring in 2m", "expiring")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "refused" || res.Refusal == nil || res.Refusal.Reason != "expiring_unconfirmed" {
		t.Fatalf("expected refused{expiring_unconfirmed}, got %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("unconfirmed must not spawn")
	}

	// With --confirm, proceed; bind; the spawned event must carry confirmation=true.
	res2, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Confirm: true, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate confirm: %v", err)
	}
	if res2.Status != "spawned" || res2.Binding == nil {
		t.Fatalf("expected spawned binding, got %+v", res2)
	}
	events := capturedDelegateEvents(t, s)
	spawned := requireEventKind(t, events, "work.delegate.spawned")
	if conf, _ := spawned["confirm"].(bool); !conf {
		t.Fatalf("spawned.confirm = %v, want true", conf)
	}
}

// D3: nil guard refuses (reason=gate_absent) and never spawns. Pure-work
// tests set LaneCapacityGuard=nil in TestMain (work_test.go), so a fresh
// store + this test exercises the no-adapter-installed path.
func TestDelegateNilGuardFailsClosed(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = nil
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "refused" || res.Refusal == nil || res.Refusal.Reason != "gate_absent" {
		t.Fatalf("expected refused{gate_absent}, got %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("nil guard must not spawn")
	}
	// And the event is refused{gate_absent}.
	events := capturedDelegateEvents(t, s)
	refused := requireEventKind(t, events, "work.delegate.refused")
	if got, _ := refused["reason"].(string); got != "gate_absent" {
		t.Fatalf("reason = %q, want gate_absent", got)
	}
}

// D4: a LIVE bound session (Has returns true) refuses a second spawn and
// returns the existing session name as the operator hint. After death
// (Has returns false) the orchestrator records .exited for the old session
// and allows the new spawn, replacing the binding.
func TestDelegateSingleHolder(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	first, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("first delegate: %v", err)
	}
	if first.Status != "spawned" || first.Binding == nil {
		t.Fatalf("first delegate expected spawned, got %+v", first)
	}

	// Second delegate on a live session must refuse with session_live.
	second, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("second delegate: %v", err)
	}
	if second.Status != "refused" || second.Refusal == nil || second.Refusal.Reason != "session_live" {
		t.Fatalf("expected refused{session_live}, got %+v", second)
	}
	if got := second.Refusal.Session; got != first.Binding.Session {
		t.Fatalf("refused.Session = %q, want %q", got, first.Binding.Session)
	}
	if len(runner.creates) != 1 {
		t.Fatalf("second delegate must NOT spawn, got %d creates", len(runner.creates))
	}

	// Mark the session dead; the next delegate records .exited for the old
	// session AND proceeds with a new spawn, replacing the binding.
	if err := runner.Kill(first.Binding.Session); err != nil {
		t.Fatalf("kill: %v", err)
	}
	third, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("third delegate (after death): %v", err)
	}
	if third.Status != "spawned" || third.Binding == nil {
		t.Fatalf("third delegate expected spawned, got %+v", third)
	}
	if third.Binding.Session != first.Binding.Session {
		t.Fatalf("session name must be deterministic across respawns: %q vs %q", third.Binding.Session, first.Binding.Session)
	}
	events := capturedDelegateEvents(t, s)
	exited := requireEventKind(t, events, "work.delegate.exited")
	if got, _ := exited["session"].(string); got != first.Binding.Session {
		t.Fatalf("exited.session = %q, want %q", got, first.Binding.Session)
	}
	if len(runner.creates) != 2 {
		t.Fatalf("expected exactly two creates, got %d", len(runner.creates))
	}
}

// D5: steer on a dead session (Has returns false) records no send-keys and
// returns a non-nil error; steer on a live session records exactly one
// send-keys. With --enter, the fake records Enter=true.
func TestDelegateSteerRequiresLive(t *testing.T) {
	if testing.Short() {
		t.Skip("fsync-heavy: skipped in -short mode")
	}
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	// First spawn a session so a binding exists.
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if spawned.Status != "spawned" {
		t.Fatalf("spawn expected, got %+v", spawned)
	}

	// Steer when live -- one send-keys recorded.
	live, err := s.Steer(SteerOptions{
		WorkID: wID, Actor: "codex", Text: "hello", Enter: true, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("steer live: %v", err)
	}
	if live.Status != "steered" || live.Session != spawned.Binding.Session {
		t.Fatalf("steer live result: %+v", live)
	}
	if len(runner.keys) != 1 || runner.keys[0].Enter != true || runner.keys[0].Text != "hello" {
		t.Fatalf("expected one enter-on send-keys for %q, got %+v", "hello", runner.keys)
	}

	// Kill the session; steer must error and record NO additional send-keys.
	if err := runner.Kill(spawned.Binding.Session); err != nil {
		t.Fatalf("kill: %v", err)
	}
	dead, err := s.Steer(SteerOptions{
		WorkID: wID, Actor: "codex", Text: "after death", Enter: true, Now: fixedNow,
	}, runner)
	if err == nil {
		t.Fatalf("steer on dead session must error, got %+v", dead)
	}
	if len(runner.keys) != 1 {
		t.Fatalf("dead steer must NOT send keys, got %d calls", len(runner.keys))
	}
}

// D6: binding roundtrip -- steer and attach recover the EXACT recorded
// session name from the durable binding (no guessing). This is the slice-1
// substitute for a separate `compound work status` verb: steer/attach read
// from the binding file, NOT from a "currently-running" projection.
func TestDelegateBindingRoundtrip(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	attach, err := s.Attach(AttachOptions{WorkID: wID, Actor: "codex"}, runner)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if attach.Session != spawned.Binding.Session {
		t.Fatalf("attach.Session = %q, want %q", attach.Session, spawned.Binding.Session)
	}
	if got := strings.Join(attach.AttachArgv, " "); !strings.Contains(got, "tmux") {
		t.Fatalf("attach.AttachArgv must include tmux, got %v", attach.AttachArgv)
	}

	// Steer also recovers the same session name (live, no extra spawn).
	steer, err := s.Steer(SteerOptions{
		WorkID: wID, Actor: "codex", Text: "round", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("steer: %v", err)
	}
	if steer.Session != spawned.Binding.Session {
		t.Fatalf("steer.Session = %q, want %q", steer.Session, spawned.Binding.Session)
	}
}

// D7: each verb path emits the documented structured event kinds with the
// documented metadata fields. We assert:
//   - delegate -> work.delegate.spawned
//   - steer    -> work.delegate.steered
//   - refused  -> work.delegate.refused
//
// And each event carries work_id, session (when applicable), harness, lane,
// capacity_state, holder. NO transcript body field -- events must never
// carry the prompt text or command stdout.
func TestDelegateEventEmissionAndNoTranscript(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := s.Steer(SteerOptions{
		WorkID: wID, Actor: "codex", Text: "PROMPT-BODY-MUST-NOT-APPEAR", Now: fixedNow,
	}, runner); err != nil {
		t.Fatalf("steer: %v", err)
	}
	events := capturedDelegateEvents(t, s)
	spawnEv := requireEventKind(t, events, "work.delegate.spawned")
	steerEv := requireEventKind(t, events, "work.delegate.steered")
	for _, e := range []map[string]any{spawnEv, steerEv} {
		if got, _ := e["work_id"].(string); got != wID {
			t.Fatalf("event.work_id = %q, want %q", got, wID)
		}
		if got, _ := e["session"].(string); got != spawned.Binding.Session {
			t.Fatalf("event.session = %q, want %q", got, spawned.Binding.Session)
		}
		if got, _ := e["harness"].(string); got != "claude" {
			t.Fatalf("event.harness = %q, want %q", got, "claude")
		}
		if got, _ := e["capacity_state"].(string); got != "ok" {
			t.Fatalf("event.capacity_state = %q, want ok", got)
		}
		if got, _ := e["holder"].(string); got != "codex" {
			t.Fatalf("event.holder = %q, want codex", got)
		}
	}
	// Lifecycle safety: no prompt text or transcript text reaches any event record.
	for _, e := range events {
		canonical, _ := json.Marshal(e)
		if strings.Contains(string(canonical), "PROMPT-BODY-MUST-NOT-APPEAR") {
			t.Fatalf("delegate event leaked prompt body: %s", canonical)
		}
	}
}

// D8: tmux unavailable (runner.Available returns an error) refuses with
// reason=tmux_absent and never spawns. The orchestrator does NOT silently
// degrade to an ungoverned spawn.
func TestDelegateTmuxAbsentFailsClosed(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	runner.avail = false
	runner.availErr = errors.New("tmux binary not in PATH")

	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "refused" || res.Refusal == nil || res.Refusal.Reason != "tmux_absent" {
		t.Fatalf("expected refused{tmux_absent}, got %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("tmux absent must not spawn")
	}
	events := capturedDelegateEvents(t, s)
	refused := requireEventKind(t, events, "work.delegate.refused")
	if got, _ := refused["reason"].(string); got != "tmux_absent" {
		t.Fatalf("reason = %q, want tmux_absent", got)
	}
}

// Dry-run: runs the guard + binding + tmux-available checks, prints the
// decision, NEVER spawns, and (importantly) writes no event record. This
// matches the packet's "preview-only" semantic and prevents a dry-run run
// from polluting the audit trail.
func TestDelegateDryRunNeverSpawnsOrPersists(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, true, "expiring in 2m", "expiring")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", DryRun: true, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate dry-run: %v", err)
	}
	if res.Status != "preview" {
		t.Fatalf("dry-run status = %q, want preview", res.Status)
	}
	if res.Decision == nil {
		t.Fatalf("dry-run must carry the guard decision: %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("dry-run must not spawn, got %d", len(runner.creates))
	}
	// Persistence invariant: zero delegate events written.
	if got := capturedDelegateEvents(t, s); len(got) != 0 {
		t.Fatalf("dry-run must NOT emit events, got %d", len(got))
	}
}

func TestDelegateCapacityGuardTargetsRequestedLaneAndDefaultsMinutes(t *testing.T) {
	orig := LaneCapacityGuard
	defer func() { LaneCapacityGuard = orig }()

	var got LaneCapacityGuardRequest
	LaneCapacityGuard = func(req LaneCapacityGuardRequest) (LaneCapacityGuardResult, error) {
		got = req
		return LaneCapacityGuardResult{Capacity: DispatchLaneCapacity{State: "ok"}}, nil
	}

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), DryRun: true, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate dry-run: %v", err)
	}
	if res.Status != "preview" {
		t.Fatalf("status = %q, want preview", res.Status)
	}
	if got.Lane != "claude-zai" {
		t.Fatalf("capacity guard lane = %q, want requested executor lane claude-zai", got.Lane)
	}
	if got.ExpectedMinutes != 60 {
		t.Fatalf("capacity guard expected_minutes = %d, want default 60", got.ExpectedMinutes)
	}
}

func TestDelegateUsesResolvedWorkspace(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	workspace := t.TempDir()
	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: workspace, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "spawned" || len(runner.creates) != 1 {
		t.Fatalf("spawn result=%+v creates=%+v", res, runner.creates)
	}
	if runner.creates[0].Dir != workspace {
		t.Fatalf("runner workspace = %q, want %q", runner.creates[0].Dir, workspace)
	}
}

// TestDelegateRefusesWorkspaceHeldByAnotherLiveLane pins the attribution
// boundary. A lane's transcript directory is derived from its workspace
// (TranscriptDirFor), so two lanes sharing one workspace write into one session
// store with nothing marking which passage belongs to which work. Measured
// 2026-08-02: 11 lanes were spawned into a single workspace and 9 reduced to one
// JSONL, so their reports could not be attributed and a naive harvest returned
// byte-identical output for four different works. A collided transcript cannot
// be separated afterward, so the refusal has to happen at spawn.
func TestDelegateRefusesWorkspaceHeldByAnotherLiveLane(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	workspace := t.TempDir()
	runner := newFakeRunner()

	firstID := prepareClaimedWork(t, s, "orchestrator:test", "", "first lane holds the shared workspace")
	first, err := s.Delegate(DelegateOptions{
		WorkID: firstID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: workspace, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("first delegate: %v", err)
	}
	if first.Status != "spawned" {
		t.Fatalf("first delegate status = %q, want spawned", first.Status)
	}

	secondID := prepareClaimedWork(t, s, "orchestrator:test", "", "second lane aims at the same workspace")
	second, err := s.Delegate(DelegateOptions{
		WorkID: secondID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: workspace, Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("second delegate: %v", err)
	}
	if second.Refusal == nil || second.Refusal.Reason != "workspace_occupied" {
		t.Fatalf("second delegate into an occupied workspace = %+v, want refusal workspace_occupied", second)
	}
	// The refusal must name the holder, or the operator cannot tell which lane to
	// terminalize and the message is a dead end.
	if !strings.Contains(second.Refusal.Message, firstID) {
		t.Fatalf("refusal message %q does not name the holding work %s", second.Refusal.Message, firstID)
	}
	if len(runner.creates) != 1 {
		t.Fatalf("runner created %d sessions, want 1 -- the refused lane must not spawn", len(runner.creates))
	}
}

// TestDelegateAllowsDistinctWorkspaces is the other half of the asymmetry: the
// guard must key on the workspace, not merely on "another lane is live".
// Without this, refusing every concurrent delegate would also pass the test above.
func TestDelegateAllowsDistinctWorkspaces(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	runner := newFakeRunner()

	firstID := prepareClaimedWork(t, s, "orchestrator:test", "", "first lane in its own workspace")
	if _, err := s.Delegate(DelegateOptions{
		WorkID: firstID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow,
	}, runner); err != nil {
		t.Fatalf("first delegate: %v", err)
	}

	secondID := prepareClaimedWork(t, s, "orchestrator:test", "", "second lane in its own workspace")
	second, err := s.Delegate(DelegateOptions{
		WorkID: secondID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("second delegate: %v", err)
	}
	if second.Refusal != nil {
		t.Fatalf("delegate into its own workspace was refused: %+v", second.Refusal)
	}
	if second.Status != "spawned" || len(runner.creates) != 2 {
		t.Fatalf("second status=%q creates=%d, want spawned and 2", second.Status, len(runner.creates))
	}
}

func TestDelegateDoesNotPersistPromptArguments(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	const privatePrompt = "PAINT-THE-THREAD-IN-CERULEAN"
	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	_, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Args: []string{"--print", privatePrompt}, Workspace: t.TempDir(), Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	for _, event := range capturedDelegateEvents(t, s) {
		canonical, _ := json.Marshal(event)
		if strings.Contains(string(canonical), privatePrompt) {
			t.Fatalf("delegate event leaked prompt argument: %s", canonical)
		}
	}
	if err := filepath.WalkDir(s.home, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte(privatePrompt)) {
			t.Fatalf("durable file %s leaked prompt argument", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("scan durable files: %v", err)
	}
}

func TestDelegatePersistsPreparedRunningLaunchBoundaryBeforeCreate(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	var seenAtCreate string
	runner.onCreate = func() {
		for id, ra := range runAttemptsByID(t, s) {
			if _, existed := before[id]; existed {
				continue
			}
			if seenAtCreate != "" {
				t.Fatalf("more than one delegate attempt existed before Create: %s and %s", seenAtCreate, id)
			}
			seenAtCreate = id
			if ra.Status != runattempt.StatusRunning || ra.Launch == nil || ra.Launch.PreparedAt == nil {
				t.Fatalf("attempt at Create = status %s launch %+v, want running+prepared before side effects", ra.Status, ra.Launch)
			}
			if ra.Launch.CommittedAt != nil || ra.Launch.StartErrorAt != nil {
				t.Fatalf("attempt launch disposition existed before Create returned: %+v", ra.Launch)
			}
		}
	}
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if seenAtCreate == "" || result.RunAttemptID != seenAtCreate {
		t.Fatalf("mint/create identity: at_create=%q result=%q", seenAtCreate, result.RunAttemptID)
	}
	if got := runAttemptAt(t, runattempt.Open(s.home), seenAtCreate); got.Status != runattempt.StatusRunning || got.Launch == nil || got.Launch.CommittedAt == nil {
		t.Fatalf("delegate attempt after durable binding = status %s launch %+v, want running+committed", got.Status, got.Launch)
	}
	linked, err := s.Get(wID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range linked.RunAttemptIDs {
		found = found || id == seenAtCreate
	}
	if !found {
		t.Fatalf("work envelope does not forward-link delegate attempt %s: %v", seenAtCreate, linked.RunAttemptIDs)
	}
}

func TestDelegateMintFailureMeansZeroCreate(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runDir := filepath.Join(s.home, "run_attempts")
	if err := os.Chmod(runDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(runDir, 0o700) })
	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "mint_failed" {
		t.Fatalf("result = %+v, want mint_failed", result)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("mint failure reached Create: %+v", runner.creates)
	}
}

func TestDelegateCreateFailureTerminalizesExactAttempt(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	runner.createErr = errors.New("opaque spawn failure")
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "spawn_failed" {
		t.Fatalf("result = %+v, want spawn_failed", result)
	}
	assertOneNewAttemptStatus(t, s, before, runattempt.StatusFailed)
}

func TestDelegateCreateCannotLaunchAfterTerminalSweep(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	createEntered := make(chan struct{})
	releaseCreate := make(chan struct{})
	runner.onCreate = func() {
		close(createEntered)
		<-releaseCreate
	}

	type delegateOutcome struct {
		result DelegateResult
		err    error
	}
	delegateDone := make(chan delegateOutcome, 1)
	delegateNow := fixedNow.Add(time.Minute)
	workspace := t.TempDir()
	go func() {
		result, err := s.Delegate(DelegateOptions{
			WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
			Workspace: workspace, Now: delegateNow,
		}, runner)
		delegateDone <- delegateOutcome{result: result, err: err}
	}()
	<-createEntered

	var launchingID string
	for id := range runAttemptsByID(t, s) {
		if _, existed := before[id]; !existed {
			if launchingID != "" {
				close(releaseCreate)
				t.Fatalf("more than one delegate attempt minted before Create returned: %s and %s", launchingID, id)
			}
			launchingID = id
		}
	}
	if launchingID == "" {
		close(releaseCreate)
		t.Fatal("delegate did not mint a RunAttempt before Create")
	}

	// A sweep that races the blocked external Create must observe the exact
	// per-attempt launch boundary as busy. Terminalizing first and allowing
	// Create to return later would permit side effects after a terminal record.
	sweep, sweepErr := runattempt.Open(s.home).SweepStale(time.Second, delegateNow.Add(time.Hour))

	// v0.1 work-admission contract serializes every work ID through the
	// compound-home mutation guard for the entire start-callback duration.
	// While onCreate holds the guard, an unrelated Mint+MarkRunning would
	// convoy behind it. Release the Create callback to free the home
	// mutation guard, then verify the unrelated attempt progresses normally.
	// The sweep-during-Create invariant (lines above) already proved the
	// per-attempt launch boundary is observed.
	close(releaseCreate)
	progressDone := make(chan error, 1)
	go func() {
		store := runattempt.Open(s.home)
		unrelated, err := store.Mint("work_unrelated_during_delegate_create", "actor:test", "harness:test", delegateNow)
		if err == nil {
			_, err = store.MarkRunning(unrelated.ID, delegateNow.Add(time.Second))
		}
		progressDone <- err
	}()
	select {
	case err := <-progressDone:
		if err != nil {
			t.Fatalf("unrelated attempt could not progress after Create released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unrelated attempt blocked behind another delegate Create")
	}
	var outcome delegateOutcome
	select {
	case outcome = <-delegateDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Delegate did not return after Create was released")
	}

	if sweepErr != nil {
		t.Fatalf("SweepStale: %v", sweepErr)
	}
	if slices.Contains(sweep.SweptIDs, launchingID) || !slices.Contains(sweep.SkippedIDs, launchingID) {
		t.Fatalf("sweep during Create = %+v, want launching attempt %s skipped and never swept", sweep, launchingID)
	}
	if outcome.err != nil {
		t.Fatalf("Delegate after protected Create: %v", outcome.err)
	}
	if outcome.result.Status != "spawned" || outcome.result.RunAttemptID != launchingID {
		t.Fatalf("delegate result = %+v, want spawned attempt %s", outcome.result, launchingID)
	}
	got := runAttemptAt(t, runattempt.Open(s.home), launchingID)
	if got.Status != runattempt.StatusRunning || got.Launch == nil || got.Launch.CommittedAt == nil {
		t.Fatalf("delegate launch evidence = status %q launch %+v, want running+committed", got.Status, got.Launch)
	}
}

// failingLaunchCommitStore preserves the real prepared/committed launch
// transaction and then reports a post-start publication failure to Delegate.
// It exercises the definitely-owned-session cleanup path without replacing
// the RunAttempt store's launch sequencing with a fake.
type failingLaunchCommitStore struct {
	*runattempt.Store
	err error
}

func (f *failingLaunchCommitStore) CommitLaunch(id string, now time.Time, start func() error) (runattempt.RunAttempt, error) {
	ra, err := f.Store.CommitLaunch(id, now, start)
	if err != nil {
		return ra, err
	}
	return ra, f.err
}

func TestDelegateLaunchEvidenceFailureStopsOwnedSessionAndRecordsFixedRefusal(t *testing.T) {
	originalGuard := LaneCapacityGuard
	originalFactory := runAttemptsFactory
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	const privateFailure = "adapter echoed private launch argument"
	var wrapped *failingLaunchCommitStore
	runAttemptsFactory = func(home string) runAttemptStore {
		if wrapped == nil {
			wrapped = &failingLaunchCommitStore{Store: runattempt.Open(home), err: errors.New(privateFailure)}
		}
		return wrapped
	}
	defer func() {
		LaneCapacityGuard = originalGuard
		runAttemptsFactory = originalFactory
	}()

	s := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err == nil || !strings.Contains(err.Error(), privateFailure) {
		t.Fatalf("Delegate error = %v, want live post-start publication error", err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "spawn_ambiguous" || result.Refusal.Message != delegateLaunchEvidenceFailureNote {
		t.Fatalf("Delegate result = %+v, want fixed-content spawn_ambiguous refusal", result)
	}
	if len(runner.creates) != 1 || len(runner.kills) != 1 || runner.kills[0] != delegateSessionName(workID) {
		t.Fatalf("owned-session cleanup = creates=%+v kills=%+v", runner.creates, runner.kills)
	}
	if live, _ := runner.Has(delegateSessionName(workID)); live {
		t.Fatal("definitely-owned session survived launch-evidence failure")
	}
	id := assertOneNewAttemptStatus(t, s, before, runattempt.StatusFailed)
	got := runAttemptAt(t, runattempt.Open(s.home), id)
	if got.Launch == nil || got.Launch.PreparedAt == nil || got.Launch.CommittedAt == nil {
		t.Fatalf("terminal attempt lost durable launch evidence: %+v", got.Launch)
	}
	for _, event := range capturedDelegateEvents(t, s) {
		canonical, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if bytes.Contains(canonical, []byte(privateFailure)) {
			t.Fatalf("durable delegate event persisted live adapter error: %s", canonical)
		}
		if event["kind"] == "work.delegate.spawned" {
			t.Fatalf("stopped launch-evidence failure emitted a spawned binding: %+v", event)
		}
	}
}

func TestDelegateLaunchEvidenceFailurePreservesBindingWhenKillFails(t *testing.T) {
	originalGuard := LaneCapacityGuard
	originalFactory := runAttemptsFactory
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	var wrapped *failingLaunchCommitStore
	runAttemptsFactory = func(home string) runAttemptStore {
		if wrapped == nil {
			wrapped = &failingLaunchCommitStore{Store: runattempt.Open(home), err: errors.New("injected launch evidence failure")}
		}
		return wrapped
	}
	defer func() {
		LaneCapacityGuard = originalGuard
		runAttemptsFactory = originalFactory
	}()

	s := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	runner.killErr = errors.New("injected kill failure")
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err == nil || !strings.Contains(err.Error(), "launch evidence failure") || !strings.Contains(err.Error(), "kill failure") {
		t.Fatalf("Delegate error = %v, want launch-publication and kill failures", err)
	}
	if result.Status != "spawned" || result.Binding == nil {
		t.Fatalf("live session was not returned as a preserved binding: %+v", result)
	}
	if live, _ := runner.Has(delegateSessionName(workID)); !live {
		t.Fatal("failed cleanup silently lost the still-live session")
	}
	id := assertOneNewAttemptStatus(t, s, before, runattempt.StatusRunning)
	got := runAttemptAt(t, runattempt.Open(s.home), id)
	if got.Launch == nil || got.Launch.PreparedAt == nil || got.Launch.CommittedAt == nil {
		t.Fatalf("live attempt launch evidence = %+v, want prepared+committed", got.Launch)
	}
	linked, getErr := s.Get(workID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if !slices.Contains(linked.RunAttemptIDs, id) {
		t.Fatalf("preserved live attempt %s is absent from work run_attempt_ids: %v", id, linked.RunAttemptIDs)
	}
	binding, alive, bindingErr := s.latestDelegateBinding(workID)
	if bindingErr != nil || !alive || binding == nil || binding.RunAttemptID != id {
		t.Fatalf("preserved binding = %+v alive=%v err=%v, want attempt %s", binding, alive, bindingErr, id)
	}
}

func TestDelegateLaunchEvidenceFailureAdvanceFailureStillSurfacesLiveBinding(t *testing.T) {
	originalGuard := LaneCapacityGuard
	originalFactory := runAttemptsFactory
	originalAdvanceHook := advanceBeforeWorkCommit
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	var wrapped *failingLaunchCommitStore
	runAttemptsFactory = func(home string) runAttemptStore {
		if wrapped == nil {
			wrapped = &failingLaunchCommitStore{Store: runattempt.Open(home), err: errors.New("injected launch evidence failure")}
		}
		return wrapped
	}
	defer func() {
		LaneCapacityGuard = originalGuard
		runAttemptsFactory = originalFactory
		advanceBeforeWorkCommit = originalAdvanceHook
	}()

	s := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	runner.killErr = errors.New("injected kill failure")
	advanceBeforeWorkCommit = func() {
		_ = os.Remove(s.advancePendingPath(workID))
	}
	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err == nil || !strings.Contains(err.Error(), "launch evidence failure") || !strings.Contains(err.Error(), "kill failure") {
		t.Fatalf("Delegate error = %v, want launch-publication, kill, and forward-link failures", err)
	}
	if result.Status != "spawned" || result.Binding == nil {
		t.Fatalf("live session was hidden after forward-link failure: %+v", result)
	}
	if live, _ := runner.Has(delegateSessionName(workID)); !live {
		t.Fatal("failed cleanup silently lost the still-live session")
	}
	id := assertOneNewAttemptStatus(t, s, before, runattempt.StatusRunning)
	if result.Binding.RunAttemptID != id {
		t.Fatalf("surfaced binding attempt = %q, want %q", result.Binding.RunAttemptID, id)
	}
	binding, alive, bindingErr := s.latestDelegateBinding(workID)
	if bindingErr != nil || !alive || binding == nil || binding.RunAttemptID != id {
		t.Fatalf("durable binding = %+v alive=%v err=%v, want attempt %s", binding, alive, bindingErr, id)
	}
}

// failingCommandStore fails SetCommand, while letting SetOrigin/SetWorkspace
// succeed once. This exercises the stamp-failure path where some audit context
// has already landed, proves that already-stamped origin/workspace survive
// rather than being tautologically guaranteed by a wrapper that never let them
// succeed, and records the best-effort setter failures in the terminal reason.
type failingCommandStore struct {
	*runattempt.Store
	mintedID string
}

func (f *failingCommandStore) Mint(workID, actor, harness string, now time.Time) (runattempt.RunAttempt, error) {
	ra, err := f.Store.Mint(workID, actor, harness, now)
	if err == nil {
		f.mintedID = ra.ID
	}
	return ra, err
}

func (f *failingCommandStore) SetCommand(_, _ string) (runattempt.RunAttempt, error) {
	return runattempt.RunAttempt{}, errors.New("injected command stamp failure")
}

// TestDelegateStampFailureRetainsAuditContextBeforeTerminalizing proves that a
// stamp failure after Mint/SetOrigin/SetWorkspace still leaves the RunAttempt
// with the context that already succeeded (origin and workspace), and records
// a SweepReason naming the stamp failure. The old code terminalized with
// Complete (no reason), so the persisted record gave an operator no hint why
// the attempt died.
func TestDelegateStampFailureRetainsAuditContextBeforeTerminalizing(t *testing.T) {
	origGuard := LaneCapacityGuard
	origResolver := DelegateHarnessResolver
	origFactory := runAttemptsFactory
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	const resolvedCommand = "resolved-claude-zai"
	DelegateHarnessResolver = func(req DelegateHarnessResolveRequest) (DelegateHarnessResolution, error) {
		return DelegateHarnessResolution{
			Harness:                    "claude-zai",
			Command:                    resolvedCommand,
			LaneID:                     "claude-zai",
			RequestedModelID:           "claude-zai",
			RequestedProviderRoute:     "claude-zai",
			RequestedProviderRouteKind: "provider_class",
			CapacityLane:               "claude-zai",
			TupleBacked:                true,
		}, nil
	}
	wrapper := &failingCommandStore{Store: nil}
	runAttemptsFactory = func(home string) runAttemptStore {
		if wrapper.Store == nil {
			wrapper.Store = runattempt.Open(home)
		}
		return wrapper
	}
	defer func() {
		LaneCapacityGuard = origGuard
		DelegateHarnessResolver = origResolver
		runAttemptsFactory = origFactory
	}()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	workspace := t.TempDir()
	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Actor: "orchestrator:test",
		Command: "claude-zai", Workspace: workspace, Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "mint_failed" {
		t.Fatalf("result = %+v, want mint_failed refusal", result)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("stamp failure must not reach runner.Create: %+v", runner.creates)
	}
	if wrapper.mintedID == "" {
		t.Fatal("failingCommandStore did not observe a Mint call")
	}

	got := runAttemptAt(t, runattempt.Open(s.home), wrapper.mintedID)
	if got.Status != runattempt.StatusFailed {
		t.Fatalf("RunAttempt status = %q, want failed", got.Status)
	}
	if got.Origin != runattempt.OriginHarnessExecution {
		t.Fatalf("RunAttempt origin = %q, want %q", got.Origin, runattempt.OriginHarnessExecution)
	}
	if got.Workspace != workspace {
		t.Fatalf("RunAttempt workspace = %q, want %q", got.Workspace, workspace)
	}
	if got.Command != "" {
		t.Fatalf("RunAttempt command = %q, want empty because the stamp setter failed", got.Command)
	}
	if !strings.Contains(got.SweepReason, "command stamp failed") {
		t.Fatalf("RunAttempt.SweepReason = %q, want it to name the command stamp failure", got.SweepReason)
	}
	body, rerr := os.ReadFile(filepath.Join(s.home, "run_attempts", wrapper.mintedID+".json"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(body), "command stamp failed") {
		t.Fatalf("persisted RunAttempt JSON must record the stamp failure reason; got:\n%s", body)
	}
}

func TestDelegateAmbiguousPartialCreateNeverKillsOrBinds(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	runner.partialCreate = true
	runner.createErr = errors.New("partial create")
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "spawn_ambiguous" {
		t.Fatalf("result = %+v, want spawn_ambiguous", result)
	}
	if len(runner.kills) != 0 {
		t.Fatalf("ambiguous partial create killed a possibly foreign session: %v", runner.kills)
	}
	assertOneNewAttemptStatus(t, s, before, runattempt.StatusFailed)
	for _, event := range capturedDelegateEvents(t, s) {
		if event["kind"] == "work.delegate.spawned" {
			t.Fatalf("ambiguous partial create emitted a binding: %+v", event)
		}
	}
}

func TestDelegateCreateProbeUncertaintyIsAmbiguousAndNeverKills(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	runner.createErr = errors.New("create failed")
	runner.statusErr = errors.New("probe transport failed")
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "spawn_ambiguous" || result.Refusal.Message != delegateProbeUnknownNote {
		t.Fatalf("result = %+v, want probe-uncertain spawn_ambiguous", result)
	}
	if len(runner.kills) != 0 {
		t.Fatalf("probe uncertainty killed a possibly foreign session: %v", runner.kills)
	}
	assertOneNewAttemptStatus(t, s, before, runattempt.StatusFailed)
}

func TestDelegateDoesNotPersistPromptFromSpawnFailure(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	const privatePrompt = "THE-RUNNER-ECHOED-MY-CERULEAN-PROMPT"
	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	runner.createErr = errors.New("failed to spawn args: " + privatePrompt)
	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Args: []string{privatePrompt}, Workspace: t.TempDir(), Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if result.Refusal == nil || result.Refusal.Reason != "spawn_failed" {
		t.Fatalf("result = %+v, want spawn_failed refusal", result)
	}
	for _, event := range capturedDelegateEvents(t, s) {
		canonical, _ := json.Marshal(event)
		if strings.Contains(string(canonical), privatePrompt) {
			t.Fatalf("spawn failure event leaked prompt argument: %s", canonical)
		}
	}
}

func TestDelegateCorruptAttachedBindingFailsClosed(t *testing.T) {
	t.Parallel()
	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	w, err := s.Get(wID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if err := s.appendEvent(Event{
		TS: ts(fixedNow.Add(1)), WorkID: wID, From: w.State, To: w.State,
		By: "orchestrator:test", Attached: []string{"wde_missing_binding"},
	}); err != nil {
		t.Fatalf("append corrupt binding reference: %v", err)
	}

	_, err = s.Attach(AttachOptions{WorkID: wID}, newFakeRunner())
	if err == nil || !strings.Contains(err.Error(), "wde_missing_binding") {
		t.Fatalf("corrupt attached binding must fail closed with its id, got %v", err)
	}
}

func TestDelegateMalformedAttachedBindingFailsClosed(t *testing.T) {
	t.Parallel()
	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	w, err := s.Get(wID)
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	const eventID = "wde_malformed_binding"
	if err := s.writeEventJSON(eventID, delegateEvent{
		SchemaVersion: delegateEventSchemaVersion,
		ID:            eventID,
		Kind:          "work.delegate.spawned",
		TS:            ts(fixedNow),
		WorkID:        wID,
		Harness:       "claude-zai",
		// Session is deliberately absent: this record cannot recover a binding.
	}); err != nil {
		t.Fatalf("write malformed binding: %v", err)
	}
	if err := s.appendEvent(Event{
		TS: ts(fixedNow.Add(1)), WorkID: wID, From: w.State, To: w.State,
		By: "orchestrator:test", Attached: []string{eventID},
	}); err != nil {
		t.Fatalf("append malformed binding reference: %v", err)
	}

	_, err = s.Attach(AttachOptions{WorkID: wID}, newFakeRunner())
	if err == nil || !strings.Contains(err.Error(), eventID) {
		t.Fatalf("malformed attached binding must fail closed with its id, got %v", err)
	}
}

func TestDelegateObservedExitIsAttachedToLifecycle(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	first, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("first delegate: %v", err)
	}
	if err := runner.Kill(first.Binding.Session); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if _, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(1),
	}, runner); err != nil {
		t.Fatalf("delegate after exit: %v", err)
	}
	events := capturedDelegateEvents(t, s)
	exitedID := firstEventID(events, "work.delegate.exited")
	life, err := s.eventsFor(wID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if exitedID == "" || !lifeHasAttached(life, exitedID) {
		t.Fatalf("observed exit %q is not attached to lifecycle: %+v", exitedID, life)
	}
}

func TestSteerDeadSessionRecordsExitAndAttachRefuses(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	// remain-on-exit keeps the session observable while the pane is dead.
	// Steer must route this through DelegateSweep: exact terminalization,
	// one exited event, then removal of the finished pane.
	runner.panes = map[string]SessionStatus{
		spawned.Binding.Session: {Exists: true, PaneDead: true, ExitCode: 1},
	}
	if _, err := s.Steer(SteerOptions{
		WorkID: wID, Text: "new direction", Now: fixedNow.Add(1),
	}, runner); err == nil {
		t.Fatal("steer on dead session must refuse")
	}
	events := capturedDelegateEvents(t, s)
	exitedID := firstEventID(events, "work.delegate.exited")
	life, err := s.eventsFor(wID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if exitedID == "" || !lifeHasAttached(life, exitedID) {
		t.Fatalf("dead steer exit %q is not attached to lifecycle: %+v", exitedID, life)
	}
	if len(runner.kills) != 1 || runner.kills[0] != spawned.Binding.Session {
		t.Fatalf("dead steer did not remove finished pane: %v", runner.kills)
	}
	if got := runAttemptAt(t, runattempt.Open(s.home), spawned.RunAttemptID); got.Status != runattempt.StatusFailed {
		t.Fatalf("dead steer left exact attempt %s", got.Status)
	}
	if _, err := s.Attach(AttachOptions{WorkID: wID}, runner); err == nil {
		t.Fatal("attach must refuse a dead session")
	}
}

func TestDelegateRejectsActorDifferentFromLeaseHolder(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:owner", "", "")
	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Actor: "executor:other",
		Command: "claude-zai", Workspace: t.TempDir(), Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Refusal == nil || res.Refusal.Reason != "lease_held" {
		t.Fatalf("actor outside lease must be refused{lease_held}, got %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("actor outside lease spawned: %+v", runner.creates)
	}
}

func TestDelegateLiveDuplicateRefusalIsAttached(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	if _, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow,
	}, runner); err != nil {
		t.Fatalf("first delegate: %v", err)
	}
	second, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(1),
	}, runner)
	if err != nil {
		t.Fatalf("second delegate: %v", err)
	}
	if second.Refusal == nil || second.Refusal.Reason != "session_live" || second.EventID == "" {
		t.Fatalf("live duplicate = %+v, want attached session_live refusal", second)
	}
	life, err := s.eventsFor(wID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if !lifeHasAttached(life, second.EventID) {
		t.Fatalf("session_live refusal %s is not attached: %+v", second.EventID, life)
	}
}

func TestDelegateKillsSpawnWhenLifecycleAttachmentFails(t *testing.T) {
	origGuard := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = origGuard }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	origWrite := writeWorkEvent
	writeWorkEvent = func(_ *os.File, _ []byte) (int, error) {
		return 0, errors.New("injected lifecycle append failure")
	}
	defer func() { writeWorkEvent = origWrite }()

	_, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow,
	}, runner)
	if err == nil || !strings.Contains(err.Error(), "injected lifecycle append failure") {
		t.Fatalf("delegate error = %v, want injected append failure", err)
	}
	if len(runner.creates) != 1 || len(runner.kills) != 1 || runner.kills[0] != delegateSessionName(wID) {
		t.Fatalf("unrecorded spawn was not killed: creates=%+v kills=%+v", runner.creates, runner.kills)
	}
	if live, _ := runner.Has(delegateSessionName(wID)); live {
		t.Fatalf("unrecorded session %s remains live", delegateSessionName(wID))
	}
	assertOneNewAttemptStatus(t, s, before, runattempt.StatusFailed)
}

func TestDelegatePostCreateCleanupReportsKillFailure(t *testing.T) {
	origGuard := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = origGuard }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	runner.killErr = errors.New("injected kill failure")
	origWrite := writeWorkEvent
	writeWorkEvent = func(_ *os.File, _ []byte) (int, error) {
		return 0, errors.New("injected lifecycle append failure")
	}
	defer func() { writeWorkEvent = origWrite }()

	result, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err == nil || !strings.Contains(err.Error(), "injected lifecycle append failure") || !strings.Contains(err.Error(), "injected kill failure") {
		t.Fatalf("cleanup hid a failure: %v", err)
	}
	if result.Status != "spawned" || result.Binding == nil {
		t.Fatalf("cleanup hid the definitely-live session: %+v", result)
	}
}

func TestDelegateAdvanceFailureFinalizesAttachedBinding(t *testing.T) {
	origGuard := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = origGuard }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	origHook := advanceBeforeWorkCommit
	advanceBeforeWorkCommit = func() {
		_ = os.Remove(s.advancePendingPath(wID))
	}
	defer func() { advanceBeforeWorkCommit = origHook }()

	_, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err == nil {
		t.Fatal("injected Advance commit failure must surface")
	}
	delegateAttemptID := assertOneNewAttemptStatus(t, s, before, runattempt.StatusFailed)
	if len(runner.kills) != 1 {
		t.Fatalf("Advance failure did not kill definitely-owned session: %v", runner.kills)
	}
	var finalized map[string]any
	for _, event := range capturedDelegateEvents(t, s) {
		if event["kind"] == "work.delegate.exited" && event["reason"] == "spawn_finalize_failure" {
			finalized = event
		}
	}
	if finalized == nil || finalized["run_attempt_id"] != delegateAttemptID {
		t.Fatalf("Advance failure missing exact spawn-finalize exit: %+v", finalized)
	}
	if binding, _, recoverErr := s.recoverBoundSession(wID); recoverErr != nil || binding != nil {
		t.Fatalf("spawn-finalize exit left active binding=%+v err=%v", binding, recoverErr)
	}
}

func TestDelegateAdvanceFailureKillFailureSurfacesForwardLinkAmbiguity(t *testing.T) {
	originalGuard := LaneCapacityGuard
	originalAdvanceHook := advanceBeforeWorkCommit
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() {
		LaneCapacityGuard = originalGuard
		advanceBeforeWorkCommit = originalAdvanceHook
	}()

	s := newExperimentalDelegateStore(t)
	workID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	before := runAttemptsByID(t, s)
	runner := newFakeRunner()
	runner.killErr = errors.New("injected kill failure")
	advanceCalls := 0
	advanceBeforeWorkCommit = func() {
		advanceCalls++
		if advanceCalls == 1 {
			_ = os.Remove(s.advancePendingPath(workID))
		}
	}

	result, err := s.Delegate(DelegateOptions{
		WorkID: workID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err == nil || !strings.Contains(err.Error(), "kill failure") || !strings.Contains(err.Error(), "forward-link preserved live run attempt") {
		t.Fatalf("Delegate error = %v, want initial Advance, kill, and forward-link recovery failures", err)
	}
	if result.Status != "spawned" || result.Binding == nil {
		t.Fatalf("live session was hidden after cleanup failure: %+v", result)
	}
	if live, _ := runner.Has(delegateSessionName(workID)); !live {
		t.Fatal("failed cleanup silently lost the still-live session")
	}
	id := assertOneNewAttemptStatus(t, s, before, runattempt.StatusRunning)
	linked, getErr := s.Get(workID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if advanceCalls != 1 || slices.Contains(linked.RunAttemptIDs, id) {
		t.Fatalf("authority-losing Advance must preserve the old work generation: calls=%d ids=%v attempt=%s", advanceCalls, linked.RunAttemptIDs, id)
	}
	binding, alive, bindingErr := s.latestDelegateBinding(workID)
	if bindingErr != nil || !alive || binding == nil || binding.RunAttemptID != id {
		t.Fatalf("preserved binding = %+v alive=%v err=%v, want attempt %s", binding, alive, bindingErr, id)
	}
}

// Naming invariant: session name is deterministic and scoped to the work id.
func TestDelegateSessionNameDeterministic(t *testing.T) {
	t.Parallel()
	got := delegateSessionName("work_TEST")
	want := "compound-work_TEST"
	if got != want {
		t.Fatalf("delegateSessionName = %q, want %q", got, want)
	}
}

func TestTmuxRunnerAttachArgv(t *testing.T) {
	t.Parallel()
	runner := NewTmuxRunner()
	want := []string{"tmux", "attach-session", "-t", "compound-work_TEST"}
	got := runner.AttachArgv("compound-work_TEST")
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("attach argv = %q, want %q", got, want)
	}
}

func TestTmuxRunnerRejectsIncompleteSpawnWithoutTouchingTmux(t *testing.T) {
	t.Parallel()
	runner := NewTmuxRunner()
	cases := []struct {
		name, session, dir, command string
	}{
		{name: "session", dir: t.TempDir(), command: "claude-zai"},
		{name: "workspace", session: "compound-work_TEST", command: "claude-zai"},
		{name: "command", session: "compound-work_TEST", dir: t.TempDir()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := runner.Create(tc.session, tc.dir, tc.command, nil); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("Create error = %v, want missing %s", err, tc.name)
			}
		})
	}
}

// work not claimed refuses (we mirror land-lease: the holder must already
// exist before delegate can spawn on their behalf).
func TestDelegateWorkNotClaimedRefuses(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	w, err := s.Create(CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "ready delegate refuses",
		State:            "ready",
	}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: w.ID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "refused" || res.Refusal == nil || res.Refusal.Reason != "not_claimed" {
		t.Fatalf("expected refused{not_claimed}, got %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("not_claimed must not spawn")
	}
}

// --- small helpers ---

func lifeHasAttached(events []Event, id string) bool {
	if id == "" {
		return false
	}
	for _, e := range events {
		for _, a := range e.Attached {
			if a == id {
				return true
			}
		}
	}
	return false
}

// red-first falsifier: DelegateSweep on a LIVE pane must heartbeat
// the exact ratt and return without writing ANY event, terminalizing the
// attempt, or killing. A regression that maps Exists&&!PaneDead to
// abandoned (the default branch) would emit work.delegate.exited and
// mark the run attempt failed -- not what the operator wants for a
// healthy in-flight session.
func TestDelegateSweepLivePaneHeartbeatsAndProtects(t *testing.T) {
	t.Parallel()
	s := newExperimentalDelegateStore(t)
	writeContextPack(t, s, "ctx_sweep_live")
	w, err := s.Create(CreateInput{
		Owner: "memo", SuccessPredicate: "ok",
		ContextPackID: "ctx_sweep_live", DesiredOutcome: "alive",
	}, fixedNow)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Pre-mint the attempt and bring the binding into the eventlog.
	holder := w.ID + "/memo"
	raStore := runattempt.Open(s.home)
	minted, err := raStore.Mint(w.ID, holder, "claude-zai", fixedNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := raStore.MarkRunning(minted.ID, fixedNow); err != nil {
		t.Fatalf("markRunning: %v", err)
	}
	binding := &DelegateBinding{
		WorkID:       w.ID,
		Session:      "sweep-live-sess",
		Harness:      "claude-zai",
		Lane:         "memo",
		Holder:       holder,
		RunAttemptID: minted.ID,
	}
	spawnedID, err := s.recordDelegateSpawned(w.ID, binding, true, minted.ID, fixedNow)
	if err != nil {
		t.Fatalf("recordSpawned: %v", err)
	}
	// Append the lifecycle record so the binding is recoverable.
	if err := s.appendEvent(Event{
		TS: ts(fixedNow), WorkID: w.ID, From: w.State, To: w.State,
		By: holder, Note: "delegate spawned", Attached: []string{spawnedID},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// LIVE pane observation: ExitCode=-1, PaneDead=false, Exists=true.
	liveObs := SessionStatus{Exists: true, ExitCode: -1}
	runner := newDelegateFakeRunner()
	runner.live[binding.Session] = true
	sweepT := fixedNow.Add(31 * time.Minute)
	before := len(lifeEvents(t, s, w.ID))
	beforeHb := runAttemptAt(t, raStore, minted.ID).LastHeartbeatAt
	_ = beforeHb

	out, err := s.DelegateSweep(binding, liveObs, runner, holder, sweepT)
	if err != nil {
		t.Fatalf("DelegateSweep live: %v", err)
	}
	// runattempt.Store caches reads per-instance. The production code
	// opens a fresh store from s.home every call, so the test must do
	// the same to observe post-Heartbeat state without stale cache.
	after := runAttemptAt(t, runattempt.Open(s.home), minted.ID)
	if out.ObservedExit != "live" || out.TerminalStatus != "live" {
		t.Fatalf("live outcome wrong: %+v", out)
	}
	// ZERO new lifecycle events, ZERO writes to disk for exit.
	if got := len(lifeEvents(t, s, w.ID)); got != before {
		t.Fatalf("live sweep appended a lifecycle event (forbidden): %d -> %d", before, got)
	}
	// ZERO kills.
	if len(runner.kills) != 0 {
		t.Fatalf("live sweep issued kill (forbidden): %v", runner.kills)
	}
	// The exact ratt is heartbeated and STILL pending/running -- not
	// terminalized.
	if after.LastHeartbeatAt == nil {
		t.Fatalf("live sweep did not heartbeat exact ratt: %+v", after)
	}
	if after.Status == runattempt.StatusSucceeded || after.Status == runattempt.StatusFailed || after.Status == runattempt.StatusAbandoned {
		t.Fatalf("live sweep terminalized the attempt (forbidden): %s", after.Status)
	}
}

// red-first falsifier: DelegateSweep on a DEAD pane + observed exit 1
// must terminalize the exact ratt as failed AND kill the session.
func TestDelegateSweepDeadPaneExitOneTerminalizesFailed(t *testing.T) {
	t.Parallel()
	s := newExperimentalDelegateStore(t)
	writeContextPack(t, s, "ctx_sweep_fail")
	w, err := s.Create(CreateInput{Owner: "memo", SuccessPredicate: "ok", ContextPackID: "ctx_sweep_fail", DesiredOutcome: "alive"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	holder := w.ID + "/memo"
	raStore := runattempt.Open(s.home)
	minted, err := raStore.Mint(w.ID, holder, "claude-zai", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raStore.MarkRunning(minted.ID, fixedNow); err != nil {
		t.Fatal(err)
	}
	binding := &DelegateBinding{WorkID: w.ID, Session: "sweep-fail-sess", Harness: "claude-zai", Lane: "memo", Holder: holder, RunAttemptID: minted.ID}
	if _, err := s.recordDelegateSpawned(w.ID, binding, true, minted.ID, fixedNow); err != nil {
		t.Fatal(err)
	}

	runner := newDelegateFakeRunner()
	runner.panes = map[string]SessionStatus{
		binding.Session: {Exists: true, PaneDead: true, ExitCode: 1},
	}
	out, err := s.DelegateSweep(binding, runner.panes[binding.Session], runner, holder, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if out.ObservedExit != "failed" || out.TerminalStatus != runattempt.StatusFailed {
		t.Fatalf("wrong outcome: %+v", out)
	}
	if len(runner.kills) != 1 || runner.kills[0] != binding.Session {
		t.Fatalf("sweep did not kill finished pane: %v", runner.kills)
	}
	after := runAttemptAt(t, runattempt.Open(s.home), minted.ID)
	if after.Status != runattempt.StatusFailed {
		t.Fatalf("exact ratt not terminalized as failed: %s", after.Status)
	}
	// Ensure no -1 leaked into the exited event (signal-only OR
	// exit-only); exit 1 must serialize 1.
	events := readDelegateEvents(t, s)
	for _, ev := range events {
		if ev["kind"] != "work.delegate.exited" {
			continue
		}
		if v, ok := ev["exit_code"].(float64); ok && int(v) == -1 {
			t.Fatalf("exited event serialized -1: %+v", ev)
		}
	}
}

// red-first falsifier: signal-only exit must NOT serialize ExitCode=-1
// in the work.delegate.exited event.
func TestDelegateExitedSignalOnlyOmitsExitCode(t *testing.T) {
	t.Parallel()
	s := newExperimentalDelegateStore(t)
	writeContextPack(t, s, "ctx_signal")
	w, err := s.Create(CreateInput{Owner: "memo", SuccessPredicate: "ok", ContextPackID: "ctx_signal", DesiredOutcome: "alive"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	binding := &DelegateBinding{WorkID: w.ID, Session: "sig-sess", Harness: "claude-zai", Lane: "memo", Holder: w.ID + "/memo", RunAttemptID: ""}
	// Signal-only ExitCode: -1, signal "TERM".
	obs := SessionStatus{Exists: true, PaneDead: true, ExitCode: -1, Signal: "TERM"}
	evID, err := s.recordDelegateExited(w.ID, binding, &obs, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	body, err := jsonFile(t, s, evID)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := body["exit_code"]; has {
		t.Fatalf("signal-only exited event leaked exit_code: %+v", body)
	}
	if sig, _ := body["signal"].(string); sig != "TERM" {
		t.Fatalf("signal field missing/wrong: %+v", body)
	}
	if obsFl, _ := body["observed_exit"].(bool); !obsFl {
		t.Fatalf("observed_exit must be true: %+v", body)
	}
}

// red-first falsifier: backward-compat recovery of an OLDER spawned
// event without RunAttemptID must still return the binding (terminalize
// step is silently skipped, but exited event is recorded).
func TestDelegateRecoverBoundSessionOlderRunsBackCompat(t *testing.T) {
	t.Parallel()
	s := newExperimentalDelegateStore(t)
	writeContextPack(t, s, "ctx_legacy")
	w, err := s.Create(CreateInput{Owner: "memo", SuccessPredicate: "ok", ContextPackID: "ctx_legacy", DesiredOutcome: "alive"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	binding := &DelegateBinding{WorkID: w.ID, Session: "legacy-sess", Harness: "claude-zai", Lane: "memo", Holder: w.ID + "/memo"}
	// Spawned event WITHOUT RunAttemptID (pre-slice shape).
	legacyID := delegateEventID("work.delegate.spawned", w.ID, fixedNow)
	if err := s.writeEventJSON(legacyID, delegateEvent{
		SchemaVersion: delegateEventSchemaVersion,
		ID:            legacyID,
		Kind:          "work.delegate.spawned",
		TS:            ts(fixedNow),
		WorkID:        w.ID,
		Harness:       "claude-zai",
		Lane:          "memo",
		Holder:        binding.Holder,
		Session:       binding.Session,
	}); err != nil {
		t.Fatal(err)
	}
	// recoverBoundSession reads lifecycle events with `Attached` refs and
	// then loads each attached wde_* by id. Attach the legacy id.
	if err := s.appendEvent(Event{
		TS: ts(fixedNow), WorkID: w.ID, From: w.State, To: w.State,
		By: binding.Holder, Note: "legacy spawned", Attached: []string{legacyID},
	}); err != nil {
		t.Fatal(err)
	}
	rec, _, err := s.recoverBoundSession(w.ID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if rec == nil || rec.Session != binding.Session || rec.RunAttemptID != "" {
		t.Fatalf("legacy recovery wrong: %+v", rec)
	}
}

// red-first falsifier: probe uncertainty (UnknownReason) must NOT map
// to any terminal status; it surfaces as an error.
func TestDelegateSweepProbeUncertaintyReturnsError(t *testing.T) {
	t.Parallel()
	s := newExperimentalDelegateStore(t)
	writeContextPack(t, s, "ctx_unk")
	w, err := s.Create(CreateInput{Owner: "memo", SuccessPredicate: "ok", ContextPackID: "ctx_unk", DesiredOutcome: "alive"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	binding := &DelegateBinding{WorkID: w.ID, Session: "unk-sess", Harness: "claude-zai", Lane: "memo", Holder: w.ID + "/memo", RunAttemptID: ""}
	obs := SessionStatus{Exists: false, UnknownReason: "tmux list-panes: fork/exec error"}
	runner := newDelegateFakeRunner()
	_, err = s.DelegateSweep(binding, obs, runner, "operator", fixedNow.Add(time.Minute))
	if err == nil {
		t.Fatal("probe uncertainty must error")
	}
	if !strings.Contains(err.Error(), "probe uncertainty") {
		t.Fatalf("wrong error: %v", err)
	}
	if len(runner.kills) != 0 {
		t.Fatalf("uncertainty must not kill: %v", runner.kills)
	}
}

func TestDelegateDeadRemainedPaneAllowsRedelegate(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	first, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-minimax", Command: "claude-minimax",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	runner.panes = map[string]SessionStatus{
		first.Binding.Session: {Exists: true, PaneDead: true, ExitCode: 0},
	}
	second, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-minimax", Command: "claude-minimax",
		Workspace: t.TempDir(), Now: fixedNow.Add(2 * time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "spawned" || len(runner.creates) != 2 || len(runner.kills) != 1 {
		t.Fatalf("re-delegate result=%+v creates=%d kills=%v", second, len(runner.creates), runner.kills)
	}
	if got := runAttemptAt(t, runattempt.Open(s.home), first.RunAttemptID); got.Status != runattempt.StatusSucceeded {
		t.Fatalf("first attempt = %s, want succeeded", got.Status)
	}
	if got := runAttemptAt(t, runattempt.Open(s.home), second.RunAttemptID); got.Status != runattempt.StatusRunning {
		t.Fatalf("second attempt = %s, want running", got.Status)
	}
}

func TestDelegateAttachDeadPaneIsReadOnly(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-qwen", Command: "claude-qwen",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	runner.panes = map[string]SessionStatus{
		spawned.Binding.Session: {Exists: true, PaneDead: true, ExitCode: 0},
	}
	beforeLife := len(lifeEvents(t, s, wID))
	beforeEvents := len(capturedDelegateEvents(t, s))
	if _, err := s.Attach(AttachOptions{WorkID: wID}, runner); err == nil {
		t.Fatal("attach must refuse a dead remained pane")
	}
	if len(lifeEvents(t, s, wID)) != beforeLife || len(capturedDelegateEvents(t, s)) != beforeEvents || len(runner.kills) != 0 {
		t.Fatalf("attach mutated dead session: life=%d->%d events=%d->%d kills=%v", beforeLife, len(lifeEvents(t, s, wID)), beforeEvents, len(capturedDelegateEvents(t, s)), runner.kills)
	}
}

func TestDelegateSweepTerminalizesOnlyBoundAttempt(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	claimed, err := s.Get(wID)
	if err != nil || len(claimed.RunAttemptIDs) != 1 {
		t.Fatalf("claim attempt: work=%+v err=%v", claimed, err)
	}
	siblingID := claimed.RunAttemptIDs[0]
	runner := newFakeRunner()
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	siblingPath := filepath.Join(s.home, "run_attempts", siblingID+".json")
	siblingBeforeSweep, err := os.ReadFile(siblingPath)
	if err != nil {
		t.Fatal(err)
	}
	obs := SessionStatus{Exists: true, PaneDead: true, ExitCode: 1}
	if _, err := s.DelegateSweep(spawned.Binding, obs, runner, "operator", fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := runAttemptAt(t, runattempt.Open(s.home), spawned.RunAttemptID); got.Status != runattempt.StatusFailed {
		t.Fatalf("bound attempt = %s, want failed", got.Status)
	}
	siblingAfterSweep, err := os.ReadFile(siblingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(siblingBeforeSweep, siblingAfterSweep) {
		t.Fatalf("sweep mutated sibling claim reservation %s", siblingID)
	}
}

func TestDelegateSweepVanishedBecomesAbandonedWithoutKill(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	delete(runner.live, spawned.Binding.Session)
	out, err := s.DelegateSweep(spawned.Binding, SessionStatus{Exists: false}, runner, "operator", fixedNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if out.TerminalStatus != runattempt.StatusAbandoned || len(runner.kills) != 0 {
		t.Fatalf("vanished outcome=%+v kills=%v", out, runner.kills)
	}
}

func TestDelegateSweepRetryDoesNotDuplicateExitedEvent(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	dead := SessionStatus{Exists: true, PaneDead: true, ExitCode: 0}
	if _, err := s.DelegateSweep(spawned.Binding, dead, runner, "operator", fixedNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	before := countDelegateEventKind(t, s, "work.delegate.exited")
	recovered, err := s.DelegateSweepResolve(wID)
	if err != nil || recovered == nil {
		t.Fatalf("resolve finalized binding: %+v %v", recovered, err)
	}
	if _, err := s.DelegateSweep(recovered, SessionStatus{Exists: false}, runner, "operator", fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if after := countDelegateEventKind(t, s, "work.delegate.exited"); after != before {
		t.Fatalf("retry duplicated exited event: %d -> %d", before, after)
	}
}

func TestDelegateSweepReportsFinishedPaneKillFailure(t *testing.T) {
	orig := LaneCapacityGuard
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	defer func() { LaneCapacityGuard = orig }()

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "orchestrator:test", "", "")
	runner := newFakeRunner()
	spawned, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude-zai", Command: "claude-zai",
		Workspace: t.TempDir(), Now: fixedNow.Add(time.Minute),
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	runner.killErr = errors.New("kill transport failed")
	_, err = s.DelegateSweep(spawned.Binding, SessionStatus{Exists: true, PaneDead: true, ExitCode: 1}, runner, "operator", fixedNow.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "kill transport failed") {
		t.Fatalf("kill failure hidden: %v", err)
	}
	if countDelegateEventKind(t, s, "work.delegate.exited") != 1 {
		t.Fatal("kill failure lost the prior exited evidence")
	}
	if got := runAttemptAt(t, runattempt.Open(s.home), spawned.RunAttemptID); got.Status != runattempt.StatusFailed {
		t.Fatalf("attempt not terminal before reported kill failure: %s", got.Status)
	}
}

func newDelegateFakeRunner() *delegateFakeRunner {
	return &delegateFakeRunner{live: map[string]bool{}, panes: map[string]SessionStatus{}}
}

// delegateFakeRunner is a SessionRunner test double for the new
// DelegateSweep tests. It tracks kills + per-session SessionStatus
// overrides and ignores create / send-keys on purpose (the sweep path
// does not need them).
type delegateFakeRunner struct {
	mu    sync.Mutex
	live  map[string]bool
	panes map[string]SessionStatus
	kills []string
}

func (d *delegateFakeRunner) Available() error { return nil }
func (d *delegateFakeRunner) Has(name string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.live[name], nil
}
func (d *delegateFakeRunner) SessionStatus(name string) (SessionStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.panes[name]; ok {
		return p, nil
	}
	if d.live[name] {
		return SessionStatus{Exists: true, ExitCode: -1}, nil
	}
	return SessionStatus{Exists: false}, nil
}
func (d *delegateFakeRunner) Create(name, dir, command string, args []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.live[name] = true
	return nil
}
func (d *delegateFakeRunner) SendKeys(name, text string, enter bool) error { return nil }
func (d *delegateFakeRunner) Kill(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.kills = append(d.kills, name)
	delete(d.live, name)
	delete(d.panes, name)
	return nil
}
func (d *delegateFakeRunner) AttachArgv(name string) []string {
	return []string{"tmux", "attach-session", "-t", name}
}

func lifeEvents(t *testing.T, s *Store, workID string) []Event {
	t.Helper()
	evs, err := s.eventsFor(workID)
	if err != nil {
		t.Fatalf("eventsFor: %v", err)
	}
	return evs
}

func runAttemptAt(t *testing.T, raStore *runattempt.Store, id string) runattempt.RunAttempt {
	t.Helper()
	ra, err := raStore.Get(id)
	if err != nil {
		t.Fatalf("raStore.Get: %v", err)
	}
	return ra
}

func runAttemptsByID(t *testing.T, s *Store) map[string]runattempt.RunAttempt {
	t.Helper()
	all, err := runattempt.Open(s.home).List()
	if err != nil {
		t.Fatalf("list run attempts: %v", err)
	}
	out := make(map[string]runattempt.RunAttempt, len(all))
	for _, ra := range all {
		out[ra.ID] = ra
	}
	return out
}

func assertOneNewAttemptStatus(t *testing.T, s *Store, before map[string]runattempt.RunAttempt, status string) string {
	t.Helper()
	var found string
	for id, ra := range runAttemptsByID(t, s) {
		if _, existed := before[id]; existed {
			continue
		}
		if found != "" {
			t.Fatalf("more than one new attempt: %s and %s", found, id)
		}
		found = id
		if ra.Status != status {
			t.Fatalf("new attempt %s status = %s, want %s", id, ra.Status, status)
		}
	}
	if found == "" {
		t.Fatal("no new attempt found")
	}
	return found
}

func countDelegateEventKind(t *testing.T, s *Store, kind string) int {
	t.Helper()
	count := 0
	for _, event := range capturedDelegateEvents(t, s) {
		if event["kind"] == kind {
			count++
		}
	}
	return count
}

func jsonFile(t *testing.T, s *Store, id string) (map[string]any, error) {
	t.Helper()
	var m map[string]any
	if err := s.readEventJSON(id, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func readDelegateEvents(t *testing.T, s *Store) []map[string]any {
	t.Helper()
	doc, err := os.ReadFile(s.eventsLog())
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(doc) == 0 {
		return nil
	}
	var out []map[string]any
	for _, line := range bytes.Split(doc, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func firstEventID(events []map[string]any, kind string) string {
	for _, e := range events {
		if k, _ := e["kind"].(string); k == kind {
			if id, _ := e["id"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

// Ensure the binding file lands at the documented location; the verb layer
// uses this path to read the recording for steer/attach.
// REMOVED: slice 1 forbids a sidecar binding path. The binding authority is
// the newest attached work.delegate.spawned eventstore record, recovered by
// walking the work's lifecycle attachments. See recoverBoundSession in
// delegate.go. A binding-path test would re-introduce a second truth.

// TestDelegateWorkspaceResolutionFailsClosed proves the workspace is never
// inferred from COMPOUND_HOME and never falls back to the process cwd. When no
// --workspace is supplied, the work has no git_delivery.target_worktree_root,
// and cwd resolution is unavailable, Delegate returns a structured refusal with
// zero side effects (no session, no run attempt, no delegate event).
func TestDelegateWorkspaceResolutionFailsClosed(t *testing.T) {
	origGuard := LaneCapacityGuard
	origReadiness := DelegateHarnessReadiness
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() {
		LaneCapacityGuard = origGuard
		DelegateHarnessReadiness = origReadiness
	})

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	// Capture cwd behavior by temporarily replacing os.Getwd with a failure.
	origGetwd := delegateOsGetwd
	delegateOsGetwd = func() (string, error) { return "", errors.New("cwd unavailable") }
	t.Cleanup(func() { delegateOsGetwd = origGetwd })

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "refused" || res.Refusal == nil {
		t.Fatalf("expected refusal, got %+v", res)
	}
	if res.Refusal.Reason != "workspace_unresolvable" {
		t.Fatalf("reason = %q, want workspace_unresolvable", res.Refusal.Reason)
	}
	if !strings.Contains(res.Refusal.Message, "no workspace resolved") {
		t.Fatalf("message = %q, want no workspace resolved", res.Refusal.Message)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("workspace refusal created session: %+v", runner.creates)
	}
	if events := capturedDelegateEvents(t, s); len(events) != 0 {
		t.Fatalf("workspace refusal emitted delegate events: %+v", events)
	}
	if attempts := runAttemptsByID(t, s); len(attempts) != 1 {
		// Only the claim-time placeholder exists; no delegate attempt minted.
		for id, ra := range attempts {
			if ra.Origin == runattempt.OriginHarnessExecution {
				t.Fatalf("workspace refusal minted delegate attempt %s", id)
			}
		}
	}

	// Also prove the resolved workspace is never s.home itself when cwd succeeds.
	origGetwd = delegateOsGetwd
	delegateOsGetwd = func() (string, error) { return s.home, nil }
	defer func() { delegateOsGetwd = origGetwd }()
	res2, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude", Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate with cwd=s.home: %v", err)
	}
	if res2.Status != "refused" || res2.Refusal == nil || res2.Refusal.Reason != "workspace_occupied" {
		t.Fatalf("cwd=s.home must be refused as workspace_occupied, got %+v", res2)
	}
	// The "occupied" refusal proves the orchestrator resolved the workspace to
	// s.home and applied workspace ownership rules rather than silently spawning
	// inside COMPOUND_HOME. The refusal path emits a delegate event, which is fine
	// because this is a post-resolution gate refusal.
}

func TestDelegateWorkspaceIsNeverCompoundHome(t *testing.T) {
	origGuard := LaneCapacityGuard
	origReadiness := DelegateHarnessReadiness
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() {
		LaneCapacityGuard = origGuard
		DelegateHarnessReadiness = origReadiness
	})

	s := newExperimentalDelegateStore(t)
	wID := prepareClaimedWork(t, s, "codex", "", "")

	origGetwd := delegateOsGetwd
	delegateOsGetwd = func() (string, error) { return s.home, nil }
	t.Cleanup(func() { delegateOsGetwd = origGetwd })

	runner := newFakeRunner()
	res, err := s.Delegate(DelegateOptions{
		WorkID: wID, Harness: "claude", Actor: "codex", Command: "claude",
		// No --workspace, no git_delivery on the work. With cwd set to s.home the
		// fallback resolves to COMPOUND_HOME itself; that must be refused.
		Now: fixedNow,
	}, runner)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.Status != "refused" || res.Refusal == nil || res.Refusal.Reason != "workspace_occupied" {
		t.Fatalf("workspace=s.home must be refused as workspace_occupied, got %+v", res)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("workspace_occupied refusal created session: %+v", runner.creates)
	}
}

func TestDelegateRefusesFrozenCloseBeforeMint(t *testing.T) {
	originalReadiness := DelegateHarnessReadiness
	originalGuard := LaneCapacityGuard
	DelegateHarnessReadiness = func(DelegateHarnessReadinessRequest) (DelegateHarnessReadinessResult, error) {
		return DelegateHarnessReadinessResult{Ready: true}, nil
	}
	LaneCapacityGuard = fakeGuard(false, false, "ok", "ok")
	t.Cleanup(func() {
		DelegateHarnessReadiness = originalReadiness
		LaneCapacityGuard = originalGuard
	})

	s := newExperimentalDelegateStore(t)
	const holder = "orchestrator:test"
	workID := prepareClaimedWork(t, s, holder, "", "a frozen close fences delegate mint")
	frozen := freezeClosePendingAfterPrepare(t, s, workID, CloseInput{
		Outcome:  "cancelled",
		MoveType: "audit_only",
		Actor:    holder,
		Summary:  "freeze close before delegate mint",
	}, fixedNow.Add(time.Minute))
	beforeAuthority := snapshotLifecycleAuthority(t, s)
	beforeAttempts := runAttemptsByID(t, s)

	runner := newFakeRunner()
	result, err := s.Delegate(DelegateOptions{
		WorkID:    workID,
		Harness:   "claude-zai",
		Actor:     holder,
		Command:   "claude-zai",
		Workspace: t.TempDir(),
		Now:       fixedNow.Add(2 * time.Minute),
	}, runner)
	if err != nil {
		t.Fatalf("delegate over frozen close error = %v, want refused authority_changed before mint", err)
	}
	if result.Status != "refused" || result.Refusal == nil || result.Refusal.Reason != "authority_changed" {
		t.Fatalf("delegate over frozen close result = %+v, want refused authority_changed", result)
	}
	if !strings.Contains(result.Refusal.Message, "close recovery_required") ||
		!strings.Contains(result.Refusal.Message, frozen.Action.ID) {
		t.Fatalf("delegate refusal %q does not bind frozen close action %s", result.Refusal.Message, frozen.Action.ID)
	}
	if len(runner.creates) != 0 {
		t.Fatalf("delegate over frozen close created a session: %+v", runner.creates)
	}
	assertLifecycleAuthorityUnchanged(t, beforeAuthority, snapshotLifecycleAuthority(t, s))
	afterAttempts := runAttemptsByID(t, s)
	if !reflect.DeepEqual(beforeAttempts, afterAttempts) {
		t.Fatalf("delegate over frozen close changed RunAttempts\nbefore=%+v\nafter=%+v", beforeAttempts, afterAttempts)
	}
}
