package product

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/mem"
)

func TestHypothesisApprovalResumesCommittedOutcomeInFreshProcess(t *testing.T) {
	for _, measured := range []bool{false, true} {
		name := "original generation"
		if measured {
			name = "newer measurement"
		}
		t.Run(name, func(t *testing.T) {
			homePath := t.TempDir()
			candidateID := mintHypothesisDraftFixture(t, homePath, "product:recovery-fixture", "synthetic onboarding change", []string{"activation"})
			restore := swapRecordLoopTransition(func(home string, tr envelope.Transition) error {
				if claimsCommittedApproval(tr) {
					return errors.New("injected committed-outcome recording failure")
				}
				return RecordTransition(home, tr)
			})
			res, approveErr := HypothesisApprove(homePath, HypothesisApproveInput{
				CandidateID: candidateID, ExpectedEffect: "Activation increases by five percentage points.",
				Metrics: []string{"activation", "retention"}, ObservationWindow: "fourteen days", ReviewedBy: "synthetic-operator",
			}, tProduct)
			restore()
			if approveErr == nil || !strings.Contains(approveErr.Error(), "injected committed-outcome") {
				t.Fatalf("fixture did not fail after candidate replacement: %v", approveErr)
			}
			approvalID := res.ApprovalID
			if approvalID == "" {
				t.Error("committed replacement lost its original approval identity on outcome failure")
				// Keep testing the actual missing recovery door on the old code.
				for _, tr := range approvalTransitionTrail(t, homePath, candidateID) {
					for _, ref := range tr.Projections.Product {
						if strings.HasPrefix(ref, "product_hypothesis_approve_intent:") {
							approvalID = strings.TrimPrefix(ref, "product_hypothesis_approve_intent:")
						}
					}
				}
			}
			candidate, err := loadHypothesisCandidate(homePath, candidateID)
			if err != nil || candidate.ExpectedEffect != "Activation increases by five percentage points." || candidate.ObservationWindow != "fourteen days" {
				t.Fatalf("CAS did not commit before injected failure: %+v err=%v", candidate, err)
			}
			if measured {
				memory, err := mem.Open(homePath)
				if err != nil {
					t.Fatal(err)
				}
				_, err = ApplyReview(homePath, memory, Review{
					SchemaVersion: ReviewSchemaVersion, ID: "review_after_approval", ProductID: candidate.ProductID, HypothesisID: candidateID,
					Result: "supported", Rationale: "synthetic before/after observation", Confidence: 0.8,
					NextAction: "retain measured result", EvidenceRefs: []string{"product_signal:synthetic_measurement"}, ReviewedBy: "synthetic-operator",
				}, "", tProduct.Add(14*24*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				candidate, err = loadHypothesisCandidate(homePath, candidateID)
				if err != nil || candidate.Result != "supported" || !containsRef(candidate.CommittedApprovalIDs, approvalID) {
					t.Errorf("real measurement must preserve the atomic approval witness: %+v err=%v", candidate, err)
				}
			}
			before, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestHypothesisApprovalResumeProcessHelper$")
			cmd.Env = append(os.Environ(), "COMPOUND_TEST_APPROVAL_RESUME=1", "COMPOUND_TEST_APPROVAL_HOME="+homePath,
				"COMPOUND_TEST_APPROVAL_CANDIDATE="+candidateID, "COMPOUND_TEST_APPROVAL_ID="+approvalID)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("fresh-process recovery failed: %v\n%s", err, out)
			}
			after, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
			if err != nil || !bytes.Equal(before.Body, after.Body) {
				t.Errorf("recovery rewrote the current candidate generation: %v", err)
			}
			intents, committed := 0, 0
			for _, tr := range approvalTransitionTrail(t, homePath, candidateID) {
				if claimsCommittedApproval(tr) {
					committed++
					if containsRef(tr.BeforeRefs, "product_hypothesis_approve:"+approvalID) {
						t.Error("committed witness was incorrectly included in the preimage references")
					}
					if tr.CreatedAt != tProduct.UTC().Format(time.RFC3339) || !containsRef(tr.AfterRefs, "product_hypothesis_approve:"+approvalID) {
						t.Errorf("recovery changed original approval identity/time: %+v", tr)
					}
				} else if strings.Contains(tr.StateDelta, "approval intent") {
					intents++
				}
			}
			if intents != 1 || committed != 1 {
				t.Errorf("need one original intent and one idempotent committed outcome, got intent=%d committed=%d", intents, committed)
			}
		})
	}
}

func TestHypothesisApprovalResumeProcessHelper(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_APPROVAL_RESUME") != "1" {
		return
	}
	approvalID := os.Getenv("COMPOUND_TEST_APPROVAL_ID")
	for i := 0; i < 2; i++ {
		res, err := HypothesisApprove(os.Getenv("COMPOUND_TEST_APPROVAL_HOME"), HypothesisApproveInput{
			CandidateID: os.Getenv("COMPOUND_TEST_APPROVAL_CANDIDATE"), ResumeApprovalID: approvalID,
		}, time.Now().Add(time.Duration(i)*time.Hour))
		if err != nil || res.ApprovalID != approvalID || res.TransitionID == "" {
			t.Fatalf("resume must finish the original approval idempotently: %+v err=%v", res, err)
		}
	}
}

func TestHypothesisApprovalSnapshotBindsRequestAndGenerations(t *testing.T) {
	homePath := t.TempDir()
	candidateID := mintHypothesisDraftFixture(t, homePath, "product:snapshot-fixture", "synthetic change", []string{"activation"})
	before, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	in := HypothesisApproveInput{CandidateID: candidateID, ExpectedEffect: "  Activation increases.  ", Metrics: []string{" activation ", "retention"}, ObservationWindow: " fourteen days ", ReviewedBy: " synthetic-operator "}
	res, err := HypothesisApprove(homePath, in, tProduct)
	if err != nil {
		t.Fatal(err)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	intentRecord, err := events.Read(stableID("tr_", "product_hypothesis_approve_intent", res.Candidate.ProductID, res.ApprovalID))
	if err != nil {
		t.Fatal(err)
	}
	var intent envelope.Transition
	if err := json.Unmarshal(intentRecord.Bytes, &intent); err != nil {
		t.Fatal(err)
	}
	snapshot, ref, err := readHypothesisApprovalSnapshot(homePath, intent, candidateID, res.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.Request, in) || snapshot.Actor != "synthetic-operator" || snapshot.CreatedAt != tProduct.UTC().Format(time.RFC3339Nano) ||
		!bytes.Equal(before.Body, snapshot.Preimage) || before.Digest != snapshot.PreDigest || before.CustodyToken != snapshot.PreCustody ||
		!bytes.Equal(after.Body, snapshot.Postimage) || after.Digest != snapshot.PostDigest || ref != res.SnapshotRef {
		t.Fatalf("snapshot lost original request or exact CAS generations: %+v", snapshot)
	}
	artifacts, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifacts.Get(ref); !os.IsNotExist(err) {
		t.Fatalf("private approval body entered the general artifact root: %v", err)
	}
}

func TestHypothesisApprovalRetryRetainsOriginalAndAmbiguityDoesNotOverwrite(t *testing.T) {
	homePath := t.TempDir()
	candidateID := mintHypothesisDraftFixture(t, homePath, "product:ambiguous-fixture", "synthetic change", []string{"activation"})
	before, err := loadHypothesisCandidate(homePath, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	in := HypothesisApproveInput{CandidateID: candidateID, ExpectedEffect: "Activation increases.", ReviewedBy: "synthetic-operator"}
	restore := swapRecordLoopTransition(func(home string, tr envelope.Transition) error {
		if claimsCommittedApproval(tr) {
			return errors.New("injected outcome failure")
		}
		return RecordTransition(home, tr)
	})
	res, err := HypothesisApprove(homePath, in, tProduct)
	restore()
	if err == nil || res.ApprovalID == "" || res.Status != "recovery_required" {
		t.Fatalf("fixture needs interrupted approval identity: %+v %v", res, err)
	}
	retry, err := HypothesisApprove(homePath, in, tProduct.Add(time.Hour))
	if err == nil || retry.ApprovalID != res.ApprovalID || retry.Status != "recovery_required" {
		t.Fatalf("plain retry minted a different operation: %+v %v", retry, err)
	}
	// A replacement without the witness is ambiguous, even if it looks like a
	// later measurement. Recovery cannot infer that the earlier CAS lost.
	before.Result, before.NextAction = "supported", "newer generation without approval provenance"
	if err := writeHypothesisCandidate(homePath, before); err != nil {
		t.Fatal(err)
	}
	current, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := HypothesisApprove(homePath, HypothesisApproveInput{CandidateID: candidateID, ResumeApprovalID: res.ApprovalID}, tProduct.Add(2*time.Hour))
	if err == nil || recovered.Status != "recovery_required" || recovered.TransitionID != "" {
		t.Fatalf("missing witness was invented as a settled outcome: %+v %v", recovered, err)
	}
	after, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
	if err != nil || !bytes.Equal(current.Body, after.Body) {
		t.Fatalf("ambiguous recovery overwrote newer generation: %v", err)
	}
	for _, tr := range approvalTransitionTrail(t, homePath, candidateID) {
		if claimsCommittedApproval(tr) || strings.Contains(tr.StateDelta, "approval refused") {
			t.Fatalf("intent-only ambiguity gained a false terminal: %+v", tr)
		}
	}
}

func TestHypothesisApprovalReviewCitationCannotProveRefusedCAS(t *testing.T) {
	homePath := t.TempDir()
	candidateID := mintHypothesisDraftFixture(t, homePath, "product:citation-fixture", "synthetic change", []string{"activation"})
	restore := swapRecordLoopTransition(func(home string, tr envelope.Transition) error {
		if !strings.Contains(tr.StateDelta, "approval intent") {
			return errors.New("injected refusal outcome failure")
		}
		if err := RecordTransition(home, tr); err != nil {
			return err
		}
		candidate, err := loadHypothesisCandidate(home, candidateID)
		if err != nil {
			return err
		}
		candidate.NextAction = "concurrent generation wins"
		return writeHypothesisCandidate(home, candidate)
	})
	initial, err := HypothesisApprove(homePath, HypothesisApproveInput{CandidateID: candidateID, ExpectedEffect: "Activation increases."}, tProduct)
	restore()
	if err == nil || initial.ApprovalID == "" {
		t.Fatalf("fixture needs refused CAS with missing outcome: %+v %v", initial, err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyReview(homePath, store, Review{
		SchemaVersion: ReviewSchemaVersion, ID: "review-citing-interrupted-approval",
		ProductID: "product:citation-fixture", HypothesisID: candidateID,
		Result: "supported", Rationale: "Synthetic before/after observations support the measurement.", Confidence: 0.8,
		NextAction: "Retain the newer measured result.", EvidenceRefs: []string{approvalWitness(initial.ApprovalID)},
		ReviewedBy: "synthetic-operator",
	}, "synthetic-review.json", tProduct.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	before, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
	if err != nil {
		t.Fatal(err)
	}
	res, err := HypothesisApprove(homePath, HypothesisApproveInput{CandidateID: candidateID, ResumeApprovalID: initial.ApprovalID}, tProduct.Add(2*time.Hour))
	if err == nil || res.Status != "recovery_required" || res.TransitionID != "" {
		t.Errorf("caller citation falsely proved approval CAS: %+v err=%v", res, err)
	}
	after, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
	if err != nil || !bytes.Equal(before.Body, after.Body) {
		t.Fatalf("recovery rewrote newer measured candidate: %v", err)
	}
	for _, tr := range approvalTransitionTrail(t, homePath, candidateID) {
		if claimsCommittedApproval(tr) {
			t.Error("review citation minted a false committed terminal")
		}
	}
}

func TestHypothesisApprovalRefusalRecoveryDoesNotInventCommit(t *testing.T) {
	for _, failOutcome := range []bool{false, true} {
		t.Run(fmt.Sprintf("refusal_write_fails=%t", failOutcome), func(t *testing.T) {
			homePath := t.TempDir()
			candidateID := mintHypothesisDraftFixture(t, homePath, "product:refused-recovery", "synthetic change", []string{"activation"})
			approvalID := ""
			restore := swapRecordLoopTransition(func(home string, tr envelope.Transition) error {
				if strings.Contains(tr.StateDelta, "approval intent") {
					for _, ref := range tr.Projections.Product {
						if strings.HasPrefix(ref, "product_hypothesis_approve_intent:") {
							approvalID = strings.TrimPrefix(ref, "product_hypothesis_approve_intent:")
						}
					}
					if err := RecordTransition(home, tr); err != nil {
						return err
					}
					candidate, err := loadHypothesisCandidate(home, candidateID)
					if err != nil {
						return err
					}
					candidate.Result, candidate.NextAction = "supported", "concurrent measurement won"
					return writeHypothesisCandidate(home, candidate)
				}
				if failOutcome {
					return errors.New("injected refusal outcome failure")
				}
				return RecordTransition(home, tr)
			})
			initial, err := HypothesisApprove(homePath, HypothesisApproveInput{CandidateID: candidateID, ExpectedEffect: "Activation increases."}, tProduct)
			restore()
			if err == nil || approvalID == "" {
				t.Fatalf("fixture did not exercise a refused CAS: id=%q err=%v", approvalID, err)
			}
			if failOutcome && (initial.Candidate.ExpectedEffect != "" || initial.Candidate.Result != "") {
				t.Fatalf("uncertain refusal reported proposed candidate as observed state: %+v", initial.Candidate)
			}
			before, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
			if err != nil {
				t.Fatal(err)
			}
			res, err := HypothesisApprove(homePath, HypothesisApproveInput{CandidateID: candidateID, ResumeApprovalID: approvalID}, tProduct.Add(time.Hour))
			want := "refused"
			if failOutcome {
				want = "recovery_required"
			}
			if err == nil || res.Status != want || res.ApprovalID != approvalID {
				t.Fatalf("recovery invented a commitment or lost refusal evidence: %+v err=%v", res, err)
			}
			after, err := candidates.ReadRecord(homePath, candidates.ProductHypothesis, candidateID)
			if err != nil || !bytes.Equal(before.Body, after.Body) {
				t.Fatalf("recovery overwrote the CAS winner: %v", err)
			}
			for _, tr := range approvalTransitionTrail(t, homePath, candidateID) {
				if claimsCommittedApproval(tr) {
					t.Fatalf("refused CAS gained committed history: %+v", tr)
				}
			}
		})
	}
}
