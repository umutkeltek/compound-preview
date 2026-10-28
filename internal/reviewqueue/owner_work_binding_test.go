package reviewqueue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/reviewartifact"
	"compound/internal/work"
)

var reviewAuthorityTestNow = time.Date(2026, 7, 23, 11, 0, 0, 0, time.UTC)

func TestResolveReviewWorkAuthorityUsesLiveSourceThenTerminalFallback(t *testing.T) {
	tests := []struct {
		name       string
		terminal   bool
		wantWorkID string
		wantSource string
	}{
		{name: "live source", wantWorkID: "work_source", wantSource: ReviewAuthoritySourceWork},
		{name: "terminal source", terminal: true, wantWorkID: "work_area", wantSource: ReviewAuthorityFallbackWork},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath, item, store := reviewAuthorityFixture(t, "panel-verdicts")
			createReviewAuthorityWork(t, store, "work_source", "", reviewAuthorityTestNow)
			createReviewAuthorityWork(t, store, "work_area", ReviewAreaSurface("panel-verdicts"), reviewAuthorityTestNow)
			if tc.terminal {
				closeReviewAuthorityWork(t, store, "work_source", reviewAuthorityTestNow.Add(time.Minute))
			}
			bindReviewAuthorityItem(t, homePath, &item, "work_source", "work_area")

			before := countReviewAuthorityWork(t, homePath)
			got, err := ResolveReviewWorkAuthority(homePath, item)
			if err != nil {
				t.Fatal(err)
			}
			if got.WorkID != tc.wantWorkID || got.Source != tc.wantSource || got.SourceWorkID != "work_source" ||
				got.FallbackWorkID != "work_area" {
				t.Fatalf("resolution = %+v", got)
			}
			if after := countReviewAuthorityWork(t, homePath); after != before {
				t.Fatalf("read-only authority resolution created work: before=%d after=%d", before, after)
			}
		})
	}
}

func TestResolveReviewWorkAuthorityFailsClosedWithoutLaundering(t *testing.T) {
	tests := []struct {
		name      string
		sourceID  string
		fallback  string
		setup     func(t *testing.T, store *work.Store)
		wantError string
	}{
		{
			name: "missing declared source does not fall back", sourceID: "work_missing", fallback: "work_area",
			setup: func(t *testing.T, store *work.Store) {
				createReviewAuthorityWork(t, store, "work_area", ReviewAreaSurface("panel-verdicts"), reviewAuthorityTestNow)
			},
			wantError: "declared source",
		},
		{
			name: "terminal source without fallback", sourceID: "work_source",
			setup: func(t *testing.T, store *work.Store) {
				createReviewAuthorityWork(t, store, "work_source", "", reviewAuthorityTestNow)
				closeReviewAuthorityWork(t, store, "work_source", reviewAuthorityTestNow.Add(time.Minute))
			},
			wantError: "fallback",
		},
		{
			name: "wrong area fallback", sourceID: "", fallback: "work_area",
			setup: func(t *testing.T, store *work.Store) {
				createReviewAuthorityWork(t, store, "work_area", ReviewAreaSurface("decision-candidates"), reviewAuthorityTestNow)
			},
			wantError: "canonical surface",
		},
		{
			name: "terminal fallback", sourceID: "", fallback: "work_area",
			setup: func(t *testing.T, store *work.Store) {
				createReviewAuthorityWork(t, store, "work_area", ReviewAreaSurface("panel-verdicts"), reviewAuthorityTestNow)
				closeReviewAuthorityWork(t, store, "work_area", reviewAuthorityTestNow.Add(time.Minute))
			},
			wantError: "terminal",
		},
		{
			name: "missing fallback", sourceID: "", fallback: "work_area",
			setup: func(t *testing.T, store *work.Store) {
			},
			wantError: "fallback",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath, item, store := reviewAuthorityFixture(t, "panel-verdicts")
			tc.setup(t, store)
			bindReviewAuthorityItem(t, homePath, &item, tc.sourceID, tc.fallback)
			if _, err := ResolveReviewWorkAuthority(homePath, item); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("resolution error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestStampReviewActionAuthorityRebindsOnlyProducerOwnedSemantics(t *testing.T) {
	homePath, item, store := reviewAuthorityFixture(t, "panel-verdicts")
	createReviewAuthorityWork(t, store, "work_source", "", reviewAuthorityTestNow)
	createReviewAuthorityWork(t, store, "work_area", ReviewAreaSurface("panel-verdicts"), reviewAuthorityTestNow)
	closeReviewAuthorityWork(t, store, "work_source", reviewAuthorityTestNow.Add(time.Minute))
	bindReviewAuthorityItem(t, homePath, &item, "work_source", "work_area")
	seed := reviewAuthorityActionSeed("work_source", item.ID)
	originalCapability := seed.ActionIntent.Capability
	originalTarget := seed.ActionIntent.Target
	item.Actions = []reviewartifact.Action{seed}
	report := reviewartifact.QueueReport{Items: []reviewartifact.QueueItem{item}}

	if err := StampReviewActionAuthority(homePath, &report, reviewAuthorityTestNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := report.Items[0].Actions[0]
	if got.ActionIntent == nil || got.ActionIntent.IDs.WorkID != "work_area" ||
		got.PolicyDecision == nil || got.PolicyAuthority == nil {
		t.Fatalf("terminal source did not select and stamp fallback: %+v", got)
	}
	if got.ActionIntent.Capability != originalCapability || got.ActionIntent.Target != originalTarget {
		t.Fatalf("authority rebind laundered producer semantics: before=%+v after=%+v", seed.ActionIntent, got.ActionIntent)
	}
	if report.Items[0].AuthorityWorkID != "work_area" || report.Items[0].AuthoritySource != ReviewAuthorityFallbackWork {
		t.Fatalf("item did not expose resolved authority: %+v", report.Items[0])
	}
	if err := policy.ValidateAuthorityBundle(*got.ActionIntent, *got.PolicyDecision, *got.PolicyAuthority); err != nil {
		t.Fatalf("stamped fallback bundle invalid: %v", err)
	}

	mismatch := reviewAuthorityActionSeed("work_other_area", item.ID)
	report.Items[0].Actions = []reviewartifact.Action{mismatch}
	if err := StampReviewActionAuthority(homePath, &report, reviewAuthorityTestNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got = report.Items[0].Actions[0]
	if got.PolicyDecision != nil || got.PolicyAuthority != nil {
		t.Fatalf("unrelated action work was laundered through fallback: %+v", got)
	}
}

func TestStampReviewGroupActionAuthoritySignsPostRecountBatchAction(t *testing.T) {
	homePath, item, store := reviewAuthorityFixture(t, "panel-verdicts")
	createReviewAuthorityWork(t, store, "work_area", ReviewAreaSurface("panel-verdicts"), reviewAuthorityTestNow)
	bindReviewAuthorityItem(t, homePath, &item, "", "work_area")
	groupAction := reviewAuthorityActionSeed("work_area", "panel-group")
	report := reviewartifact.QueueReport{
		Items: []reviewartifact.QueueItem{item},
		Groups: []reviewartifact.QueueGroup{{
			Key: "panel-verdicts|prun_test|needs_human", Area: "panel-verdicts",
			ItemIDs: []string{item.ID}, SafeBatchAction: &groupAction,
		}},
	}
	if err := StampReviewGroupActionAuthority(homePath, &report, reviewAuthorityTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := report.Groups[0].SafeBatchAction
	if got == nil || got.PolicyDecision == nil || got.PolicyAuthority == nil ||
		got.ActionIntent == nil || got.ActionIntent.IDs.WorkID != "work_area" {
		t.Fatalf("post-Recount group action remained unsigned: %+v", got)
	}
}

func reviewAuthorityFixture(t *testing.T, area string) (string, reviewartifact.QueueItem, *work.Store) {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", reviewAuthorityTestNow); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(homePath, "reviews", area)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewID := "review_test"
	path := filepath.Join(dir, reviewID+".json")
	if err := os.WriteFile(path, []byte(`{"id":"review_test","status":"review_required"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	return homePath, reviewartifact.QueueItem{Area: area, ID: reviewID, JSONPath: path}, store
}

func bindReviewAuthorityItem(t *testing.T, homePath string, item *reviewartifact.QueueItem, sourceWorkID, fallbackWorkID string) {
	t.Helper()
	binding, _, err := reviewartifact.RecordWorkBinding(homePath, reviewartifact.RecordWorkBindingInput{
		Area: item.Area, ReviewID: item.ID, CoreReviewPath: item.JSONPath,
		SourceWorkID: sourceWorkID, FallbackWorkID: fallbackWorkID,
	}, reviewAuthorityTestNow)
	if err != nil {
		t.Fatal(err)
	}
	item.WorkBinding = &binding
}

func createReviewAuthorityWork(t *testing.T, store *work.Store, workID, surface string, now time.Time) {
	t.Helper()
	if _, err := store.Create(work.CreateInput{
		WorkID: workID, Owner: "review-owner", CanonicalSurface: surface,
		SuccessPredicate: "review owner authority remains explicit",
	}, now); err != nil {
		t.Fatal(err)
	}
}

func closeReviewAuthorityWork(t *testing.T, store *work.Store, workID string, now time.Time) {
	t.Helper()
	if _, err := store.Claim(workID, "review-test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(workID, work.AdvanceInput{Actor: "review-test", To: "running"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Close(workID, work.CloseInput{
		Outcome: "completed", MoveType: "control_plane_repair", Actor: "review-test",
		Summary: "terminal source provenance fixture", Changed: true, ProofRefs: []string{"proof:review-authority"},
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func reviewAuthorityActionSeed(workID, targetID string) reviewartifact.Action {
	intent := &envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent, ID: "act_" + reviewartifact.Digest12(workID+"|"+targetID),
			CreatedAt: reviewAuthorityTestNow.Format(time.RFC3339),
			Producer:  envelope.Producer{Kind: "owner_projection", Name: "compound"},
			IDs:       envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:   "private", Risk: "tier1", Uncertainty: envelope.Uncertainty{Level: "known"},
		},
		Actor: "compound", Capability: "review.panel_verdict", Action: "record_panel_review",
		Target:        envelope.ActionTarget{Kind: "panel_verdict", ID: targetID},
		MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local",
		Preview:      "Record the bounded panel review without applying its memory verdict.",
		RollbackPlan: "Retain the append-only review and record a superseding disposition.",
	}
	return reviewartifact.Action{
		ID: "record_panel_review", Label: "Record review", Command: "compound panel verdict-record",
		ActionIntent: intent,
		AuthorityContext: &policy.AuthorityContext{
			TargetScope: "private", VerifiedRecoverability: policy.RecoverabilityVerified,
			OperatorControlContinuity: policy.OperatorControlContinuous,
			ExpectedReceipt:           "Recorded one bounded panel review.",
			ExpectedProof:             []string{"panel-verdict:" + targetID},
		},
	}
}

func countReviewAuthorityWork(t *testing.T, homePath string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(homePath, "work", "work"))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
