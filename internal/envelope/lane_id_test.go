package envelope

import (
	"encoding/json"
	"testing"
)

func TestOldReceiptWithoutProducerLaneIDUnmarshalsCleanly(t *testing.T) {
	raw := []byte(`{
		"schema_version": "compound.receipt.v0.1",
		"id": "rec_legacy_fixture",
		"created_at": "2026-07-09T00:00:00Z",
		"producer": {"kind": "cli", "name": "compound"},
		"ids": {"work_id": "work_fixture"},
		"authority": {"source_class": "runtime", "status": "current"},
		"privacy": "private",
		"risk": "tier1",
		"evidence_refs": [],
		"uncertainty": {"level": "known"},
		"outcome": "completed",
		"move_type": "memory_delta",
		"state_delta": {"changed": true, "summary": "fixture"},
		"cost": {},
		"skills": {},
		"memory": {"promoted": ["claim_fixture"]},
		"next": {}
	}`)
	var rec Receipt
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("legacy receipt unmarshal: %v", err)
	}
	if rec.Producer.LaneID != "" {
		t.Fatalf("legacy receipt producer lane_id = %q, want empty additive default", rec.Producer.LaneID)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("legacy receipt validate: %v", err)
	}
}
