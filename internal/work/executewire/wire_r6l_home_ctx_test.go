// work_r6l_readiness_home_ctx_20260911 — the seam's two readiness call sites
// (PreflightViaActuator and LaunchViaActuator) used to fill
// HarnessHome: homePath (the compound home, not the user home) and never
// set ContextPackID, so the readiness evaluator could not locate the lane
// settings under the user home and the instruction_pack rung had no
// context pack id to consult. The interactive delegate path leaves
// HarnessHome empty (so the evaluator falls back to os.UserHomeDir) and
// passes ContextPackID: w.IDs.ContextPackID; this test package mirrors
// the same shape so the readiness guard's binding reconciliation and
// instruction-pack context validation both run against the live home and
// the envelope's bound context pack.
//
// RED on main today: a tuple-backed Claude lane with installed hooks
// and a context pack bound to the envelope (compound work show →
// ids.context_pack_id) still says
//   lane_unready:blocking_hook_live: mutation-covering binding status=missing
//   instruction_pack: work has no context_pack_id
// because homePath (compound home) has no lane settings under it AND the
// request never set ContextPackID. GREEN after the fix: the request
// carries HarnessHome="" (so the evaluator falls back to the user home)
// and ContextPackID=ctx_test_r6l, both fields are observable in the
// captured request.

package executewire

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// seedWorkWithContextPack writes a ready envelope that already carries
// git_delivery (the launch wire's workspace) AND ids.context_pack_id set
// to ctxPackID. The wire reads wenv.IDs.ContextPackID off this field, so a
// test that wants to assert the value lands on the request MUST seed it
// on the envelope itself.
//
// internal/work.Store.Create validates that the context pack resolves at
// <home>/context/packs/<id>.json and passes envelope.ContextPack.Validate
// (which requires Excluded to be present, not nil). The seam's
// readiness request never inspects the pack body — it only carries the
// id forward onto the request — so the pack just needs to be VALID
// and resolve at the expected path. The minimal valid stub below meets
// that bar.
func seedWorkWithContextPack(t *testing.T, store *work.Store, id, targetRoot, ctxPackID string) {
	t.Helper()
	sha := initGitRepoOrSkip(t, targetRoot)
	writeMinimalContextPack(t, targetRoot, ctxPackID)
	if _, err := store.Create(work.CreateInput{
		WorkID:           id,
		Owner:            "orchestrator:claude_minimax",
		Actor:            "claude_code:orchestrator",
		State:            "ready",
		Risk:             "tier1",
		Privacy:          "private",
		SuccessPredicate: "r6l readiness home + context pack fixture predicate",
		ContextPackID:    ctxPackID,
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: targetRoot,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    sha,
			PublicationMode:    "local_only",
		},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

// writeMinimalContextPack writes the smallest envelope.ContextPack body
// that passes Validate (SchemaVersion, ID, Excluded non-nil) at
// <home>/context/packs/<id>.json. Mirrors the helper
// internal/work.active_packet_test.writeContextPack and only inlines the
// fields Validate walks so the executewire test surface stays free of a
// work-package import cycle for the helper.
func writeMinimalContextPack(t *testing.T, homePath, id string) {
	t.Helper()
	dir := filepath.Join(homePath, "context", "packs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir context/packs: %v", err)
	}
	body := []byte(`{"schema_version":"` + envelope.SVContextPack + `","id":"` + id + `","created_at":"2026-09-11T00:00:00Z","producer":{"kind":"test","name":"r6l-fixture"},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","uncertainty":{"level":"known"},"summary_md":"r6l readiness home + context pack fixture","task_contract":{"intent":"r6l","scope":"internal/work/executewire"},"included":[],"excluded":[],"active_instructions":[],"active_hooks":[],"active_decisions":[],"required_checks":[],"avoid":[]}`)
	if err := os.WriteFile(filepath.Join(dir, id+".json"), body, 0o644); err != nil {
		t.Fatalf("write context pack %s: %v", id, err)
	}
}

// r6lCaptureRequest swaps LaunchReadiness with a stub that captures the
// request and returns Ready: true so neither the preflight nor the
// launch wire takes a downstream branch. Tests inspect the captured
// request.
func r6lCaptureRequest(t *testing.T) *atomic.Pointer[work.DelegateHarnessReadinessRequest] {
	t.Helper()
	var captured atomic.Pointer[work.DelegateHarnessReadinessRequest]
	prev := LaunchReadiness
	LaunchReadiness = func(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
		// copy by value so the test's assertion sees the request as the
		// wire built it, not whatever later iteration might overwrite.
		frozen := req
		captured.Store(&frozen)
		return work.DelegateHarnessReadinessResult{Ready: true}, nil
	}
	t.Cleanup(func() { LaunchReadiness = prev })
	return &captured
}

// expectedUserHome is the home the evaluator at
// cmd/compound/work_delegate_readiness.go falls back to when
// HarnessHome is empty — os.UserHomeDir(). The seam MUST leave
// HarnessHome empty (the explicit contract); the user home is the
// EXPECTED value the evaluator derives, not a value the seam is
// expected to copy verbatim onto every request.
func expectedUserHome(t *testing.T) string {
	t.Helper()
	uh, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	return uh
}

// assertHomeAndContext asserts the captured request carries
// HarnessHome equal to "" (so the evaluator falls back) AND
// ContextPackID equal to ctxPackID. The RED shape had HarnessHome ==
// homePath (compound home, which has no lane settings) and
// ContextPackID == "".
func assertHomeAndContext(t *testing.T, got *work.DelegateHarnessReadinessRequest, homePath, ctxPackID string) {
	t.Helper()
	if got == nil {
		t.Fatal("LaunchReadiness was never invoked; the seam did not build a readiness request")
	}
	if got.HarnessHome == homePath {
		t.Fatalf("readiness HarnessHome == compound home %q; the seam must NOT mirror homePath here — the evaluator falls back to the user home only when HarnessHome is empty (RED: \"blocking_hook_live: mutation-covering binding status=missing\")", got.HarnessHome)
	}
	if got.HarnessHome != "" {
		// A non-empty value is acceptable only if it resolves to the
		// user home, per the packet's design decision #1
		// ("explicitly os.UserHomeDir() if the evaluator does not
		// fall back"). The current evaluator DOES fall back, so the
		// explicit value is only acceptable as a fallback
		// implementation.
		want := expectedUserHome(t)
		if got.HarnessHome != want {
			t.Fatalf("readiness HarnessHome = %q, want \"\" (or user home %q)", got.HarnessHome, want)
		}
	}
	if got.ContextPackID == "" {
		t.Fatalf("readiness ContextPackID = \"\"; the seam must carry wenv.IDs.ContextPackID so the evaluator's instruction_pack rung has the envelope's bound context pack (RED: \"instruction_pack: work has no context_pack_id\")")
	}
	if got.ContextPackID != ctxPackID {
		t.Fatalf("readiness ContextPackID = %q, want %q (the envelope's ids.context_pack_id)", got.ContextPackID, ctxPackID)
	}
}

// TestPreflightReadinessRequestNamesUserHomeAndContextPack — Falsifier
// for PreflightViaActuator.
//
// The seam's readiness request MUST carry HarnessHome != homePath (the
// compound home) AND ContextPackID equal to the envelope's
// ids.context_pack_id. The pre-fix seam filled `HarnessHome: homePath`
// and never set ContextPackID, so the evaluator's home reconciliation
// found no lane settings under the compound home (binding missing) and
// the instruction_pack rung surfaced "work has no context_pack_id".
//
// RED before fix:
//
//	req.HarnessHome == homePath
//	req.ContextPackID == ""
//
// GREEN after fix:
//
//	req.HarnessHome == ""      (evaluator falls back to user home)
//	req.ContextPackID == ctx_test_r6l_preflight
func TestPreflightReadinessRequestNamesUserHomeAndContextPack(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifest(t, homePath, "capable")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const (
		workID    = "work_r6l_preflight_home_ctx"
		ctxPackID = "ctx_test_r6l_preflight"
	)
	seedWorkWithContextPack(t, store, workID, homePath, ctxPackID)
	captured := r6lCaptureRequest(t)

	if err := PreflightViaActuator(store, workID, "claude_minimax", now); err != nil {
		t.Fatalf("PreflightViaActuator: %v", err)
	}
	got := captured.Load()
	assertHomeAndContext(t, got, homePath, ctxPackID)
}

// TestLaunchReadinessRequestNamesUserHomeAndContextPack — Falsifier
// for LaunchViaActuator.
//
// Mirrors the preflight falsifier on the launch path: the seam's
// readiness request MUST carry HarnessHome != homePath AND
// ContextPackID equal to the envelope's ids.context_pack_id. The
// launch wire runs the readiness check BEFORE minting a RunAttempt,
// so a regression to HarnessHome: homePath / ContextPackID: ""
// reproduces the live
//
//	"lane_unready: blocking_hook_live: mutation-covering binding
//	status=missing"
//
// and
//
//	"lane_unready: instruction_pack: work has no context_pack_id"
//
// refusal shapes observed on the live run, exactly as the packet's
// "The defect, with evidence" section describes.
//
// Drives LaunchViaActuator end-to-end against a recording adapter so a
// regression that lets the seam reach adapter.Run without a valid
// readiness request still surfaces (the request check happens BEFORE
// the adapter call; the adapter is not reached).
func TestLaunchReadinessRequestNamesUserHomeAndContextPack(t *testing.T) {
	homePath := initTestHome(t)
	now := time.Now().UTC()
	seedExecuteStageManifest(t, homePath, "capable")
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	const (
		workID    = "work_r6l_launch_home_ctx"
		ctxPackID = "ctx_test_r6l_launch"
	)
	seedWorkWithContextPack(t, store, workID, homePath, ctxPackID)
	captured := r6lCaptureRequest(t)

	// Drive the seam in the production wired-call shape — claim,
	// advance to running, then LaunchViaActuator. A non-Claude adapter
	// path is fine because the readiness check runs before the
	// GovernedPermissionAdapter / posture branch. We swap in a minimal
	// adapter so the test does not need an Open call against a real
	// adapter registry.
	prevAdapter := AdapterFor
	recording := &r6lCapturingAdapter{name: "claude_minimax", home: homePath}
	AdapterFor = func(_ string, name string) (harness.Adapter, bool) {
		if name == "claude_minimax" {
			return recording, true
		}
		return nil, false
	}
	t.Cleanup(func() { AdapterFor = prevAdapter })
	prevInstalled := launchContextInstalled.Load()
	SetLaunchContextInstalled(true)
	t.Cleanup(func() { SetLaunchContextInstalled(prevInstalled) })

	if _, err := store.Claim(workID, "actor:r6l-launch-home-ctx", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Advance(workID, work.AdvanceInput{To: "running", Actor: "actor:r6l-launch-home-ctx", Note: "r6l launch readiness fixture"}, now); err != nil {
		t.Fatalf("advance: %v", err)
	}

	if _, err := LaunchViaActuator(store, workID, "claude_minimax", "claude_minimax", "r6l launch readiness home + context pack", "actor:r6l-launch-home-ctx", now); err != nil {
		t.Fatalf("LaunchViaActuator: %v", err)
	}
	got := captured.Load()
	assertHomeAndContext(t, got, homePath, ctxPackID)
}

// r6lCapturingAdapter is a minimal non-Claude adapter — the test only
// needs an adapter that mints a RunAttempt so the launch wire's
// downstream reaches a stable endpoint. The readiness request check
// happens BEFORE adapter.Run is invoked, so the adapter is not coupled
// to the readiness contract under test.
type r6lCapturingAdapter struct {
	name string
	home string
}

func (a *r6lCapturingAdapter) Name() string { return a.name }
func (a *r6lCapturingAdapter) Run(ctx context.Context, in harness.RunInput) (harness.RunOutput, error) {
	now := time.Now().UTC()
	raStore := runattempt.Open(a.home)
	ra, err := raStore.MintWithActionIntent(in.WorkID, "compound", a.name, in.ActionIntentID, now)
	if err != nil {
		return harness.RunOutput{}, fmt.Errorf("r6l adapter mint: %w", err)
	}
	terminal, err := raStore.Complete(ra.ID, runattempt.StatusSucceeded, now)
	if err != nil {
		return harness.RunOutput{}, fmt.Errorf("r6l adapter complete: %w", err)
	}
	return harness.RunOutput{RunAttempt: terminal}, nil
}

// _ keeps filepath in the import set for the assertion helper
// above (may move to a different file in a refactor).
var _ = filepath.EvalSymlinks
