package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

var fixedNow = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

func TestLoadConfigOverlaysDefaultPolicy(t *testing.T) {
	homePath := t.TempDir()
	configDir := filepath.Join(homePath, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "policy.yaml")
	if err := os.WriteFile(configPath, []byte("placement_guard_mode: block\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, source, err := LoadConfig(homePath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if source != configPath {
		t.Fatalf("source = %q, want %q", source, configPath)
	}
	if cfg.PlacementGuardMode != "block" {
		t.Fatalf("placement mode = %q, want block", cfg.PlacementGuardMode)
	}
	if len(cfg.ForbiddenPaths) == 0 || len(cfg.ClassOutcome) == 0 {
		t.Fatalf("minimal config wiped default safety policy: forbidden=%v classes=%v", cfg.ForbiddenPaths, cfg.ClassOutcome)
	}

	pd := Evaluate(mkAction("local_reversible", "easy", "docs/x.md", "repo"), cfg, fixedNow)
	if pd.Outcome != "allow_with_obligations" {
		t.Fatalf("default class outcome missing after overlay, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	pd = Evaluate(mkAction("local_reversible", "easy", ".env", "repo"), cfg, fixedNow)
	if pd.Outcome != "deny" {
		t.Fatalf("default forbidden path missing after overlay, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}

	ctx := EvaluationContext{Placement: &PlacementGuardContext{
		Confidence: "block",
		OwnerRef:   "owner:decisions",
		TargetRef:  "docs/x.md",
		Reason:     "wrong owner",
	}}
	pd = EvaluateWithContext(mkAction("local_reversible", "easy", "docs/x.md", "repo"), cfg, ctx, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("placement_guard_mode from config should hold confidence:block placement, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
}

// mkAction builds a valid ActionIntent for a given class/reversibility/path/blast.
func mkAction(class, reversibility, path, blast string) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_test01",
			CreatedAt:     fixedNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "hook", Name: "compound"},
			IDs:           envelope.IDs{WorkID: "work_test"},
			Authority:     envelope.Authority{SourceClass: "runtime"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "claude_code",
		Capability:    "filesystem.write",
		Action:        "write_file",
		Target:        envelope.ActionTarget{Kind: "file", Path: path},
		MutationClass: class,
		Reversibility: reversibility,
		BlastRadius:   blast,
	}
}

// The three documented P4 proof cases.
func TestProofCases(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name, class, rev, path, blast, want string
	}{
		{"local reversible write -> allow_with_obligations", "local_reversible", "easy", "docs/x.md", "repo", "allow_with_obligations"},
		{"forbidden path -> deny", "local_reversible", "easy", "config/secret.key", "repo", "deny"},
		{"public -> hold", "public", "manual", "site/index.html", "public", "hold"},
		{"spend (financial) -> hold", "financial", "manual", "billing/charge", "account", "hold"},
		{"destructive -> hold", "destructive", "hard", "db/table", "repo", "hold"},
		{"security -> hold", "security", "manual", "auth/policy", "repo", "hold"},
		{"read -> allow", "read", "easy", "docs/x.md", "repo", "allow"},
	}
	for _, c := range cases {
		pd := Evaluate(mkAction(c.class, c.rev, c.path, c.blast), cfg, fixedNow)
		if pd.Outcome != c.want {
			t.Errorf("%s: got %q, want %q (reasons: %v)", c.name, pd.Outcome, c.want, pd.Reasons)
		}
		if err := pd.Validate(); err != nil {
			t.Errorf("%s: emitted decision invalid: %v", c.name, err)
		}
		if err := envelope.RoundTrip(&pd); err != nil {
			t.Errorf("%s: round-trip: %v", c.name, err)
		}
		if pd.Outcome == "allow_with_obligations" && len(pd.Obligations) == 0 {
			t.Errorf("%s: allow_with_obligations must carry obligations", c.name)
		}
	}
}

// A "reversible" class with irreversible reversibility escalates to hold (D3 fail-closed).
func TestReversibilityEscalation(t *testing.T) {
	pd := Evaluate(mkAction("local_reversible", "irreversible", "docs/x.md", "repo"), DefaultConfig(), fixedNow)
	if pd.Outcome != "hold" {
		t.Errorf("irreversible local write should escalate to hold, got %q", pd.Outcome)
	}
}

// A wide blast radius escalates a soft class to hold.
func TestBlastRadiusEscalation(t *testing.T) {
	pd := Evaluate(mkAction("local_reversible", "easy", "docs/x.md", "production"), DefaultConfig(), fixedNow)
	if pd.Outcome != "hold" {
		t.Errorf("production blast radius should escalate to hold, got %q", pd.Outcome)
	}
}

func TestSourceControlDirtyEscalatesMutation(t *testing.T) {
	ctx := EvaluationContext{SourceControl: &SourceControlContext{
		EntityRef:    "repo:demo",
		Status:       "dirty",
		ChangedPaths: 2,
		EvidenceRefs: []string{"source-control:repo:demo:abc123"},
	}}
	pd := EvaluateWithContext(mkAction("local_reversible", "easy", "docs/x.md", "repo"), DefaultConfig(), ctx, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("dirty repo mutation should hold, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if len(pd.Obligations) != 0 {
		t.Fatalf("held decision should not keep allow obligations: %v", pd.Obligations)
	}
	if !testContains(pd.EvidenceRefs, "source-control:repo:demo:abc123") {
		t.Fatalf("policy evidence refs = %v", pd.EvidenceRefs)
	}
	if !reasonContains(pd.Reasons, "source_control_status dirty") {
		t.Fatalf("reasons missing source-control gate: %v", pd.Reasons)
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("decision invalid: %v", err)
	}
}

func TestSourceControlDirtyRepoUsesExactTargetStatus(t *testing.T) {
	tests := []struct {
		name       string
		sourceJSON string
		class      string
		want       string
	}{
		{
			name:       "clean target keeps reversible baseline",
			sourceJSON: `{"entity_ref":"repo:demo","git_root":"/repo/demo","status":"dirty","changed_paths":2,"target_path":"docs/x.md","target_status":"clean"}`,
			class:      "local_reversible",
			want:       "allow_with_obligations",
		},
		{
			name:       "dirty target holds",
			sourceJSON: `{"entity_ref":"repo:demo","git_root":"/repo/demo","status":"dirty","changed_paths":2,"target_path":"docs/x.md","target_status":"dirty"}`,
			class:      "local_reversible",
			want:       "hold",
		},
		{
			name:       "unknown target holds",
			sourceJSON: `{"entity_ref":"repo:demo","git_root":"/repo/demo","status":"dirty","changed_paths":2,"target_path":"docs/x.md"}`,
			class:      "local_reversible",
			want:       "hold",
		},
		{
			name:       "clean target does not relax destructive baseline",
			sourceJSON: `{"entity_ref":"repo:demo","git_root":"/repo/demo","status":"dirty","changed_paths":2,"target_path":"docs/x.md","target_status":"clean"}`,
			class:      "destructive",
			want:       "hold",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sc SourceControlContext
			if err := json.Unmarshal([]byte(tt.sourceJSON), &sc); err != nil {
				t.Fatal(err)
			}
			pd := EvaluateWithContext(mkAction(tt.class, "easy", "docs/x.md", "repo"), DefaultConfig(), EvaluationContext{SourceControl: &sc}, fixedNow)
			if pd.Outcome != tt.want {
				t.Fatalf("outcome = %q reasons=%v, want %q", pd.Outcome, pd.Reasons, tt.want)
			}
		})
	}
	denied := mkAction("local_reversible", "easy", ".env", "repo")
	ownDirty := SourceControlContext{
		Status:                     "dirty",
		SessionOwnershipKnown:      true,
		SessionOwnedChangedPaths:   1,
		SessionForeignChangedPaths: 0,
	}
	if pd := EvaluateWithContext(denied, DefaultConfig(), EvaluationContext{SourceControl: &ownDirty}, fixedNow); pd.Outcome != "deny" {
		t.Fatalf("own-session attribution lowered deny to %q reasons=%v", pd.Outcome, pd.Reasons)
	}
}

func TestSourceControlDirtySessionOwnership(t *testing.T) {
	tests := []struct {
		name    string
		class   string
		context SourceControlContext
		want    string
	}{
		{
			name:  "same session dirt preserves reversible baseline",
			class: "local_reversible",
			context: SourceControlContext{
				EntityRef:                  "repo:demo",
				Status:                     "dirty",
				ChangedPaths:               1,
				TargetPath:                 "docs/b.md",
				TargetStatus:               "unknown",
				SessionOwnershipKnown:      true,
				SessionOwnedChangedPaths:   1,
				SessionForeignChangedPaths: 0,
			},
			want: "allow_with_obligations",
		},
		{
			name:  "foreign dirt holds even when target is clean",
			class: "local_reversible",
			context: SourceControlContext{
				EntityRef:                  "repo:demo",
				Status:                     "dirty",
				ChangedPaths:               1,
				TargetPath:                 "docs/b.md",
				TargetStatus:               "clean",
				SessionOwnershipKnown:      true,
				SessionForeignChangedPaths: 1,
			},
			want: "hold",
		},
		{
			name:  "mixed own and foreign dirt holds",
			class: "local_reversible",
			context: SourceControlContext{
				EntityRef:                  "repo:demo",
				Status:                     "dirty",
				ChangedPaths:               2,
				TargetPath:                 "docs/b.md",
				TargetStatus:               "clean",
				SessionOwnershipKnown:      true,
				SessionOwnedChangedPaths:   1,
				SessionForeignChangedPaths: 1,
			},
			want: "hold",
		},
		{
			name:  "read stays exempt with foreign dirt",
			class: "read",
			context: SourceControlContext{
				EntityRef:                  "repo:demo",
				Status:                     "dirty",
				ChangedPaths:               1,
				TargetPath:                 "docs/b.md",
				TargetStatus:               "dirty",
				SessionOwnershipKnown:      true,
				SessionForeignChangedPaths: 1,
			},
			want: "allow",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pd := EvaluateWithContext(mkAction(tt.class, "easy", "docs/b.md", "repo"), DefaultConfig(), EvaluationContext{SourceControl: &tt.context}, fixedNow)
			if pd.Outcome != tt.want {
				t.Fatalf("outcome = %q reasons=%v, want %q", pd.Outcome, pd.Reasons, tt.want)
			}
		})
	}
}

func TestSourceControlCleanOrReadDoesNotEscalate(t *testing.T) {
	clean := EvaluationContext{SourceControl: &SourceControlContext{EntityRef: "repo:demo", Status: "clean", EvidenceRefs: []string{"source-control:repo:demo:abc123"}}}
	if pd := EvaluateWithContext(mkAction("local_reversible", "easy", "docs/x.md", "repo"), DefaultConfig(), clean, fixedNow); pd.Outcome != "allow_with_obligations" {
		t.Fatalf("clean repo should keep baseline, got %q", pd.Outcome)
	}
	dirty := EvaluationContext{SourceControl: &SourceControlContext{EntityRef: "repo:demo", Status: "dirty", ChangedPaths: 3}}
	if pd := EvaluateWithContext(mkAction("read", "easy", "docs/x.md", "repo"), DefaultConfig(), dirty, fixedNow); pd.Outcome != "allow" {
		t.Fatalf("read action should not be blocked by dirty repo, got %q", pd.Outcome)
	}
}

func TestSourceControlDirtyExactRecordedMutationReviewAvoidsOnlyDirtyEscalation(t *testing.T) {
	ai := mkAction("local_reversible", "easy", "docs/x.md", "repo")
	ai.EvidenceRefs = []string{"mutation-input:sha256:exact"}
	var sc SourceControlContext
	if err := json.Unmarshal([]byte(`{
  "entity_ref":"repo:demo",
  "git_root":"/repo/demo",
  "remote":"git@example.invalid/demo.git",
  "head":"abc123",
  "status":"dirty",
  "changed_paths":1,
  "changed_state_fingerprint":"sha256:state",
  "evidence_refs":["source-control:repo:demo:abc123:state"],
  "reviewed_mutation":{
    "status":"recorded",
    "decision":"authorize_mutation",
    "review_id":"scrr_exact",
	"plan_digest":"sha256:exact-plan",
	"authorization_digest":"sha256:exact-authorization",
	"reviewed_by":"operator:test",
    "owner_work_id":"work_owner",
    "action_work_id":"work_test",
    "repo_id":"repo:demo",
    "git_root":"/repo/demo",
    "remote":"git@example.invalid/demo.git",
    "head":"abc123",
    "changed_state_fingerprint":"sha256:state",
    "mutation_input_hash":"mutation-input:sha256:exact",
    "capability":"filesystem.write",
    "action":"write_file",
    "mutation_class":"local_reversible",
    "target_paths":["docs/x.md"],
    "evidence_refs":["source-control-review:scrr_exact"]
  }
}`), &sc); err != nil {
		t.Fatal(err)
	}
	pd := EvaluateWithContext(ai, DefaultConfig(), EvaluationContext{SourceControl: &sc}, fixedNow)
	if pd.Outcome != "allow_with_obligations" {
		t.Fatalf("exact recorded review should preserve baseline allow_with_obligations, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !reasonContains(pd.Reasons, "exact recorded source-control review") {
		t.Fatalf("review allowance reason missing: %v", pd.Reasons)
	}

	mismatches := []struct {
		name   string
		mutate func(*envelope.ActionIntent, *SourceControlContext, *ReviewedSourceControlMutation)
	}{
		{"status", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.Status = "review_required"
		}},
		{"decision", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.Decision = "parked"
		}},
		{"review id", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.ReviewID = ""
		}},
		{"plan digest", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.PlanDigest = ""
		}},
		{"authorization digest", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.AuthorizationDigest = ""
		}},
		{"reviewer", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.ReviewedBy = "codex"
		}},
		{"owner work", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.OwnerWorkID = ""
		}},
		{"repo", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.RepoID = "repo:other"
		}},
		{"root", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.GitRoot = "/repo/other"
		}},
		{"remote", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.Remote = "git@example.invalid/other.git"
		}},
		{"head", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.Head = "other"
		}},
		{"fingerprint", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.ChangedStateFingerprint = "sha256:other"
		}},
		{"action work", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.ActionWorkID = "work_other"
		}},
		{"capability", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.Capability = "filesystem.delete"
		}},
		{"action", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.Action = "delete_file"
		}},
		{"class", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.MutationClass = "destructive"
		}},
		{"target", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.TargetPaths = []string{"docs/other.md"}
		}},
		{"input", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.MutationInputHash = "mutation-input:sha256:other"
		}},
		{"unbound input", func(_ *envelope.ActionIntent, _ *SourceControlContext, r *ReviewedSourceControlMutation) {
			r.MutationInputHash = "mutation-input-unbound:sha256:exact"
		}},
		{"missing input evidence", func(a *envelope.ActionIntent, _ *SourceControlContext, _ *ReviewedSourceControlMutation) {
			a.EvidenceRefs = nil
		}},
	}
	for _, tc := range mismatches {
		t.Run("mismatched "+tc.name, func(t *testing.T) {
			changedAction := ai
			changedAction.EvidenceRefs = append([]string{}, ai.EvidenceRefs...)
			changedContext := sc
			changedReview := *sc.ReviewedMutation
			changedReview.TargetPaths = append([]string{}, sc.ReviewedMutation.TargetPaths...)
			changedReview.EvidenceRefs = append([]string{}, sc.ReviewedMutation.EvidenceRefs...)
			changedContext.ReviewedMutation = &changedReview
			tc.mutate(&changedAction, &changedContext, &changedReview)
			got := EvaluateWithContext(changedAction, DefaultConfig(), EvaluationContext{SourceControl: &changedContext}, fixedNow)
			if got.Outcome != "hold" || !reasonContains(got.Reasons, "recorded review did not exactly match") {
				t.Fatalf("mismatch %s did not fail closed: outcome=%s reasons=%v", tc.name, got.Outcome, got.Reasons)
			}
		})
	}
}

// An unknown mutation class fails closed.
func TestUnknownClassFailsClosed(t *testing.T) {
	cfg := DefaultConfig()
	ai := mkAction("local_reversible", "easy", "docs/x.md", "repo")
	ai.MutationClass = "experimental" // not in MutationClasses... still: policy must not silently allow
	pd := Evaluate(ai, cfg, fixedNow)
	if pd.Outcome != "hold" {
		t.Errorf("unknown class should fail closed to hold, got %q", pd.Outcome)
	}
}

// A standing grant allows an otherwise-held action only inside its explicit
// envelope; an expired grant does not.
func TestStandingGrant(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{ID: "grant_deploy", Capability: "deploy.run", MutationClass: "public", BlastRadius: "public", ExpiresAt: fixedNow.Add(365 * 24 * time.Hour).Format(time.RFC3339)}}
	ai := mkAction("public", "manual", "site/index.html", "public")
	ai.Capability = "deploy.run"
	pd := Evaluate(ai, cfg, fixedNow)
	// A granted non-read mutation still carries its trust duties (allow_with_obligations),
	// yet still flows (Allowed): autonomy is not regressed.
	if !Allowed(pd) || pd.Outcome != "allow_with_obligations" || pd.StandingGrantID != "grant_deploy" {
		t.Errorf("matching grant should allow_with_obligations and keep grant id, got outcome=%q grant=%q obligations=%v", pd.Outcome, pd.StandingGrantID, pd.Obligations)
	}
	// Expired grant -> falls through to baseline (hold).
	cfg.StandingGrants[0].ExpiresAt = fixedNow.Add(-365 * 24 * time.Hour).Format(time.RFC3339)
	if pd := Evaluate(ai, cfg, fixedNow); pd.Outcome != "hold" {
		t.Errorf("expired grant should fall to baseline hold, got %q", pd.Outcome)
	}
}

func TestStandingGrantDoesNotBypassEnvelope(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{ID: "grant_loose", Capability: "deploy.run", MutationClass: "public"}}
	ai := mkAction("public", "manual", "site/index.html", "public")
	ai.Capability = "deploy.run"

	pd := Evaluate(ai, cfg, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("unscoped public grant should hold, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if pd.StandingGrantID != "" {
		t.Fatalf("out-of-envelope grant should not attach id, got %q", pd.StandingGrantID)
	}
	if !reasonContains(pd.Reasons, `standing grant "grant_loose" does not cover blast_radius "public"`) {
		t.Fatalf("reasons missing grant envelope miss: %v", pd.Reasons)
	}

	cfg.StandingGrants[0].BlastRadius = "public"
	pd = Evaluate(ai, cfg, fixedNow)
	if !Allowed(pd) || pd.Outcome != "allow_with_obligations" || pd.StandingGrantID != "grant_loose" {
		t.Fatalf("explicit public grant should allow_with_obligations, got outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}
}

func TestStandingGrantRequiresExplicitHumanReversibility(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{ID: "grant_local", Capability: "filesystem.write", MutationClass: "local_reversible"}}
	ai := mkAction("local_reversible", "irreversible", "docs/x.md", "repo")

	pd := Evaluate(ai, cfg, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("unscoped irreversible grant should hold, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !reasonContains(pd.Reasons, `standing grant "grant_local" does not cover reversibility "irreversible"`) {
		t.Fatalf("reasons missing reversibility miss: %v", pd.Reasons)
	}

	cfg.StandingGrants[0].Reversibility = "irreversible"
	pd = Evaluate(ai, cfg, fixedNow)
	if !Allowed(pd) || pd.Outcome != "allow_with_obligations" || pd.StandingGrantID != "grant_local" {
		t.Fatalf("explicit irreversible grant should allow_with_obligations, got outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}
}

func TestStandingGrantScopeAndBudget(t *testing.T) {
	maxUSD := 10.0
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:            "grant_billing",
		Capability:    "billing.charge",
		MutationClass: "financial",
		BlastRadius:   "account",
		TargetPath:    "billing/approved/**",
		MaxUSD:        &maxUSD,
	}}
	ai := mkAction("financial", "manual", "billing/approved/charge.json", "account")
	ai.Capability = "billing.charge"
	cost := 12.5
	ai.EstimatedCost.USD = &cost

	pd := Evaluate(ai, cfg, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("over-budget grant should hold, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !reasonContains(pd.Reasons, `estimated_cost.usd 12.50 exceeds max_usd 10.00`) {
		t.Fatalf("reasons missing budget miss: %v", pd.Reasons)
	}

	cost = 7.5
	ai.EstimatedCost.USD = &cost
	pd = Evaluate(ai, cfg, fixedNow)
	if !Allowed(pd) || pd.Outcome != "allow_with_obligations" || pd.StandingGrantID != "grant_billing" {
		t.Fatalf("in-budget scoped grant should allow_with_obligations, got outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}

	ai.Target.Path = "billing/unapproved/charge.json"
	pd = Evaluate(ai, cfg, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("out-of-scope target should hold, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !reasonContains(pd.Reasons, `target path "billing/unapproved/charge.json" outside target_path "billing/approved/**"`) {
		t.Fatalf("reasons missing target scope miss: %v", pd.Reasons)
	}
}

// A matched standing grant on a NON-READ mutation still carries the granted class's
// trust duties (emit a Receipt, record a rollback_plan, capture a pre-image ref): the
// autonomous/unattended path is exactly where nothing else guards the write, so it must
// keep its accountability. Autonomy is NOT regressed -- the decision is still Allowed(),
// so a granted action still flows without a per-item operator gate. A granted READ needs
// no such duty and stays a bare allow.
func TestStandingGrantKeepsTrustObligations(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:            "grant_ext",
		Capability:    "deploy.run",
		MutationClass: "external_side_effect",
		BlastRadius:   "repo",
		ExpiresAt:     fixedNow.Add(365 * 24 * time.Hour).Format(time.RFC3339),
	}}
	ai := mkAction("external_side_effect", "easy", "site/index.html", "repo")
	ai.Capability = "deploy.run"

	// (a) granted external_side_effect mutation -> allowed, carrying its trust duties.
	pd := Evaluate(ai, cfg, fixedNow)
	if !Allowed(pd) {
		t.Fatalf("granted external mutation should be allowed, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if pd.Outcome != "allow_with_obligations" {
		t.Fatalf("granted external mutation should carry obligations, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	for _, want := range []string{"emit a Receipt", "record a rollback_plan", "capture pre-image artifact ref"} {
		if !testContains(pd.Obligations, want) {
			t.Fatalf("granted external mutation missing trust obligation %q, got %v", want, pd.Obligations)
		}
	}
	if pd.StandingGrantID != "grant_ext" {
		t.Fatalf("granted decision should keep the grant id, got %q", pd.StandingGrantID)
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("granted decision must be a valid envelope: %v", err)
	}

	// (b) autonomy preserved: the same granted decision still passes the gate
	//     (it would still flow without a per-item operator gate).
	if err := Gate(pd); err != nil {
		t.Fatalf("granted mutation must still flow without a per-item gate: %v", err)
	}

	// (c) an UNGRANTED external_side_effect mutation still holds (unchanged).
	if pd := Evaluate(ai, DefaultConfig(), fixedNow); pd.Outcome != "hold" {
		t.Fatalf("ungranted external mutation should hold, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}

	// (d) a granted READ stays a bare allow with no rollback obligation
	//     (the change is scoped to non-read).
	readCfg := DefaultConfig()
	readCfg.StandingGrants = []StandingGrant{{ID: "grant_read", Capability: "filesystem.read", MutationClass: "read"}}
	readAI := mkAction("read", "easy", "docs/x.md", "repo")
	readAI.Capability = "filesystem.read"
	rpd := Evaluate(readAI, readCfg, fixedNow)
	if rpd.Outcome != "allow" {
		t.Fatalf("granted read should stay a bare allow, got %q reasons=%v", rpd.Outcome, rpd.Reasons)
	}
	if len(rpd.Obligations) != 0 {
		t.Fatalf("granted read should carry no obligations, got %v", rpd.Obligations)
	}
	if rpd.StandingGrantID != "grant_read" {
		t.Fatalf("granted read should keep the grant id, got %q", rpd.StandingGrantID)
	}
}

// Forbidden path beats a standing grant (hard safety wins).
func TestForbiddenBeatsGrant(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{ID: "grant_all", MutationClass: "local_reversible"}}
	ai := mkAction("local_reversible", "easy", "deploy/.env", "repo")
	if pd := Evaluate(ai, cfg, fixedNow); pd.Outcome != "deny" {
		t.Errorf("forbidden path must deny even with a grant, got %q", pd.Outcome)
	}
}

func TestGate(t *testing.T) {
	mk := func(o string) envelope.PolicyDecision { return envelope.PolicyDecision{Outcome: o} }
	if err := Gate(mk("allow")); err != nil {
		t.Errorf("allow should pass the gate: %v", err)
	}
	if err := Gate(mk("allow_with_obligations")); err != nil {
		t.Errorf("allow_with_obligations should pass the gate: %v", err)
	}
	if err := Gate(mk("hold")); err == nil {
		t.Error("hold should be blocked by the gate")
	}
	if err := Gate(mk("deny")); err == nil {
		t.Error("deny should be blocked by the gate")
	}
}

func TestPlacementGuardObserveRecordsEvidenceOnly(t *testing.T) {
	ctx := EvaluationContext{Placement: &PlacementGuardContext{
		Mode:         "observe",
		Confidence:   "block",
		OwnerRef:     "owner:decisions",
		TargetRef:    "docs/x.md",
		Reason:       "wrong owner",
		EvidenceRefs: []string{"placement:obs"},
	}}
	pd := EvaluateWithContext(mkAction("local_reversible", "easy", "docs/x.md", "repo"), DefaultConfig(), ctx, fixedNow)
	if pd.Outcome != "allow_with_obligations" {
		t.Fatalf("observe should keep baseline outcome, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !testContains(pd.EvidenceRefs, "placement:obs") {
		t.Fatalf("placement evidence missing: %v", pd.EvidenceRefs)
	}
	if !reasonContains(pd.Reasons, "observe mode records only") {
		t.Fatalf("observe reason missing: %v", pd.Reasons)
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("decision invalid: %v", err)
	}
}

func TestPlacementGuardWarnAddsReceiptObligation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ClassOutcome["placement_test"] = ClassRule{Outcome: "allow"}
	ctx := EvaluationContext{Placement: &PlacementGuardContext{
		Mode:       "warn",
		Confidence: "block",
		OwnerRef:   "owner:memory",
		TargetRef:  "docs/x.md",
	}}
	pd := EvaluateWithContext(mkAction("placement_test", "easy", "docs/x.md", "repo"), cfg, ctx, fixedNow)
	if pd.Outcome != "allow_with_obligations" {
		t.Fatalf("warn should escalate allow to allow_with_obligations, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !testContains(pd.Obligations, "record placement warning") {
		t.Fatalf("warn obligation missing: %v", pd.Obligations)
	}
	if err := pd.Validate(); err != nil {
		t.Fatalf("decision invalid: %v", err)
	}
}

func TestPlacementGuardBlockHoldsAndClearsGrant(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:            "grant_deploy",
		Capability:    "deploy.run",
		MutationClass: "public",
		BlastRadius:   "public",
		ExpiresAt:     fixedNow.Add(365 * 24 * time.Hour).Format(time.RFC3339),
	}}
	ai := mkAction("public", "manual", "site/index.html", "public")
	ai.Capability = "deploy.run"
	ctx := EvaluationContext{Placement: &PlacementGuardContext{
		Mode:       "block",
		Confidence: "block",
		OwnerRef:   "owner:site",
		TargetRef:  "site/index.html",
	}}
	pd := EvaluateWithContext(ai, cfg, ctx, fixedNow)
	if pd.Outcome != "hold" {
		t.Fatalf("block should hold, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if len(pd.Obligations) != 0 || pd.StandingGrantID != "" || pd.ExpiresAt != "" {
		t.Fatalf("held placement decision should clear allow metadata: obligations=%v grant=%q expires=%q", pd.Obligations, pd.StandingGrantID, pd.ExpiresAt)
	}
	if !reasonContains(pd.Reasons, "block mode holds confidence:block placement") {
		t.Fatalf("block reason missing: %v", pd.Reasons)
	}
}

func TestPlacementGuardNonBlockConfidenceFailsOpen(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ClassOutcome["placement_test"] = ClassRule{Outcome: "allow"}
	ctx := EvaluationContext{Placement: &PlacementGuardContext{
		Mode:       "block",
		Confidence: "warn",
		OwnerRef:   "owner:site",
		TargetRef:  "docs/x.md",
	}}
	pd := EvaluateWithContext(mkAction("placement_test", "easy", "docs/x.md", "repo"), cfg, ctx, fixedNow)
	if pd.Outcome != "allow" {
		t.Fatalf("non-block confidence should fail open, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !reasonContains(pd.Reasons, "confidence below block threshold") {
		t.Fatalf("threshold reason missing: %v", pd.Reasons)
	}
}

func TestPlacementGuardOpenWriteAndOverrideWin(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ClassOutcome["placement_test"] = ClassRule{Outcome: "allow"}
	tests := []struct {
		name string
		ctx  PlacementGuardContext
		env  string
		want string
	}{
		{name: "open write", ctx: PlacementGuardContext{Mode: "block", Confidence: "block", OpenWrite: true}, want: "OPEN_WRITE declared"},
		{name: "fixture override", ctx: PlacementGuardContext{Mode: "block", Confidence: "block", Override: true}, want: "PLACEMENT_OVERRIDE active"},
		{name: "env override", ctx: PlacementGuardContext{Mode: "block", Confidence: "block"}, env: "1", want: "PLACEMENT_OVERRIDE active"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PLACEMENT_OVERRIDE", tt.env)
			pd := EvaluateWithContext(
				mkAction("placement_test", "easy", "docs/x.md", "repo"),
				cfg,
				EvaluationContext{Placement: &tt.ctx},
				fixedNow,
			)
			if pd.Outcome != "allow" {
				t.Fatalf("guard escape should keep allow, got %q reasons=%v", pd.Outcome, pd.Reasons)
			}
			if !reasonContains(pd.Reasons, tt.want) {
				t.Fatalf("reason missing %q: %v", tt.want, pd.Reasons)
			}
		})
	}
}

func TestPlacementGuardReadActionUnchanged(t *testing.T) {
	ctx := EvaluationContext{Placement: &PlacementGuardContext{
		Mode:         "block",
		Confidence:   "block",
		OwnerRef:     "owner:site",
		TargetRef:    "docs/x.md",
		EvidenceRefs: []string{"placement:read"},
	}}
	pd := EvaluateWithContext(mkAction("read", "easy", "docs/x.md", "repo"), DefaultConfig(), ctx, fixedNow)
	if pd.Outcome != "allow" {
		t.Fatalf("read action should remain allow, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !testContains(pd.EvidenceRefs, "placement:read") {
		t.Fatalf("placement evidence missing: %v", pd.EvidenceRefs)
	}
	if reasonContains(pd.Reasons, "placement_guard") {
		t.Fatalf("read action should not carry placement guard reasons: %v", pd.Reasons)
	}
}

func TestPlacementGuardUnknownModeFailsOpenObserve(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ClassOutcome["placement_test"] = ClassRule{Outcome: "allow"}
	ctx := EvaluationContext{Placement: &PlacementGuardContext{
		Mode:       "strict",
		Confidence: "block",
		OwnerRef:   "owner:site",
		TargetRef:  "docs/x.md",
	}}
	pd := EvaluateWithContext(mkAction("placement_test", "easy", "docs/x.md", "repo"), cfg, ctx, fixedNow)
	if pd.Outcome != "allow" {
		t.Fatalf("unknown mode should fail open observe, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if !reasonContains(pd.Reasons, `unknown mode "strict", fail-open observe`) {
		t.Fatalf("unknown mode reason missing: %v", pd.Reasons)
	}
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		pat, path string
		match     bool
	}{
		{"**/*.key", "secret.key", true},
		{"**/*.key", "a/b/secret.key", true},
		{"**/.env", ".env", true},
		{"**/.env", "config/.env", true},
		{"**/.env", "config/env", false},
		{"**/.git/**", "repo/.git/config", true},
		{"**/*secret*", "my-secrets.txt", true},
		{"**/*.key", "key.txt", false},
	}
	for _, c := range cases {
		_, ok := matchForbidden([]string{c.pat}, c.path)
		if ok != c.match {
			t.Errorf("matchForbidden(%q, %q) = %v, want %v", c.pat, c.path, ok, c.match)
		}
	}
}

func testContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func reasonContains(xs []string, want string) bool {
	for _, x := range xs {
		if strings.Contains(x, want) {
			return true
		}
	}
	return false
}
