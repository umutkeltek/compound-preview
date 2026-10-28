package policy

import (
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestAutonomyHaltUnsetByDefault(t *testing.T) {
	home := t.TempDir()
	halted, reason, err := AutonomyHalted(home, "job_health")
	if err != nil {
		t.Fatalf("AutonomyHalted: %v", err)
	}
	if halted || reason != "" {
		t.Fatalf("missing halt file must read as not halted, got halted=%t reason=%q", halted, reason)
	}
	halt, source, err := LoadAutonomyHalt(home)
	if err != nil {
		t.Fatalf("LoadAutonomyHalt: %v", err)
	}
	if source != "unset" || halt.Global || len(halt.DisabledClasses) != 0 {
		t.Fatalf("unset home should be empty halt, got %+v source=%q", halt, source)
	}
}

func TestAutonomyHaltGlobalBlocksEveryClass(t *testing.T) {
	home := t.TempDir()
	if err := WriteAutonomyHalt(home, AutonomyHalt{Global: true, Reason: "incident review"}); err != nil {
		t.Fatalf("WriteAutonomyHalt: %v", err)
	}
	for _, class := range []string{"job_health", "source_connection_readiness", "", "director"} {
		halted, reason, err := AutonomyHalted(home, class)
		if err != nil {
			t.Fatalf("AutonomyHalted(%q): %v", class, err)
		}
		if !halted {
			t.Fatalf("global halt must block class %q", class)
		}
		if reason == "" {
			t.Fatalf("global halt must carry a reason for class %q", class)
		}
	}
}

func TestAutonomyHaltPerClassBlocksOnlyThatClass(t *testing.T) {
	home := t.TempDir()
	if err := WriteAutonomyHalt(home, AutonomyHalt{
		DisabledClasses: map[string]string{"job_health": "noisy"},
	}); err != nil {
		t.Fatalf("WriteAutonomyHalt: %v", err)
	}
	blocked, reason, err := AutonomyHalted(home, "job_health")
	if err != nil {
		t.Fatalf("AutonomyHalted job_health: %v", err)
	}
	if !blocked {
		t.Fatalf("per-class disable must block its own class")
	}
	if reason == "" {
		t.Fatalf("per-class halt must carry a reason")
	}
	other, _, err := AutonomyHalted(home, "source_connection_readiness")
	if err != nil {
		t.Fatalf("AutonomyHalted other: %v", err)
	}
	if other {
		t.Fatalf("per-class disable of job_health must NOT block a different class")
	}
}

func TestAutonomyHaltDecisionIsRecordableDeny(t *testing.T) {
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_halt_test",
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "auto-admit:director",
		Capability:    "work.advance",
		Action:        "advance_work_to_admitted",
		Target:        envelope.ActionTarget{Kind: "work", ID: "work_x", Path: "work/work/work_x.json"},
		MutationClass: "local_reversible",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "advance work_x proposed -> admitted",
		RollbackPlan:  "leave proposed",
	}
	pd := AutonomyHaltDecision(ai, "autonomy_halt(global): incident review", time.Now())
	if pd.Outcome != "deny" {
		t.Fatalf("halt decision must be deny, got %q", pd.Outcome)
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("halt decision must validate: %v", err)
	}
	if len(pd.Reasons) == 0 || pd.Reasons[0] == "" {
		t.Fatalf("halt decision must record a reason: %+v", pd.Reasons)
	}
}
