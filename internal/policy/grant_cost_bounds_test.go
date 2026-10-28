package policy

import (
	"math"
	"path/filepath"
	"testing"

	"compound/internal/envelope"
)

func costPolicyFixture(t *testing.T, autonomy bool) (Config, envelope.ActionIntent) {
	t.Helper()
	if autonomy {
		scope := t.TempDir()
		cfg := envelopeConfig(scope)
		cfg.StandingGrants[0].Capability = "filesystem.write"
		return cfg, monitorShell(t, cfg, filepath.Join(scope, "app"), "bun install").Intent
	}
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID: "grant_existing_push", Capability: "shell.git_push_existing_remote",
		MutationClass: "external_side_effect", ReversibilityCeiling: "irreversible",
		BlastRadius: "internet", Destination: "git-remote:existing",
	}}
	return cfg, MonitorToolAction(monitorRequest("Bash", map[string]any{
		"command": "git push origin main",
	}), cfg, fixedNow).Intent
}

func TestGrantCostBoundsInBothAdmissionPaths(t *testing.T) {
	zeroUSD, usd, overUSD := 0.0, 0.01, 0.02
	zeroTokens, tokens, overTokens := 0, 1, 2
	negativeUSD, negativeTokens := -1.0, -1
	for _, mode := range []struct {
		name     string
		autonomy bool
	}{{"standing", false}, {"autonomy", true}} {
		for _, tt := range []struct {
			name      string
			cost      envelope.EstimatedCost
			maxUSD    *float64
			maxTokens *int
			allowed   bool
		}{
			{"unspecified", envelope.EstimatedCost{}, nil, nil, true},
			{"unspecified_with_zero_limits", envelope.EstimatedCost{}, &zeroUSD, &zeroTokens, true},
			{"unspecified_retains_legacy_limits", envelope.EstimatedCost{}, &negativeUSD, &negativeTokens, true},
			{"unknown_token_dimension_unchanged", envelope.EstimatedCost{USD: &zeroUSD}, nil, &negativeTokens, true},
			{"unknown_usd_dimension_unchanged", envelope.EstimatedCost{Tokens: &zeroTokens}, &negativeUSD, nil, true},
			{"explicit_zero", envelope.EstimatedCost{USD: &zeroUSD, Tokens: &zeroTokens}, nil, nil, true},
			{"usd_needs_ceiling", envelope.EstimatedCost{USD: &usd, Tokens: &zeroTokens}, nil, nil, false},
			{"tokens_need_ceiling", envelope.EstimatedCost{USD: &zeroUSD, Tokens: &tokens}, nil, nil, false},
			{"usd_at_ceiling", envelope.EstimatedCost{USD: &usd}, &usd, nil, true},
			{"tokens_at_ceiling", envelope.EstimatedCost{Tokens: &tokens}, nil, &tokens, true},
			{"usd_over_ceiling", envelope.EstimatedCost{USD: &overUSD}, &usd, nil, false},
			{"tokens_over_ceiling", envelope.EstimatedCost{Tokens: &overTokens}, nil, &tokens, false},
			{"usd_ceiling_cannot_cover_tokens", envelope.EstimatedCost{USD: &usd, Tokens: &tokens}, &usd, nil, false},
			{"token_ceiling_cannot_cover_usd", envelope.EstimatedCost{USD: &usd, Tokens: &tokens}, nil, &tokens, false},
			{"both_at_ceiling", envelope.EstimatedCost{USD: &usd, Tokens: &tokens}, &usd, &tokens, true},
			{"zero_ceiling", envelope.EstimatedCost{USD: &zeroUSD, Tokens: &zeroTokens}, &zeroUSD, &zeroTokens, true},
			{"zero_ceiling_blocks_spend", envelope.EstimatedCost{USD: &usd}, &zeroUSD, &zeroTokens, false},
			{"zero_ceiling_blocks_tokens", envelope.EstimatedCost{Tokens: &tokens}, &zeroUSD, &zeroTokens, false},
		} {
			t.Run(mode.name+"/"+tt.name, func(t *testing.T) {
				cfg, intent := costPolicyFixture(t, mode.autonomy)
				intent.EstimatedCost = tt.cost
				cfg.StandingGrants[0].MaxUSD = tt.maxUSD
				cfg.StandingGrants[0].MaxTokens = tt.maxTokens
				decision := Evaluate(intent, cfg, fixedNow)
				if Allowed(decision) != tt.allowed {
					t.Fatalf("budget decision allowed=%v, want %v: %+v", Allowed(decision), tt.allowed, decision)
				}
			})
		}
	}
}

func TestInvalidCostCannotConsumeStandingGrantOrAutonomyEnvelope(t *testing.T) {
	negativeUSD, negativeTokens := -1.0, -1
	nan, infinity, negativeInfinity := math.NaN(), math.Inf(1), math.Inf(-1)
	limitUSD, limitTokens := 1.0, 10
	for _, autonomy := range []bool{false, true} {
		for _, cost := range []envelope.EstimatedCost{
			{USD: &negativeUSD}, {Tokens: &negativeTokens},
			{USD: &nan}, {USD: &infinity}, {USD: &negativeInfinity},
		} {
			cfg, intent := costPolicyFixture(t, autonomy)
			intent.EstimatedCost = cost
			cfg.StandingGrants[0].MaxUSD = &limitUSD
			cfg.StandingGrants[0].MaxTokens = &limitTokens
			decision := Evaluate(intent, cfg, fixedNow)
			if Allowed(decision) || decision.StandingGrantID != "" {
				t.Errorf("invalid cost consumed grant (autonomy=%v cost=%+v): %+v", autonomy, cost, decision)
			}
		}
	}
}

func TestInvalidCeilingCannotAuthorizeKnownCost(t *testing.T) {
	zeroUSD, zeroTokens := 0.0, 0
	negativeUSD, negativeTokens := -1.0, -1
	nan, infinity := math.NaN(), math.Inf(1)
	for _, autonomy := range []bool{false, true} {
		for _, limits := range []StandingGrant{
			{MaxUSD: &negativeUSD}, {MaxTokens: &negativeTokens},
			{MaxUSD: &nan}, {MaxUSD: &infinity},
		} {
			cfg, intent := costPolicyFixture(t, autonomy)
			intent.EstimatedCost = envelope.EstimatedCost{USD: &zeroUSD, Tokens: &zeroTokens}
			cfg.StandingGrants[0].MaxUSD = limits.MaxUSD
			cfg.StandingGrants[0].MaxTokens = limits.MaxTokens
			decision := Evaluate(intent, cfg, fixedNow)
			if Allowed(decision) {
				t.Errorf("invalid ceiling authorized known cost (autonomy=%v): %+v", autonomy, decision)
			}
		}
	}
}
