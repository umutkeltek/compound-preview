package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStateDeltaUnknownJSONAndLegacyBytes(t *testing.T) {
	legacy := `{"changed":false,"summary":"bounded result","files_changed":[],"artifacts":[],"verification":[]}`
	var delta StateDelta
	if err := json.Unmarshal([]byte(legacy), &delta); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(delta)
	if err != nil || string(body) != legacy {
		t.Fatalf("legacy bytes drifted: %s %v", body, err)
	}
	delta.ChangeKnowledge = "unknown"
	body, err = json.Marshal(delta)
	if err != nil || strings.Contains(string(body), `"changed"`) || !strings.Contains(string(body), `"change_knowledge":"unknown"`) {
		t.Fatalf("uncertainty asserted a boolean: %s %v", body, err)
	}
	var again StateDelta
	if err = json.Unmarshal(body, &again); err != nil || again.ChangeKnowledge != "unknown" {
		t.Fatal(err)
	}
	for _, value := range []string{"false", "true", "null"} {
		if err = json.Unmarshal([]byte(`{"change_knowledge":"unknown","changed":`+value+`}`), &again); err == nil {
			t.Fatal("accepted unknown with changed", value)
		}
	}
}

func TestReceiptUnknownEffectsCannotComplete(t *testing.T) {
	r := Receipt{Header: hdr(SVReceipt, "rec_unknown"), Outcome: "failed", MoveType: "audit_only", StateDelta: StateDelta{ChangeKnowledge: "unknown", Summary: "remote outcome unknown", FilesChanged: []string{}, Artifacts: []string{}, Verification: []string{}}, Cost: ReceiptCost{ModelCalls: []string{}}, Skills: ReceiptSkills{Used: []string{}, Missed: []string{}, Candidate: []string{}}, Memory: ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}}, Next: ReceiptNext{SuggestedWork: []string{}}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"completed", "no_op"} {
		r.Outcome = outcome
		if err := r.Validate(); err == nil {
			t.Fatal("unknown effects accepted as", outcome)
		}
	}
}
