package capgraph

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/reviewartifact"
)

func TestRecordCapabilityLifecycleProposalConcurrentFirstWriterWinsImmutablePair(t *testing.T) {
	homePath := t.TempDir()
	first := lifecycleProposalRecordCandidate(t, "teach the skill the first verified failure pattern", time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC))
	second := lifecycleProposalRecordCandidate(t, "teach the skill a conflicting failure pattern", time.Date(2026, 7, 10, 9, 1, 0, 0, time.UTC))
	if first.ID != second.ID {
		t.Fatalf("test requires one derived proposal id, got %s and %s", first.ID, second.ID)
	}

	type result struct {
		proposal CapabilityLifecycleProposal
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, proposal := range []CapabilityLifecycleProposal{first, second} {
		proposal := proposal
		go func() {
			<-start
			_, err := RecordCapabilityLifecycleProposal(homePath, proposal)
			results <- result{proposal: proposal, err: err}
		}()
	}
	close(start)
	got := []result{<-results, <-results}

	var winner CapabilityLifecycleProposal
	successes, collisions := 0, 0
	for _, result := range got {
		switch {
		case result.err == nil:
			successes++
			winner = result.proposal
		case strings.Contains(result.err.Error(), "semantic collision"):
			collisions++
		default:
			t.Fatalf("unexpected concurrent record error: %v", result.err)
		}
	}
	if successes != 1 || collisions != 1 {
		t.Fatalf("concurrent results successes=%d collisions=%d results=%+v", successes, collisions, got)
	}

	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, first.ID+".json")
	markdownPath := filepath.Join(dir, first.ID+".md")
	persisted, err := loadCapabilityLifecycleProposalCanonical(jsonPath, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Statement != winner.Statement {
		t.Fatalf("persisted winner statement = %q, want %q", persisted.Statement, winner.Statement)
	}
	markdown, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := RenderCapabilityLifecycleProposalReview(persisted); string(markdown) != want {
		t.Fatalf("markdown did not derive from JSON winner:\n%s\nwant:\n%s", markdown, want)
	}

	before, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	loser := first
	if winner.Statement == first.Statement {
		loser = second
	}
	if _, err := RecordCapabilityLifecycleProposal(homePath, loser); err == nil || !strings.Contains(err.Error(), "semantic collision") {
		t.Fatalf("losing replay should remain a semantic collision, got %v", err)
	}
	after, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("losing concurrent semantics overwrote the immutable JSON winner")
	}
}

func TestRecordCapabilityLifecycleProposalRecoversMarkdownFromImmutableJSON(t *testing.T) {
	homePath := t.TempDir()
	proposal := lifecycleProposalRecordCandidate(t, "teach the skill one verified failure pattern", time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC))
	paths, err := RecordCapabilityLifecycleProposal(homePath, proposal)
	if err != nil {
		t.Fatal(err)
	}
	jsonBefore, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	winner, err := loadCapabilityLifecycleProposalCanonical(paths.PlanPath, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(paths.ReviewPath); err != nil {
		t.Fatal(err)
	}
	replay := proposal
	replay.CreatedAt = proposal.CreatedAt[:10] + "T10:00:00Z"
	if _, err := RecordCapabilityLifecycleProposal(homePath, replay); err != nil {
		t.Fatal(err)
	}
	assertCapabilityLifecycleProposalPair(t, paths, winner, jsonBefore)

	if err := os.WriteFile(paths.ReviewPath, []byte("stale markdown projection\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	collision := lifecycleProposalRecordCandidate(t, "teach the skill a conflicting failure pattern", time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC))
	if collision.ID != proposal.ID {
		t.Fatalf("collision id = %s, want %s", collision.ID, proposal.ID)
	}
	if _, err := RecordCapabilityLifecycleProposal(homePath, collision); err == nil || !strings.Contains(err.Error(), "semantic collision") {
		t.Fatalf("semantic collision should be rejected after repairing winner markdown, got %v", err)
	}
	assertCapabilityLifecycleProposalPair(t, paths, winner, jsonBefore)
}

func TestRecordCapabilityLifecycleProposalRefusesUnreadableCanonical(t *testing.T) {
	homePath := t.TempDir()
	proposal := lifecycleProposalRecordCandidate(t, "teach the skill one verified failure pattern", time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC))
	dir, err := reviewartifact.Dir(homePath, CapabilityLifecycleProposalReviewArea)
	if err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(dir, proposal.ID+".json")
	markdownPath := filepath.Join(dir, proposal.ID+".md")
	poison := []byte(`{"schema_version":`)
	if err := os.WriteFile(jsonPath, poison, 0o644); err != nil {
		t.Fatal(err)
	}
	markdownSentinel := []byte("existing projection must remain untouched\n")
	if err := os.WriteFile(markdownPath, markdownSentinel, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RecordCapabilityLifecycleProposal(homePath, proposal); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unreadable canonical should block record, got %v", err)
	}
	jsonAfter, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	markdownAfter, err := os.ReadFile(markdownPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(jsonAfter, poison) || !bytes.Equal(markdownAfter, markdownSentinel) {
		t.Fatalf("unreadable canonical was overwritten: json=%q markdown=%q", jsonAfter, markdownAfter)
	}
}

func lifecycleProposalRecordCandidate(t *testing.T, statement string, now time.Time) CapabilityLifecycleProposal {
	t.Helper()
	proposal, err := BuildCapabilityLifecycleProposal(CapabilityLifecycleProposalInput{
		ProposalKind:  "revise",
		Subject:       statement,
		Statement:     statement,
		Detail:        "A bounded proposal with one stable capability and evidence identity.",
		Confidence:    0.82,
		EvidenceRefs:  []string{"session:proposal-record", "check:proposal-record"},
		CapabilityRef: "skill:proposal-record",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

func assertCapabilityLifecycleProposalPair(t *testing.T, paths CapabilityLifecycleProposalPaths, winner CapabilityLifecycleProposal, jsonBefore []byte) {
	t.Helper()
	jsonAfter, err := os.ReadFile(paths.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(jsonAfter, jsonBefore) {
		t.Fatal("markdown recovery changed immutable proposal JSON bytes")
	}
	markdown, err := os.ReadFile(paths.ReviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := RenderCapabilityLifecycleProposalReview(winner); string(markdown) != want {
		t.Fatalf("recovered markdown:\n%s\nwant:\n%s", markdown, want)
	}
}
