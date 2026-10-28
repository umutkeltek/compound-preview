package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func withExactRequirement(t *testing.T, ai envelope.ActionIntent) envelope.ActionIntent {
	t.Helper()
	b, err := json.Marshal(ai)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	body["requires_exact_approval"] = json.RawMessage("true")
	b, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &ai); err != nil {
		t.Fatal(err)
	}
	return ai
}

func TestExactRequirementSurvivesL3EffectAuthority(t *testing.T) {
	ai := mkAction("external_side_effect", "easy", "/synthetic/agents/job.plist", "narrow")
	ai.Capability = "system.launchd.repair_existing.review"
	ai.Destination = "launchd:gui/1001/example.job:work_test:synthetic"
	cfg := DefaultConfig()
	cfg.OperatorAutonomyLevel = AutonomyLevelFullProjectScope
	cfg.OperatorAutonomyAuthorityRef = "compound://registries/operator-state"
	cfg.StandingGrants = []StandingGrant{{
		ID: "grant_exact_effect", Capability: "system.launchd.repair_existing",
		MutationClass: ai.MutationClass, TargetPath: ai.Target.Path,
		Destination: ai.Destination, BlastRadius: ai.BlastRadius,
		ExpiresAt: fixedNow.Add(time.Hour).Format(time.RFC3339),
	}}
	if base := Evaluate(ai, cfg, fixedNow); !Allowed(base) {
		t.Fatalf("control must reproduce L3 effect authority: %+v", base)
	}
	required := withExactRequirement(t, ai)
	pd := Evaluate(required, cfg, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("exact review must hold despite L3 effect authority: %+v", pd)
	}
	if ActionBindingForIntent(ai, cfg).FullIntentDigest == ActionBindingForIntent(required, cfg).FullIntentDigest {
		t.Fatal("exact requirement must be part of full intent binding")
	}
	if required.Capability != ai.Capability || required.Target != ai.Target || required.Destination != ai.Destination || required.MutationClass != ai.MutationClass || required.BlastRadius != ai.BlastRadius {
		t.Fatal("exact requirement changed action classification")
	}
}

type exactRequirementConsumer struct{ calls int }

func (c *exactRequirementConsumer) Consume(_ string, _ time.Time) error {
	c.calls++
	return nil
}

func TestExactRequirementCanOnlyClearWithoutSecurityFloor(t *testing.T) {
	for _, floor := range []string{"", "hold", "deny"} {
		t.Run("floor="+floor, func(t *testing.T) {
			ai := withExactRequirement(t, mkAction("local_reversible", "easy", "docs/example.md", "repo"))
			ai.SecurityFloorOutcome = floor
			cfg := DefaultConfig()
			pd := Evaluate(ai, cfg, fixedNow)
			want := "hold"
			if floor == "deny" {
				want = "deny"
			}
			if pd.Outcome != want {
				t.Fatalf("requirement/floor result=%s, want %s", pd.Outcome, want)
			}
			binding := ActionBindingForIntent(ai, cfg)
			approval, err := NewExactActionApproval("approval_test_requirement", "operator:synthetic", binding, fixedNow.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			consumer := &exactRequirementConsumer{}
			result, err := ApplyExactActionApproval(MonitoredAction{Intent: ai, Binding: binding, Decision: pd}, approval, consumer, fixedNow)
			if floor != "" {
				if err == nil || consumer.calls != 0 || result.Outcome != pd.Outcome {
					t.Fatalf("exact approval bypassed security floor: decision=%+v calls=%d err=%v", result, consumer.calls, err)
				}
			} else if err != nil || !Allowed(result) || consumer.calls != 1 {
				t.Fatalf("real exact approval failed to clear requirement: decision=%+v calls=%d err=%v", result, consumer.calls, err)
			}
		})
	}
}

func TestExactRequirementOmittedIntentRemainsUnchanged(t *testing.T) {
	ai := mkAction("local_reversible", "easy", "docs/example.md", "repo")
	b, err := json.Marshal(ai)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "requires_exact_approval") {
		t.Fatal("omitted requirement changed legacy serialized intent")
	}
	if pd := Evaluate(ai, DefaultConfig(), fixedNow); pd.Outcome != "allow_with_obligations" {
		t.Fatalf("omitted requirement changed legacy policy: %+v", pd)
	}
}
