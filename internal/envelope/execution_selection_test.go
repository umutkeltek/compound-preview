package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

func validSelection() *ExecutionSelection {
	return &ExecutionSelection{
		LaunchProfileID:       "claude-minimax",
		LaunchProfileRevision: "sha256:eeb7c22957a53d0876ede9bf0a28515c27665f5f2e329e83d93768d715400f4a",
		Harness:               "claude_code",
		ProviderRoute:         "minimax/MiniMax-M3",
		RequestedModelID:      "MiniMax-M3",
		RequestedEffort:       "xhigh",
		Source:                "explicit",
		Revision:              1,
		SelectedBy:            "claude_code:orchestrator",
		SelectedAt:            "2026-09-11T18:00:00Z",
	}
}

func selectionWork(id string) WorkEnvelope {
	return WorkEnvelope{
		Header:             hdr(SVWork, id),
		Owner:              "orchestrator:claude_minimax",
		State:              "ready",
		SuccessPredicate:   "selection survives reopen",
		NonGoals:           []string{},
		ActionIntentIDs:    []string{},
		ArtifactIDs:        []string{},
		VerificationIDs:    []string{},
		MemoryCandidateIDs: []string{},
	}
}

// A legacy envelope without the field stays valid and re-marshals without it;
// a complete selection is accepted; each required field is named on refusal.
func TestExecutionSelectionIsOptionalAndCompleteWhenPresent(t *testing.T) {
	legacy := selectionWork("work_sel_legacy")
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy work without execution_selection must stay valid: %v", err)
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "execution_selection") {
		t.Fatalf("absent selection must not appear in marshaled bytes: %s", raw)
	}

	current := selectionWork("work_sel_current")
	current.ExecutionSelection = validSelection()
	if err := current.Validate(); err != nil {
		t.Fatalf("complete selection rejected: %v", err)
	}

	cases := map[string]func(s *ExecutionSelection){
		"execution_selection.launch_profile_id": func(s *ExecutionSelection) { s.LaunchProfileID = "" },
		"execution_selection.harness":           func(s *ExecutionSelection) { s.Harness = "" },
		"execution_selection.source":            func(s *ExecutionSelection) { s.Source = "guess" },
		"execution_selection.revision":          func(s *ExecutionSelection) { s.Revision = 0 },
		"execution_selection.selected_by":       func(s *ExecutionSelection) { s.SelectedBy = "" },
		"execution_selection.selected_at":       func(s *ExecutionSelection) { s.SelectedAt = "" },
		"execution_selection.launch_profile_revision": func(s *ExecutionSelection) {
			s.LaunchProfileRevision = "eeb7c229"
		},
	}
	for field, mutate := range cases {
		w := selectionWork("work_sel_bad")
		w.ExecutionSelection = validSelection()
		mutate(w.ExecutionSelection)
		err := w.Validate()
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: expected refusal naming the field, got %v", field, err)
		}
	}
}

// The saved selection round-trips byte-for-byte through JSON: this is the
// property "exit, reopen, same combination" rests on.
func TestExecutionSelectionRoundTrips(t *testing.T) {
	w := selectionWork("work_sel_rt")
	w.ExecutionSelection = validSelection()
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var back WorkEnvelope
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ExecutionSelection == nil || *back.ExecutionSelection != *w.ExecutionSelection {
		t.Fatalf("selection did not round-trip: %+v vs %+v", back.ExecutionSelection, w.ExecutionSelection)
	}
}
