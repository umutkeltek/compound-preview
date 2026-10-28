package policy

import (
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestActualCoreTargetFloorCannotBeClearedByGrantOrAutonomy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		system, bound bool
	}{
		{"persisted system ordinary work", true, true},
		{"missing persisted binding", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			ai := envelope.ActionIntent{
				Header:     envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_synthetic_core", CreatedAt: now.Format(time.RFC3339Nano)},
				Actor:      "operator:caller-authored-label",
				Capability: "filesystem.write", Action: "edit_file",
				Target:        envelope.ActionTarget{Kind: "file", Path: "/synthetic/config/harness-permissions/codex.yaml"},
				MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "repo",
			}
			ai = GuardActualEditableSurface(ai, "permissions", tc.system, tc.bound)
			cfg := DefaultConfig()
			cfg.OperatorAutonomyLevel = AutonomyLevelFullProjectScope
			cfg.StandingGrants = []StandingGrant{{
				ID: "synthetic-wide-grant", Capability: "*", MutationClass: "*",
				TargetPath: "/synthetic", Reversibility: "easy", BlastRadius: "repo",
			}}
			pd := Evaluate(ai, cfg, now)
			if ai.MutationClass != "security" || ai.SecurityFloorOutcome != "deny" || pd.Outcome != "deny" || pd.StandingGrantID != "" {
				t.Fatalf("actual core target escaped frontier: action=%+v decision=%+v", ai, pd)
			}
		})
	}
}
