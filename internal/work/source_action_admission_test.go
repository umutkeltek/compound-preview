package work

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func sourceAdmissionAction(w envelope.WorkEnvelope, repo, id string) envelope.ActionIntent {
	return envelope.ActionIntent{Header: newHeader(envelope.SVActionIntent, id, fixedNow.Add(time.Minute), w.ID, "", "private", "tier1"), Actor: "operator:test", Capability: "file.write", Action: "implement_release_in_isolated_worktree", Target: envelope.ActionTarget{Kind: "directory", ID: "product-source", Path: repo}, MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local", RollbackPlan: "restore source before-images"}
}
func sourceAdmissionDecision(w envelope.WorkEnvelope, ai envelope.ActionIntent) envelope.PolicyDecision {
	return envelope.PolicyDecision{Header: newHeader(envelope.SVPolicyDecision, "pol_"+strings.TrimPrefix(ai.ID, "act_"), fixedNow.Add(time.Minute), w.ID, "", "private", "tier1"), ActionIntentID: ai.ID, Outcome: "allow"}
}
func sourceAdmissionWork(t *testing.T, repo, base string, bound bool) (*Store, envelope.WorkEnvelope) {
	t.Helper()
	s := newStore(t)
	in := CreateInput{Owner: "operator:test", Actor: "operator:test", State: "ready", SuccessPredicate: "source admission is known before editing"}
	if bound {
		in.GitDelivery = localGitDelivery(repo, base)
	}
	w, err := s.Create(in, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.Claim(w.ID, "operator:test", fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	w, err = s.Advance(w.ID, AdvanceInput{Actor: "operator:test", To: "running"}, fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return s, w
}
func assertSourceAdmissionUnchanged(t *testing.T, s *Store, w envelope.WorkEnvelope, ai envelope.ActionIntent, pd envelope.PolicyDecision, before []byte) {
	t.Helper()
	after, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused source action changed work bytes")
	}
	for _, id := range []string{ai.ID, pd.ID} {
		var stored map[string]any
		if err := s.readEventJSON(id, &stored); err == nil {
			t.Fatalf("refusal persisted %s", id)
		}
	}
}

func TestSourceActionAdmissionRejectsExactIncidentBeforeRecording(t *testing.T) {
	repo, base := initLandingRepo(t)
	for _, door := range []string{"record", "evaluate", "direct_receipt", "reservation"} {
		t.Run(door, func(t *testing.T) {
			s, w := sourceAdmissionWork(t, repo, base, false)
			ai := sourceAdmissionAction(w, repo, "act_incident_"+door)
			pd := sourceAdmissionDecision(w, ai)
			before, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			in := ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: "running", Outcome: "completed", Changed: true, Summary: "source edit", MoveType: "outward_delta"}
			authorized := false
			switch door {
			case "record":
				_, err = s.RecordAction(w.ID, ai, pd, fixedNow.Add(time.Minute))
			case "evaluate":
				_, err = s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(time.Minute))
			case "direct_receipt":
				_, err = s.RecordReceiptedAction(w.ID, in, fixedNow.Add(time.Minute))
			case "reservation":
				err = s.ReserveReceiptedAction(w.ID, in, fixedNow.Add(time.Minute), func() error { authorized = true; return nil })
			}
			if err == nil || !strings.Contains(err.Error(), "git_delivery") {
				t.Fatalf("%s admitted incident source shape: %v", door, err)
			}
			if authorized {
				t.Fatal("reservation consumed authorization before delivery admission")
			}
			assertSourceAdmissionUnchanged(t, s, w, ai, pd, before)
			if _, present, _, err := s.preflightReceiptedActionReservation(w.ID); err != nil || present {
				t.Fatalf("refusal left reservation: %v %v", present, err)
			}
		})
	}
}

func TestSourceActionAdmissionPreviewsRefuseMissingBinding(t *testing.T) {
	repo, base := initLandingRepo(t)
	s, w := sourceAdmissionWork(t, repo, base, false)
	_, err := s.AdvancePlan(w.ID, fixedNow.Add(time.Minute), AdvancePlanOptions{Actor: "operator:test", To: "running", Capability: "file.write", TargetKind: "directory", TargetPath: repo})
	if err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("advance preview: %v", err)
	}
	_, err = s.CreatePlan(CreateInput{Owner: "operator:test", Actor: "operator:test", SuccessPredicate: "declared source create"}, fixedNow, CreatePlanOptions{Actor: "operator:test", Capability: "file.write", TargetKind: "directory", TargetPath: repo})
	if err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("create preview: %v", err)
	}
	ctx := &WorkActionContext{Kind: "directory", SourceRef: repo, Command: []string{"sh", "proof.sh"}, ActionSummary: "edit source"}
	plan, err := s.CreatePlan(CreateInput{Owner: "operator:test", Actor: "operator:test", SuccessPredicate: "source execution preview", State: "ready"}, fixedNow.Add(2*time.Minute), CreatePlanOptions{Actor: "operator:test", ActionContext: ctx})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := s.AdmitCreatePlan(plan, "", fixedNow.Add(2*time.Minute), AdmitCreatePlanOptions{Actor: "operator:test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(admitted.WorkID, "operator:test", fixedNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, err = s.ActionExecutionPlan(admitted.WorkID, fixedNow.Add(4*time.Minute), ActionExecutionPlanOptions{Actor: "operator:test", Capability: "file.write", TargetKind: "directory", TargetPath: repo})
	if err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("execution preview: %v", err)
	}
}

func TestSourceActionAdmissionAllowsBindingAndLinkedWorktree(t *testing.T) {
	repo, base := initLandingRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	landingGit(t, repo, "worktree", "add", "-b", "admission-linked", linked, base)
	for _, target := range []string{repo, linked} {
		s, w := sourceAdmissionWork(t, repo, base, true)
		ai := sourceAdmissionAction(w, target, "act_bound_source")
		if _, err := s.RecordAction(w.ID, ai, sourceAdmissionDecision(w, ai), fixedNow.Add(time.Minute)); err != nil {
			t.Fatalf("bound source enrollment before profile: %v", err)
		}
	}
	other, _ := initLandingRepo(t)
	landingGit(t, other, "remote", "add", "origin", "https://example.invalid/other-product.git")
	s, w := sourceAdmissionWork(t, repo, base, true)
	ai := sourceAdmissionAction(w, other, "act_foreign_source")
	if _, err := s.RecordAction(w.ID, ai, sourceAdmissionDecision(w, ai), fixedNow.Add(time.Minute)); err == nil {
		t.Fatal("foreign repository accepted binding")
	}
}

func TestSourceActionAdmissionPreservesReadAndRuntimeTargets(t *testing.T) {
	repo, base := initLandingRepo(t)
	writeLandingFile(t, filepath.Join(repo, ".gitignore"), "runtime/\n")
	landingGit(t, repo, "add", ".gitignore")
	landingGit(t, repo, "commit", "-m", "runtime ignore")
	for _, kind := range []string{"read", "private_runtime", "git_worktree", "installed_package", "ignored_file", "ignored_directory", "non_git"} {
		t.Run(kind, func(t *testing.T) {
			s, w := sourceAdmissionWork(t, repo, base, false)
			ai := sourceAdmissionAction(w, repo, "act_runtime_"+kind)
			switch kind {
			case "read":
				ai.MutationClass = "read"
			case "ignored_file":
				ai.Target.Kind = "file"
				ai.Target.Path = filepath.Join(repo, "runtime", "future.json")
			case "ignored_directory":
				ai.Target.Path = filepath.Join(repo, "runtime")
			case "non_git":
				ai.Target.Path = t.TempDir()
			default:
				ai.Target.Kind = kind
			}
			if _, err := s.RecordAction(w.ID, ai, sourceAdmissionDecision(w, ai), fixedNow.Add(time.Minute)); err != nil {
				t.Fatalf("runtime/read admission: %v", err)
			}
		})
	}
}

func TestSourceActionAdmissionRecognizesTrackedAndNewFiles(t *testing.T) {
	repo, base := initLandingRepo(t)
	for _, shape := range []struct{ cap, kind, path string }{{"file.write", "file", filepath.Join(repo, "README.md")}, {"file.write", "file", filepath.Join(repo, "future", "code.ts")}, {"source.edit", "file", filepath.Join(repo, "future.ts")}, {"build-implement", "repository", repo}} {
		s, w := sourceAdmissionWork(t, repo, base, false)
		ai := sourceAdmissionAction(w, shape.path, "act_source_variant")
		ai.Capability = shape.cap
		ai.Target.Kind = shape.kind
		if _, err := s.RecordAction(w.ID, ai, sourceAdmissionDecision(w, ai), fixedNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "git_delivery") {
			t.Fatalf("%+v admitted: %v", shape, err)
		}
	}
}

func TestSourcePublicationAdmissionRequiresCandidateNativeProfile(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "ready"}[ready], func(t *testing.T) {
			repo, base := initLandingRepo(t)
			if ready {
				declaration := map[string]any{"schema_version": "compound.proof_consumers.v1", "profile": "bun_consumers_v1", "consumers": []map[string]any{{"id": "product-flow", "command": []string{"bun", "test"}, "dir": ".", "timeout_seconds": 60, "required_tools": []string{"bun"}, "report_kind": "bun_junit", "required_tests": []string{`["proof.test.ts","","passes",["proof.test.ts"]]`}}}}
				body, err := json.Marshal(declaration)
				if err != nil {
					t.Fatal(err)
				}
				writeLandingFile(t, filepath.Join(repo, "package.json"), "{}\n")
				writeLandingFile(t, filepath.Join(repo, "bun.lock"), "{}\n")
				writeLandingFile(t, filepath.Join(repo, "compound-proof.json"), string(body))
				landingGit(t, repo, "add", "package.json", "bun.lock", "compound-proof.json")
				landingGit(t, repo, "commit", "-m", "enroll native source proof")
			}
			s, w := sourceAdmissionWork(t, repo, base, true)
			ai := sourceAdmissionAction(w, repo, "act_public_source")
			ai.Capability = "git-amplify-deployment"
			ai.Target.Kind = "website"
			ai.MutationClass = "public"
			pd := sourceAdmissionDecision(w, ai)
			before, err := os.ReadFile(s.workPath(w.ID))
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RecordAction(w.ID, ai, pd, fixedNow.Add(time.Minute))
			if ready && err != nil {
				t.Fatalf("supported publication declaration refused: %v", err)
			}
			if !ready {
				if err == nil || !strings.Contains(err.Error(), "canonical proof profile") {
					t.Fatalf("publication without native profile admitted: %v", err)
				}
				assertSourceAdmissionUnchanged(t, s, w, ai, pd, before)
			}
		})
	}
}

// Closure corruption fixtures deliberately load a historical performed action.
// Today's admission must refuse creating this legacy missing-binding condition.
func attachHistoricalSourceAction(t *testing.T, s *Store, w envelope.WorkEnvelope, ai envelope.ActionIntent, pd envelope.PolicyDecision, now time.Time) {
	t.Helper()
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		t.Fatal(err)
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.ActionIntentIDs = append(current.ActionIntentIDs, ai.ID)
	current.PolicyDecisionID = pd.ID
	if err := s.writeJSON(s.workPath(w.ID), &current); err != nil {
		t.Fatal(err)
	}
	if err := s.appendEvent(Event{TS: ts(now), WorkID: w.ID, From: current.State, To: current.State, Note: "historical performed source action", Attached: []string{ai.ID, pd.ID}}); err != nil {
		t.Fatal(err)
	}
}

func TestSourceActionAdmissionDoesNotIgnoreTrackedDirectorySource(t *testing.T) {
	repo, base := initLandingRepo(t)
	writeLandingFile(t, filepath.Join(repo, ".gitignore"), "runtime/\n")
	if err := os.MkdirAll(filepath.Join(repo, "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLandingFile(t, filepath.Join(repo, "runtime", "source.ts"), "export const source = true;\n")
	landingGit(t, repo, "add", ".gitignore")
	landingGit(t, repo, "add", "-f", "runtime/source.ts")
	landingGit(t, repo, "commit", "-m", "tracked source below ignored directory")
	s, w := sourceAdmissionWork(t, repo, base, false)
	ai := sourceAdmissionAction(w, filepath.Join(repo, "runtime"), "act_ignored_tracked_source")
	if _, err := s.RecordAction(w.ID, ai, sourceAdmissionDecision(w, ai), fixedNow.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("ignored directory hid tracked source: %v", err)
	}
}

func TestSourceActionAdmissionDoesNotTreatProposalAsPerformedReceipt(t *testing.T) {
	repo, base := initLandingRepo(t)
	s, w := sourceAdmissionWork(t, repo, base, false)
	ai := sourceAdmissionAction(w, repo, "act_source_proposal")
	pd := sourceAdmissionDecision(w, ai)
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RecordReceiptedAction(w.ID, ReceiptedActionInput{Action: ai, PreEvaluatedDecision: &pd, ExpectedWorkState: "running", Outcome: "completed", Changed: true, Summary: "source edit", MoveType: "outward_delta"}, fixedNow.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "git_delivery") {
		t.Fatalf("proposal bypassed fresh source admission: %v", err)
	}
	after, err := os.ReadFile(s.workPath(w.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("proposal refusal changed work")
	}
	var decision envelope.PolicyDecision
	if err := s.readEventJSON(pd.ID, &decision); err == nil {
		t.Fatal("proposal refusal persisted decision")
	}
	if _, present, _, err := s.preflightReceiptedActionPending(w.ID); err != nil || present {
		t.Fatalf("proposal refusal persisted pending outcome: %v %v", present, err)
	}
}

func TestSourcePublicationAdmissionPreservesNativeReviewClassification(t *testing.T) {
	for _, exact := range []bool{true, false} {
		t.Run(map[bool]string{true: "native_review", false: "different_action"}[exact], func(t *testing.T) {
			repo, base := initLandingRepo(t)
			// The review prepares public-redaction evidence. A native source consumer
			// declaration is not a prerequisite for recording that review's authority.
			s, w := sourceAdmissionWork(t, repo, base, true)
			ai := sourceAdmissionAction(w, repo, "act_native_public_review")
			ai.Capability = PublicRedactionReviewCapability
			ai.Action = PublicRedactionReviewAction
			ai.Target.Kind = "repository"
			ai.MutationClass = "public"
			if !exact {
				ai.Action = "publish_source"
			}
			_, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(time.Minute))
			if exact {
				if err != nil {
					t.Fatalf("native review misclassified as source publication: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "canonical proof profile") {
				t.Fatalf("review capability alone exempted a different public action: %v", err)
			}
		})
	}
}
