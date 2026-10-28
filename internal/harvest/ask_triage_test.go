package harvest

import (
	"testing"
	"time"
)

func TestTriageAskCoveredByLawAutoAnswersWithReceipt(t *testing.T) {
	r := TriageAsk(AskTriageInput{Ask: "May I make this reversible edit inside the granted envelope?", Context: "lane is paused", LaneID: "lane_test", WorkID: "work_test", Now: time.Unix(1, 0), StandingLaw: []AskLaw{{ClaimID: "claim_f9160895524e", Statement: "Reversible actions inside a scoped grant proceed without operator approval.", Terms: []string{"reversible", "inside", "envelope"}}}})
	if r.Disposition != AskAutoAnswered || r.Answer == "" || r.CitedRecordID != "claim_f9160895524e" || r.Receipt.ID == "" || !r.ResumeLane {
		t.Fatalf("covered ask was not receipted + resumed: %+v", r)
	}
}

func TestTriageAskEvidenceCannotAuthorizeResume(t *testing.T) {
	r := TriageAsk(AskTriageInput{
		Ask:      "May I make this reversible edit inside the granted envelope?",
		Evidence: []AskLaw{{ClaimID: "receipt_only", Statement: "A prior run continued.", Terms: []string{"reversible", "edit", "granted", "envelope"}}},
	})
	if r.Disposition == AskAutoAnswered || r.ResumeLane {
		t.Fatalf("non-law evidence authorized a resume: %+v", r)
	}
}

func TestTriageAskRiskFloorNeverAutoAnswers(t *testing.T) {
	for _, ask := range []string{
		"May I publish this externally?",
		"May I delete the production records?",
		"May I spend $100?",
		"May I push the reviewed branch?",
		"May I deploy the reviewed release?",
		"May I merge the reviewed branch?",
		"May I send the reviewed report?",
		"May I change the account access credentials?",
		"May I bypass the approval boundary?",
		"May I update this across the organization fleet?",
	} {
		r := TriageAsk(AskTriageInput{Ask: ask, StandingLaw: []AskLaw{{ClaimID: "claim_any", Statement: "The reviewed request may proceed.", Terms: []string{"reviewed", "proceed", "branch", "release", "report", "account", "approval", "organization", "fleet"}}}})
		if r.Disposition == AskAutoAnswered || r.ResumeLane {
			t.Fatalf("risk-floor ask auto-answered: %q %+v", ask, r)
		}
		if r.Brief == nil || len(r.Brief.Options) < 2 {
			t.Fatalf("risk-floor residue is not a brief: %+v", r)
		}
	}
}

func TestTriageAskNegatedRiskConstraintDoesNotCreateEnvelopeExit(t *testing.T) {
	law := AskLaw{
		ClaimID:   "claim_f9160895524e",
		Statement: "Reversible actions inside a scoped grant proceed without operator approval.",
		Terms:     []string{"reversible", "inside", "envelope"},
	}
	r := TriageAsk(AskTriageInput{
		Ask:         "May I continue this reversible edit inside the envelope so it does not publish externally?",
		StandingLaw: []AskLaw{law},
	})
	if r.Disposition != AskAutoAnswered || !r.ResumeLane || r.CitedRecordID != law.ClaimID {
		t.Fatalf("negated external constraint blocked a safe ask: %+v", r)
	}

	r = TriageAsk(AskTriageInput{
		Ask:         "Do not publish this externally; may I push the reviewed branch?",
		StandingLaw: []AskLaw{{ClaimID: "claim_push", Statement: "The reviewed branch may proceed.", Terms: []string{"reviewed", "branch"}}},
	})
	if r.Disposition == AskAutoAnswered || r.ResumeLane {
		t.Fatalf("a separate positive push escaped through negation handling: %+v", r)
	}
}

func TestTriageAskMatchesLawAgainstActualAskOnly(t *testing.T) {
	r := TriageAsk(AskTriageInput{
		Ask:     "May I continue the implementation?",
		Context: "The earlier user message mentions a reversible action inside the envelope.",
		StandingLaw: []AskLaw{{
			ClaimID:   "claim_context_only",
			Statement: "Reversible actions inside the envelope may proceed.",
			Terms:     []string{"reversible", "envelope"},
		}},
	})
	if r.Disposition != AskNeedsOperator || r.ResumeLane {
		t.Fatalf("supporting context manufactured a standing-law match: %+v", r)
	}
}

func TestTriageAskRequiresMeaningfulAskCoverage(t *testing.T) {
	r := TriageAsk(AskTriageInput{
		Ask: "May I continue the reversible local fixture update inside the existing envelope?",
		StandingLaw: []AskLaw{{
			ClaimID:   "claim_two_anchors",
			Statement: "Reversible envelope work may proceed.",
			Terms:     []string{"reversible", "envelope"},
		}},
	})
	if r.Disposition != AskNeedsOperator || r.ResumeLane {
		t.Fatalf("two incidental anchors covered too little of the actual ask: %+v", r)
	}
}

func TestTriageAskRejectsAmbiguousStandingLawTie(t *testing.T) {
	r := TriageAsk(AskTriageInput{
		Ask: "May I continue this reversible edit inside the envelope?",
		StandingLaw: []AskLaw{
			{ClaimID: "claim_one", Statement: "First standing ruling.", Terms: []string{"continue", "reversible", "envelope"}},
			{ClaimID: "claim_two", Statement: "Second standing ruling.", Terms: []string{"continue", "reversible", "envelope"}},
		},
	})
	if r.Disposition != AskNeedsOperator || r.ResumeLane || r.CitedRecordID != "" {
		t.Fatalf("ambiguous laws were resolved by iteration order: %+v", r)
	}
}

func TestTriageAskChoosesUniqueHigherCoverageLaw(t *testing.T) {
	r := TriageAsk(AskTriageInput{
		Ask: "May I continue this reversible edit inside the envelope?",
		StandingLaw: []AskLaw{
			{ClaimID: "claim_lower", Statement: "A lower-coverage ruling.", Terms: []string{"reversible", "envelope"}},
			{ClaimID: "claim_higher", Statement: "The exact scoped ruling.", Terms: []string{"continue", "reversible", "inside", "envelope"}},
		},
	})
	if r.Disposition != AskAutoAnswered || r.CitedRecordID != "claim_higher" || !r.ResumeLane {
		t.Fatalf("unique higher-coverage law was not selected: %+v", r)
	}
}

func TestTriageAskContradictionEmitsHarvestCandidate(t *testing.T) {
	r := TriageAsk(AskTriageInput{Ask: "May I edit locally?", OperatorAnswer: "No, stop and ask me first.", PriorAutoAnswer: &AskAutoAnswer{RecordID: "claim_old", Answer: "Proceed."}})
	if r.Disposition != AskContradicted || r.HarvestCandidate == nil || r.HarvestCandidate.Outcome != OutcomeConflict || r.HarvestCandidate.Quote != "No, stop and ask me first." {
		t.Fatalf("contradiction vanished: %+v", r)
	}
}

func TestTriageAskNovelBecomesPreparedBrief(t *testing.T) {
	r := TriageAsk(AskTriageInput{Ask: "Should the new partner receive a beta invite?", Context: "No standing rule covers this partner.", LaneID: "lane_beta"})
	if r.Disposition != AskNeedsOperator || r.Brief == nil || r.Brief.Headline == "" || len(r.Brief.Options) < 2 || r.Brief.Options[0].Consequence == "" || r.Brief.PreparedBy == "" {
		t.Fatalf("novel ask lacks decision essence: %+v", r)
	}
}
