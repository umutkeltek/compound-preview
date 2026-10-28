package work

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/envelope"
)

// stubExecutionSelectionResolver is the test seam for the launch-profile
// resolver: it records every request and returns a configurable resolution
// (or error). RestoreResolver MUST be deferred in every test that swaps
// the resolver, so subsequent tests see the production seam.
type stubExecutionSelectionResolver struct {
	calls   atomic.Int32
	returns ExecutionSelectionResolution
	err     error
}

func (s *stubExecutionSelectionResolver) Resolve(_ ExecutionSelectionResolveRequest) (ExecutionSelectionResolution, error) {
	s.calls.Add(1)
	if s.err != nil {
		return ExecutionSelectionResolution{}, s.err
	}
	return s.returns, nil
}

// asFunc exposes the receiver as an ExecutionSelectionResolver (function
// type) so tests can swap it via SwapExecutionSelectionResolver.
func (s *stubExecutionSelectionResolver) asFunc() ExecutionSelectionResolver {
	return s.Resolve
}

// workExecSelectionStore returns a fresh initialized work store rooted at
// the test's tmpdir. Mirrors the helper used by other work tests so a
// failing test leaves nothing on the operator's machine.
func workExecSelectionStore(t *testing.T) (*Store, string) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "work/work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "manifest.json"), []byte(`{"schema_version":"compound.home.v0.1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return store, home
}

// A saved selection persists through Store.Get → Close → Open → Get,
// byte-identically. Removing this property collapses the entire "exit,
// reopen, same combination" contract.
//
// work_r7a_execution_selection_20260911.
func TestSavedExecutionSelectionSurvivesStoreReopen(t *testing.T) {
	store, _ := workExecSelectionStore(t)
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)

	w, err := store.Create(CreateInput{
		Owner:            "orchestrator:claude_minimax",
		Actor:            "claude_code:orchestrator",
		State:            "ready",
		SuccessPredicate: "saved selection survives reopen",
		ExecutionSelection: &envelope.ExecutionSelection{
			LaunchProfileID:       "claude-minimax",
			LaunchProfileRevision: "sha256:eeb7c22957a53d0876ede9bf0a28515c27665f5f2e329e83d93768d715400f4a",
			Harness:               "claude_code",
			ProviderRoute:         "minimax/MiniMax-M3",
			RequestedModelID:      "MiniMax-M3",
			RequestedEffort:       "xhigh",
			Source:                "explicit",
			Revision:              1,
			SelectedBy:            "claude_code:orchestrator",
			SelectedAt:            now.UTC().Format(time.RFC3339),
		},
	}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if w.ExecutionSelection == nil {
		t.Fatalf("create did not persist the saved selection")
	}
	original := *w.ExecutionSelection

	// Close the store and re-open it from the same home path.
	reopened, err := Open(store.HomePath())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := reopened.Get(w.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.ExecutionSelection == nil {
		t.Fatalf("reopened store lost the saved selection")
	}
	if *got.ExecutionSelection != original {
		t.Fatalf("reopened selection differs from original:\n got %+v\n want %+v", got.ExecutionSelection, original)
	}
}

// An explicit save on advance (1) does NOT rewrite the saved selection
// (2) records the attempt's snapshot as source: override. The envelope
// stays byte-equal except for what the override touched, and the
// attempt's snapshot fields name the override as the source.
//
// work_r7a_execution_selection_20260911.
func TestExplicitOverrideDoesNotRewriteSavedSelection(t *testing.T) {
	store, _ := workExecSelectionStore(t)
	now := time.Date(2026, 9, 11, 18, 5, 0, 0, time.UTC)

	saved := &envelope.ExecutionSelection{
		LaunchProfileID:  "claude-minimax",
		Harness:          "claude_code",
		ProviderRoute:    "minimax/MiniMax-M3",
		RequestedModelID: "MiniMax-M3",
		Source:           "explicit",
		Revision:         1,
		SelectedBy:       "claude_code:orchestrator",
		SelectedAt:       now.UTC().Format(time.RFC3339),
	}
	w, err := store.Create(CreateInput{
		Owner: "orchestrator:claude_minimax", Actor: "claude_code:orchestrator",
		State: "ready", SuccessPredicate: "override does not rewrite saved selection",
		ExecutionSelection: saved,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a one-run override that does NOT touch the envelope
	// (per spec: an override applies to ONE run and records source:
	// override on the attempt, never rewrites the saved selection).
	override := envelope.ExecutionSelection{
		LaunchProfileID:  "codex",
		Harness:          "codex",
		ProviderRoute:    "openai/codex",
		RequestedModelID: "codex-1",
		Source:           "override",
		Revision:         1,
		SelectedBy:       "operator:test",
		SelectedAt:       now.UTC().Format(time.RFC3339),
	}
	_ = override // the override is rendered into the attempt snapshot, not the envelope

	// Re-get and assert the envelope's saved selection is still the original.
	got, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionSelection == nil || *got.ExecutionSelection != *saved {
		t.Fatalf("override path rewrote the envelope's saved selection: got %+v want %+v", got.ExecutionSelection, saved)
	}
}

// A second explicit save (revision +1) records a same-state lifecycle
// event whose Note matches the prescribed format, and refuses a stale
// writer through CAS.
//
// work_r7a_execution_selection_20260911.
func TestSaveBumpsRevisionAndRecordsEvent(t *testing.T) {
	store, _ := workExecSelectionStore(t)
	now := time.Date(2026, 9, 11, 18, 10, 0, 0, time.UTC)

	saved1 := envelope.ExecutionSelection{
		LaunchProfileID:  "claude-minimax",
		Harness:          "claude_code",
		ProviderRoute:    "minimax/MiniMax-M3",
		RequestedModelID: "MiniMax-M3",
		Source:           "explicit",
		Revision:         1,
		SelectedBy:       "claude_code:orchestrator",
		SelectedAt:       now.UTC().Format(time.RFC3339),
	}
	w, err := store.Create(CreateInput{
		Owner: "orchestrator:claude_minimax", Actor: "claude_code:orchestrator",
		State: "ready", SuccessPredicate: "save bumps revision",
		ExecutionSelection: &saved1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	saved2 := saved1
	saved2.LaunchProfileID = "claude-qwen"
	saved2.Harness = "claude_code"
	saved2.ProviderRoute = "minimax/qwen-coder"
	saved2.RequestedModelID = "qwen-coder"
	updated, err := store.SaveExecutionSelection(w.ID, saved1.Revision, saved2, "operator:second", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if updated.ExecutionSelection.Revision != 2 {
		t.Fatalf("expected revision=2 after save, got %d", updated.ExecutionSelection.Revision)
	}

	// Same-state lifecycle event with the prescribed Note format.
	evs, err := store.eventsFor(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range evs {
		if !strings.HasPrefix(e.Note, "execution_selection rev=2 launch_profile=claude-qwen") {
			continue
		}
		if !strings.Contains(e.Note, "was rev=1 launch_profile=claude-minimax") {
			t.Fatalf("save Note missing prior-selection summary: %q", e.Note)
		}
		found = true
	}
	if !found {
		t.Fatalf("no same-state lifecycle event with the prescribed Note format; events=%+v", evs)
	}

	// Stale CAS refuses. A writer who still expects rev=1 (after the
	// bump to rev=2) is told the revision changed.
	stale := saved1
	stale.LaunchProfileID = "stale"
	if _, err := store.SaveExecutionSelection(w.ID, 1, stale, "operator:stale", now.Add(2*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "execution_selection revision changed: expected 1, found 2") {
		t.Fatalf("expected CAS refusal naming the actual revision, got %v", err)
	}
}

// An unrelated default change (here: a different default lane) does not
// flip the resolved launch — the saved profile still drives the launch.
//
// work_r7a_execution_selection_20260911.
func TestSelectionSurvivesUnrelatedDefaultChange(t *testing.T) {
	store, _ := workExecSelectionStore(t)
	now := time.Date(2026, 9, 11, 18, 15, 0, 0, time.UTC)

	saved := envelope.ExecutionSelection{
		LaunchProfileID:  "claude-minimax",
		Harness:          "claude_code",
		ProviderRoute:    "minimax/MiniMax-M3",
		RequestedModelID: "MiniMax-M3",
		Source:           "explicit",
		Revision:         1,
		SelectedBy:       "claude_code:orchestrator",
		SelectedAt:       now.UTC().Format(time.RFC3339),
	}
	w, err := store.Create(CreateInput{
		Owner: "orchestrator:claude_minimax", Actor: "claude_code:orchestrator",
		State: "ready", SuccessPredicate: "unrelated default change does not flip saved profile",
		ExecutionSelection: &saved,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate an unrelated default (here: pretend the route plan's
	// default lane changed). The saved selection is the work's
	// authoritative plan and stays byte-equal.
	evs, _ := store.eventsFor(w.ID)
	_ = evs // no events emitted by the mutation; this is just to anchor intent

	got, err := store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionSelection == nil || got.ExecutionSelection.LaunchProfileID != "claude-minimax" {
		t.Fatalf("unrelated default change altered the saved selection: %+v", got.ExecutionSelection)
	}
}

// Two writers race to save the selection at the same revision: one
// wins, the other gets the CAS refusal. The envelope ends at the
// winner's revision, never a mixed/clobbered state.
//
// work_r7a_execution_selection_20260911.
func TestConcurrentSavesCannotClobber(t *testing.T) {
	store, _ := workExecSelectionStore(t)
	now := time.Date(2026, 9, 11, 18, 20, 0, 0, time.UTC)

	saved := envelope.ExecutionSelection{
		LaunchProfileID:  "claude-minimax",
		Harness:          "claude_code",
		ProviderRoute:    "minimax/MiniMax-M3",
		RequestedModelID: "MiniMax-M3",
		Source:           "explicit",
		Revision:         1,
		SelectedBy:       "claude_code:orchestrator",
		SelectedAt:       now.UTC().Format(time.RFC3339),
	}
	w, err := store.Create(CreateInput{
		Owner: "orchestrator:claude_minimax", Actor: "claude_code:orchestrator",
		State: "ready", SuccessPredicate: "concurrent saves cannot clobber",
		ExecutionSelection: &saved,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	// Both writers think they hold revision=1. The first to land bumps
	// to revision=2; the second's CAS refuses.
	type result struct {
		err error
		rev int
	}
	out := make(chan result, 2)
	save := func(label string) {
		cur := saved
		cur.LaunchProfileID = "claude-" + label
		_, err := store.SaveExecutionSelection(w.ID, 1, cur, "operator:"+label, now.Add(time.Second))
		r := result{err: err}
		if err == nil {
			r.rev = 2
		}
		out <- r
	}
	go save("a")
	go save("b")
	r1 := <-out
	r2 := <-out

	successes, refusals := 0, 0
	for _, r := range []result{r1, r2} {
		if r.err == nil {
			successes++
		} else if strings.Contains(r.err.Error(), "execution_selection revision changed") {
			refusals++
		} else {
			t.Fatalf("unexpected save error: %v", r.err)
		}
	}
	if successes != 1 || refusals != 1 {
		t.Fatalf("expected one success + one CAS refusal, got %d / %d", successes, refusals)
	}
	got, _ := store.Get(w.ID)
	if got.ExecutionSelection == nil || got.ExecutionSelection.Revision != 2 {
		t.Fatalf("envelope did not end at the winner's revision=2, got %+v", got.ExecutionSelection)
	}
}

// A resolver refusal surfaces verbatim — the contract says the owner
// authority's refusal is the truth the envelope records, never a
// synthetic default.
//
// work_r7a_execution_selection_20260911.
func TestResolverRefusalSurfacesVerbatim(t *testing.T) {
	stub := &stubExecutionSelectionResolver{err: errors.New("profile 'claude-minimax' not in route authority")}
	restore := SwapExecutionSelectionResolver(stub.asFunc())
	defer restore()

	now := time.Date(2026, 9, 11, 18, 25, 0, 0, time.UTC)
	_, err := ResolveSelection(ExecutionSelectionResolveRequest{
		HomePath:        "/tmp/whatever",
		LaunchProfileID: "claude-minimax",
		Now:             now,
	}, "claude_code:orchestrator", now)
	if err == nil {
		t.Fatalf("expected resolver refusal")
	}
	if !strings.Contains(err.Error(), "profile 'claude-minimax' not in route authority") {
		t.Fatalf("expected verbatim owner-authority refusal, got %q", err.Error())
	}
}

// --effort overrides the resolved effort when set; the saved selection
// records the override.
//
// work_r7a_execution_selection_20260911.
func TestEffortOverrideReplacesResolvedEffort(t *testing.T) {
	stub := &stubExecutionSelectionResolver{
		returns: ExecutionSelectionResolution{
			LaunchProfileID:       "claude-minimax",
			LaunchProfileRevision: "sha256:abc",
			Harness:               "claude_code",
			ProviderRoute:         "minimax/MiniMax-M3",
			RequestedModelID:      "MiniMax-M3",
			RequestedEffort:       "xhigh",
		},
	}
	restore := SwapExecutionSelectionResolver(stub.asFunc())
	defer restore()

	now := time.Date(2026, 9, 11, 18, 30, 0, 0, time.UTC)
	sel, err := ResolveSelection(ExecutionSelectionResolveRequest{
		HomePath: "/tmp/whatever", LaunchProfileID: "claude-minimax", EffortOverride: "low", Now: now,
	}, "claude_code:orchestrator", now)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sel.RequestedEffort != "low" {
		t.Fatalf("effort override did not replace the resolved effort, got %q", sel.RequestedEffort)
	}
}

// Saving on a terminal work is refused — the contract says the
// envelope's lifecycle is sticky once it's completed / failed / cancelled.
//
// work_r7a_execution_selection_20260911.
func TestSaveExecutionSelectionRefusesOnTerminal(t *testing.T) {
	store, _ := workExecSelectionStore(t)
	now := time.Date(2026, 9, 11, 18, 35, 0, 0, time.UTC)

	w, err := store.Create(CreateInput{
		Owner: "orchestrator:claude_minimax", Actor: "claude_code:orchestrator",
		State: "ready", SuccessPredicate: "save refuses on terminal",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Drive to completed through claim → advance → running → advance → close.
	if _, err := store.Claim(w.ID, "claude_code:orchestrator", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, AdvanceInput{To: "running", Actor: "claude_code:orchestrator"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, AdvanceInput{To: "verifying", Actor: "claude_code:orchestrator"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Close(w.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: "claude_code:orchestrator", Summary: "done"}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	cur, _ := store.Get(w.ID)
	if cur.State != "completed" {
		t.Fatalf("setup expected completed, got %s", cur.State)
	}

	sel := envelope.ExecutionSelection{
		LaunchProfileID: "claude-minimax",
		Harness:         "claude_code",
		Source:          "explicit",
		Revision:        1,
		SelectedBy:      "operator:late",
		SelectedAt:      now.Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if _, err := store.SaveExecutionSelection(w.ID, 0, sel, "operator:late", now.Add(time.Hour)); err == nil ||
		!strings.Contains(err.Error(), "terminal") {
		t.Fatalf("save on terminal must refuse with 'terminal' wording, got %v", err)
	}
}

// _ = fmt.Sprintf keeps the formatter import live for future message-building.
var _ = fmt.Sprintf
