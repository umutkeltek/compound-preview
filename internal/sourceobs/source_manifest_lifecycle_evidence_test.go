package sourceobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestSourceManifestLifecycleRejectsDanglingHighRiskEvidenceWithoutMutation(t *testing.T) {
	homePath, family, _ := lifecycleOwnedManifest(t)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCapturing, lifecycleProofs(SourceLifecycleGateSourceInventory), nil, lifecycleTestNow)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleOwnedSnapshot, lifecycleProofs(SourceLifecycleGateExactCustody), nil, lifecycleTestNow.Add(time.Minute))

	proofs := lifecycleProofs(
		SourceLifecycleGateIsolatedRestore,
		SourceLifecycleGateSemanticParity,
		SourceLifecycleGateRealConsumerReplay,
		SourceLifecycleGateLegacyUnavailableNegativeControl,
	)
	for i := range proofs {
		proofs[i].EvidenceRef = "transition:tr_missing_evidence"
	}
	assertLifecycleAdvanceFailsUnchanged(
		t,
		homePath,
		family,
		SourceLifecycleVerified,
		proofs,
		nil,
		"evidence transition tr_missing_evidence",
	)

	manifest, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Lifecycle.State != SourceLifecycleOwnedSnapshot || manifest.Lifecycle.Revision != 2 {
		t.Fatalf("dangling evidence mutated lifecycle: %+v", manifest.Lifecycle)
	}
	if strings.TrimSpace(manifest.AuthorityRef) == "" {
		t.Fatal("test manifest lost authority ref")
	}
}

func TestSourceManifestLifecycleRejectsAssertionShapedHighRiskProofs(t *testing.T) {
	homePath, family, _ := lifecycleOwnedManifest(t)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCapturing, lifecycleProofs(SourceLifecycleGateSourceInventory), nil, lifecycleTestNow)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleOwnedSnapshot, lifecycleProofs(SourceLifecycleGateExactCustody), nil, lifecycleTestNow.Add(time.Minute))

	proofs := lifecycleProofs(
		SourceLifecycleGateIsolatedRestore,
		SourceLifecycleGateSemanticParity,
		SourceLifecycleGateRealConsumerReplay,
		SourceLifecycleGateLegacyUnavailableNegativeControl,
	)
	manifestPath := SourceManifestPath(homePath, family)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AdvanceSourceManifestLifecycle(homePath, family, SourceManifestLifecycleAdvanceRequest{
		ToState:          SourceLifecycleVerified,
		ExpectedRevision: manifest.Lifecycle.Revision,
		WorkID:           "work_assertion_shaped",
		Actor:            "codex:test",
		ActionIntentID:   "act_assertion_shaped",
		PolicyDecisionID: "pol_assertion_shaped",
		Proofs:           proofs,
	}, lifecycleTestNow.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "requires transition:<id> evidence") {
		t.Fatalf("assertion-shaped proof error = %v", err)
	}
	assertFileBytes(t, manifestPath, before)
}

func TestSourceManifestLifecycleBindsCompletedEvidencePairToEveryHighRiskGate(t *testing.T) {
	homePath, family, _ := lifecycleOwnedManifest(t)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCapturing, lifecycleProofs(SourceLifecycleGateSourceInventory), nil, lifecycleTestNow)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleOwnedSnapshot, lifecycleProofs(SourceLifecycleGateExactCustody), nil, lifecycleTestNow.Add(time.Minute))

	gates := []string{
		SourceLifecycleGateIsolatedRestore,
		SourceLifecycleGateSemanticParity,
		SourceLifecycleGateRealConsumerReplay,
		SourceLifecycleGateLegacyUnavailableNegativeControl,
	}
	transitionRef := writeLifecycleEvidenceFixture(t, homePath, lifecycleEvidenceFixture{
		Family: family,
		Gates:  gates,
		State:  "completed",
	})
	proofs := lifecycleProofs(gates...)
	for i := range proofs {
		proofs[i].EvidenceRef = transitionRef
	}
	report := mustAdvanceLifecycle(t, homePath, family, SourceLifecycleVerified, proofs, nil, lifecycleTestNow.Add(2*time.Minute))
	if len(report.Transition.Proofs) != len(gates) {
		t.Fatalf("proof count = %d, want %d", len(report.Transition.Proofs), len(gates))
	}
	body, err := os.ReadFile(SourceManifestPath(homePath, family))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"evidence_binding", "receipt_sha256", "transition_sha256", "evidence_work_id"} {
		if !strings.Contains(string(body), required) {
			t.Fatalf("manifest proof omitted %q binding:\n%s", required, body)
		}
	}
}

func TestSourceManifestLifecycleRejectsCrossWorkEvidencePair(t *testing.T) {
	homePath, family := ownedSnapshotLifecycle(t)
	transitionRef := writeLifecycleEvidenceFixture(t, homePath, lifecycleEvidenceFixture{
		Family:           family,
		Gates:            verifiedLifecycleGates(),
		State:            "completed",
		ReceiptWorkID:    "work_receipt_subject",
		TransitionWorkID: "work_transition_subject",
	})
	assertVerifiedEvidenceRejected(t, homePath, family, transitionRef, "receipt work_id")
}

func TestSourceManifestLifecycleRejectsNonterminalEvidenceWork(t *testing.T) {
	homePath, family := ownedSnapshotLifecycle(t)
	transitionRef := writeLifecycleEvidenceFixture(t, homePath, lifecycleEvidenceFixture{
		Family: family,
		Gates:  verifiedLifecycleGates(),
		State:  "running",
	})
	assertVerifiedEvidenceRejected(t, homePath, family, transitionRef, "must be completed")
}

func TestSourceManifestLifecycleRejectsCrossFamilyEvidenceWork(t *testing.T) {
	homePath, family := ownedSnapshotLifecycle(t)
	transitionRef := writeLifecycleEvidenceFixture(t, homePath, lifecycleEvidenceFixture{
		Family:           family,
		CanonicalSurface: "another_source_family",
		Gates:            verifiedLifecycleGates(),
		State:            "completed",
	})
	assertVerifiedEvidenceRejected(t, homePath, family, transitionRef, "canonical_surface")
}

func TestSourceManifestLifecycleRejectsEvidenceWithoutExactGateToken(t *testing.T) {
	homePath, family := ownedSnapshotLifecycle(t)
	transitionRef := writeLifecycleEvidenceFixture(t, homePath, lifecycleEvidenceFixture{
		Family: family,
		Gates:  []string{SourceLifecycleGateIsolatedRestore},
		State:  "completed",
	})
	assertVerifiedEvidenceRejected(t, homePath, family, transitionRef, SourceLifecycleGateSemanticParity)
}

func TestSourceLifecycleGateEvidenceRefPreservesSourceFamilyIdentity(t *testing.T) {
	left := SourceLifecycleGateEvidenceRef("family/a", SourceLifecycleGateSemanticParity)
	right := SourceLifecycleGateEvidenceRef("family_a", SourceLifecycleGateSemanticParity)
	if left == right {
		t.Fatalf("distinct source families collided: %q", left)
	}
	if !strings.Contains(left, "family%2Fa") {
		t.Fatalf("gate ref does not carry escaped source identity: %q", left)
	}
}

func TestSourceManifestLifecycleRejectsUnsafeEvidenceObjectIDsBeforePathResolution(t *testing.T) {
	homePath, family := ownedSnapshotLifecycle(t)
	assertVerifiedEvidenceRejected(t, homePath, family, "transition:tr_/../../outside", "invalid evidence transition id")
}

func TestSourceManifestLifecycleRejectsTransitionWithoutReceipt(t *testing.T) {
	homePath, family := ownedSnapshotLifecycle(t)
	transitionRef := writeLifecycleEvidenceFixture(t, homePath, lifecycleEvidenceFixture{
		Family: family,
		Gates:  verifiedLifecycleGates(),
		State:  "completed",
	})
	transitionID := strings.TrimPrefix(transitionRef, "transition:")
	var transition envelope.Transition
	transitionPath := filepath.Join(homePath, "events", transitionID+".json")
	readLifecycleTestJSON(t, transitionPath, &transition)
	transition.AfterRefs = nil
	writeLifecycleTestJSON(t, transitionPath, transition)
	assertVerifiedEvidenceRejected(t, homePath, family, transitionRef, "has no receipt ref")
}

func TestStoredSourceLifecycleEvidenceBindingRequiresCanonicalSHA256Refs(t *testing.T) {
	proof := SourceLifecycleProof{
		Gate:        SourceLifecycleGateSemanticParity,
		EvidenceRef: "transition:tr_evidence",
		EvidenceBinding: &SourceLifecycleEvidenceBinding{
			SchemaVersion:        SourceLifecycleEvidenceBindingSchemaVersion,
			EvidenceWorkID:       "work_evidence",
			EvidenceWorkSHA256:   strings.Repeat("a", 64),
			ReceiptID:            "rec_evidence",
			ReceiptSHA256:        "sha256:" + strings.Repeat("b", 64),
			TransitionID:         "tr_evidence",
			TransitionSHA256:     "sha256:" + strings.Repeat("c", 64),
			CanonicalSurface:     "family",
			RequiredGateProofRef: SourceLifecycleGateEvidenceRef("family", SourceLifecycleGateSemanticParity),
		},
	}
	if err := validateStoredSourceLifecycleEvidenceBindings("family", []SourceLifecycleProof{proof}); err == nil || !strings.Contains(err.Error(), "invalid evidence_binding hash") {
		t.Fatalf("prefixless SHA accepted: %v", err)
	}
}

func TestSourceManifestDoctorRejectsEvidenceEnvelopeDriftAfterAdvance(t *testing.T) {
	homePath, family := ownedSnapshotLifecycle(t)
	gates := verifiedLifecycleGates()
	transitionRef := writeLifecycleEvidenceFixture(t, homePath, lifecycleEvidenceFixture{Family: family, Gates: gates, State: "completed"})
	proofs := lifecycleProofs(gates...)
	for i := range proofs {
		proofs[i].EvidenceRef = transitionRef
	}
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleVerified, proofs, nil, lifecycleTestNow.Add(2*time.Minute))

	transitionID := strings.TrimPrefix(transitionRef, "transition:")
	var transition envelope.Transition
	readLifecycleTestJSON(t, filepath.Join(homePath, "events", transitionID+".json"), &transition)
	var receipt envelope.Receipt
	readLifecycleTestJSON(t, filepath.Join(homePath, "events", transition.AfterRefs[0]+".json"), &receipt)
	receipt.StateDelta.Summary = "tampered after lifecycle advance"
	writeLifecycleTestJSON(t, filepath.Join(homePath, "events", receipt.ID+".json"), receipt)

	doctor, err := DoctorSourceManifest(homePath, family)
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Status != "fail" {
		t.Fatalf("doctor status = %s, want fail: %+v", doctor.Status, doctor.Checks)
	}
	found := false
	for _, check := range doctor.Checks {
		if check.Name == "lifecycle_evidence" && check.Status == "fail" && strings.Contains(check.Detail, "hash") {
			found = true
		}
	}
	if !found {
		t.Fatalf("doctor omitted lifecycle evidence drift: %+v", doctor.Checks)
	}
}

type lifecycleEvidenceFixture struct {
	Family           string
	CanonicalSurface string
	Gates            []string
	State            string
	WorkID           string
	ReceiptWorkID    string
	TransitionWorkID string
}

func ownedSnapshotLifecycle(t *testing.T) (string, string) {
	t.Helper()
	homePath, family, _ := lifecycleOwnedManifest(t)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleCapturing, lifecycleProofs(SourceLifecycleGateSourceInventory), nil, lifecycleTestNow)
	mustAdvanceLifecycle(t, homePath, family, SourceLifecycleOwnedSnapshot, lifecycleProofs(SourceLifecycleGateExactCustody), nil, lifecycleTestNow.Add(time.Minute))
	return homePath, family
}

func verifiedLifecycleGates() []string {
	return []string{
		SourceLifecycleGateIsolatedRestore,
		SourceLifecycleGateSemanticParity,
		SourceLifecycleGateRealConsumerReplay,
		SourceLifecycleGateLegacyUnavailableNegativeControl,
	}
}

func assertVerifiedEvidenceRejected(t *testing.T, homePath, family, transitionRef, want string) {
	t.Helper()
	proofs := lifecycleProofs(verifiedLifecycleGates()...)
	for i := range proofs {
		proofs[i].EvidenceRef = transitionRef
	}
	assertLifecycleAdvanceFailsUnchanged(t, homePath, family, SourceLifecycleVerified, proofs, nil, want)
}

func bindLifecycleTestEnvelopeEvidence(t *testing.T, homePath, family string, proofs []SourceLifecycleProof) []SourceLifecycleProof {
	t.Helper()
	needsBinding := false
	gates := make([]string, 0, len(proofs))
	for _, proof := range proofs {
		if sourceLifecycleGateRequiresEnvelopeEvidence(proof.Gate) {
			needsBinding = true
			gates = append(gates, proof.Gate)
		}
	}
	if !needsBinding {
		return proofs
	}
	for _, proof := range proofs {
		if sourceLifecycleGateRequiresEnvelopeEvidence(proof.Gate) && strings.HasPrefix(proof.EvidenceRef, "transition:") {
			return proofs
		}
	}
	transitionRef := writeLifecycleEvidenceFixture(t, homePath, lifecycleEvidenceFixture{Family: family, Gates: gates, State: "completed"})
	out := append([]SourceLifecycleProof{}, proofs...)
	for i := range out {
		if sourceLifecycleGateRequiresEnvelopeEvidence(out[i].Gate) {
			out[i].EvidenceRef = transitionRef
		}
	}
	return out
}

func writeLifecycleEvidenceFixture(t *testing.T, homePath string, in lifecycleEvidenceFixture) string {
	t.Helper()
	fixtureKey := strings.Join([]string{
		in.Family,
		in.CanonicalSurface,
		strings.Join(in.Gates, ","),
		in.State,
		in.WorkID,
		in.ReceiptWorkID,
		in.TransitionWorkID,
	}, "|")
	workID := firstLifecycleTestValue(in.WorkID, idFor("work_evidence_", fixtureKey))
	receiptWorkID := firstLifecycleTestValue(in.ReceiptWorkID, workID)
	transitionWorkID := firstLifecycleTestValue(in.TransitionWorkID, workID)
	canonicalSurface := firstLifecycleTestValue(in.CanonicalSurface, in.Family)
	state := firstLifecycleTestValue(in.State, "completed")
	receiptID := idFor("rec_evidence_", fixtureKey)
	transitionID := idFor("tr_evidence_", fixtureKey)
	actionID := idFor("act_evidence_", fixtureKey)
	createdAt := lifecycleTestNow.UTC().Format(time.RFC3339)

	workEnvelope := envelope.WorkEnvelope{
		Header: envelope.Header{
			SchemaVersion: envelope.SVWork,
			ID:            workID,
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{TransitionID: transitionID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Owner:              "source-evidence",
		CanonicalSurface:   canonicalSurface,
		State:              state,
		SuccessPredicate:   "prove source lifecycle gates",
		NonGoals:           []string{},
		ActionIntentIDs:    []string{actionID},
		ArtifactIDs:        []string{},
		VerificationIDs:    []string{},
		MemoryCandidateIDs: []string{},
	}
	if state != "completed" {
		workEnvelope.IDs.TransitionID = ""
	}
	proofRefs := make([]string, 0, len(in.Gates))
	for _, gate := range in.Gates {
		proofRefs = append(proofRefs, SourceLifecycleGateEvidenceRef(in.Family, gate))
	}
	receipt := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            receiptID,
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: receiptWorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  proofRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    "completed",
		MoveType:   "outward_delta",
		StateDelta: envelope.StateDelta{Changed: true, Summary: "verified source lifecycle gates", FilesChanged: []string{}, Artifacts: []string{}, Verification: []string{}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Used: []string{}, Missed: []string{}, Candidate: []string{}},
		Memory:     envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs:  proofRefs,
	}
	transition := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            transitionID,
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: transitionWorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  proofRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		BeforeRefs: []string{},
		ActionRefs: []string{actionID},
		AfterRefs:  []string{receiptID},
		StateDelta: "verified source lifecycle gates",
		Projections: envelope.Projections{
			Memory: []string{}, Skill: []string{}, Product: []string{}, Cost: []string{}, Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{}, Typed: []envelope.ProjectionRef{},
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
	}

	writeLifecycleTestJSON(t, filepath.Join(homePath, "work", "work", workID+".json"), workEnvelope)
	writeLifecycleTestJSON(t, filepath.Join(homePath, "events", receiptID+".json"), receipt)
	writeLifecycleTestJSON(t, filepath.Join(homePath, "events", transitionID+".json"), transition)
	return "transition:" + transitionID
}

func firstLifecycleTestValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func writeLifecycleTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLifecycleTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, value); err != nil {
		t.Fatal(err)
	}
}
