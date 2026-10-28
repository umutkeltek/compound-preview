package policy

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
)

// Decode real wire cost objects: explicit zero must not be erased to make a
// grant match, and a producer may set only one known cost dimension.
func TestStandingGrantAcceptsExplicitZeroCost(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID: "grant_existing_push", Capability: "shell.git_push_existing_remote",
		MutationClass: "external_side_effect", ReversibilityCeiling: "irreversible",
		BlastRadius: "internet", Destination: "git-remote:existing",
	}}
	for _, cost := range []string{
		`{}`, `{"usd":0}`, `{"tokens":0}`, `{"usd":0,"tokens":0,"human_review_minutes":0}`,
	} {
		t.Run(cost, func(t *testing.T) {
			intent := MonitorToolAction(monitorRequest("Bash", map[string]any{
				"command": "git push origin main",
			}), cfg, fixedNow).Intent
			if err := json.Unmarshal([]byte(cost), &intent.EstimatedCost); err != nil {
				t.Fatal(err)
			}
			decision := Evaluate(intent, cfg, fixedNow)
			if decision.Outcome != "allow_with_obligations" || decision.StandingGrantID != "grant_existing_push" {
				t.Fatalf("zero-cost push must retain its grant and accountability: %+v", decision)
			}
			if len(decision.Obligations) == 0 {
				t.Fatal("zero-cost write lost its receipt/rollback obligations")
			}
		})
	}
}

func TestZeroCostPreservesGrantBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Config, *envelope.ActionIntent)
	}{
		{"no_grant", func(c *Config, _ *envelope.ActionIntent) { c.StandingGrants = nil }},
		{"capability", func(c *Config, _ *envelope.ActionIntent) { c.StandingGrants[0].Capability = "other.write" }},
		{"class", func(c *Config, _ *envelope.ActionIntent) { c.StandingGrants[0].MutationClass = "read" }},
		{"destination", func(_ *Config, a *envelope.ActionIntent) { a.Destination = "git-remote:new" }},
		{"scope", func(c *Config, _ *envelope.ActionIntent) { c.StandingGrants[0].TargetPath = "approved/**" }},
		{"reversibility", func(c *Config, _ *envelope.ActionIntent) { c.StandingGrants[0].ReversibilityCeiling = "easy" }},
		{"expired", func(c *Config, _ *envelope.ActionIntent) {
			c.StandingGrants[0].ExpiresAt = fixedNow.Add(-time.Second).Format(time.RFC3339)
		}},
		{"forbidden_target", func(c *Config, a *envelope.ActionIntent) {
			c.ForbiddenPaths = []string{"protected/**"}
			a.Target.Path = "protected/source"
		}},
		{"security_floor", func(_ *Config, a *envelope.ActionIntent) {
			a.SecurityFloorOutcome, a.SecurityReasons = "deny", []string{"untrusted source cannot authorize push"}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, intent := costPolicyFixture(t, false)
			if err := json.Unmarshal([]byte(`{"usd":0,"tokens":0}`), &intent.EstimatedCost); err != nil {
				t.Fatal(err)
			}
			tt.change(&cfg, &intent)
			decision := Evaluate(intent, cfg, fixedNow)
			if Allowed(decision) {
				t.Fatalf("zero cost bypassed a non-cost boundary: %+v", decision)
			}
		})
	}
}

func TestAutonomyEnvelopeAcceptsExplicitZeroCost(t *testing.T) {
	scope := t.TempDir()
	cfg := envelopeConfig(scope)
	// A different capability forces the L3 path-scope fallback instead of the
	// ordinary exact grant matcher; both consumers must agree about zero cost.
	cfg.StandingGrants[0].Capability = "filesystem.write"
	intent := monitorShell(t, cfg, filepath.Join(scope, "app"), "bun install").Intent
	if err := json.Unmarshal([]byte(`{"usd":0,"tokens":0}`), &intent.EstimatedCost); err != nil {
		t.Fatal(err)
	}
	decision := Evaluate(intent, cfg, envelopeNow())
	if !Allowed(decision) || decision.StandingGrantID != "grant_project_scope" {
		t.Fatalf("zero-cost project action must remain inside its autonomy envelope: %+v", decision)
	}
}
