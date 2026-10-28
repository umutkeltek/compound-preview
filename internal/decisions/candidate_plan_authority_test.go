package decisions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
)

// writeParkedDecisionCandidate seeds one parked decision candidate and returns
// its id so the authority tests can build a plan review over it.
func writeParkedDecisionCandidate(t *testing.T, home, candidateID string) {
	t.Helper()
	dir := filepath.Join(home, candidates.InboxDir(candidates.Decision))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := decisionCandidateRecord{
		ID:           candidateID,
		Kind:         string(candidates.Decision),
		Subject:      "decision:destructive-action-hold",
		Statement:    "Hold destructive actions until the owner approves them.",
		EvidenceRefs: []string{"trace:decision", "receipt:hold"},
		Verdict:      candidates.Park,
	}
	data, err := json.MarshalIndent(candidate, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, candidateID+".json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A plan bound to a durable owner work id makes the annotator seed an owner
// ActionIntent + AuthorityContext that the shared review-authority stamp turns
// into a current, allowed authority bundle -- the exact input a console needs
// to expose one safe action instead of "Authority unavailable".
func TestDecisionCandidateReviewSeedsSignableAllowedAuthority(t *testing.T) {
	home := t.TempDir()
	candidateID := "cand_decision_signed_review"
	writeParkedDecisionCandidate(t, home, candidateID)

	plan, err := BuildDecisionCandidatePlan(home, candidateID, "", time.Date(2026, 7, 20, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	plan.WorkID = "work_decision_candidate_review"
	if _, err := RecordDecisionCandidatePlan(home, plan); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: DecisionCandidateReviewArea}, time.Date(2026, 7, 20, 6, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 || report.Items[0].WorkBinding == nil ||
		report.Items[0].WorkBinding.SourceWorkID != plan.WorkID {
		t.Fatalf("bound decision plan did not preserve source work companion: %+v", report.Items)
	}
	AnnotateDecisionCandidateQueue(&report)

	if len(report.Items) != 1 || len(report.Items[0].Actions) != 1 {
		t.Fatalf("decision candidate action missing: %+v", report)
	}
	action := report.Items[0].Actions[0]
	if action.ActionIntent == nil || action.AuthorityContext == nil {
		t.Fatalf("bound plan must seed the owner authority bundle: %+v", action)
	}
	if action.ActionIntent.IDs.WorkID != plan.WorkID {
		t.Fatalf("seed lost the owner work id: %q", action.ActionIntent.IDs.WorkID)
	}
	if action.ActionIntent.Target.Kind != "decision_candidate" || action.ActionIntent.Target.ID != candidateID {
		t.Fatalf("seed targets the wrong decision candidate: %+v", action.ActionIntent.Target)
	}
	if err := action.ActionIntent.Validate(); err != nil {
		t.Fatalf("seeded action intent is not structurally valid: %v", err)
	}

	// The stamp evaluates policy once and projects the authority face. The
	// producer's job is to make that projection current, allowed, and bundle-valid.
	evaluation := policy.EvaluateAuthority(
		*action.ActionIntent,
		policy.DefaultConfig(),
		policy.EvaluationContext{Authority: action.AuthorityContext},
		"compound://policy/effective",
		time.Date(2026, 7, 20, 6, 1, 0, 0, time.UTC),
	)
	if err := policy.ValidateAuthorityBundle(*action.ActionIntent, evaluation.Decision, evaluation.Authority); err != nil {
		t.Fatalf("seeded bundle does not validate after policy projection: %v", err)
	}
	if !evaluation.Authority.Current || !evaluation.Authority.Allowed {
		t.Fatalf("review-evidence action must project current+allowed authority: current=%v allowed=%v",
			evaluation.Authority.Current, evaluation.Authority.Allowed)
	}
}

// Without an owner work id the plan review stays unsigned: the annotator seeds
// no bundle so the shared stamp has nothing to sign and every authority-aware
// surface fails closed -- never a fabricated, unsigned button.
func TestDecisionCandidateReviewWithoutWorkFailsClosed(t *testing.T) {
	home := t.TempDir()
	candidateID := "cand_decision_unsigned_review"
	writeParkedDecisionCandidate(t, home, candidateID)

	plan, err := BuildDecisionCandidatePlan(home, candidateID, "", time.Date(2026, 7, 20, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if plan.WorkID != "" {
		t.Fatalf("plan should have no owner work by default: %q", plan.WorkID)
	}
	if _, err := RecordDecisionCandidatePlan(home, plan); err != nil {
		t.Fatal(err)
	}

	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: DecisionCandidateReviewArea}, time.Date(2026, 7, 20, 6, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	AnnotateDecisionCandidateQueue(&report)

	if len(report.Items) != 1 || len(report.Items[0].Actions) != 1 {
		t.Fatalf("decision candidate action missing: %+v", report)
	}
	action := report.Items[0].Actions[0]
	if action.ActionIntent != nil || action.AuthorityContext != nil {
		t.Fatalf("unbound plan must not seed an authority bundle: %+v", action)
	}
}
