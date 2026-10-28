package work

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	homepkg "compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
)

type privateDeliveryFixture struct {
	s                       *Store
	w                       envelope.WorkEnvelope
	repo, base, target, ref string
	action                  envelope.ActionIntent
	evidence                privateRuntimeDeliveryEvidence
	artifacts               *homepkg.ArtifactStore
}

func newPrivateDeliveryFixture(t *testing.T) privateDeliveryFixture {
	t.Helper()
	s := newStore(t)
	if err := os.Chmod(s.home, 0o700); err != nil {
		t.Fatal(err)
	}
	w := governedActionRunningWork(t, s, "operator:test")
	repo, base := initLandingRepo(t)
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeDeliveryFile(t, repo, ".gitignore", "docs_private/\n")
	const relative = "docs_private/plan/body"
	target := filepath.Join(repo, relative)
	before, after := []byte("before"), []byte("after")
	writeDeliveryFile(t, repo, relative, string(before))
	artifacts, err := homepkg.NewArtifactStore(s.home)
	if err != nil {
		t.Fatal(err)
	}
	beforeRef, err := artifacts.PutPrivate(before)
	if err != nil {
		t.Fatal(err)
	}
	afterRef, err := artifacts.PutPrivate(after)
	if err != nil {
		t.Fatal(err)
	}
	mutate := deliveryTestAction(w, "act_private_delivery_effect", "update_private_runtime", target, "local_reversible", fixedNow.Add(time.Minute))
	decision := deliveryTestDecision(t, s, mutate, fixedNow.Add(time.Minute))
	w, err = s.RecordAction(w.ID, mutate, decision, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, after, 0o600); err != nil {
		t.Fatal(err)
	}

	consume := deliveryTestAction(w, "act_private_delivery_consume", "check_private_runtime", target, "read", fixedNow.Add(2*time.Minute))
	consume.EvidenceRefs = []string{mutate.ID, afterRef}
	consumeDecision := deliveryTestDecision(t, s, consume, fixedNow.Add(2*time.Minute))
	observed, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(observed, after) {
		t.Fatalf("consumer: %q, %v", observed, err)
	}
	reportRef, err := artifacts.PutPrivate([]byte("exact after-image read and compared successfully"))
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := s.RecordReceiptedAction(w.ID, ReceiptedActionInput{
		Action: consume, PreEvaluatedDecision: &consumeDecision,
		ExpectedWorkState: w.State, Outcome: "completed", Changed: false,
		Summary: "actual byte comparison passed", MoveType: "audit_only",
		ProofRefs: []string{mutate.ID, afterRef, reportRef}, AfterRefs: []string{afterRef, reportRef},
	}, fixedNow.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	w, events, err := s.Show(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	event, err := completedReceiptedActionEvent(events, consumed.Action, consumed.Decision, consumed.Receipt, consumed.Transition)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := gitrepository.ReadIdentity(repo, base)
	if err != nil {
		t.Fatal(err)
	}
	originalRecord, err := s.events.Read(mutate.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence := privateRuntimeDeliveryEvidence{
		SchemaVersion: privateRuntimeDeliveryEvidenceSchema,
		WorkID:        w.ID, ActionIntentID: mutate.ID,
		ActionSHA256:   fmt.Sprintf("%x", sha256.Sum256(originalRecord.Bytes)),
		RepositoryRoot: repo, RepositoryRef: base, RepositoryFingerprint: identity.Fingerprint,
		TargetPath: target,
		Files:      []privateRuntimeDeliveryEntry{{Path: relative, BeforeArtifact: beforeRef, AfterArtifact: afterRef}},
		Consumer: deliveryOutcomeEvidence{
			ActionID: consume.ID, PolicyDecisionID: consumed.Decision.ID,
			ReceiptID: consumed.Receipt.ID, TransitionID: consumed.Transition.ID,
			ActionSHA256: deliveryTestDigest(t, consumed.Action), PolicySHA256: deliveryTestDigest(t, consumed.Decision),
			ReceiptSHA256: deliveryTestDigest(t, consumed.Receipt), TransitionSHA256: deliveryTestDigest(t, consumed.Transition),
			EventSHA256: deliveryTestDigest(t, event), ReportRef: reportRef,
		},
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.PutPrivate(body)
	if err != nil {
		t.Fatal(err)
	}
	return privateDeliveryFixture{s: s, w: w, repo: repo, base: base, target: target, ref: ref, action: mutate, evidence: evidence, artifacts: artifacts}
}

// This crosses the public Work API. Custody and a real deterministic consumer
// outcome are evidence; neither a path exception nor a prose "passed" report
// substitutes for the per-action, operator-provenanced binding.
func TestPrivateRuntimeDeliveryBindingClosesExactEffect(t *testing.T) {
	f := newPrivateDeliveryFixture(t)
	s, w, ref, mutate := f.s, f.w, f.ref, f.action
	unbound, err := s.ClosePlan(w.ID, fixedNow.Add(4*time.Minute), ClosePlanOptions{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", ProofRefs: []string{ref}})
	if err != nil || unbound.Status != "not_closeable" || !strings.Contains(unbound.NoAction, "requires git_delivery") {
		t.Fatalf("unbound effect must still require Git delivery: %v; status=%s reason=%s", err, unbound.Status, unbound.NoAction)
	}

	bound, err := s.Advance(w.ID, AdvanceInput{
		To: w.State, Actor: "operator:test", ExpectedWorkState: w.State,
		ExpectedPolicyDecisionID: w.PolicyDecisionID,
		ExpectedActionIntentID:   w.ActionIntentIDs[len(w.ActionIntentIDs)-1],
		NonSourceDelivery:        &ActionDeliveryRequest{ActionIntentID: mutate.ID, Kind: envelope.DeliveryPrivateRuntime, EvidenceRef: ref},
	}, fixedNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(bound.NonSourceDelivery) != 1 || bound.NonSourceDelivery[0].BindingActionIntentID != bound.ActionIntentIDs[len(bound.ActionIntentIDs)-1] {
		t.Fatalf("same-state advance did not bind exact original action to its generated action: %+v", bound.NonSourceDelivery)
	}
	plan, err := s.ClosePlan(w.ID, fixedNow.Add(6*time.Minute), ClosePlanOptions{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", ProofRefs: []string{ref}})
	if err != nil || plan.Status != "close_policy_allowed" {
		t.Fatalf("bound close plan = %+v, error = %v", plan, err)
	}
	closed, err := s.Close(w.ID, CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Changed: true, Summary: "private runtime effect delivered and consumed", ProofRefs: []string{ref}}, fixedNow.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if closed.Work.State != "completed" || closed.Receipt.Outcome != "completed" || !contains(closed.Receipt.ProofRefs, ref) {
		t.Fatalf("private delivery did not close truthfully: %+v", closed)
	}
}

func deliveryTestAction(w envelope.WorkEnvelope, id, verb, target, mutation string, at time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{Header: newHeader(envelope.SVActionIntent, id, at, w.ID, "", "private", "tier1"),
		Actor: "operator:test", Capability: "build-implement", Action: verb,
		Target:        envelope.ActionTarget{Kind: "private_runtime", ID: w.ID, Path: target},
		MutationClass: mutation, Reversibility: "easy", BlastRadius: "local", RollbackPlan: "restore custodied before-image"}
}

func deliveryTestDecision(t *testing.T, s *Store, action envelope.ActionIntent, at time.Time) envelope.PolicyDecision {
	t.Helper()
	cfg, _, err := policy.LoadConfig(s.home)
	if err != nil {
		t.Fatal(err)
	}
	decision := policy.Evaluate(action, cfg, at)
	if decision.Outcome != "allow" && decision.Outcome != "allow_with_obligations" {
		t.Fatalf("fixture action refused: %+v", decision)
	}
	return decision
}

func deliveryTestDigest(t *testing.T, value any) string {
	t.Helper()
	digest, err := exactReceiptedActionDigest(value)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func (f privateDeliveryFixture) bindingInput() AdvanceInput {
	return AdvanceInput{To: f.w.State, Actor: "operator:test", ExpectedWorkState: f.w.State,
		ExpectedPolicyDecisionID: f.w.PolicyDecisionID, ExpectedActionIntentID: f.w.ActionIntentIDs[len(f.w.ActionIntentIDs)-1],
		NonSourceDelivery: &ActionDeliveryRequest{ActionIntentID: f.action.ID, Kind: envelope.DeliveryPrivateRuntime, EvidenceRef: f.ref}}
}

func TestPrivateRuntimeDeliveryPlanCommandBindsExactGeneration(t *testing.T) {
	f := newPrivateDeliveryFixture(t)
	plan, err := f.s.AdvancePlan(f.w.ID, fixedNow.Add(4*time.Minute), AdvancePlanOptions{Actor: "operator:test", To: f.w.State, NonSourceDelivery: f.bindingInput().NonSourceDelivery})
	if err != nil {
		t.Fatal(err)
	}
	for flag, want := range map[string]string{
		"--delivery-action":          f.action.ID,
		"--delivery-kind":            envelope.DeliveryPrivateRuntime,
		"--delivery-evidence":        f.ref,
		"--expected-work-state":      f.w.State,
		"--expected-policy-decision": f.w.PolicyDecisionID,
		"--expected-action-intent":   f.w.ActionIntentIDs[len(f.w.ActionIntentIDs)-1],
	} {
		found := false
		for i := 0; i+1 < len(plan.AdvanceCommand); i++ {
			if plan.AdvanceCommand[i] == flag && plan.AdvanceCommand[i+1] == want {
				found = true
			}
		}
		if !found {
			t.Errorf("command omits %s %q: %v", flag, want, plan.AdvanceCommand)
		}
	}
}

func TestPrivateRuntimeDeliveryBindingRefusesInvalidEvidenceWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *privateDeliveryFixture, *AdvanceInput)
	}{
		{"executor", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) { in.Actor = "codex" }},
		{"missing generation", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) { in.ExpectedActionIntentID = "" }},
		{"stale generation", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			in.ExpectedPolicyDecisionID = "pol_stale"
		}},
		{"lifecycle transition", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) { in.To = "verifying" }},
		{"foreign original action", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			in.NonSourceDelivery.ActionIntentID = "act_foreign"
		}},
		{"unsupported kind", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			in.NonSourceDelivery.Kind = envelope.DeliveryWorktreeRetirement
		}},
		{"changed after image", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			writeDeliveryFile(t, f.repo, f.evidence.Files[0].Path, "newer")
		}},
		{"tracked file", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			landingGit(t, f.repo, "add", "-f", "--", f.evidence.Files[0].Path)
		}},
		{"missing consumed body", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			digest, err := homepkg.ArtifactHex(f.evidence.Consumer.ReportRef)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(f.s.home, "private/artifacts/sha256", digest)); err != nil {
				t.Fatal(err)
			}
		}},
		{"foreign work manifest", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) { f.evidence.WorkID = "work_foreign" }},
		{"changed original bytes", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			f.evidence.ActionSHA256 = strings.Repeat("0", 64)
		}},
		{"wrong consumer receipt", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			f.evidence.Consumer.ReceiptID = "rec_foreign"
		}},
		{"consumer digest mismatch", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			f.evidence.Consumer.EventSHA256 = strings.Repeat("0", 64)
		}},
		{"missing rollback", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			f.evidence.Files[0].BeforeArtifact = ""
		}},
		{"ambiguous rollback", func(t *testing.T, f *privateDeliveryFixture, in *AdvanceInput) {
			f.evidence.Files[0].BeforeAbsent = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPrivateDeliveryFixture(t)
			in := f.bindingInput()
			tc.change(t, &f, &in)
			body, err := json.Marshal(f.evidence)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := f.artifacts.PutPrivate(body)
			if err != nil {
				t.Fatal(err)
			}
			in.NonSourceDelivery.EvidenceRef = ref
			before, err := workEnvelopeSHA256(f.w)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.Advance(f.w.ID, in, fixedNow.Add(5*time.Minute)); err == nil {
				t.Fatal("invalid non-source evidence bound")
			}
			current, err := f.s.Get(f.w.ID)
			if err != nil {
				t.Fatal(err)
			}
			after, err := workEnvelopeSHA256(current)
			if err != nil || after != before {
				t.Fatalf("refusal mutated work: before=%s after=%s err=%v", before, after, err)
			}
		})
	}
}

func TestPrivateRuntimeDeliveryDoesNotLaunderSourceAtClose(t *testing.T) {
	for _, kind := range []string{"tracked after binding", "changed after binding", "commit artifact", "uncovered source action", "explicit Git contract"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrivateDeliveryFixture(t)
			bound, err := f.s.Advance(f.w.ID, f.bindingInput(), fixedNow.Add(5*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "tracked after binding":
				landingGit(t, f.repo, "add", "-f", "--", f.evidence.Files[0].Path)
			case "changed after binding":
				writeDeliveryFile(t, f.repo, f.evidence.Files[0].Path, "newer")
			case "commit artifact":
				_, err = f.s.Advance(f.w.ID, AdvanceInput{To: bound.State, Actor: "operator:test", ArtifactID: "commit:" + f.base}, fixedNow.Add(6*time.Minute))
			case "uncovered source action":
				action := deliveryTestAction(bound, "act_uncovered_source", "edit_source", filepath.Join(f.repo, "source.go"), "local_reversible", fixedNow.Add(6*time.Minute))
				decision := deliveryTestDecision(t, f.s, action, fixedNow.Add(6*time.Minute))
				_, err = f.s.RecordAction(f.w.ID, action, decision, fixedNow.Add(6*time.Minute))
			case "explicit Git contract":
				_, err = f.s.Advance(f.w.ID, AdvanceInput{To: bound.State, Actor: "operator:test", GitDelivery: localGitDelivery(f.repo, f.base)}, fixedNow.Add(6*time.Minute))
			}
			if err != nil {
				t.Fatal(err)
			}
			plan, err := f.s.ClosePlan(f.w.ID, fixedNow.Add(7*time.Minute), ClosePlanOptions{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", ProofRefs: []string{f.ref}})
			if err == nil && plan.Status == "close_policy_allowed" {
				t.Fatalf("unsafe close plan allowed: %+v", plan)
			}
			if _, err := f.s.Close(f.w.ID, CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Changed: true, Summary: "must refuse", ProofRefs: []string{f.ref}}, fixedNow.Add(8*time.Minute)); err == nil {
				t.Fatal("non-source binding bypassed source or live evidence guard")
			}
			current, err := f.s.Get(f.w.ID)
			if err != nil || current.State == "completed" {
				t.Fatalf("unsafe close mutated terminal authority: %+v %v", current, err)
			}
		})
	}
}

func TestPrivateRuntimeDeliveryDoesNotAcceptFailedAttempt(t *testing.T) {
	f := newPrivateDeliveryFixture(t)
	if _, err := f.s.Advance(f.w.ID, f.bindingInput(), fixedNow.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	attempts := runattempt.Open(f.s.home)
	attempt, err := attempts.MintWithActionIntent(f.w.ID, "operator:test", "synthetic", f.action.ID, fixedNow.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attempts.Complete(attempt.ID, runattempt.StatusFailed, fixedNow.Add(7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Advance(f.w.ID, AdvanceInput{Actor: "operator:test", To: "running", RunAttemptIDs: []string{attempt.ID}}, fixedNow.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Close(f.w.ID, CloseInput{Actor: "operator:test", Outcome: "completed", MoveType: "control_plane_repair", Changed: true, Summary: "readback cannot accept failed execution", ProofRefs: []string{f.ref}}, fixedNow.Add(9*time.Minute)); err == nil {
		t.Fatal("non-source classification accepted a failed execution without a genuine retry")
	}
	current, err := f.s.Get(f.w.ID)
	if err != nil || current.State == "completed" {
		t.Fatalf("failed attempt published completion: %+v %v", current, err)
	}
}
