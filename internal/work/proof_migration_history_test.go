package work

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
)

func completedProofMigrationFixture(t *testing.T) (*Store, envelope.WorkEnvelope, ResourceMigrationInput, []byte, []byte) {
	t.Helper()
	s, _, old, in := proofMigrationStoreFixture(t)
	if err := os.Chmod(s.home, 0700); err != nil {
		t.Fatal(err)
	}
	_, held := s.MigrateCoordinationResources(old.ID, in, fixedNow)
	approveResourceAction(t, s, held, fixedNow)
	if _, err := s.MigrateCoordinationResources(old.ID, in, fixedNow.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"running", "verifying"} {
		if _, err := s.Advance(old.ID, AdvanceInput{To: state, Actor: in.Actor}, fixedNow.Add(time.Duration(4+i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Close(old.ID, CloseInput{Outcome: "completed", MoveType: "audit_only", Actor: in.Actor, Summary: "native migration inspected", ProofRefs: []string{"proof:synthetic-migration"}}, fixedNow.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	oldBody, err := os.ReadFile(s.workPath(old.ID))
	if err != nil {
		t.Fatal(err)
	}
	prep, err := os.ReadFile(s.proofMigrationPendingPath())
	if err != nil {
		t.Fatal(err)
	}
	return s, old, in, oldBody, prep
}

func TestProofMigrationSuccessorRequiresFreshApprovalAndPreservesCompletedHistory(t *testing.T) {
	s, old, in, oldBody, prep := completedProofMigrationFixture(t)
	now := fixedNow.Add(10 * time.Minute)
	next := claimedProofWork(t, s, "native v3 upgrade", proofActorAlpha, now)
	in.ProofTargetRevision = landlease.ProofDomainFencedV3
	in.EvidenceRef = "test://fresh-upgrade-pause"
	in.ExpiresAt = now.Add(3 * time.Minute)
	_, held := s.MigrateCoordinationResources(next.ID, in, now)
	// This must reach a NEW exact approval, not mistake completed v2 history for pending work.
	approveResourceAction(t, s, held, now)
	retained, err := os.ReadFile(s.proofMigrationPendingPath())
	if err != nil || !bytes.Equal(retained, prep) {
		t.Fatalf("unapproved upgrade changed predecessor preparation: %v", err)
	}
	if _, err := s.MigrateCoordinationResources(next.ID, in, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	status, err := landlease.ReadProofDomainStatus(in.BackingHome + "/work/resource-leases")
	if err != nil || status.SchemaVersion != "compound.proof_domain.v0.3" || status.Phase != "active" {
		t.Fatalf("upgrade status=%+v err=%v", status, err)
	}
	after, err := os.ReadFile(s.workPath(old.ID))
	if err != nil || !bytes.Equal(oldBody, after) {
		t.Fatalf("historical Work changed: %v", err)
	}
	body, err := os.ReadFile(s.proofMigrationPendingPath())
	if err != nil {
		t.Fatal(err)
	}
	// Read generically so the falsifier compiles before the preservation field exists.
	var pending struct {
		PredecessorPreparationRef string
		Action                    envelope.ActionIntent
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.PredecessorPreparationRef == "" {
		t.Fatal("new preparation lost historical custody reference")
	}
	canonicalHome, err := filepath.EvalSymlinks(s.home)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := home.OpenArtifactReader(canonicalHome)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := reader.GetPrivate(pending.PredecessorPreparationRef)
	if err != nil || !bytes.Equal(archived, prep) {
		t.Fatalf("exact historical bytes were not privately custodied: %v", err)
	}
	found := false
	for _, ref := range pending.Action.EvidenceRefs {
		found = found || ref == pending.PredecessorPreparationRef
	}
	if !found {
		t.Fatal("fresh exact action does not bind retained history")
	}
}
