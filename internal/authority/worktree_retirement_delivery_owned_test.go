package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	homepkg "compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

type ownedRetirementFixture struct {
	s        *work.Store
	homePath string
	plan     WorktreeRetirementPlan
	w        envelope.WorkEnvelope
	request  work.WorktreeRetirementDeliveryCaptureRequest
	result   WorktreeRetirementResult
	failed   WorktreeRetirementResult
	now      time.Time
}

func retirementTestOwner(t *testing.T, s *work.Store, workID string) WorktreeRetirementOwnerProof {
	t.Helper()
	w, err := s.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := s.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range leases {
		if lease.WorkID == workID {
			return WorktreeRetirementOwnerProof{WorkID: workID, State: w.State, LeaseHolder: lease.By, LeaseClaimedAt: lease.ClaimAt, LeaseWorkSHA256: lease.WorkSHA256, WorkDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(body))}
		}
	}
	t.Fatal("owner lease absent")
	return WorktreeRetirementOwnerProof{}
}

func newOwnedRetirementFixture(t *testing.T) ownedRetirementFixture {
	return newOwnedRetirementFixtureWithRetry(t, false)
}

func newOwnedRetirementFixtureWithRetry(t *testing.T, retry bool) ownedRetirementFixture {
	if retry {
		return newOwnedRetirementFixtureWithFailure(t, "remove")
	}
	return newOwnedRetirementFixtureWithFailure(t, "")
}

func newOwnedRetirementFixtureWithFailure(t *testing.T, failure string) ownedRetirementFixture {
	t.Helper()
	now := time.Now().UTC()
	homePath := t.TempDir()
	if err := homepkg.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	s, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(work.CreateInput{Owner: "operator:test", Actor: "operator:test", State: "ready", SuccessPredicate: "retire exact approved fixture with recoverable proof"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(w.ID, "operator:test", now); err != nil {
		t.Fatal(err)
	}
	w, err = s.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "operator:test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newRetirementGitFixture(t)
	planRequest := retirementPlanRequest(fixture)
	planRequest.OwnerProof = retirementTestOwner(t, s, w.ID)
	plan, err := BuildWorktreeRetirementPlan(context.Background(), planRequest, WorktreeRetirementDependencies{RecoveryInventoryPreflight: work.PreflightRetirementRecoveryInventory, Git: ExecWorktreeRetirementGit{}, Liveness: staticRetirementLiveness{state: WorktreeLivenessUnused}}, now)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := RecordWorktreeRetirementPlan(homePath, plan)
	if err != nil {
		t.Fatal(err)
	}
	review, err := LoadWorktreeRetirementReview(paths.ReviewYAMLPath)
	if err != nil {
		t.Fatal(err)
	}
	review.Decision, review.Rationale, review.ReviewedBy, review.ReviewedAt = "authorize_retirement", "retire exact synthetic fixture", "operator:test", now.Format(time.RFC3339Nano)
	report, err := RecordWorktreeRetirementReview(homePath, review, paths.ReviewYAMLPath, now)
	if err != nil {
		t.Fatal(err)
	}
	req := WorktreeRetirementApplyRequest{HomePath: homePath, PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath, Actor: "operator:test", OwnerProof: planRequest.OwnerProof, SourceProof: planRequest.SourceProof}
	deps := WorktreeRetirementApplyDependencies{RecoveryInventoryPreflight: work.PreflightRetirementRecoveryInventory,
		Git: ExecWorktreeRetirementGit{}, Liveness: staticRetirementLiveness{state: WorktreeLivenessUnused},
		Evaluate: func(action envelope.ActionIntent, at time.Time) (envelope.PolicyDecision, error) {
			return policy.Evaluate(action, policy.DefaultConfig(), at), nil
		},
		RecordOutcome: func(outcome WorktreeRetirementActionOutcome, at time.Time) (WorktreeRetirementActionProof, error) {
			recorded, err := s.RecordReceiptedAction(w.ID, work.ReceiptedActionInput{Action: outcome.Action, PreEvaluatedDecision: &outcome.Decision, ExpectedWorkState: "running", Outcome: outcome.Outcome, Changed: outcome.Changed, Summary: outcome.Summary, MoveType: "control_plane_repair", ProofRefs: outcome.ProofRefs, BeforeRefs: outcome.BeforeRefs, AfterRefs: outcome.AfterRefs, FailureCause: outcome.FailureCause}, at)
			if err != nil {
				return WorktreeRetirementActionProof{}, err
			}
			owner := retirementTestOwner(t, s, w.ID)
			return WorktreeRetirementActionProof{ActionIntentID: recorded.Action.ID, PolicyDecisionID: recorded.Decision.ID, ReceiptID: recorded.Receipt.ID, TransitionID: recorded.Transition.ID, LeaseWorkSHA256: owner.LeaseWorkSHA256, WorkDigest: owner.WorkDigest}, nil
		},
	}

	var first WorktreeRetirementResult
	var completedEffects *recordingRetirementGit
	finalBoundaryReached := false
	if failure != "" {
		injected := errors.New("transient worktree removal")
		if failure == "remove" {
			deps.Git = &failOnceWorktreeRemoveGit{delegate: ExecWorktreeRetirementGit{}, err: injected}
		} else {
			completedEffects = &recordingRetirementGit{delegate: ExecWorktreeRetirementGit{}}
			deps.Git = completedEffects
			resolver := &controlledRetirementRecoveryResolver{delegate: SHA256FileWorktreeRetirementRecoveryResolver{}}
			deps.RecoveryResolver = resolver
			deps.AfterPhase = func(phase string) error {
				if phase == WorktreeRetirementPhaseRefDeleted {
					finalBoundaryReached = true
					resolver.fail = true
				}
				return nil
			}
		}
		first, err = ApplyWorktreeRetirement(context.Background(), req, deps, now)
		if err == nil || first.ReceiptID == "" || first.TransitionID == "" {
			t.Fatalf("failed retirement did not preserve its authentic outcome: %+v, %v", first, err)
		}
		if failure == "remove" && (!errors.Is(err, injected) || first.Status != "failed" || first.WorktreeRemoved) {
			t.Fatalf("unexpected removal failure: %+v, %v", first, err)
		}
		if failure != "remove" && (!finalBoundaryReached || first.Status != "partial" || !first.WorktreeRemoved || !first.RefDeleted) {
			t.Fatalf("unexpected final verification failure: %+v, %v", first, err)
		}
		req.OwnerProof = retirementTestOwner(t, s, w.ID)
		deps.RecoveryResolver = nil
		deps.AfterPhase = nil
		now = now.Add(time.Second)
	}
	result, err := ApplyWorktreeRetirement(context.Background(), req, deps, now)
	if err != nil {
		t.Fatal(err)
	}
	if completedEffects != nil && (completedEffects.worktreeRemoveCount() != 1 || completedEffects.refDeleteCount() != 1) {
		t.Fatal("finalization retry repeated Git effects")
	}
	if _, err := os.Stat(fixture.worktree); !os.IsNotExist(err) {
		t.Fatalf("fixture worktree was not retired: %v", err)
	}
	w, err = s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ownedRetirementFixture{s: s, homePath: homePath, plan: plan, w: w, now: now, result: result, failed: first, request: work.WorktreeRetirementDeliveryCaptureRequest{
		SchemaVersion: work.WorktreeRetirementDeliveryCaptureSchema, CaptureID: "owned_retirement",
		PlanPath: paths.PlanPath, ReviewReportPath: report.ReviewPath, ResultPath: result.ResultPath,
		ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID, ExpectedActionIntentID: w.ActionIntentIDs[len(w.ActionIntentIDs)-1],
	}}
}

func TestWorktreeRetirementDeliveryFailedThenResumed(t *testing.T) {
	for _, failure := range []string{"remove", "final verification"} {
		t.Run(failure, func(t *testing.T) {
			assertRetirementFailureClassifiedAndClosed(t, newOwnedRetirementFixtureWithFailure(t, failure))
		})
	}
}

func assertRetirementFailureClassifiedAndClosed(t *testing.T, f ownedRetirementFixture) {
	t.Helper()
	events, err := eventstore.Open(f.homePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeReceipt, err := events.Read(f.failed.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	capture := bindOwnedRetirement(t, f)
	close := work.CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair",
		Summary: "exact failed retirement recovered", ProofRefs: []string{capture.Delivery.EvidenceRef}}
	before := retirementHomeSnapshot(t, f.homePath)
	if _, err := f.s.Close(f.w.ID, close, f.now.Add(3*time.Second)); err == nil {
		t.Fatal("completed binding alone covered an unbound failed action")
	}
	if !reflect.DeepEqual(before, retirementHomeSnapshot(t, f.homePath)) {
		t.Fatal("refused close mutated failed evidence")
	}
	w, err := f.s.Get(f.w.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.request.CaptureID = "failed_retirement"
	f.request.ActionIntentID = f.failed.ActionIntentID
	f.request.ExpectedWorkState = w.State
	f.request.ExpectedPolicyDecisionID = w.PolicyDecisionID
	f.request.ExpectedActionIntentID = w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
	f.now = f.now.Add(4 * time.Second)
	failed := bindOwnedRetirement(t, f)
	if failed.Delivery.ActionIntentID != f.failed.ActionIntentID {
		t.Fatal("failed action classification silently selected success")
	}
	close.ProofRefs = append(close.ProofRefs, failed.Delivery.EvidenceRef)
	if _, err := f.s.Close(f.w.ID, close, f.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	afterReceipt, err := events.Read(f.failed.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeReceipt.Bytes, afterReceipt.Bytes) {
		t.Fatal("classification rewrote the historical failed receipt")
	}
	var receipt envelope.Receipt
	if err := json.Unmarshal(afterReceipt.Bytes, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != "failed" {
		t.Fatal("historical failure became success")
	}
}

func TestWorktreeRetirementDeliveryPreservesHistoricalContext(t *testing.T) {
	f := newOwnedRetirementFixtureWithRetry(t, true)
	pack := envelope.ContextPack{Header: f.w.Header, SummaryMD: "new session context", Excluded: []envelope.ExcludedRef{}}
	pack.SchemaVersion, pack.ID, pack.IDs = envelope.SVContextPack, "ctx_retirement_later", envelope.IDs{}
	body, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.homePath, "context", "packs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pack.ID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := f.s.Advance(f.w.ID, work.AdvanceInput{To: "running", Actor: "operator:test",
		ContextPackID: pack.ID}, f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	f.request.ExpectedWorkState = w.State
	f.request.ExpectedPolicyDecisionID = w.PolicyDecisionID
	f.request.ExpectedActionIntentID = w.ActionIntentIDs[len(w.ActionIntentIDs)-1]
	f.now = f.now.Add(2 * time.Second)
	assertRetirementFailureClassifiedAndClosed(t, f)
}

func TestWorktreeRetirementDeliveryFailedSelectionControls(t *testing.T) {
	for _, kind := range []string{"failed only", "foreign failure", "later failure", "contradictory failure", "uncovered source"} {
		t.Run(kind, func(t *testing.T) {
			f := newOwnedRetirementFixtureWithRetry(t, true)
			f.request.ActionIntentID = f.failed.ActionIntentID
			wantError := ""
			switch kind {
			case "failed only":
				body, err := json.Marshal(f.failed)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.request.ResultPath, body, 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign failure":
				other := newOwnedRetirementFixtureWithRetry(t, true)
				f.request.ActionIntentID = other.failed.ActionIntentID
			default:
				report, err := LoadWorktreeRetirementReviewReport(f.request.ReviewReportPath)
				if err != nil {
					t.Fatal(err)
				}
				action := newWorktreeRetirementActionForAttempt(f.plan, report, "operator:test",
					f.now.Add(time.Second), "sha256:"+strings.Repeat("c", 64))
				cause := &envelope.FailureCause{SurfaceOutcome: "failed", Mechanism: "synthetic later failure"}
				if kind == "contradictory failure" {
					cause.SurfaceOutcome = "partial"
					wantError = "contradicts its surface outcome"
				} else if kind == "uncovered source" {
					action.Action = "edit_tracked_source"
					wantError = "differs from sealed operation"
				} else {
					wantError = "must precede completed witness"
				}
				decision := policy.Evaluate(action, policy.DefaultConfig(), f.now.Add(time.Second))
				recorded, err := f.s.RecordReceiptedAction(f.w.ID, work.ReceiptedActionInput{
					Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
					Outcome: "failed", Changed: false, MoveType: "control_plane_repair", Summary: "synthetic later failure",
					ProofRefs: worktreeRetirementApplyProofRefs(f.plan, report), FailureCause: cause,
				}, f.now.Add(time.Second))
				if err != nil {
					t.Fatal(err)
				}
				f.request.ActionIntentID = recorded.Action.ID
				f.request.ExpectedPolicyDecisionID = recorded.Work.PolicyDecisionID
				f.request.ExpectedActionIntentID = recorded.Work.ActionIntentIDs[len(recorded.Work.ActionIntentIDs)-1]
			}
			before := retirementHomeSnapshot(t, f.homePath)
			_, err := f.s.CaptureWorktreeRetirementDelivery(f.w.ID, f.request, "operator:test", f.now.Add(2*time.Second))
			if err == nil || (wantError != "" && !strings.Contains(err.Error(), wantError)) {
				t.Fatalf("invalid failed selection: error=%v want=%q", err, wantError)
			}
			if !reflect.DeepEqual(before, retirementHomeSnapshot(t, f.homePath)) {
				t.Fatal("invalid failed selection mutated home")
			}
		})
	}
}

func TestWorktreeRetirementDeliveryPublicCaptureCommand(t *testing.T) {
	f := newOwnedRetirementFixture(t)
	body, err := json.Marshal(f.request)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "capture.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := work.RunWorkDeliveryCaptureCLI([]string{f.w.ID, "--request", path, "--by", "operator:test", "--json"}, f.s, f.now.Add(time.Second), &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, &stderr)
	}
	var result work.PrivateRuntimeDeliveryCaptureResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "captured_not_bound" || result.Delivery.Kind != envelope.DeliveryWorktreeRetirement {
		t.Fatalf("result=%+v", result)
	}
	stdout.Reset()
	stderr.Reset()
	if code := work.RunWorkDeliveryCaptureCLI(result.RetryCommand[3:], f.s, f.now.Add(2*time.Second), &stdout, &stderr); code != 0 {
		t.Fatalf("retry code=%d stderr=%s", code, &stderr)
	}
	var replay work.PrivateRuntimeDeliveryCaptureResult
	if err := json.Unmarshal(stdout.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ActionID != result.ActionID || replay.ReceiptID != result.ReceiptID || replay.Delivery.EvidenceRef != result.Delivery.EvidenceRef {
		t.Fatalf("retry replaced transaction: %+v %+v", result, replay)
	}
}

func TestWorktreeRetirementDeliveryRefusesInvalidEvidenceBeforeCustody(t *testing.T) {
	for _, kind := range []string{"missing receipt", "changed actor", "changed result id", "missing recovery", "foreign work", "wrong generation", "changed reviewed yaml"} {
		t.Run(kind, func(t *testing.T) {
			f := newOwnedRetirementFixture(t)
			workID, request := f.w.ID, f.request
			changedResult := f.result
			switch kind {
			case "missing receipt":
				changedResult.ReceiptID = "rec_missing"
			case "changed actor":
				changedResult.Actor = "operator:foreign"
			case "changed result id":
				changedResult.ID = "wtrres_forged"
			case "missing recovery":
				if err := os.Remove(f.plan.RecoveryProofs[0].Path); err != nil {
					t.Fatal(err)
				}
			case "foreign work":
				foreign, err := f.s.Create(work.CreateInput{Owner: "operator:test", Actor: "operator:test", State: "ready", SuccessPredicate: "separate work"}, f.now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.Claim(foreign.ID, "operator:test", f.now); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.Advance(foreign.ID, work.AdvanceInput{To: "running", Actor: "operator:test"}, f.now); err != nil {
					t.Fatal(err)
				}
				workID = foreign.ID
			case "wrong generation":
				request.ExpectedPolicyDecisionID = "pol_not_current"
			case "changed reviewed yaml":
				report, err := LoadWorktreeRetirementReviewReport(request.ReviewReportPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(report.SourcePath, []byte("different reviewed bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "missing receipt" || kind == "changed actor" || kind == "changed result id" {
				changedResult.ResultDigest = worktreeRetirementResultDigest(changedResult)
				body, err := json.Marshal(changedResult)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(request.ResultPath, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := retirementHomeSnapshot(t, f.homePath)
			if _, err := f.s.CaptureWorktreeRetirementDelivery(workID, request, "operator:test", f.now.Add(time.Second)); err == nil {
				t.Fatal("invalid evidence accepted")
			}
			if !reflect.DeepEqual(before, retirementHomeSnapshot(t, f.homePath)) {
				t.Fatal("refused capture changed home")
			}
		})
	}
}

func retirementHomeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = fmt.Sprintf("%x", sha256.Sum256(body))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func bindOwnedRetirement(t *testing.T, f ownedRetirementFixture) work.PrivateRuntimeDeliveryCaptureResult {
	t.Helper()
	capture, err := f.s.CaptureWorktreeRetirementDelivery(f.w.ID, f.request, "operator:test", f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	w, err := f.s.Get(f.w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Advance(f.w.ID, work.AdvanceInput{To: w.State, Actor: "operator:test", ExpectedWorkState: w.State, ExpectedPolicyDecisionID: w.PolicyDecisionID, ExpectedActionIntentID: w.ActionIntentIDs[len(w.ActionIntentIDs)-1], NonSourceDelivery: &capture.Delivery}, f.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return capture
}

func TestWorktreeRetirementDeliveryKeepsUncoveredSourceGate(t *testing.T) {
	f := newOwnedRetirementFixture(t)
	capture := bindOwnedRetirement(t, f)
	action := envelope.ActionIntent{Header: f.w.Header, Actor: "operator:test", Capability: "build-implement", Action: "edit_tracked_source",
		Target:        envelope.ActionTarget{Kind: "path", ID: f.w.ID, Path: filepath.Join(f.plan.RepoRoot, "seed.txt")},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local", RollbackPlan: "restore tracked source"}
	action.SchemaVersion, action.ID, action.IDs.WorkID = envelope.SVActionIntent, "act_uncovered_source", f.w.ID
	action.CreatedAt = f.now.Add(3 * time.Second).Format(time.RFC3339Nano)
	cfg, _, err := policy.LoadConfig(f.homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RecordAction(f.w.ID, action, policy.Evaluate(action, cfg, f.now.Add(3*time.Second)), f.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Close(f.w.ID, work.CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Summary: "must not accept uncovered source", ProofRefs: []string{capture.Delivery.EvidenceRef}}, f.now.Add(4*time.Second)); err == nil {
		t.Fatal("retirement binding laundered source action")
	}
	w, err := f.s.Get(f.w.ID)
	if err != nil || w.State != "running" {
		t.Fatalf("failed close changed lifecycle: %+v %v", w, err)
	}
}

func TestWorktreeRetirementDeliverySurvivesLaterMainAndUsesOwnedRecovery(t *testing.T) {
	f := newOwnedRetirementFixture(t)
	retirementRunGit(t, f.plan.RepoRoot, "commit", "--allow-empty", "-m", "later main")
	tip := retirementGitOutput(t, f.plan.RepoRoot, "rev-parse", "HEAD")
	retirementRunGit(t, f.plan.RepoRoot, "update-ref", "refs/remotes/origin/main", tip)
	capture := bindOwnedRetirement(t, f)
	// Custody now owns the verified recovery bytes. Remove only the synthetic
	// original fixture file, proving close cannot fall back to that source.
	if err := os.Remove(f.plan.RecoveryProofs[0].Path); err != nil {
		t.Fatal(err)
	}
	closed, err := f.s.Close(f.w.ID, work.CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Summary: "historical exact retirement with owned recovery", ProofRefs: []string{capture.Delivery.EvidenceRef}}, f.now.Add(3*time.Second))
	if err != nil || closed.Work.State != "completed" {
		t.Fatalf("close: %+v %v", closed, err)
	}
}

func TestWorktreeRetirementDeliveryMissingCustodyStopsClose(t *testing.T) {
	f := newOwnedRetirementFixture(t)
	capture := bindOwnedRetirement(t, f)
	ref := "artifact:" + f.plan.RecoveryProofs[0].Digest
	digest, err := homepkg.ArtifactHex(ref)
	if err != nil {
		t.Fatal(err)
	}
	// The normal immutable artifact writer cannot produce missing custody.
	// Deliberately remove this isolated fixture object to probe the reader gate.
	if err := os.Remove(filepath.Join(f.homePath, "private", "artifacts", "sha256", digest)); err != nil {
		t.Fatal(err)
	}
	before := retirementHomeSnapshot(t, f.homePath)
	if _, err := f.s.Close(f.w.ID, work.CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Summary: "must refuse missing recovery custody", ProofRefs: []string{capture.Delivery.EvidenceRef}}, f.now.Add(3*time.Second)); err == nil {
		t.Fatal("close accepted missing custody")
	}
	if !reflect.DeepEqual(before, retirementHomeSnapshot(t, f.homePath)) {
		t.Fatal("refused close changed home")
	}
	readOnly, err := work.OpenReadOnly(f.homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.CaptureWorktreeRetirementDelivery(f.w.ID, f.request, "operator:test", f.now.Add(4*time.Second)); err == nil {
		t.Fatal("read-only capture repaired custody")
	}
	if !reflect.DeepEqual(before, retirementHomeSnapshot(t, f.homePath)) {
		t.Fatal("read-only capture mutated home")
	}
}

func TestWorktreeRetirementDeliveryOwnedJourney(t *testing.T) {
	f := newOwnedRetirementFixture(t)
	resultBytes, err := os.ReadFile(f.result.ResultPath)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := f.s.CaptureWorktreeRetirementDelivery(f.w.ID, f.request, "operator:test", f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if capture.Status != "captured_not_bound" || capture.Delivery.Kind != envelope.DeliveryWorktreeRetirement || capture.Delivery.ActionIntentID != f.result.ActionIntentID {
		t.Fatalf("capture=%+v", capture)
	}
	current, err := f.s.Get(f.w.ID)
	if err != nil || current.State != "running" || len(current.NonSourceDelivery) != 0 {
		t.Fatalf("capture changed acceptance: %+v %v", current, err)
	}
	plan, err := f.s.ClosePlan(f.w.ID, f.now.Add(2*time.Second), work.ClosePlanOptions{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair"})
	if err != nil || plan.Status != "not_closeable" {
		t.Fatalf("capture bypassed explicit binding: %+v %v", plan, err)
	}
	_, err = f.s.Advance(f.w.ID, work.AdvanceInput{To: "running", Actor: "operator:test", ExpectedWorkState: current.State, ExpectedPolicyDecisionID: current.PolicyDecisionID, ExpectedActionIntentID: current.ActionIntentIDs[len(current.ActionIntentIDs)-1], NonSourceDelivery: &capture.Delivery}, f.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	closed, err := f.s.Close(f.w.ID, work.CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Summary: "exact authorized retirement is authenticated", ProofRefs: []string{capture.Delivery.EvidenceRef}}, f.now.Add(4*time.Second))
	if err != nil || closed.Work.State != "completed" {
		t.Fatalf("close: %+v %v", closed, err)
	}
	after, err := os.ReadFile(f.result.ResultPath)
	if err != nil || string(after) != string(resultBytes) {
		t.Fatal("delivery capture rewrote historical result")
	}
}
