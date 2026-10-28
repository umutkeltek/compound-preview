package reviewqueue

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

var stampFixtureNow = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

// newFreshHome inits a bare COMPOUND_HOME under t.TempDir().
func newFreshHome(t *testing.T) string {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", stampFixtureNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	return homePath
}

// newStampWorkspace is newFreshHome plus a standing-grants registry that
// matches newValidStampAction's ActionIntent, so StampReviewActionAuthority's
// policy.EvaluateAuthority call has a grant to allow against.
func newStampWorkspace(t *testing.T) string {
	t.Helper()
	homePath := newFreshHome(t)
	maxUSD := 1.0
	maxTokens := 100
	grants := policy.StandingGrantRegistry{
		SchemaVersion: "compound.standing-grants.v0.1",
		Grants: []policy.StandingGrant{{
			ID:                   "grant_review_authority_test",
			Capability:           "artifact.update",
			MutationClass:        "local_reversible",
			ReversibilityCeiling: "reversible",
			BlastRadius:          "repo",
			TargetPath:           "artifacts/**",
			MaxUSD:               &maxUSD,
			MaxTokens:            &maxTokens,
		}},
	}
	data, err := json.Marshal(grants)
	if err != nil {
		t.Fatalf("marshal standing grants fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "registries", "standing-grants.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return homePath
}

// newNonTerminalWork mints a fresh, non-terminal WorkEnvelope and returns its
// generated ID.
func newNonTerminalWork(t *testing.T, homePath string) string {
	t.Helper()
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	w, err := workStore.Create(work.CreateInput{
		Owner:            "codex",
		SuccessPredicate: "reviewqueue stamp fixture work",
	}, stampFixtureNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	return w.ID
}

// closeWorkAs advances a work item to running then closes it with the given
// outcome, returning its ID (unchanged) once it is in a terminal state.
func closeWorkAs(t *testing.T, homePath, workID, outcome string) string {
	t.Helper()
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	if _, err := workStore.Claim(workID, "codex", stampFixtureNow); err != nil {
		t.Fatalf("claim work: %v", err)
	}
	if _, err := workStore.Advance(workID, work.AdvanceInput{Actor: "codex", To: "running"}, stampFixtureNow); err != nil {
		t.Fatalf("advance work to running: %v", err)
	}
	if _, err := workStore.Close(workID, work.CloseInput{Actor: "codex", Outcome: outcome, MoveType: "audit_only", Summary: "close for terminal-state fixture"}, stampFixtureNow); err != nil {
		t.Fatalf("close work as %s: %v", outcome, err)
	}
	return workID
}

// newValidStampAction builds an ActionIntent + AuthorityContext that
// StampReviewActionAuthority accepts for a given WorkID, matching the
// standing grant newStampWorkspace installs.
func newValidStampAction(workID string) (envelope.ActionIntent, *policy.AuthorityContext) {
	estimatedUSD := 0.25
	estimatedTokens := 10
	estimatedReviewMinutes := 1
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_review_authority_test",
			CreatedAt:     stampFixtureNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{"evidence:review-authority-action"},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "compound",
		Capability:    "artifact.update",
		Action:        "update",
		Target:        envelope.ActionTarget{Kind: "file", Path: "artifacts/item.json"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "repo",
		EstimatedCost: envelope.EstimatedCost{
			USD:            &estimatedUSD,
			Tokens:         &estimatedTokens,
			HumanReviewMin: &estimatedReviewMinutes,
		},
	}
	ctx := &policy.AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    policy.RecoverabilityVerified,
		OperatorControlContinuity: policy.OperatorControlContinuous,
		ExpectedReceipt:           "receipt for governed action",
		ExpectedProof:             []string{"proof:recovery-path"},
		EvidenceRefs:              []string{"evidence:owner-outcome"},
	}
	return ai, ctx
}

func reportWithAction(action reviewartifact.Action) *reviewartifact.QueueReport {
	return &reviewartifact.QueueReport{
		Items: []reviewartifact.QueueItem{{
			ID:      "item_test",
			Actions: []reviewartifact.Action{action},
		}},
	}
}

// --- Build ---

func TestBuildOnFreshHomeReturnsEmptyReportNoError(t *testing.T) {
	homePath := newFreshHome(t)
	report, err := Build(homePath, Options{Now: stampFixtureNow})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(report.Items) != 0 {
		t.Fatalf("len(report.Items) = %d, want 0 on a fresh home", len(report.Items))
	}
}

// TestBuildAndStampOnStrippedHomeCreateNoDirs proves the queue assembly honors
// its read-only contract in fact, not just in its doc comments: on a home whose
// work/ and events/ have been stripped, Build + both stamp passes must return
// the empty report and leave both directories ABSENT. Before the
// work.OpenReadOnly swap, every stamp MkdirAll'd work/* and events/ as a side
// effect of merely assembling the report — so `compound workbench`,
// `compound attention`, and `compound narrate` (all read-classified in the
// registry) left footprints in COMPOUND_HOME through this shared helper.
func TestBuildAndStampOnStrippedHomeCreateNoDirs(t *testing.T) {
	homePath := newFreshHome(t)
	for _, d := range []string{"work", "events"} {
		if err := os.RemoveAll(filepath.Join(homePath, d)); err != nil {
			t.Fatalf("strip %s: %v", d, err)
		}
	}
	report, err := Build(homePath, Options{Now: stampFixtureNow})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := StampReviewActionAuthority(homePath, &report, stampFixtureNow); err != nil {
		t.Fatalf("StampReviewActionAuthority: %v", err)
	}
	if err := StampReviewGroupActionAuthority(homePath, &report, stampFixtureNow); err != nil {
		t.Fatalf("StampReviewGroupActionAuthority: %v", err)
	}
	for _, d := range []string{"work", "events"} {
		if _, statErr := os.Stat(filepath.Join(homePath, d)); !os.IsNotExist(statErr) {
			t.Errorf("read-only queue assembly must not create %s/ (stat err=%v)", d, statErr)
		}
	}
}

// TestBuildDefaultsNowWhenZero locks in the opts.Now fallback documented on
// Build: a zero Options.Now must not error or panic, it falls back to
// time.Now() internally.
func TestBuildDefaultsNowWhenZero(t *testing.T) {
	homePath := newFreshHome(t)
	if _, err := Build(homePath, Options{}); err != nil {
		t.Fatalf("Build with zero Now: %v", err)
	}
}

func TestBuildErrorsWithoutCompoundHome(t *testing.T) {
	if _, err := Build("", Options{Now: stampFixtureNow}); err == nil {
		t.Fatal("expected an error building the review queue with no COMPOUND_HOME")
	}
}

// TestBuildAnnotatesSeededPromotionReview proves Build's per-domain Annotate
// wiring is real (not vacuous) by seeding one routeevidence promotion-review
// artifact directly on disk and confirming it surfaces, annotated, in the
// assembled report -- exercising reviewartifact.Queue, one of the 24
// Annotate* passes, StampReviewActionAuthority, and Recount together.
func TestBuildAnnotatesSeededPromotionReview(t *testing.T) {
	homePath := newFreshHome(t)
	review := routeevidence.PromotionReview{
		SchemaVersion:   routeevidence.PromotionReviewSchemaVersion,
		ID:              "route_promotion_reviewqueue_test",
		CreatedAt:       stampFixtureNow.Format(time.RFC3339),
		Status:          "review_required",
		Recommendation:  "hold_insufficient_evidence",
		Reason:          "samples 2 below min_samples 10",
		LaneID:          "auto/judge",
		CapabilityClass: "classify-extract",
		EvidenceRefs:    []string{"route_a"},
	}
	if _, err := reviewartifact.Write(homePath, "route-promotions", review.ID, review, " ", "# Route promotion\n"); err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}

	report, err := Build(homePath, Options{Now: stampFixtureNow.Add(time.Minute)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if report.Counts.Total != 1 || report.Counts.ReviewRequired != 1 {
		t.Fatalf("counts = %+v, want exactly the one seeded review-required item", report.Counts)
	}
	if len(report.Items) != 1 || report.Items[0].ID != review.ID {
		t.Fatalf("items = %+v, want the seeded promotion review", report.Items)
	}
	if report.Items[0].Status != "review_required" {
		t.Fatalf("item status = %q, want review_required", report.Items[0].Status)
	}
}

// --- StampReviewActionAuthority ---

func TestStampReviewActionAuthorityRequiresReport(t *testing.T) {
	err := StampReviewActionAuthority(newFreshHome(t), nil, stampFixtureNow)
	if err == nil {
		t.Fatal("expected an error for a nil report")
	}
}

// TestStampReviewActionAuthorityClearsPriorStamps proves the function
// zeroes PolicyDecision/PolicyAuthority on every action before
// re-evaluating -- a stale stamp from a prior run must never survive
// untouched.
func TestStampReviewActionAuthorityClearsPriorStamps(t *testing.T) {
	homePath := newFreshHome(t)
	staleDecision := &envelope.PolicyDecision{}
	staleAuthority := &policy.AuthorityFace{}
	report := reportWithAction(reviewartifact.Action{
		ID:              "action_stale",
		PolicyDecision:  staleDecision,
		PolicyAuthority: staleAuthority,
		// No ActionIntent/AuthorityContext: this action is ineligible, so the
		// stamp should end up nil, not merely left as-is.
	})
	if err := StampReviewActionAuthority(homePath, report, stampFixtureNow); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	got := report.Items[0].Actions[0]
	if got.PolicyDecision != nil || got.PolicyAuthority != nil {
		t.Fatalf("expected stale stamp cleared, got PolicyDecision=%+v PolicyAuthority=%+v", got.PolicyDecision, got.PolicyAuthority)
	}
}

func TestStampReviewActionAuthorityAllowsValidNonTerminalWork(t *testing.T) {
	homePath := newStampWorkspace(t)
	workID := newNonTerminalWork(t, homePath)
	ai, ctx := newValidStampAction(workID)
	report := reportWithAction(reviewartifact.Action{ID: "action_valid", ActionIntent: &ai, AuthorityContext: ctx})

	if err := StampReviewActionAuthority(homePath, report, stampFixtureNow.Add(time.Minute)); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	got := report.Items[0].Actions[0]
	if got.PolicyDecision == nil {
		t.Fatal("expected PolicyDecision to be stamped for a valid non-terminal work item")
	}
	if got.PolicyAuthority == nil {
		t.Fatal("expected PolicyAuthority to be stamped for a valid non-terminal work item")
	}
	if got.PolicyDecision.ActionIntentID != ai.ID {
		t.Fatalf("PolicyDecision.ActionIntentID = %q, want %q", got.PolicyDecision.ActionIntentID, ai.ID)
	}
	if got.PolicyAuthority.ActionIntentID != ai.ID {
		t.Fatalf("PolicyAuthority.ActionIntentID = %q, want %q", got.PolicyAuthority.ActionIntentID, ai.ID)
	}
}

func TestStampReviewActionAuthoritySkipsIneligibleActions(t *testing.T) {
	tests := []struct {
		name          string
		needsWork     bool
		terminalState string // non-empty closes the minted work with this outcome first
		buildAction   func(t *testing.T, workID string) reviewartifact.Action
	}{
		{
			name:      "action intent nil",
			needsWork: true,
			buildAction: func(t *testing.T, workID string) reviewartifact.Action {
				_, ctx := newValidStampAction(workID)
				return reviewartifact.Action{ID: "a", AuthorityContext: ctx}
			},
		},
		{
			name:      "authority context nil",
			needsWork: true,
			buildAction: func(t *testing.T, workID string) reviewartifact.Action {
				ai, _ := newValidStampAction(workID)
				return reviewartifact.Action{ID: "a", ActionIntent: &ai}
			},
		},
		{
			name:      "malformed action intent fails Validate",
			needsWork: true,
			buildAction: func(t *testing.T, workID string) reviewartifact.Action {
				ai, ctx := newValidStampAction(workID)
				ai.Capability = "" // Validate() requires capability
				return reviewartifact.Action{ID: "a", ActionIntent: &ai, AuthorityContext: ctx}
			},
		},
		{
			name: "work id missing work_ prefix",
			buildAction: func(t *testing.T, _ string) reviewartifact.Action {
				ai, ctx := newValidStampAction("not-a-work-id")
				return reviewartifact.Action{ID: "a", ActionIntent: &ai, AuthorityContext: ctx}
			},
		},
		{
			name: "work id has work_live_ prefix",
			buildAction: func(t *testing.T, _ string) reviewartifact.Action {
				ai, ctx := newValidStampAction("work_live_abc123")
				return reviewartifact.Action{ID: "a", ActionIntent: &ai, AuthorityContext: ctx}
			},
		},
		{
			name: "work id path traversal",
			buildAction: func(t *testing.T, _ string) reviewartifact.Action {
				ai, ctx := newValidStampAction("work_abc/../etc")
				return reviewartifact.Action{ID: "a", ActionIntent: &ai, AuthorityContext: ctx}
			},
		},
		{
			name: "work id does not exist",
			buildAction: func(t *testing.T, _ string) reviewartifact.Action {
				ai, ctx := newValidStampAction("work_does_not_exist000000")
				return reviewartifact.Action{ID: "a", ActionIntent: &ai, AuthorityContext: ctx}
			},
		},
		{
			name:          "work is terminal (completed)",
			needsWork:     true,
			terminalState: "completed",
			buildAction: func(t *testing.T, workID string) reviewartifact.Action {
				ai, ctx := newValidStampAction(workID)
				return reviewartifact.Action{ID: "a", ActionIntent: &ai, AuthorityContext: ctx}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := newStampWorkspace(t)
			var workID string
			if tc.needsWork {
				workID = newNonTerminalWork(t, homePath)
				if tc.terminalState != "" {
					workID = closeWorkAs(t, homePath, workID, tc.terminalState)
				}
			}
			action := tc.buildAction(t, workID)
			report := reportWithAction(action)
			if err := StampReviewActionAuthority(homePath, report, stampFixtureNow.Add(time.Minute)); err != nil {
				t.Fatalf("stamp: %v", err)
			}
			got := report.Items[0].Actions[0]
			if got.PolicyDecision != nil || got.PolicyAuthority != nil {
				t.Fatalf("expected action to stay unstamped, got PolicyDecision=%+v PolicyAuthority=%+v", got.PolicyDecision, got.PolicyAuthority)
			}
		})
	}
}

// --- reviewAuthorityTerminalWorkState ---

func TestReviewAuthorityTerminalWorkState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"completed", true},
		{"failed", true},
		{"cancelled", true},
		{"  completed  ", true}, // trimmed
		{"ready", false},
		{"running", false},
		{"blocked", false},
		{"", false},
		{"Completed", false}, // case-sensitive, not normalized
	}
	for _, tc := range tests {
		if got := reviewAuthorityTerminalWorkState(tc.state); got != tc.want {
			t.Errorf("reviewAuthorityTerminalWorkState(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}
