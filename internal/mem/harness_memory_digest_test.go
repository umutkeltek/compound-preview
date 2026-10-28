package mem

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// digestFixtureClaim writes one claim directly to ledger/claims.jsonl (bypassing
// Capture/Adjudicate) so the test can pin exact Visibility/ClaimType/TopicRefs
// combinations for every gate the digest must enforce.
func digestFixtureClaim(t *testing.T, s *Store, c Claim) {
	t.Helper()
	if c.EvidenceRefs == nil {
		c.EvidenceRefs = []string{}
	}
	if c.CreatedAt == "" {
		c.CreatedAt = rfc3339(t0)
	}
	if err := appendJSONL(s.claimsLog(), &c); err != nil {
		t.Fatalf("write fixture claim %s: %v", c.ID, err)
	}
}

func digestFixtureNeedsReview(t *testing.T, s *Store, seq int64, claimID string) {
	t.Helper()
	if err := appendJSONL(s.edgesLog(), &Edge{
		Seq:          seq,
		Kind:         EdgeKindNeedsReview,
		From:         "synth_contradiction_" + claimID,
		To:           claimID,
		Confidence:   1,
		EvidenceRefs: []string{"cand_synth"},
	}); err != nil {
		t.Fatalf("write needs_review edge for %s: %v", claimID, err)
	}
}

// buildHarnessDigestFixture seeds a ledger with exactly one included claim and
// one excluded claim per gate this digest enforces (spec point 3's ABSOLUTE
// visibility gates), across all three topic groups a `--harness claude-code`
// render compiles. Returns the fictional entity/object strings the test
// asserts on so the invariants stay legible at the call site.
func buildHarnessDigestFixture(t *testing.T, s *Store) {
	t.Helper()
	digestFixtureClaim(t, s, Claim{
		ID: "clm_sl_redact", Seq: 1, Entity: "tool:redact", Predicate: "floor",
		Object: "clean redact scan gates every public push", ClaimType: "constraint",
		Visibility: "ambient", TopicRefs: []string{"standing-law"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureClaim(t, s, Claim{
		ID: "clm_sl_proofcheck", Seq: 2, Entity: "tool:proof-check", Predicate: "pipe_floor",
		Object: "never pipe battery through tail or head", ClaimType: "constraint",
		Visibility: "ambient", TopicRefs: []string{"standing-law"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureClaim(t, s, Claim{
		ID: "clm_sl_retired", Seq: 3, Entity: "tool:old-rule", Predicate: "floor",
		Object: "a retired standing-law rule text", ClaimType: "constraint",
		Visibility: "retired", TopicRefs: []string{"standing-law"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureClaim(t, s, Claim{
		ID: "clm_sl_repostate", Seq: 4, Entity: "repo:fictional-compound", Predicate: "head",
		Object: "db210680fictional", ClaimType: "status",
		Visibility: "operational", TopicRefs: []string{"standing-law"}, ValidFrom: "2026-01-01T00:00:00Z",
	})

	digestFixtureClaim(t, s, Claim{
		ID: "clm_trap_silentcatch", Seq: 5, Entity: "pattern:silent-catch", Predicate: "trap",
		Object: "a silently swallowed error hides the real failure", ClaimType: "constraint",
		Visibility: "contextual", TopicRefs: []string{"trap"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureClaim(t, s, Claim{
		ID: "clm_trap_private", Seq: 6, Entity: "finding:private-trap-note", Predicate: "trap",
		Object: "THIS-MUST-NEVER-RENDER a genuinely private trap detail", ClaimType: "constraint",
		Visibility: "private", TopicRefs: []string{"trap"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureClaim(t, s, Claim{
		ID: "clm_trap_decision", Seq: 7, Entity: "decision:fictional-freeze", Predicate: "decided",
		Object: "a decision claim tagged trap must never enter the digest", ClaimType: "decision",
		Visibility: "operational", TopicRefs: []string{"trap"}, ValidFrom: "2026-01-01T00:00:00Z",
	})

	digestFixtureClaim(t, s, Claim{
		ID: "clm_h_ccperm", Seq: 8, Entity: "claude-code:permission-mode", Predicate: "default",
		Object: "plan-mode default, bypass is explicit opt-in", ClaimType: "status",
		Visibility: "operational", TopicRefs: []string{"harness:claude-code"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureClaim(t, s, Claim{
		ID: "clm_h_codex", Seq: 9, Entity: "codex:effort-default", Predicate: "value",
		Object: "a codex-harness claim must never appear in a claude-code digest", ClaimType: "status",
		Visibility: "operational", TopicRefs: []string{"harness:codex"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureClaim(t, s, Claim{
		ID: "clm_h_contested", Seq: 10, Entity: "claude-code:contested-setting", Predicate: "value",
		Object: "a needs-review claim must never appear in the digest", ClaimType: "status",
		Visibility: "operational", TopicRefs: []string{"harness:claude-code"}, ValidFrom: "2026-01-01T00:00:00Z",
	})
	digestFixtureNeedsReview(t, s, 11, "clm_h_contested")
	digestFixtureClaim(t, s, Claim{
		ID: "clm_h_volatile", Seq: 12, Entity: "claude-code:session-cache", Predicate: "state",
		Object: "a volatile-visibility claim must never appear in the digest", ClaimType: "status",
		Visibility: "volatile", ExpiresAt: "2099-01-01T00:00:00Z", TopicRefs: []string{"harness:claude-code"},
		ValidFrom: "2026-01-01T00:00:00Z",
	})
}

func TestRenderHarnessDigestGroupsGatesAndOrdersDeterministically(t *testing.T) {
	s := newStore(t)
	buildHarnessDigestFixture(t, s)

	report, body, err := s.RenderHarnessDigest(
		HarnessDigestOptions{Harness: "claude-code"},
		HarnessDigestMeta{GeneratedBy: "compound 0.1.0 (build fictionalsha)", RegenerateCmd: "compound memory render-harness-digest --harness claude-code --out /fictional/out.md"},
		t0,
	)
	if err != nil {
		t.Fatalf("RenderHarnessDigest: %v", err)
	}

	// spec point 3, the absolute falsifier: a gated claim rendered is the
	// failure mode that matters most. Assert the private fixture's content
	// NEVER appears anywhere in the rendered body, by any substring.
	text := string(body)
	if strings.Contains(text, "THIS-MUST-NEVER-RENDER") || strings.Contains(text, "finding:private-trap-note") {
		t.Fatalf("private-visibility fixture claim leaked into rendered digest:\n%s", text)
	}

	// Every other excluded fixture must also be absent: wrong harness,
	// decision claim_type, contested/needs_review, retired, volatile,
	// operational repo-state.
	forbidden := []string{
		"codex:effort-default", "decision:fictional-freeze", "claude-code:contested-setting",
		"tool:old-rule", "claude-code:session-cache", "repo:fictional-compound",
	}
	for _, want := range forbidden {
		if strings.Contains(text, want) {
			t.Fatalf("gated fixture %q leaked into rendered digest:\n%s", want, text)
		}
	}

	// The four legitimate, promoted, ambient-visible positions must all render.
	included := []string{"tool:redact", "tool:proof-check", "pattern:silent-catch", "claude-code:permission-mode"}
	for _, want := range included {
		if !strings.Contains(text, want) {
			t.Fatalf("expected entity %q in rendered digest, missing:\n%s", want, text)
		}
	}

	if report.PositionCount != 4 {
		t.Fatalf("report.PositionCount = %d, want 4: %+v", report.PositionCount, report)
	}
	if report.ExcludedCount != 6 {
		t.Fatalf("report.ExcludedCount = %d, want 6 (2 gated per in-scope group): %+v", report.ExcludedCount, report)
	}
	if len(report.Groups) != 3 {
		t.Fatalf("report.Groups = %+v, want 3 groups", report.Groups)
	}
	wantGroupCounts := map[string]int{"standing-law": 2, "trap": 1, "harness:claude-code": 1}
	for _, g := range report.Groups {
		if g.PositionCount != wantGroupCounts[g.Topic] {
			t.Fatalf("group %s position_count = %d, want %d", g.Topic, g.PositionCount, wantGroupCounts[g.Topic])
		}
	}

	// Deterministic ordering: group order is the fixed declared order
	// (standing-law, trap, harness:<harness>); within standing-law, entity
	// order is alphabetical ("tool:proof-check" before "tool:redact").
	idxStandingLaw := strings.Index(text, "### standing-law")
	idxTrap := strings.Index(text, "### trap")
	idxHarness := strings.Index(text, "### harness:claude-code")
	if idxStandingLaw < 0 || idxTrap < 0 || idxHarness < 0 {
		t.Fatalf("missing one or more group headers:\n%s", text)
	}
	if !(idxStandingLaw < idxTrap && idxTrap < idxHarness) {
		t.Fatalf("group headers out of declared order: standing-law=%d trap=%d harness=%d", idxStandingLaw, idxTrap, idxHarness)
	}
	idxProofCheck := strings.Index(text, "tool:proof-check")
	idxRedact := strings.Index(text, "tool:redact")
	if !(idxProofCheck < idxRedact) {
		t.Fatalf("within-group entity ordering not alphabetical: proof-check=%d redact=%d", idxProofCheck, idxRedact)
	}

	if report.Harness != "claude-code" {
		t.Fatalf("report.Harness = %q, want claude-code", report.Harness)
	}
	if report.SchemaVersion != HarnessDigestSchemaVersion {
		t.Fatalf("report.SchemaVersion = %q, want %q", report.SchemaVersion, HarnessDigestSchemaVersion)
	}
	if report.ContentSHA256 == "" || len(report.ContentSHA256) != 64 {
		t.Fatalf("report.ContentSHA256 = %q, want a 64-hex-char sha256", report.ContentSHA256)
	}
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("rendered body should open with YAML front matter, got:\n%.80s", text)
	}
	if !strings.Contains(text, "content_sha256: "+report.ContentSHA256) {
		t.Fatalf("front matter should embed the report's content_sha256:\n%.400s", text)
	}
}

// TestRenderHarnessDigestIncludesLegacyCanonicalTopicAxis pins the real-home
// compatibility shape: promoted claims that predate TopicRefs still belong to
// a digest group through the Position topic axis.
func TestRenderHarnessDigestIncludesLegacyCanonicalTopicAxis(t *testing.T) {
	s := newStore(t)
	_, err := s.ImportTopics([]byte(`{
		"schema_version": "compound.memory_topics.v0.1",
		"topics": [
			{"slug": "standing-law", "narrows_to": ["candidate-governance"]},
			{"slug": "candidate-governance", "part_of": ["standing-law"]}
		]
	}`), t0)
	if err != nil {
		t.Fatal(err)
	}
	digestFixtureClaim(t, s, Claim{
		ID:        "clm_legacy_canonical_topic",
		Seq:       10,
		Entity:    "candidate-inbox",
		Predicate: "authority_status",
		Object:    "candidate inbox is evidence, not truth",
		ClaimType: "constraint",
		Qualifiers: map[string]string{
			"domain": "candidate-governance",
		},
		Visibility: "operational",
		State:      ClaimStatePromoted,
		ValidFrom:  "2026-01-01T00:00:00Z",
	})

	report, body, err := s.RenderHarnessDigest(
		HarnessDigestOptions{Harness: "claude-code"},
		HarnessDigestMeta{},
		t0,
	)
	if err != nil {
		t.Fatalf("RenderHarnessDigest: %v", err)
	}
	if report.PositionCount != 1 || report.Groups[0].PositionCount != 1 {
		t.Fatalf("legacy canonical-topic claim produced report %+v, want one standing-law position", report)
	}
	if !strings.Contains(string(body), "candidate-inbox") {
		t.Fatalf("digest omitted legacy canonical-topic claim:\n%s", body)
	}
}

// TestRenderHarnessDigestDifferentHarnessScopesIndependently pins that
// harness:<name> really does scope by name — a codex render sees the codex
// claim and NOT the claude-code one, the mirror image of the test above.
func TestRenderHarnessDigestDifferentHarnessScopesIndependently(t *testing.T) {
	s := newStore(t)
	buildHarnessDigestFixture(t, s)

	report, body, err := s.RenderHarnessDigest(HarnessDigestOptions{Harness: "codex"}, HarnessDigestMeta{}, t0)
	if err != nil {
		t.Fatalf("RenderHarnessDigest: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "codex:effort-default") {
		t.Fatalf("codex-scoped digest should include the codex harness claim:\n%s", text)
	}
	if strings.Contains(text, "claude-code:permission-mode") {
		t.Fatalf("codex-scoped digest must not include the claude-code harness claim:\n%s", text)
	}
	wantGroupCounts := map[string]int{"standing-law": 2, "trap": 1, "harness:codex": 1}
	for _, g := range report.Groups {
		if g.PositionCount != wantGroupCounts[g.Topic] {
			t.Fatalf("group %s position_count = %d, want %d", g.Topic, g.PositionCount, wantGroupCounts[g.Topic])
		}
	}
}

// TestRenderHarnessDigestContentHashStableAcrossGeneratedAt pins that the
// content_sha256 is computed over the body only: two renders of the same
// unchanged ledger at different clock times must produce byte-identical
// content_sha256 even though generated_at moves (mirrors
// internal/harness.InstructionPack's GeneratedAt-excluded hashing).
func TestRenderHarnessDigestContentHashStableAcrossGeneratedAt(t *testing.T) {
	s := newStore(t)
	buildHarnessDigestFixture(t, s)

	reportA, bodyA, err := s.RenderHarnessDigest(HarnessDigestOptions{Harness: "claude-code"}, HarnessDigestMeta{GeneratedBy: "compound dev"}, t0)
	if err != nil {
		t.Fatalf("render A: %v", err)
	}
	reportB, bodyB, err := s.RenderHarnessDigest(HarnessDigestOptions{Harness: "claude-code"}, HarnessDigestMeta{GeneratedBy: "compound dev"}, t0.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("render B: %v", err)
	}
	if reportA.ContentSHA256 != reportB.ContentSHA256 {
		t.Fatalf("content_sha256 drifted across identical ledger state: A=%s B=%s", reportA.ContentSHA256, reportB.ContentSHA256)
	}
	if reportA.GeneratedAt == reportB.GeneratedAt {
		t.Fatalf("generated_at should differ between the two renders (t0 vs t0+3h), got both %s", reportA.GeneratedAt)
	}
	if string(bodyA) == string(bodyB) {
		t.Fatalf("full rendered bytes should differ (generated_at is embedded in front matter) even though content_sha256 matches")
	}
}

func TestRenderHarnessDigestUnknownHarnessRendersEmptyGroupNotError(t *testing.T) {
	s := newStore(t)
	buildHarnessDigestFixture(t, s)

	report, body, err := s.RenderHarnessDigest(HarnessDigestOptions{Harness: "some-未来-harness"}, HarnessDigestMeta{}, t0)
	if err != nil {
		t.Fatalf("RenderHarnessDigest: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "### harness:some-未来-harness") {
		t.Fatalf("expected an empty harness group header, got:\n%s", text)
	}
	if !strings.Contains(text, "(none)") {
		t.Fatalf("expected an explicit (none) marker for the empty harness group, got:\n%s", text)
	}
	for _, g := range report.Groups {
		if g.Topic == "harness:some-未来-harness" && g.PositionCount != 0 {
			t.Fatalf("unknown harness group should have position_count 0, got %+v", g)
		}
	}
}

func TestRenderHarnessDigestRequiresHarness(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.RenderHarnessDigest(HarnessDigestOptions{}, HarnessDigestMeta{}, t0); err == nil {
		t.Fatal("expected an error when --harness is blank")
	}
}

func TestWriteHarnessDigestAtomicAndVerifyRoundTripsCleanThenDetectsDrift(t *testing.T) {
	s := newStore(t)
	buildHarnessDigestFixture(t, s)

	report, body, err := s.RenderHarnessDigest(
		HarnessDigestOptions{Harness: "claude-code"},
		HarnessDigestMeta{GeneratedBy: "compound dev", RegenerateCmd: "compound memory render-harness-digest --harness claude-code --out /fictional/nested/digest.md"},
		t0,
	)
	if err != nil {
		t.Fatalf("RenderHarnessDigest: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "nested", "operator-local", "digest.md")
	if err := WriteHarnessDigestAtomic(outPath, body); err != nil {
		t.Fatalf("WriteHarnessDigestAtomic: %v", err)
	}

	drift, err := VerifyHarnessDigestFile(outPath)
	if err != nil {
		t.Fatalf("VerifyHarnessDigestFile: %v", err)
	}
	if drift.Drifted {
		t.Fatalf("fresh render should never drift: %+v", drift)
	}
	if drift.DeclaredContentSHA256 != report.ContentSHA256 || drift.RecomputedContentSHA256 != report.ContentSHA256 {
		t.Fatalf("drift report hashes = %+v, want both to equal report.ContentSHA256=%s", drift, report.ContentSHA256)
	}

	// Hand-edit the body (below the front matter) without updating the
	// declared hash — digest-doctor must catch this without mutating the file.
	edited := strings.Replace(string(body), "tool:redact", "tool:redact-hand-edited", 1)
	if err := WriteHarnessDigestAtomic(outPath, []byte(edited)); err != nil {
		t.Fatalf("re-write edited body: %v", err)
	}
	driftAfterEdit, err := VerifyHarnessDigestFile(outPath)
	if err != nil {
		t.Fatalf("VerifyHarnessDigestFile after edit: %v", err)
	}
	if !driftAfterEdit.Drifted {
		t.Fatalf("digest-doctor should detect the hand-edit as drift: %+v", driftAfterEdit)
	}

	// Read-only invariant: re-verifying must not change the on-disk bytes.
	againDrift, err := VerifyHarnessDigestFile(outPath)
	if err != nil {
		t.Fatalf("VerifyHarnessDigestFile (second read): %v", err)
	}
	if againDrift != driftAfterEdit {
		t.Fatalf("VerifyHarnessDigestFile is not idempotent/read-only: first=%+v second=%+v", driftAfterEdit, againDrift)
	}
}

func TestVerifyHarnessDigestFileMissingFrontMatterErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-digest.md")
	if err := WriteHarnessDigestAtomic(path, []byte("# just a plain file\nno front matter here\n")); err != nil {
		t.Fatalf("write plain file: %v", err)
	}
	if _, err := VerifyHarnessDigestFile(path); err == nil {
		t.Fatal("expected an error for a file with no front matter")
	}
}
