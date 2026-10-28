package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Away mode: a December grant set imports now and self-activates (not_before),
// and a presence window on operator state is a KEY that only unlocks grants
// that ask for it (requires_presence: away). It is never a ceiling: setting
// presence=away must not move any decision by itself.

func awayWindow(from, through time.Time) *OperatorPresence {
	return &OperatorPresence{
		State:   PresenceAway,
		From:    from.UTC().Format(time.RFC3339),
		Through: through.UTC().Format(time.RFC3339),
		SetBy:   "operator:test",
		Reason:  "away-mode test window",
	}
}

func TestStandingGrantNotBeforeGatesMatch(t *testing.T) {
	notBefore := fixedNow.Add(24 * time.Hour)
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:            "grant_december_promo",
		Capability:    "filesystem.write",
		MutationClass: "external_side_effect",
		NotBefore:     notBefore.Format(time.RFC3339),
		ExpiresAt:     fixedNow.Add(72 * time.Hour).Format(time.RFC3339),
	}}
	ai := mkAction("external_side_effect", "easy", "docs/promo.md", "repo")

	today := Evaluate(ai, cfg, fixedNow)
	if today.Outcome != "hold" || today.StandingGrantID != "" {
		t.Fatalf("grant with not_before tomorrow must not match today: outcome=%q grant=%q reasons=%v", today.Outcome, today.StandingGrantID, today.Reasons)
	}
	if !reasonContains(today.Reasons, "not active before") {
		t.Fatalf("hold should name the not_before miss, reasons=%v", today.Reasons)
	}

	atStart := Evaluate(ai, cfg, notBefore)
	if atStart.Outcome != "allow_with_obligations" || atStart.StandingGrantID != "grant_december_promo" {
		t.Fatalf("not_before is inclusive: want grant at not_before, got outcome=%q grant=%q reasons=%v", atStart.Outcome, atStart.StandingGrantID, atStart.Reasons)
	}

	later := Evaluate(ai, cfg, notBefore.Add(time.Hour))
	if later.Outcome != "allow_with_obligations" || later.StandingGrantID != "grant_december_promo" {
		t.Fatalf("grant should match at not_before+1h, got outcome=%q grant=%q reasons=%v", later.Outcome, later.StandingGrantID, later.Reasons)
	}

	cfg.StandingGrants[0].NotBefore = "next tuesday"
	if pd := Evaluate(ai, cfg, notBefore.Add(time.Hour)); pd.Outcome != "hold" {
		t.Fatalf("an unparseable not_before must fail closed, got outcome=%q reasons=%v", pd.Outcome, pd.Reasons)
	}
}

func TestStandingGrantNotBeforeGatesAutonomyEnvelope(t *testing.T) {
	notBefore := fixedNow.Add(24 * time.Hour)
	cfg := DefaultConfig()
	cfg.OperatorAutonomyLevel = AutonomyLevelFullProjectScope
	cfg.OperatorAutonomyAuthorityRef = OperatorStateAuthorityRef
	// The capability pin makes matchGrant miss, so only the envelope path can
	// clear this hold -- which is the path under test.
	cfg.StandingGrants = []StandingGrant{{
		ID:         "grant_december_project",
		Capability: "some.other.capability",
		TargetPath: "proj/**",
		NotBefore:  notBefore.Format(time.RFC3339),
	}}
	ai := mkAction("external_side_effect", "easy", "proj/x.txt", "repo")

	if pd := Evaluate(ai, cfg, fixedNow); pd.Outcome != "hold" {
		t.Fatalf("envelope must not clear a hold through a grant before its not_before, got outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}
	pd := Evaluate(ai, cfg, notBefore.Add(time.Hour))
	if pd.Outcome != "allow_with_obligations" || pd.StandingGrantID != "grant_december_project" || !reasonContains(pd.Reasons, "autonomy envelope") {
		t.Fatalf("envelope should clear inside the grant after not_before, got outcome=%q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}
}

func TestImportStandingGrantsValidatesActivationWindow(t *testing.T) {
	cases := []struct {
		name    string
		grant   string
		wantErr string
	}{
		{
			name: "not_before after expires_at",
			grant: `    not_before: "2026-12-10T00:00:00Z"
    expires_at: "2026-12-01T00:00:00Z"`,
			wantErr: "not_before",
		},
		{
			name:    "unparseable not_before",
			grant:   `    not_before: "december"`,
			wantErr: "invalid not_before",
		},
		{
			name:    "unknown requires_presence",
			grant:   `    requires_presence: "sometimes"`,
			wantErr: "requires_presence",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			homePath := t.TempDir()
			sourcePath := filepath.Join(t.TempDir(), "standing-grants.yaml")
			raw := `authority:
  decision_ref: "grant:test-away-mode"
  reviewed_by: "operator:test"
standing_grants:
  - id: grant_window
    capability: reviews.respond
` + c.grant + "\n"
			if err := os.WriteFile(sourcePath, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := ImportStandingGrants(homePath, sourcePath, fixedNow)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("import error = %v, want one naming %q", err, c.wantErr)
			}
			if _, statErr := os.Stat(StandingGrantsPath(homePath)); !os.IsNotExist(statErr) {
				t.Fatalf("a refused import must write nothing, stat=%v", statErr)
			}
		})
	}

	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "standing-grants.yaml")
	raw := `authority:
  decision_ref: "grant:test-away-mode"
  reviewed_by: "operator:test"
standing_grants:
  - id: grant_window
    capability: reviews.respond
    not_before: "2026-12-01T00:00:00Z"
    expires_at: "2027-01-15T00:00:00Z"
    requires_presence: " Away "
`
	if err := os.WriteFile(sourcePath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandingGrants(homePath, sourcePath, fixedNow); err != nil {
		t.Fatalf("valid window import: %v", err)
	}
	registry, err := LoadStandingGrantsFromHome(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Grants) != 1 || registry.Grants[0].NotBefore != "2026-12-01T00:00:00Z" || registry.Grants[0].RequiresPresence != PresenceAway {
		t.Fatalf("normalized grant lost its window: %+v", registry.Grants)
	}
}

func TestEffectivePresence(t *testing.T) {
	from := fixedNow
	through := fixedNow.Add(48 * time.Hour)
	cases := []struct {
		name  string
		state OperatorState
		now   time.Time
		want  string
	}{
		{"absent", OperatorState{}, fixedNow, PresencePresent},
		{"before window", OperatorState{Presence: awayWindow(from, through)}, from.Add(-time.Second), PresencePresent},
		{"at from", OperatorState{Presence: awayWindow(from, through)}, from, PresenceAway},
		{"inside", OperatorState{Presence: awayWindow(from, through)}, from.Add(24 * time.Hour), PresenceAway},
		{"at through is exclusive", OperatorState{Presence: awayWindow(from, through)}, through, PresencePresent},
		{"after", OperatorState{Presence: awayWindow(from, through)}, through.Add(time.Hour), PresencePresent},
		{"explicit present", OperatorState{Presence: &OperatorPresence{State: PresencePresent, SetBy: "operator:test"}}, from.Add(time.Hour), PresencePresent},
	}
	for _, c := range cases {
		if got := c.state.EffectivePresence(c.now); got != c.want {
			t.Errorf("%s: EffectivePresence = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOperatorStateRejectsMalformedPresence(t *testing.T) {
	cases := map[string]string{
		"unknown state":          `{"presence":{"state":"vacation","set_by":"operator:test"}}`,
		"away without window":    `{"presence":{"state":"away","set_by":"operator:test"}}`,
		"away bad time":          `{"presence":{"state":"away","from":"soon","through":"2026-12-31T00:00:00Z","set_by":"operator:test"}}`,
		"through before from":    `{"presence":{"state":"away","from":"2026-12-31T00:00:00Z","through":"2026-12-01T00:00:00Z","set_by":"operator:test"}}`,
		"empty window":           `{"presence":{"state":"away","from":"2026-12-01T00:00:00Z","through":"2026-12-01T00:00:00Z","set_by":"operator:test"}}`,
		"away without set_by":    `{"presence":{"state":"away","from":"2026-12-01T00:00:00Z","through":"2026-12-31T00:00:00Z"}}`,
		"present carries window": `{"presence":{"state":"present","from":"2026-12-01T00:00:00Z","through":"2026-12-31T00:00:00Z","set_by":"operator:test"}}`,
	}
	for name, raw := range cases {
		if _, err := parseOperatorState([]byte(raw)); err == nil || !strings.Contains(err.Error(), "presence") {
			t.Errorf("%s: parse error = %v, want a presence validation error", name, err)
		}
	}
	if _, err := parseOperatorState([]byte(`{"presence":{"state":"away","from":"2026-12-01T00:00:00Z","through":"2026-12-31T00:00:00Z","set_by":"operator:test"}}`)); err != nil {
		t.Fatalf("valid away window rejected: %v", err)
	}
}

func TestOperatorStateReadsLegacyNeedsUmut(t *testing.T) {
	legacy, err := parseOperatorState([]byte(`{"needs_umut":["pol_legacy"],"autonomy_lever":{"level":"L3"}}`))
	if err != nil {
		t.Fatalf("parse legacy operator state: %v", err)
	}
	if len(legacy.NeedsMe) != 1 || legacy.NeedsMe[0] != "pol_legacy" {
		t.Fatalf("legacy needs_umut not read into NeedsMe: %+v", legacy.NeedsMe)
	}
	both, err := parseOperatorState([]byte(`{"needs_me":["pol_new"],"needs_umut":["pol_legacy"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(both.NeedsMe) != 1 || both.NeedsMe[0] != "pol_new" {
		t.Fatalf("needs_me must win over needs_umut, got %+v", both.NeedsMe)
	}
	emptyCanonical, err := parseOperatorState([]byte(`{"needs_me":[],"needs_umut":["pol_legacy"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyCanonical.NeedsMe) != 0 {
		t.Fatalf("a present needs_me key wins even when empty, got %+v", emptyCanonical.NeedsMe)
	}

	homePath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(OperatorStatePath(homePath), []byte(`{"needs_umut":["pol_legacy"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	unresolved, err := CurrentOperatorAuthorityUnresolved(homePath, "work_x", "act_x", "pol_legacy")
	if err != nil || !unresolved {
		t.Fatalf("legacy needs_umut row must read as unresolved, got %v err=%v", unresolved, err)
	}
}

// TestPresenceIsAKeyNotACeiling is the safety property: presence only decides
// whether a grant that ASKS for a presence state is eligible. Both decision
// paths (matchGrant and the L3 envelope) are checked.
func TestPresenceIsAKeyNotACeiling(t *testing.T) {
	window := awayWindow(fixedNow.Add(-time.Hour), fixedNow.Add(time.Hour))
	presences := map[string]*OperatorPresence{PresencePresent: nil, PresenceAway: window}
	type cell struct{ presence, requires string }
	eligible := map[cell]bool{
		{PresencePresent, ""}:              true,
		{PresencePresent, PresenceAny}:     true,
		{PresencePresent, PresencePresent}: true,
		{PresencePresent, PresenceAway}:    false,
		{PresenceAway, ""}:                 true,
		{PresenceAway, PresenceAny}:        true,
		{PresenceAway, PresencePresent}:    false,
		{PresenceAway, PresenceAway}:       true,
	}
	for c, want := range eligible {
		// matchGrant path: a capability+class grant for a held class.
		cfg := DefaultConfig()
		cfg.OperatorPresence = presences[c.presence]
		cfg.StandingGrants = []StandingGrant{{
			ID: "grant_matrix", Capability: "filesystem.write", MutationClass: "external_side_effect",
			RequiresPresence: c.requires,
		}}
		ai := mkAction("external_side_effect", "easy", "docs/x.md", "repo")
		pd := Evaluate(ai, cfg, fixedNow)
		if got := pd.StandingGrantID == "grant_matrix" && pd.Outcome == "allow_with_obligations"; got != want {
			t.Errorf("matchGrant presence=%s requires=%q: eligible=%v want %v (outcome=%q reasons=%v)", c.presence, c.requires, got, want, pd.Outcome, pd.Reasons)
		}
		if !want && !reasonContains(pd.Reasons, "requires_presence") {
			t.Errorf("matchGrant presence=%s requires=%q: hold should name the presence miss, reasons=%v", c.presence, c.requires, pd.Reasons)
		}

		// Envelope path: a path grant pinned to another capability, L3, over a
		// local_reversible hold (escalated by reversibility) -- the one class
		// the envelope still clears while away, so only the key is under test.
		env := DefaultConfig()
		env.OperatorAutonomyLevel = AutonomyLevelFullProjectScope
		env.OperatorAutonomyAuthorityRef = OperatorStateAuthorityRef
		env.OperatorPresence = presences[c.presence]
		env.StandingGrants = []StandingGrant{{
			ID: "grant_matrix_scope", Capability: "some.other.capability", TargetPath: "proj/**",
			RequiresPresence: c.requires,
		}}
		pd = Evaluate(mkAction("local_reversible", "hard", "proj/x.txt", "repo"), env, fixedNow)
		if got := pd.StandingGrantID == "grant_matrix_scope" && pd.Outcome == "allow_with_obligations"; got != want {
			t.Errorf("envelope presence=%s requires=%q: eligible=%v want %v (outcome=%q reasons=%v)", c.presence, c.requires, got, want, pd.Outcome, pd.Reasons)
		}
	}
}

// TestPresenceAwayNeverWidens: with no grant asking for a presence state,
// flipping presence to away never lowers an outcome. It changes exactly one
// thing -- the L3 envelope stops clearing non-local_reversible holds -- and
// every decision the envelope did not make stays byte-identical.
func TestPresenceAwayNeverWidens(t *testing.T) {
	base := DefaultConfig()
	base.OperatorAutonomyLevel = AutonomyLevelFullProjectScope
	base.OperatorAutonomyAuthorityRef = OperatorStateAuthorityRef
	usd := 0.05
	base.StandingGrants = []StandingGrant{
		{ID: "grant_external", Capability: "filesystem.write", MutationClass: "external_side_effect"},
		{ID: "grant_scope", Capability: "shell.operator_scope", ReversibilityCeiling: "irreversible", TargetPath: "proj/**"},
		{ID: "grant_spend", Capability: "filesystem.write", MutationClass: "financial", MaxUSD: &usd, NotBefore: fixedNow.Add(24 * time.Hour).Format(time.RFC3339)},
	}
	intents := []struct {
		name     string
		path     string
		cls      string
		rev      string
		bl       string
		narrowed bool // the envelope clears it while present, not while away
	}{
		{"financial no grant", "money/pay.json", "financial", "manual", "account", false},
		{"financial before not_before", "docs/spend.md", "financial", "easy", "repo", false},
		{"external exact grant", "docs/x.md", "external_side_effect", "easy", "repo", false},
		{"public held", "site/index.html", "public", "manual", "public", false},
		{"local reversible", "docs/y.md", "local_reversible", "easy", "repo", false},
		{"local reversible held in scope", "proj/l.txt", "local_reversible", "hard", "repo", false},
		{"destructive in scope", "proj/z.txt", "destructive", "easy", "repo", true},
		{"external in scope", "proj/mail.json", "external_side_effect", "irreversible", "internet", true},
		{"financial in scope", "proj/pay.json", "financial", "easy", "repo", true},
		{"forbidden", "proj/.env", "local_reversible", "easy", "repo", false},
		{"read", "docs/r.md", "read", "easy", "repo", false},
	}
	away := base
	away.OperatorPresence = awayWindow(fixedNow.Add(-time.Hour), fixedNow.Add(time.Hour))
	for _, in := range intents {
		ai := mkAction(in.cls, in.rev, in.path, in.bl)
		p := Evaluate(ai, base, fixedNow)
		a := Evaluate(ai, away, fixedNow)
		if severity[a.Outcome] < severity[p.Outcome] {
			t.Errorf("%s: presence=away WIDENED the decision: present=%q away=%q", in.name, p.Outcome, a.Outcome)
		}
		if in.narrowed {
			if p.Outcome != "allow_with_obligations" || a.Outcome != "hold" || !reasonContains(a.Reasons, "operator presence is away") {
				t.Errorf("%s: want present allow (envelope) and away hold naming the narrowing, got present=%q away=%q reasons=%v", in.name, p.Outcome, a.Outcome, a.Reasons)
			}
			continue
		}
		if p.Outcome != a.Outcome || p.StandingGrantID != a.StandingGrantID || strings.Join(p.Reasons, "|") != strings.Join(a.Reasons, "|") {
			t.Errorf("%s: presence=away changed a decision the envelope did not make: present=%q/%q %v away=%q/%q %v", in.name, p.Outcome, p.StandingGrantID, p.Reasons, a.Outcome, a.StandingGrantID, a.Reasons)
		}
		if in.name == "financial no grant" && (p.Outcome != "hold" || a.Outcome != "hold") {
			t.Errorf("financial without a grant must hold in both states, got present=%q away=%q", p.Outcome, a.Outcome)
		}
	}
}

// liveShapedProjectsConfig mirrors the live registry's operator projects-tree
// grant: path-scoped, no mutation_class, no expiry, ceiling irreversible,
// pinned to a capability no real intent carries -- so only the L3 envelope can
// use it, and before away mode it cleared every hold under the tree.
func liveShapedProjectsConfig(projects string) Config {
	cfg := DefaultConfig()
	cfg.OperatorAutonomyLevel = AutonomyLevelFullProjectScope
	cfg.OperatorAutonomyAuthorityRef = OperatorStateAuthorityRef
	cfg.StandingGrants = []StandingGrant{{
		ID:                   "grant_operator_projects_tree_20260831",
		Capability:           "shell.operator_scope",
		ReversibilityCeiling: "irreversible",
		TargetPath:           projects + "/**",
	}}
	return cfg
}

func TestAwayNarrowsEnvelopeToLocalReversible(t *testing.T) {
	projects := "/srv/op/Projects"
	present := liveShapedProjectsConfig(projects)
	away := liveShapedProjectsConfig(projects)
	away.OperatorPresence = awayWindow(fixedNow.Add(-time.Hour), fixedNow.Add(time.Hour))

	shell := func(cfg Config, command string) MonitoredAction {
		return MonitorToolAction(ToolActionRequest{
			WorkID: "work_away", Actor: "lane", Harness: "lane", Tool: "Bash",
			Cwd: projects + "/app", Arguments: map[string]any{"command": command}, ToolSchemaRevision: "builtin:bash",
		}, cfg, fixedNow)
	}
	for _, command := range []string{
		"gog gmail send --to someone@example.invalid --subject s --body b",
		"asc reviews respond --review-id 1 --response thanks",
		"curl -X POST https://api.paddle.com/prices -d x",
	} {
		if got := shell(present, command).Decision; got.Outcome != "allow_with_obligations" {
			t.Fatalf("present (today's behavior) %q: want envelope allow, got %q reasons=%v", command, got.Outcome, got.Reasons)
		}
		got := shell(away, command).Decision
		if got.Outcome != "hold" || !reasonContains(got.Reasons, "operator presence is away") {
			t.Fatalf("away %q: want hold naming the narrowing, got %q reasons=%v", command, got.Outcome, got.Reasons)
		}
	}
	// Local work inside the tree keeps flowing while away.
	if got := shell(away, "go build ./...").Decision; !Allowed(got) {
		t.Fatalf("away: local build inside the tree must stay allowed, got %q reasons=%v", got.Outcome, got.Reasons)
	}

	send := mkAction("external_side_effect", "irreversible", projects+"/app/outbox/reply.json", "internet")
	send.Capability = "gmail.message.send"
	send.Action = "send_message"
	if pd := Evaluate(send, present, fixedNow); pd.Outcome != "allow_with_obligations" {
		t.Fatalf("present: send inside the tree is envelope-cleared today, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}
	if pd := Evaluate(send, away, fixedNow); pd.Outcome != "hold" {
		t.Fatalf("away: send must hold without an exact away grant, got %q reasons=%v", pd.Outcome, pd.Reasons)
	}

	withGrant := away
	withGrant.StandingGrants = append(append([]StandingGrant{}, away.StandingGrants...), StandingGrant{
		ID:                   "grant_gmail_send_away",
		Capability:           "gmail.message.send",
		MutationClass:        "external_side_effect",
		ReversibilityCeiling: "irreversible",
		RequiresPresence:     PresenceAway,
	})
	pd := Evaluate(send, withGrant, fixedNow)
	if pd.Outcome != "allow_with_obligations" || pd.StandingGrantID != "grant_gmail_send_away" {
		t.Fatalf("away + exact away grant: want allow via grant_gmail_send_away, got %q grant=%q reasons=%v", pd.Outcome, pd.StandingGrantID, pd.Reasons)
	}
	// The same exact grant is a key only while away.
	presentWithGrant := withGrant
	presentWithGrant.OperatorPresence = nil
	presentWithGrant.StandingGrants = withGrant.StandingGrants[1:]
	if pd := Evaluate(send, presentWithGrant, fixedNow); pd.Outcome != "hold" {
		t.Fatalf("present: an away-only grant must not allow, got %q grant=%q", pd.Outcome, pd.StandingGrantID)
	}
}

func TestImportStandingGrantsPreservesAwayRowFields(t *testing.T) {
	homePath := t.TempDir()
	sourcePath := filepath.Join(t.TempDir(), "standing-grants.json")
	raw := `{
  "schema_version": "compound.standing_grants.v0.1",
  "generated_at": "2026-10-07T00:00:00Z",
  "authority": {"decision_ref": "grant:test-december-away", "reviewed_by": "operator:test"},
  "standing_grants": [{
    "id": "grant_reviews_respond_away",
    "capability": "reviews.respond",
    "mutation_class": "external_side_effect",
    "reversibility_ceiling": "irreversible",
    "max_usd": 0,
    "not_before": "2026-12-01T00:00:00Z",
    "expires_at": "2027-01-15T00:00:00Z",
    "requires_presence": "away",
    "reason": "answer App Store reviews while the operator is away",
    "rollback": "delete the reply in App Store Connect"
  }]
}`
	if err := os.WriteFile(sourcePath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandingGrants(homePath, sourcePath, fixedNow); err != nil {
		t.Fatalf("import a December row with reason/rollback/not_before/requires_presence: %v", err)
	}
	registry, err := LoadStandingGrantsFromHome(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Grants) != 1 {
		t.Fatalf("want 1 grant, got %+v", registry.Grants)
	}
	g := registry.Grants[0]
	if g.NotBefore != "2026-12-01T00:00:00Z" || g.RequiresPresence != PresenceAway ||
		g.Reason != "answer App Store reviews while the operator is away" || g.Rollback != "delete the reply in App Store Connect" {
		t.Fatalf("round-trip lost a field: %+v", g)
	}
	body, err := os.ReadFile(StandingGrantsPath(homePath))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"not_before"`, `"requires_presence"`, `"reason"`, `"rollback"`} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("registry file dropped %s:\n%s", key, body)
		}
	}
}

func installOperatorStateForTest(t *testing.T, homePath string) {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "operator-state.json")
	raw := []byte(`{
  "mode": "closure_over_expansion",
  "needs_umut": ["pol_waiting"],
  "frozen_surfaces": ["kept-verbatim"],
  "autonomy_lever": {"level": "L3", "set_at": "2026-06-30T12:00:00Z", "set_by": "fixture", "meaning": "test"}
}`)
	if err := os.WriteFile(sourcePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportOperatorState(homePath, sourcePath, fixedNow); err != nil {
		t.Fatalf("seed operator state: %v", err)
	}
}

func runPresence(t *testing.T, homePath string, now time.Time, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runPresenceCLI(homePath, args, &stdout, &stderr, now)
	return code, stdout.String(), stderr.String()
}

func TestPresenceCLIRoundTrip(t *testing.T) {
	homePath := t.TempDir()
	from := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	through := time.Date(2027, 1, 5, 0, 0, 0, 0, time.UTC)

	if code, _, stderr := runPresence(t, homePath, fixedNow, "set", "--state", "away",
		"--from", from.Format(time.RFC3339), "--through", through.Format(time.RFC3339), "--by", "operator:test"); code == 0 || !strings.Contains(stderr, "import-operator-state") {
		t.Fatalf("set without an installed operator state must refuse and point at the import, code=%d stderr=%s", code, stderr)
	}

	installOperatorStateForTest(t, homePath)

	if code, _, _ := runPresence(t, homePath, fixedNow, "set", "--state", "away",
		"--from", through.Format(time.RFC3339), "--through", from.Format(time.RFC3339), "--by", "operator:test"); code == 0 {
		t.Fatalf("an inverted window must be refused")
	}
	if code, _, _ := runPresence(t, homePath, fixedNow, "set", "--state", "away",
		"--from", from.Format(time.RFC3339), "--through", through.Format(time.RFC3339)); code != 2 {
		t.Fatalf("set without --by must be a usage error")
	}
	if records, _ := LoadOperatorStateSourceAuthority(homePath); len(records) != 1 {
		t.Fatalf("refused sets must write nothing, source authority records=%d", len(records))
	}

	code, out, stderr := runPresence(t, homePath, fixedNow, "set", "--state", "away",
		"--from", from.Format(time.RFC3339), "--through", through.Format(time.RFC3339),
		"--by", "operator:test", "--reason", "december service")
	if code != 0 {
		t.Fatalf("set away: code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, OperatorStateAuthorityRef) {
		t.Fatalf("set output should name the authority it wrote: %s", out)
	}

	showAt := func(now time.Time) PresenceReport {
		t.Helper()
		code, out, stderr := runPresence(t, homePath, now, "show", "--json")
		if code != 0 {
			t.Fatalf("show: code=%d stderr=%s", code, stderr)
		}
		var report PresenceReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("show --json not JSON: %v\n%s", err, out)
		}
		return report
	}
	if r := showAt(fixedNow); r.Effective != PresencePresent || r.Presence == nil || r.Presence.State != PresenceAway {
		t.Fatalf("before the window show must say present with the scheduled window, got %+v", r)
	}
	if r := showAt(from.Add(72 * time.Hour)); r.Effective != PresenceAway || r.Presence.Through != through.Format(time.RFC3339) || r.Presence.SetBy != "operator:test" {
		t.Fatalf("inside the window show must say away, got %+v", r)
	}
	if r := showAt(through); r.Effective != PresencePresent {
		t.Fatalf("at through (exclusive) show must say present, got %+v", r)
	}
	code, text, _ := runPresence(t, homePath, from.Add(time.Hour), "show")
	if code != 0 || !strings.Contains(text, "effective: away") {
		t.Fatalf("text show inside the window: code=%d\n%s", code, text)
	}

	// The set went through the import door: a second source-authority record,
	// every other key kept, and the policy config now carries the window.
	records, err := LoadOperatorStateSourceAuthority(homePath)
	if err != nil || len(records) != 2 || records[1].ImportedVia != operatorStateImportedVia {
		t.Fatalf("set must append through the import door, records=%+v err=%v", records, err)
	}
	state, err := LoadOperatorStateFromHome(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if OperatorStateLevel(state) != "L3" || len(state.NeedsMe) != 1 || state.Raw["frozen_surfaces"] == nil || state.Raw["needs_umut"] == nil {
		t.Fatalf("set must preserve the rest of operator state, got level=%s needs=%v raw=%v", OperatorStateLevel(state), state.NeedsMe, state.Raw)
	}
	cfg, _, err := LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OperatorPresence == nil || cfg.OperatorPresence.Effective(from.Add(time.Hour)) != PresenceAway {
		t.Fatalf("policy config must carry the presence window, got %+v", cfg.OperatorPresence)
	}

	if code, _, stderr := runPresence(t, homePath, from.Add(time.Hour), "set", "--state", "present", "--by", "operator:test"); code != 0 {
		t.Fatalf("set present: code=%d stderr=%s", code, stderr)
	}
	if r := showAt(from.Add(72 * time.Hour)); r.Effective != PresencePresent || r.Presence == nil || r.Presence.From != "" {
		t.Fatalf("set present must clear the window, got %+v", r)
	}
}

func TestPresenceDoctorLine(t *testing.T) {
	homePath := t.TempDir()
	installOperatorStateForTest(t, homePath)
	from := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	through := time.Date(2027, 1, 5, 0, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(filepath.Join(homePath, "registries"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := StandingGrantRegistry{SchemaVersion: StandingGrantsRegistrySchema, Grants: []StandingGrant{
		{ID: "g_away_1", Capability: "reviews.respond", RequiresPresence: PresenceAway},
		{ID: "g_away_2", Capability: "mail.read", RequiresPresence: PresenceAway},
		{ID: "g_any", Capability: "promo.text"},
	}}
	body, _ := json.Marshal(registry)
	if err := os.WriteFile(StandingGrantsPath(homePath), body, 0o644); err != nil {
		t.Fatal(err)
	}

	var quiet bytes.Buffer
	RenderPresenceDoctorLine(&quiet, homePath, fixedNow)
	if quiet.Len() != 0 {
		t.Fatalf("no window: doctor must print nothing, got %q", quiet.String())
	}

	if code, _, stderr := runPresence(t, homePath, fixedNow, "set", "--state", "away",
		"--from", from.Format(time.RFC3339), "--through", through.Format(time.RFC3339), "--by", "operator:test"); code != 0 {
		t.Fatalf("set away: %s", stderr)
	}
	var scheduled bytes.Buffer
	RenderPresenceDoctorLine(&scheduled, homePath, fixedNow)
	if scheduled.Len() != 0 {
		t.Fatalf("a window not yet active must print nothing, got %q", scheduled.String())
	}
	var active bytes.Buffer
	RenderPresenceDoctorLine(&active, homePath, from.Add(time.Hour))
	line := active.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "away") || !strings.Contains(line, through.Format(time.RFC3339)) || !strings.Contains(line, "2 grant(s)") {
		t.Fatalf("active window doctor line = %q", line)
	}
}
