package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestImportStandingGrantsWritesAuthorityAndPolicyConsumes(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "standing-grants.yaml")
	raw := []byte(`generated_at: "2026-07-01T12:00:00Z"
authority:
  decision_ref: "grant:test-standing-grants-import"
  reviewed_by: "operator:test"
grants:
  - grant_id: grant_review_finding
    capabilities: ["review.finding", "review.summary"]
    target_scope: "state/runs/*"
    reversibility_ceiling: reversible
`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := ImportStandingGrants(homePath, sourcePath, fixedNow)
	if err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
	if report.AuthorityRef != StandingGrantsAuthorityRef || report.GrantCount != 2 || report.CapabilityCount != 2 {
		t.Fatalf("bad import report: %+v", report)
	}
	if _, err := os.Stat(StandingGrantsPath(homePath)); err != nil {
		t.Fatalf("standing grants authority missing: %v", err)
	}
	registry, err := LoadStandingGrantsFromHome(homePath)
	if err != nil {
		t.Fatalf("load standing grants: %v", err)
	}
	if len(registry.Grants) != 2 || registry.Grants[0].ReversibilityCeiling != "reversible" {
		t.Fatalf("bad normalized grants: %+v", registry.Grants)
	}
	records, err := LoadStandingGrantsSourceAuthority(homePath)
	if err != nil {
		t.Fatalf("load source authority: %v", err)
	}
	if len(records) != 1 || records[0].AuthorityStatus != "compound_primary" || records[0].Retires != "config_only_authority:policy.yaml:standing_grants" {
		t.Fatalf("bad source authority records: %+v", records)
	}

	cfg, source, err := LoadConfig(homePath)
	if err != nil {
		t.Fatalf("load policy config: %v", err)
	}
	if source != "built-in" {
		t.Fatalf("expected built-in config plus standing-grants overlay, got %q", source)
	}
	if len(cfg.StandingGrants) != 2 || !contains(cfg.PolicyEvidenceRefs, StandingGrantsAuthorityRef) {
		t.Fatalf("standing grants overlay missing: grants=%+v evidence=%v", cfg.StandingGrants, cfg.PolicyEvidenceRefs)
	}
	action := mkAction("local_reversible", "easy", "state/runs/demo/finding.md", "repo")
	action.Capability = "review.finding"
	pd := Evaluate(action, cfg, fixedNow)
	if !Allowed(pd) || pd.Outcome != "allow_with_obligations" || pd.StandingGrantID != "grant_review_finding:review-finding" {
		t.Fatalf("expected imported grant allow_with_obligations, got outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}
	if !contains(pd.EvidenceRefs, StandingGrantsAuthorityRef) {
		t.Fatalf("policy decision missing standing-grants evidence: %v", pd.EvidenceRefs)
	}
}

// The import mints auto-allow authority, so it refuses a source that does not
// cite an operator-provenance decision record — and writes nothing.
func TestImportStandingGrantsRefusesUncitedAuthority(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "no authority block (non-expiring grant)",
			raw: `grants:
  - grant_id: grant_uncited
    capabilities: ["review.finding"]
    target_scope: "state/runs/*"
    reversibility_ceiling: reversible
`,
		},
		{
			name: "expiring grant still requires the citation",
			raw: `grants:
  - grant_id: grant_uncited_expiring
    capabilities: ["review.finding"]
    target_scope: "state/runs/*"
    reversibility_ceiling: reversible
    valid_until: "2027-01-01T00:00:00Z"
`,
		},
		{
			name: "non-operator reviewer",
			raw: `authority:
  decision_ref: "grant:self-asserted"
  reviewed_by: "claude-code-executor"
grants:
  - grant_id: grant_self_asserted
    capabilities: ["review.finding"]
    target_scope: "state/runs/*"
    reversibility_ceiling: reversible
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			sourcePath := filepath.Join(t.TempDir(), "standing-grants.yaml")
			if err := os.WriteFile(sourcePath, []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ImportStandingGrants(homePath, sourcePath, fixedNow); err == nil {
				t.Fatal("import without an operator-provenance decision citation must be refused")
			}
			if _, err := os.Stat(StandingGrantsPath(homePath)); !os.IsNotExist(err) {
				t.Fatalf("refused import must not write the authority: stat err=%v", err)
			}
		})
	}
}

func TestStandingGrantReversibilityCeilingDoesNotOpenIrreversible(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:                   "grant_repo_edit_reversible",
		Capability:           "mechanical-edit",
		TargetPath:           "repo/*",
		ReversibilityCeiling: "reversible",
	}}
	allowed := mkAction("local_reversible", "easy", "repo/file.go", "repo")
	allowed.Capability = "mechanical-edit"
	pd := Evaluate(allowed, cfg, fixedNow)
	if !Allowed(pd) || pd.Outcome != "allow_with_obligations" || pd.StandingGrantID != "grant_repo_edit_reversible" {
		t.Fatalf("reversible action should be covered (allow_with_obligations), got outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}

	blocked := mkAction("local_reversible", "irreversible", "repo/file.go", "repo")
	blocked.Capability = "mechanical-edit"
	pd = Evaluate(blocked, cfg, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("irreversible action should fall through baseline hold, got outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}
	if !reasonContains(pd.Reasons, `standing grant "grant_repo_edit_reversible" covers reversibility up to "reversible", not "irreversible"`) {
		t.Fatalf("missing ceiling miss reason: %v", pd.Reasons)
	}
}

func TestNormalizeStandingGrantRejectsBothExactAndCeiling(t *testing.T) {
	_, err := NormalizeStandingGrantSource(StandingGrantSource{StandingGrants: []StandingGrant{{
		ID:                   "grant_bad",
		Capability:           "review.finding",
		Reversibility:        "reversible",
		ReversibilityCeiling: "reversible",
	}}})
	if err == nil || !strings.Contains(err.Error(), "cannot set both") {
		t.Fatalf("expected exact/ceiling rejection, got %v", err)
	}
}

func TestStandingGrantAuthorityKeepsPolicyYAMLFallback(t *testing.T) {
	homePath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homePath, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	policyYAML := []byte(`standing_grants:
  - id: config_grant
    capability: config.capability
    target_path: config/*
    reversibility_ceiling: reversible
`)
	if err := os.WriteFile(filepath.Join(homePath, "config", "policy.yaml"), policyYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(t.TempDir(), "standing-grants.yaml")
	raw := []byte(`authority:
  decision_ref: "grant:test-standing-grants-fallback"
  reviewed_by: "operator:test"
grants:
  - grant_id: authority_grant
    capabilities: ["authority.capability"]
    target_scope: authority/*
    reversibility_ceiling: reversible
`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandingGrants(homePath, sourcePath, fixedNow); err != nil {
		t.Fatal(err)
	}
	cfg, source, err := LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if source != filepath.Join(homePath, "config", "policy.yaml") {
		t.Fatalf("policy source = %q", source)
	}
	if len(cfg.StandingGrants) != 2 {
		t.Fatalf("config fallback and authority overlay should both remain, got %+v", cfg.StandingGrants)
	}
}

// An unscoped product-wide grant imported before an exact per-door grant covers the
// same intent; the exact grant must still be the one consumed, and the broad grant
// must still catch intents the exact scope does not reach.
func TestMatchGrantPrefersScopedCoveringGrantOverEarlierUnscopedOne(t *testing.T) {
	zero := 0.0
	cfg := DefaultConfig()
	broad := StandingGrant{
		ID: "grant_broad_metadata_fixture", Capability: "app_store.metadata.write", MutationClass: "external_side_effect",
		ReversibilityCeiling: "reversible", BlastRadius: "one_app_version_metadata", MaxUSD: &zero,
		ExpiresAt: fixedNow.Add(time.Hour).Format(time.RFC3339),
	}
	exact := broad
	exact.ID, exact.Destination, exact.TargetPath = "grant_exact_promo_fixture", "asc-app:1", "/loop/apply-artifacts/apply/**"
	cfg.StandingGrants = []StandingGrant{broad, exact}

	action := mkAction("external_side_effect", "easy", "/loop/apply-artifacts/apply/apply_x/desired", "one_app_version_metadata")
	action.Capability, action.Destination, action.EstimatedCost = "app_store.metadata.write", "asc-app:1", envelope.EstimatedCost{USD: &zero}
	pd := Evaluate(action, cfg, fixedNow)
	if !Allowed(pd) || pd.StandingGrantID != "grant_exact_promo_fixture" {
		t.Fatalf("exact grant must win over the earlier broad one: outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}

	outside := action
	outside.Target.Path = "/elsewhere/desired"
	pd = Evaluate(outside, cfg, fixedNow)
	if !Allowed(pd) || pd.StandingGrantID != "grant_broad_metadata_fixture" {
		t.Fatalf("broad grant must still cover an intent outside the exact scope: outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}

	cfg.StandingGrants = []StandingGrant{exact, broad}
	pd = Evaluate(action, cfg, fixedNow)
	if pd.StandingGrantID != "grant_exact_promo_fixture" {
		t.Fatalf("choice must not depend on registry order: grant=%q", pd.StandingGrantID)
	}
}
