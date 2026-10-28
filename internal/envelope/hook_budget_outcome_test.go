package envelope

import (
	"encoding/json"
	"testing"
)

func TestPreBudgetOutcomeHookEventRemainsByteStable(t *testing.T) {
	const legacyJSON = `{"schema_version":"compound.hook_event.v0.1","id":"hook_legacy_budget_shape","created_at":"2026-08-24T00:00:00Z","producer":{"kind":"cli","name":"compound","version":""},"ids":{},"scope":{},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"harness":"codex","hook_name":"PreToolUse","side_effects":[],"duration_ms":0,"status":"fail_open","fail_open_reason":"budget_exceeded"}`
	var event HookEvent
	if err := json.Unmarshal([]byte(legacyJSON), &event); err != nil {
		t.Fatal(err)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != legacyJSON {
		t.Fatalf("pre-field hook event changed:\n got %s\nwant %s", encoded, legacyJSON)
	}
}
