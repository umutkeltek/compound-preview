package capgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/reviewartifact"
)

// Red-first falsifiers for every remaining hand-copied terminal-status set
// this package carried after reviewartifact.TerminalStatus became the owner
// (commit c896db46 collapsed seven copies and named follow-up debt; the audit
// for this cycle found FOUR in capgraph, not two). All omit "completed", so a
// completed artifact is billed to the operator as an open decision — or, for
// skill-surfacing, re-offered a one-shot import — the same defect class, in
// the same words, one package over:
//
//	evaluation_review_queue.go   six-word switch      -> no readiness at all
//	external_skill_review.go     six-word switch      -> blocked on YAML draft
//	surfacing_misses.go          six-word map         -> import action re-offered
//	lifecycle_proposal.go:227    five-word else-if    -> needs_review + actions
//
// The decision-report switch (external_skill_review.go, report.Status with
// "recorded"/"candidate_recorded") is a different predicate over a different
// value with word-specific next-actions and is intentionally not touched.
//
// Hand-copies still standing elsewhere, cited as follow-up debt (each needs
// its own red-first cycle; none is touched here): panel/review_queue.go:96,
// product/review_queue.go:47 and :100, routeevidence/review_queue.go:102,
// cmd/compound orient_operator.go sourceControlReviewItemRecorded (:849,
// :1274), reviewartifact/operator_card.go BandForStatus (next commit pair),
// and home/archive.go terminalReviewStatus — that last one is a documented
// import-cycle exception that already carries "completed" and a sync note.
//
// Red output at this commit:
//
//	TestSkillCandidateQueueBillsCompletedAsTerminal: Readiness = "blocked", want "no_action"
//	TestCapabilityEvaluationQueueBillsCompletedAsTerminal: Readiness = "", want "no_action"
//	TestSkillSurfacingOffersNoImportActionForTerminalWords/completed: offered 1 action(s), want none
//	TestCapabilityLifecycleProposalQueueBillsCompletedAsTerminal: Readiness = "needs_review", want "no_action"

// TestSkillCandidateQueueBillsCompletedAsTerminal pins the skill-candidate
// annotator: a completed artifact must land on no_action, not on the
// fill-review-YAML / blocked path. With the stale six-word set the item falls
// to default ("fill review YAML...") and, with no YAML path, is then marked
// blocked — finished work rendered as an operator debt.
func TestSkillCandidateQueueBillsCompletedAsTerminal(t *testing.T) {
	item := &reviewartifact.QueueItem{
		Area:   skillCandidateReviewArea,
		ID:     "skill_completed_fixture",
		Status: "completed",
	}
	annotateSkillCandidateQueueItem(item)
	if item.Readiness != "no_action" {
		t.Fatalf("Readiness = %q, want \"no_action\": a completed skill-candidate artifact is finished work, not a pending operator decision (next_action=%q)", item.Readiness, item.NextAction)
	}
}

// TestCapabilityEvaluationQueueBillsCompletedAsTerminal pins the capability-
// evaluation annotator. With the stale six-word set, "completed" matches no
// switch case at all, so the item passes through the annotator entirely
// unannotated — no readiness, no card, no next action.
func TestCapabilityEvaluationQueueBillsCompletedAsTerminal(t *testing.T) {
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, []byte(`{"id":"eval_completed_fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	item := &reviewartifact.QueueItem{
		Area:     capabilityEvaluationReviewArea,
		ID:       "eval_completed_fixture",
		Status:   "completed",
		JSONPath: planPath,
	}
	annotateCapabilityEvaluationQueueItem(item)
	if item.Readiness != "no_action" {
		t.Fatalf("Readiness = %q, want \"no_action\": a completed capability-evaluation artifact is finished work, not a pending operator decision (next_action=%q)", item.Readiness, item.NextAction)
	}
}

// TestSkillSurfacingOffersNoImportActionForTerminalWords pins the surfacing
// annotator: a terminal item must carry no import action — the card is closed
// and the one-shot import stays one-shot. With the stale six-word map a
// "completed" item falls through and is re-offered the import command.
func TestSkillSurfacingOffersNoImportActionForTerminalWords(t *testing.T) {
	for _, status := range []string{"ok", "applied", "resolved", "accepted", "recorded", "candidate_recorded", "completed"} {
		t.Run(status, func(t *testing.T) {
			item := &reviewartifact.QueueItem{Area: skillSurfacingReviewArea, ID: "surf_" + status, Status: status, JSONPath: "surfacing.json"}
			annotateSkillSurfacingItem(item)
			if len(item.Actions) != 0 {
				t.Fatalf("surfacing annotator: status %q offered %d action(s), want none: a terminal artifact's one-shot import must not be re-offered", status, len(item.Actions))
			}
		})
	}
}

// TestCapabilityLifecycleProposalQueueBillsCompletedAsTerminal pins the
// lifecycle-proposal annotator. The five-word else-if omits "completed" (and
// never carried "candidate_recorded"), so such a proposal is annotated
// needs_review with live decision actions instead of being filed away.
func TestCapabilityLifecycleProposalQueueBillsCompletedAsTerminal(t *testing.T) {
	homePath := t.TempDir()
	snapshot := lifecycleReviewFixture(t, homePath)

	proposal := snapshot.Proposal
	proposal.Status = "completed"
	data, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	item := &reviewartifact.QueueItem{
		Area:     CapabilityLifecycleProposalReviewArea,
		ID:       proposal.ID,
		Status:   "completed",
		JSONPath: snapshot.Path,
	}
	annotateCapabilityLifecycleProposalItem(item, lifecycleReviewNow)
	if item.Readiness != "no_action" {
		t.Fatalf("Readiness = %q, want \"no_action\": a completed lifecycle proposal is finished work, not a pending operator decision (next_action=%q)", item.Readiness, item.NextAction)
	}
}

// TestQueueAnnotatorsCoverEveryTerminalWord pins all four annotators against
// the full owner set, so a future terminal word added to reviewartifact cannot
// be silently billed by a stale capgraph copy. Each word here is a status some
// producer in this repo writes to mean "finished".
func TestQueueAnnotatorsCoverEveryTerminalWord(t *testing.T) {
	for _, status := range []string{"ok", "applied", "resolved", "accepted", "recorded", "candidate_recorded", "completed"} {
		t.Run(status, func(t *testing.T) {
			item := &reviewartifact.QueueItem{Area: skillCandidateReviewArea, ID: "skill_" + status, Status: status}
			annotateSkillCandidateQueueItem(item)
			if item.Readiness != "no_action" {
				t.Fatalf("skill annotator: status %q Readiness = %q, want no_action", status, item.Readiness)
			}

			planPath := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(planPath, []byte(`{"id":"eval_`+status+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			evalItem := &reviewartifact.QueueItem{Area: capabilityEvaluationReviewArea, ID: "eval_" + status, Status: status, JSONPath: planPath}
			annotateCapabilityEvaluationQueueItem(evalItem)
			if evalItem.Readiness != "no_action" {
				t.Fatalf("evaluation annotator: status %q Readiness = %q, want no_action", status, evalItem.Readiness)
			}
		})
	}
}
