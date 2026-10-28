package director

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"compound/internal/reviewartifact"
	"compound/internal/work"
)

// seedNeedsYouCard writes a raw needs-you review artifact (status
// review_required derives band needs_you) with an explicit created_at and
// group_key so the director's decision_preparation scanner sees a deterministic
// group. Omitting createdAt (zero) writes no timestamp — an age-unknown card.
func seedNeedsYouCard(t *testing.T, home, area, id, groupKey, status string, createdAt time.Time) {
	t.Helper()
	payload := map[string]any{
		"id":               id,
		"status":           status,
		"evidence_refs":    []string{"compound://evidence/" + id},
		"operator_summary": "Review " + id + ".",
		"context_label":    "Prep fixture " + area,
		"group_key":        groupKey,
	}
	if !createdAt.IsZero() {
		payload["created_at"] = createdAt.UTC().Format(time.RFC3339)
	}
	if _, err := reviewartifact.Write(home, area, id, payload, "  ", "# Review\n"); err != nil {
		t.Fatalf("write needs-you card %s/%s: %v", area, id, err)
	}
}

// seedDecisionBrief writes a valid compound.decision_brief.v0.1 sidecar next to
// a review artifact so the queue attaches a non-nil Brief to that member.
func seedDecisionBrief(t *testing.T, home, area, id string) {
	t.Helper()
	payload := map[string]any{
		"schema_version": reviewartifact.DecisionBriefSchemaVersion,
		"headline":       "Prepared translation for " + id,
		"findings": []map[string]any{
			{"finding": "one cited observation", "evidence_ref": "compound://evidence/" + id},
		},
		"recommendation": map[string]any{"option": "hold", "confidence": 0.5, "rationale": "test fixture"},
		"prepared_by":    "codex",
		"propose_only":   true,
	}
	if _, err := reviewartifact.WriteJSON(home, area, id+".brief.json", payload, "  "); err != nil {
		t.Fatalf("write decision brief %s/%s: %v", area, id, err)
	}
}

func countProposalsByKind(report Report, kind string) int {
	n := 0
	for _, p := range report.Proposals {
		if p.Kind == kind {
			n++
		}
	}
	return n
}

func countSkippedByClassReason(report Report, class, reason string) int {
	n := 0
	for _, s := range report.Skipped {
		if s.Class == class && s.Reason == reason {
			n++
		}
	}
	return n
}

// TestDecisionPreparationProposesStaleUnbriefedNeedsYouGroup is the happy path:
// a needs-you group sitting past the threshold with no wired action and no
// brief becomes exactly one propose-only decision_preparation envelope that
// carries the required evidence and a preparation-class route plan.
func TestDecisionPreparationProposesStaleUnbriefedNeedsYouGroup(t *testing.T) {
	home := seededDirectorHome(t)
	stale := directorTestNow.Add(-3 * time.Hour)
	seedNeedsYouCard(t, home, "prep-area", "prep_a", "prep-area|shared", "review_required", stale)
	seedNeedsYouCard(t, home, "prep-area", "prep_b", "prep-area|shared", "review_required", stale.Add(time.Minute))

	report, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if got := countProposalsByKind(report, "decision_preparation"); got != 1 {
		t.Fatalf("stale unbriefed needs-you group should yield exactly one decision_preparation proposal, got %d: %+v", got, report.Proposals)
	}
	prop := findProposalByKind(report, "decision_preparation")
	if prop.CapabilityClass != "classify-extract" {
		t.Fatalf("decision_preparation must route on a preparation capability class, got %q", prop.CapabilityClass)
	}
	if len(prop.RoutePlan.Chain) == 0 {
		t.Fatalf("decision_preparation proposal missing a preparation route chain: %+v", prop.RoutePlan)
	}
	if prop.WorkState != "proposed" || prop.PlanPath != "" {
		t.Fatalf("dry-run decision_preparation must stay proposed with no plan written: %+v", prop)
	}
	if !strings.Contains(prop.Consequence, "translated") {
		t.Fatalf("consequence should promise the card gets translated: %q", prop.Consequence)
	}
	assertHasPrefix(t, prop.EvidenceRefs, "review_group:prep-area:")
	assertHasRef(t, prop.EvidenceRefs, "review:prep-area/prep_a")
	assertHasRef(t, prop.EvidenceRefs, "review:prep-area/prep_b")
	assertHasPrefix(t, prop.EvidenceRefs, "decision_preparation_stale_since:")
	assertHasPrefix(t, prop.EvidenceRefs, "decision_preparation_no_action:")
	assertHasRef(t, prop.EvidenceRefs, "proposed_by:director")
	assertHasPrefix(t, prop.EvidenceRefs, "director_trigger:decision_preparation:")
}

// TestDecisionPreparationSilenceDefault pins the silence default: any home
// whose needs-you groups are fresh, already briefed, age-unknown, or terminal
// yields ZERO decision_preparation candidates.
func TestDecisionPreparationSilenceDefault(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{"fresh_under_threshold", func(t *testing.T, home string) {
			seedNeedsYouCard(t, home, "prep-area", "fresh_a", "prep-area|shared", "review_required", directorTestNow.Add(-30*time.Minute))
		}},
		{"exactly_at_threshold", func(t *testing.T, home string) {
			seedNeedsYouCard(t, home, "prep-area", "edge_a", "prep-area|shared", "review_required", directorTestNow.Add(-decisionPreparationStaleAfter))
		}},
		{"already_briefed", func(t *testing.T, home string) {
			seedNeedsYouCard(t, home, "prep-area", "briefed_a", "prep-area|shared", "review_required", directorTestNow.Add(-3*time.Hour))
			seedDecisionBrief(t, home, "prep-area", "briefed_a")
		}},
		{"age_unknown", func(t *testing.T, home string) {
			seedNeedsYouCard(t, home, "prep-area", "notime_a", "prep-area|shared", "review_required", time.Time{})
		}},
		{"terminal_not_needs_you", func(t *testing.T, home string) {
			seedNeedsYouCard(t, home, "prep-area", "done_a", "prep-area|shared", "resolved", directorTestNow.Add(-3*time.Hour))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := seededDirectorHome(t)
			tc.setup(t, home)
			report, err := Propose(home, Options{Now: directorTestNow})
			if err != nil {
				t.Fatal(err)
			}
			if got := countProposalsByKind(report, "decision_preparation"); got != 0 {
				t.Fatalf("%s should yield zero decision_preparation candidates, got %d: %+v", tc.name, got, report.Proposals)
			}
		})
	}
}

// TestDecisionPreparationDedupesAgainstExistingBrief proves condition (c): once
// a member carries a brief, the group is no longer proposed (a suppressed
// duplicate is evidence, not another envelope).
func TestDecisionPreparationDedupesAgainstExistingBrief(t *testing.T) {
	home := seededDirectorHome(t)
	stale := directorTestNow.Add(-3 * time.Hour)
	seedNeedsYouCard(t, home, "prep-area", "prep_a", "prep-area|shared", "review_required", stale)

	before, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if countProposalsByKind(before, "decision_preparation") != 1 {
		t.Fatalf("expected one candidate before any brief exists: %+v", before.Proposals)
	}

	seedDecisionBrief(t, home, "prep-area", "prep_a")

	after, err := Propose(home, Options{Now: directorTestNow.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if got := countProposalsByKind(after, "decision_preparation"); got != 0 {
		t.Fatalf("an already-briefed group must not be re-proposed, got %d: %+v", got, after.Proposals)
	}
}

// TestDecisionPreparationDedupesOpenPreparationWork proves the class dedupes
// against open preparation work carrying its director trigger.
func TestDecisionPreparationDedupesOpenPreparationWork(t *testing.T) {
	home := seededDirectorHome(t)
	stale := directorTestNow.Add(-3 * time.Hour)
	seedNeedsYouCard(t, home, "prep-area", "prep_a", "prep-area|shared", "review_required", stale)

	dry, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	prop := findProposalByKind(dry, "decision_preparation")
	if prop == nil {
		t.Fatalf("expected a candidate to seed open work against: %+v", dry.Proposals)
	}

	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := store.Create(work.CreateInput{
		Owner:            "compound-tests",
		Actor:            "operator-test",
		SuccessPredicate: "existing open preparation work blocks duplicate director proposal",
		EvidenceRefs:     []string{prop.TriggerRef},
		State:            "proposed",
	}, directorTestNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(existing.ID, work.AdvanceInput{Actor: "operator-test", To: "ready"}, directorTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	after, err := Propose(home, Options{Now: directorTestNow.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if got := countProposalsByKind(after, "decision_preparation"); got != 0 {
		t.Fatalf("open preparation work should suppress the proposal, got %d: %+v", got, after.Proposals)
	}
	var dup *SuppressedDuplicate
	for i := range after.SuppressedDuplicates {
		if after.SuppressedDuplicates[i].TriggerRef == prop.TriggerRef {
			dup = &after.SuppressedDuplicates[i]
			break
		}
	}
	if dup == nil || dup.Reason != "open_work" || len(dup.ExistingWorkIDs) == 0 {
		t.Fatalf("expected an open_work suppressed duplicate for the trigger: %+v", after.SuppressedDuplicates)
	}
}

// TestDecisionPreparationRespectsPerRunCap proves the class obeys the shared
// per-run cap of five, recording the overflow as cap_reached skips. The cards
// are seeded under the "job-health" area precisely because reviewDebtCandidates
// skips that area and no other director scanner reads reviews/job-health as a
// generic group — so decision_preparation is the ONLY class firing here and the
// cap is exercised in isolation (a normal area would also raise review_debt
// candidates that share the same five-slot round-robin).
func TestDecisionPreparationRespectsPerRunCap(t *testing.T) {
	home := seededDirectorHome(t)
	stale := directorTestNow.Add(-3 * time.Hour)
	for i := 0; i < 7; i++ {
		seedNeedsYouCard(t, home, "job-health", fmt.Sprintf("prep_%02d", i), fmt.Sprintf("job-health|g%02d", i), "review_required", stale)
	}

	report, err := Propose(home, Options{Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	if got := countProposalsByKind(report, "decision_preparation"); got != 5 {
		t.Fatalf("decision_preparation should cap at five proposals, got %d: %+v", got, report.Proposals)
	}
	if got := countSkippedByClassReason(report, "decision_preparation", "cap_reached"); got != 2 {
		t.Fatalf("two decision_preparation candidates should skip as cap_reached, got %d: %+v", got, report.Skipped)
	}
	for _, s := range report.Skipped {
		if s.Class == "decision_preparation" && len(s.EvidenceRefs) == 0 {
			t.Fatalf("cap skip should preserve evidence refs: %+v", s)
		}
	}
}

// TestDecisionPreparationCannotAdvancePastProposedWithoutOperator is the
// propose-only falsifier: an admitted decision_preparation proposal stays
// proposed and refuses a non-operator advance.
func TestDecisionPreparationCannotAdvancePastProposedWithoutOperator(t *testing.T) {
	home := seededDirectorHome(t)
	seedDirectorApplyGrant(t, home, directorTestNow)
	stale := directorTestNow.Add(-3 * time.Hour)
	seedNeedsYouCard(t, home, "prep-area", "prep_a", "prep-area|shared", "review_required", stale)

	report, err := Propose(home, Options{Apply: true, Now: directorTestNow})
	if err != nil {
		t.Fatal(err)
	}
	prop := findProposalByKind(report, "decision_preparation")
	if prop == nil || prop.PlanPath == "" {
		t.Fatalf("apply run should record one decision_preparation create-plan preview: %+v", report.Proposals)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := store.AdmitCreatePlanPath(prop.PlanPath, directorTestNow.Add(time.Minute), work.AdmitCreatePlanOptions{Actor: "operator-test"})
	if err != nil {
		t.Fatal(err)
	}
	if admitted.WorkState != "proposed" {
		t.Fatalf("admitted decision_preparation work should remain proposed: %+v", admitted)
	}
	if _, err := store.Advance(admitted.WorkID, work.AdvanceInput{Actor: "codex", To: "ready"}, directorTestNow.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "operator provenance") {
		t.Fatalf("non-operator advance should fail on operator provenance, got %v", err)
	}
	after, err := store.Get(admitted.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "proposed" {
		t.Fatalf("failed non-operator advance mutated decision_preparation work: %+v", after)
	}
}

// TestEvaluateBriefableGroup gives the three deterministic triggers real teeth
// at the pure-core level — the generic queue reader never annotates actions, so
// the wired-action guard can only be exercised here.
func TestEvaluateBriefableGroup(t *testing.T) {
	now := directorTestNow
	stale := now.Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	fresh := now.Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	edge := now.Add(-decisionPreparationStaleAfter).UTC().Format(time.RFC3339)

	mkItem := func(id, createdAt string, brief *reviewartifact.DecisionBrief, actions []reviewartifact.Action) reviewartifact.QueueItem {
		return reviewartifact.QueueItem{
			Area:      "prep-area",
			ID:        id,
			Status:    "review_required",
			Band:      reviewartifact.BandNeedsYou,
			CreatedAt: createdAt,
			Brief:     brief,
			Actions:   actions,
		}
	}
	mkGroup := func(band string, memberIDs []string, safe *reviewartifact.Action) reviewartifact.QueueGroup {
		return reviewartifact.QueueGroup{
			Area:            "prep-area",
			Key:             "prep-area|shared",
			Band:            band,
			ItemIDs:         memberIDs,
			SafeBatchAction: safe,
			TerminalReason:  "owner has not exposed a safe action for this decision group",
		}
	}
	act := &reviewartifact.Action{ID: "apply", Label: "Apply", Command: "compound apply"}

	cases := []struct {
		name   string
		group  reviewartifact.QueueGroup
		items  []reviewartifact.QueueItem
		wantOK bool
	}{
		{
			name:   "eligible",
			group:  mkGroup(reviewartifact.BandNeedsYou, []string{"prep_b", "prep_a"}, nil),
			items:  []reviewartifact.QueueItem{mkItem("prep_a", stale, nil, nil), mkItem("prep_b", stale, nil, nil)},
			wantOK: true,
		},
		{
			name:   "not_needs_you",
			group:  mkGroup(reviewartifact.BandWatching, []string{"prep_a"}, nil),
			items:  []reviewartifact.QueueItem{mkItem("prep_a", stale, nil, nil)},
			wantOK: false,
		},
		{
			name:   "safe_batch_action_wired",
			group:  mkGroup(reviewartifact.BandNeedsYou, []string{"prep_a"}, act),
			items:  []reviewartifact.QueueItem{mkItem("prep_a", stale, nil, nil)},
			wantOK: false,
		},
		{
			name:   "member_action_wired",
			group:  mkGroup(reviewartifact.BandNeedsYou, []string{"prep_a"}, nil),
			items:  []reviewartifact.QueueItem{mkItem("prep_a", stale, nil, []reviewartifact.Action{*act})},
			wantOK: false,
		},
		{
			name:   "already_briefed",
			group:  mkGroup(reviewartifact.BandNeedsYou, []string{"prep_a"}, nil),
			items:  []reviewartifact.QueueItem{mkItem("prep_a", stale, &reviewartifact.DecisionBrief{}, nil)},
			wantOK: false,
		},
		{
			name:   "fresh_under_threshold",
			group:  mkGroup(reviewartifact.BandNeedsYou, []string{"prep_a"}, nil),
			items:  []reviewartifact.QueueItem{mkItem("prep_a", fresh, nil, nil)},
			wantOK: false,
		},
		{
			name:   "exactly_at_threshold",
			group:  mkGroup(reviewartifact.BandNeedsYou, []string{"prep_a"}, nil),
			items:  []reviewartifact.QueueItem{mkItem("prep_a", edge, nil, nil)},
			wantOK: false,
		},
		{
			name:   "age_unknown",
			group:  mkGroup(reviewartifact.BandNeedsYou, []string{"prep_a"}, nil),
			items:  []reviewartifact.QueueItem{mkItem("prep_a", "", nil, nil)},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := map[string]reviewartifact.QueueItem{}
			for _, it := range tc.items {
				idx[it.Area+"\x00"+it.ID] = it
			}
			info, ok := evaluateBriefableGroup(tc.group, idx, now, decisionPreparationStaleAfter)
			if ok != tc.wantOK {
				t.Fatalf("evaluateBriefableGroup ok = %v, want %v (info=%+v)", ok, tc.wantOK, info)
			}
			if !ok {
				return
			}
			if len(info.MemberIDs) != 2 || info.MemberIDs[0] != "prep_a" || info.MemberIDs[1] != "prep_b" {
				t.Fatalf("member ids should be sorted [prep_a prep_b], got %v", info.MemberIDs)
			}
			assertHasRef(t, info.EvidenceRefs, "review_group:prep-area:prep-area|shared")
			assertHasRef(t, info.EvidenceRefs, "review:prep-area/prep_a")
			assertHasRef(t, info.EvidenceRefs, "review:prep-area/prep_b")
			assertHasPrefix(t, info.EvidenceRefs, "decision_preparation_stale_since:")
			assertHasPrefix(t, info.EvidenceRefs, "decision_preparation_no_action:")

			cand := decisionPreparationCandidate(info)
			if cand.Kind != "decision_preparation" || cand.CapabilityClass != "classify-extract" {
				t.Fatalf("candidate kind/class = %q/%q", cand.Kind, cand.CapabilityClass)
			}
			wantCmd := []string{"compound", "reviews", "brief-plan", "--item", "prep_a", "--json"}
			if strings.Join(cand.Command, " ") != strings.Join(wantCmd, " ") {
				t.Fatalf("work chain command = %v, want %v", cand.Command, wantCmd)
			}
		})
	}
}
