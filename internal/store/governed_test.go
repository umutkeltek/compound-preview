package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"compound/internal/eventstore"
	"compound/internal/persist"
)

func validExperimentalPostgresConfig(profile string) persist.Config {
	return persist.Config{
		Backend:              persist.BackendPostgres,
		DSN:                  "process-memory-test-dsn",
		ExperimentalPostgres: true,
		PostgresProfile:      profile,
		VectorDimensions:     1536,
	}
}

// gated-write-refuses-without-grant: an external_side_effect store export must
// be refused by the default policy (no standing grant), never silently allowed.
func TestStoreGovernedGateRefusesWithoutGrant(t *testing.T) {
	homePath := t.TempDir()
	now := time.Now()
	cfg := validExperimentalPostgresConfig("test")
	ai, err := storeGovernedIntent("work_testgate", "migrate_apply", cfg, "preview", now)
	if err != nil {
		t.Fatalf("build intent: %v", err)
	}
	evaluation, err := gateStoreGovernedWrite(homePath, ai, now)
	if err == nil {
		t.Fatalf("expected refusal without a standing grant, got allowed (outcome=%s)", evaluation.Decision.Outcome)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal should name itself: %v", err)
	}
}

// The governed receipt+transition pair must validate against the envelope
// schema — a proof that cannot be recorded is a governance hole.
func TestStoreGovernedProofPairValidates(t *testing.T) {
	now := time.Now()
	ai, err := storeGovernedIntent("work_testgate", "sync_apply", validExperimentalPostgresConfig("test"), "preview", now)
	if err != nil {
		t.Fatalf("build intent: %v", err)
	}
	receipt, transition := storeGovernedReceiptAndTransition(ai, "store sync --confirm: 3 claims, 2 edges → postgres", now)
	if err := receipt.Validate(); err != nil {
		t.Fatalf("receipt must validate: %v", err)
	}
	if err := transition.Validate(); err != nil {
		t.Fatalf("transition must validate: %v", err)
	}
	if receipt.IDs.WorkID != ai.IDs.WorkID || transition.IDs.WorkID != ai.IDs.WorkID {
		t.Fatalf("proof pair must hang off the intent's work_id")
	}
	if len(transition.ActionRefs) != 1 || transition.ActionRefs[0] != ai.ID {
		t.Fatalf("transition must bind the action intent id")
	}
}

// recordStoreGovernedProof must durably persist both the Receipt and the
// Transition; this is the happy path using the real event store.
func TestRecordStoreGovernedProofPersistsPair(t *testing.T) {
	homePath := t.TempDir()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	now := time.Now()
	ai, err := storeGovernedIntent("work_pair", "sync_apply", validExperimentalPostgresConfig("test"), "preview", now)
	if err != nil {
		t.Fatalf("build intent: %v", err)
	}
	receiptID, transitionID, err := recordStoreGovernedProof(store, ai, "summary", now)
	if err != nil {
		t.Fatalf("record proof: %v", err)
	}
	if receiptID == "" || transitionID == "" {
		t.Fatalf("proof ids must be set")
	}
	if _, err := store.Read(receiptID); err != nil {
		t.Fatalf("receipt must be readable: %v", err)
	}
	if _, err := store.Read(transitionID); err != nil {
		t.Fatalf("transition must be readable: %v", err)
	}
}

// recordStoreGovernedProof must never leave an orphan Receipt if the Transition
// write fails: it compensates by removing the Receipt and returns a clear error.
func TestRecordStoreGovernedProofCompensatesOrphanReceipt(t *testing.T) {
	fake := &fakeEventstoreRecorder{failAfter: 1}
	now := time.Now()
	ai, err := storeGovernedIntent("work_orphan", "sync_apply", validExperimentalPostgresConfig("test"), "preview", now)
	if err != nil {
		t.Fatalf("build intent: %v", err)
	}
	receiptID, transitionID, err := recordStoreGovernedProof(fake, ai, "summary", now)
	if err == nil {
		t.Fatal("expected error when transition write fails")
	}
	if receiptID != "" {
		t.Fatalf("receipt id should be empty after successful compensation, got %q", receiptID)
	}
	if transitionID != "" {
		t.Fatal("transition id should be empty")
	}
	if !fake.removed {
		t.Fatal("expected the orphan receipt to be removed")
	}
}

// If both the Transition write and the Receipt compensation fail, the error
// must surface both facts so the operator knows an orphan proof was left behind.
func TestRecordStoreGovernedProofSurfacesCompensationFailure(t *testing.T) {
	fake := &fakeEventstoreRecorder{failAfter: 1, failRemove: true}
	now := time.Now()
	ai, err := storeGovernedIntent("work_orphan2", "sync_apply", validExperimentalPostgresConfig("test"), "preview", now)
	if err != nil {
		t.Fatalf("build intent: %v", err)
	}
	receiptID, transitionID, err := recordStoreGovernedProof(fake, ai, "summary", now)
	if err == nil {
		t.Fatal("expected error")
	}
	if receiptID == "" {
		t.Fatal("expected orphan receipt id to be returned so operator can repair")
	}
	if transitionID != "" {
		t.Fatal("transition id should be empty")
	}
	if !strings.Contains(err.Error(), "compensation failed") || !strings.Contains(err.Error(), "manual repair") || !strings.Contains(err.Error(), "orphan receipt") {
		t.Fatalf("error should mention orphan receipt and manual repair: %v", err)
	}
}

// fakeEventstoreRecorder implements eventstoreRecorder for failure-path testing.
// It succeeds for the first failAfter PutJSON calls, then returns putErr.
type fakeEventstoreRecorder struct {
	puts       int
	failAfter  int
	failRemove bool
	removed    bool
}

func (f *fakeEventstoreRecorder) PutJSON(id string, value any, indent string) (eventstore.PutResult, error) {
	f.puts++
	if f.failAfter > 0 && f.puts > f.failAfter {
		return eventstore.PutResult{}, errors.New("injected put failure")
	}
	return eventstore.PutResult{Created: true, DiagnosticPath: id}, nil
}

func (f *fakeEventstoreRecorder) Read(raw string) (eventstore.Record, error) {
	return eventstore.Record{ID: raw}, nil
}

func (f *fakeEventstoreRecorder) Remove(raw string) error {
	if f.failRemove {
		return errors.New("injected remove failure")
	}
	f.removed = true
	return nil
}
