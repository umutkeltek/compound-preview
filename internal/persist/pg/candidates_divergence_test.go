package pg

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"compound/internal/persist"
)

func TestAppendCandidateRejectsDivergentContent(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	cand := persist.Candidate{
		ID:              fmt.Sprintf("cand-divergent-%d", now.UnixNano()),
		Entity:          "test::divergent-candidate",
		Predicate:       "color",
		Object:          "blue",
		ObservationDate: "2026-08-10",
		Confidence:      0.9,
		Derivation:      "extracted",
		Visibility:      "candidate",
		State:           "proposed",
	}

	if err := store.AppendCandidate(cand, now); err != nil {
		t.Fatalf("AppendCandidate 1st: %v", err)
	}

	// Identical re-insert must be idempotent.
	if err := store.AppendCandidate(cand, now); err != nil {
		t.Fatalf("AppendCandidate identical: %v", err)
	}

	// Divergent content with the same id must be rejected, not silently dropped.
	cand.Object = "red"
	if err := store.AppendCandidate(cand, now); err == nil {
		t.Fatal("expected error for divergent candidate id, got nil")
	} else if !strings.Contains(err.Error(), "different content") {
		t.Fatalf("expected different-content error, got: %v", err)
	}

	got, err := store.GetCandidate(cand.ID)
	if err != nil {
		t.Fatalf("GetCandidate: %v", err)
	}
	if got == nil || got.Object != "blue" {
		t.Fatalf("divergent insert corrupted original candidate: object=%q", got.Object)
	}
}

func TestBulkAppendCandidatesRejectsDivergentContent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()
	sharedID := fmt.Sprintf("cand-bulk-conflict-%d", now.UnixNano())

	seed := persist.Candidate{
		ID: sharedID, Entity: "test::bulk-candidates", Predicate: "p", Object: "first",
		Confidence: 0.5, Derivation: "extracted", Visibility: "candidate", State: "proposed",
	}
	if err := store.AppendCandidate(seed, now); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	newID := fmt.Sprintf("cand-bulk-ok-%d", now.UnixNano())
	newCand := persist.Candidate{
		ID: newID, Entity: "test::bulk-candidates", Predicate: "p", Object: "second",
		Confidence: 0.5, Derivation: "extracted", Visibility: "candidate", State: "proposed",
	}
	conflict := persist.Candidate{
		ID: sharedID, Entity: "test::bulk-candidates", Predicate: "p", Object: "second",
		Confidence: 0.5, Derivation: "extracted", Visibility: "candidate", State: "proposed",
	}

	_, err := store.BulkAppendCandidates(ctx, []persist.Candidate{newCand, conflict})
	if err == nil {
		t.Fatal("expected bulk append to fail on divergent id conflict")
	}
	if !strings.Contains(err.Error(), "different content") {
		t.Fatalf("expected different-content error, got: %v", err)
	}

	// The new candidate must NOT be visible: the whole phase rolled back.
	got, err := store.GetCandidate(newID)
	if err != nil {
		t.Fatalf("GetCandidate: %v", err)
	}
	if got != nil {
		t.Fatalf("bulk phase was not rolled back; found %s after conflict", newID)
	}

	// The original seed must be unchanged.
	seeded, err := store.GetCandidate(sharedID)
	if err != nil {
		t.Fatalf("GetCandidate seed: %v", err)
	}
	if seeded == nil || seeded.Object != "first" {
		t.Fatalf("seed candidate corrupted: object=%q", seeded.Object)
	}
}
