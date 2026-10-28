package latenttax

import (
	"strings"
	"testing"
)

// fourArms builds a valid factorial dose with a filler arm matched to the instruction
// arm's token length. instrBlock/fillerBlock let a test deliberately mismatch them.
func fourArms(dose int, instrBlock, fillerBlock string) DoseArms {
	return DoseArms{
		Dose: dose,
		Arms: []Arm{
			{ID: "oic", Kind: ArmObligationInContext, ContextBlock: instrBlock, TokenLength: countTokens(instrBlock), GateEnabled: false},
			{ID: "gate", Kind: ArmGateOnly, ContextBlock: "", TokenLength: 0, GateEnabled: true},
			{ID: "both", Kind: ArmBoth, ContextBlock: instrBlock, TokenLength: countTokens(instrBlock), GateEnabled: true},
			{ID: "fill", Kind: ArmFiller, ContextBlock: fillerBlock, TokenLength: countTokens(fillerBlock), GateEnabled: false},
		},
	}
}

func validArmConfig() ArmConfig {
	// dose 20: 20-token instruction block, 20-token filler block (exact match).
	instr := strings.TrimSpace(strings.Repeat("obligation ", 20))
	filler := strings.TrimSpace(strings.Repeat("lorem ", 20))
	return ArmConfig{
		SchemaVersion: ArmConfigSchemaVersion,
		ConfigID:      "cfg-valid",
		ToleranceFrac: 0.05,
		Doses:         []DoseArms{fourArms(20, instr, filler)},
	}
}

func TestArmConfigValidPasses(t *testing.T) {
	if err := validArmConfig().Validate(); err != nil {
		t.Fatalf("valid arm config should pass, got: %v", err)
	}
}

// TestDoseMismatchedFillerFailsValidation is the headline gate: a filler arm whose
// token length diverges from its matched instruction arm beyond tolerance MUST be a
// deterministic config error. This test fails if the dose-matching check is absent —
// i.e. if "context length wearing an instruction costume" can slip through as a tax.
func TestDoseMismatchedFillerFailsValidation(t *testing.T) {
	instr := strings.TrimSpace(strings.Repeat("obligation ", 20)) // 20 tokens
	shortFiller := "lorem ipsum dolor"                            // 3 tokens — way under 5% of 20
	ac := validArmConfig()
	ac.Doses = []DoseArms{fourArms(20, instr, shortFiller)}
	err := ac.Validate()
	if err == nil {
		t.Fatal("dose-mismatched filler arm must fail validation deterministically")
	}
	if !strings.Contains(err.Error(), "filler") || !strings.Contains(err.Error(), "tolerance") {
		t.Fatalf("error should name the filler dose-match failure, got: %v", err)
	}
	// Deterministic: the same bad config fails the same way every time.
	if err2 := ac.Validate(); err2.Error() != err.Error() {
		t.Fatalf("validation must be deterministic; got %q then %q", err, err2)
	}
}

func TestArmTokenLengthMustMatchBlock(t *testing.T) {
	ac := validArmConfig()
	ac.Doses[0].Arms[0].TokenLength = 999 // lie about the block length
	if err := ac.Validate(); err == nil || !strings.Contains(err.Error(), "token_length") {
		t.Fatalf("declared token_length that mismatches the block must fail, got: %v", err)
	}
}

func TestGateOnlyArmMustBeEmptyContext(t *testing.T) {
	ac := validArmConfig()
	ac.Doses[0].Arms[1].ContextBlock = "leaked rule"
	ac.Doses[0].Arms[1].TokenLength = 2
	if err := ac.Validate(); err == nil || !strings.Contains(err.Error(), "gate_only") {
		t.Fatalf("gate_only arm with a non-empty context block must fail, got: %v", err)
	}
}

func TestMissingArmKindFails(t *testing.T) {
	ac := validArmConfig()
	ac.Doses[0].Arms = ac.Doses[0].Arms[:3] // drop the filler arm
	if err := ac.Validate(); err == nil || !strings.Contains(err.Error(), "filler") {
		t.Fatalf("a dose missing the filler arm must fail, got: %v", err)
	}
}

func TestDoseZeroBaselineMatches(t *testing.T) {
	ac := validArmConfig()
	ac.Doses = []DoseArms{fourArms(0, "", "")} // baseline: empty instruction + empty filler
	if err := ac.Validate(); err != nil {
		t.Fatalf("dose-0 baseline with empty matched arms should pass, got: %v", err)
	}
}

func TestBothArmMustMatchInstructionLength(t *testing.T) {
	ac := validArmConfig()
	// make the both arm carry a different-length block than obligation_in_context
	ac.Doses[0].Arms[2].ContextBlock = "shorter block here"
	ac.Doses[0].Arms[2].TokenLength = 3
	if err := ac.Validate(); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("both arm length diverging from obligation_in_context must fail, got: %v", err)
	}
}

func TestPlanRunsMatrixIsDeterministic(t *testing.T) {
	ts := TaskSet{
		SchemaVersion: TaskSetSchemaVersion, TaskSetID: "ts1", Version: "1",
		Tasks: []Task{{ID: "t1", Prompt: "p", SuccessCriteria: "c", CapabilityClass: "cap"}},
	}
	ac := validArmConfig()
	runs, err := PlanRuns(ts, ac, "2026-07-13T00:00:00Z")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(runs) != 4 { // 1 task * 1 dose * 4 arms
		t.Fatalf("expected 4 run records, got %d", len(runs))
	}
	for _, r := range runs {
		if r.Verdict != VerdictUngraded || r.WorkID != "" {
			t.Fatalf("planned runs must be ungraded and unbound, got %+v", r)
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("planned run invalid: %v", err)
		}
	}
	again, _ := PlanRuns(ts, ac, "2026-07-13T00:00:00Z")
	for i := range runs {
		if runs[i].RunID != again[i].RunID {
			t.Fatalf("plan run ids must be deterministic; %s != %s", runs[i].RunID, again[i].RunID)
		}
	}
}
