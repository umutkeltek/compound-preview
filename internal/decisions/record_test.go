package decisions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	homepkg "compound/internal/home"
	"compound/internal/mem"
)

// TestRecordRequiresReviewedBy is falsifier d (664e1a7 parity): `decisions
// record` refuses without --reviewed-by, and writes nothing to disk.
func TestRecordRequiresReviewedBy(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Record(store, home, RecordInput{
		Kind:  KindGrant,
		Title: "Grant director apply",
		Scope: "director-apply",
	})
	if err == nil {
		t.Fatal("record without --reviewed-by must be refused")
	}
	if !strings.Contains(err.Error(), "reviewed-by") && !strings.Contains(err.Error(), "reviewed_by") {
		t.Fatalf("refusal must name reviewed-by, got %v", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(home, "decisions", "operator"))
	if readErr == nil && len(entries) != 0 {
		t.Fatalf("no file should be written on refusal, found %v", entries)
	}
}

func TestPolicyDecisionImportSharesAutonomyAdmissionLock(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "typed-grant.md")
	body := `---
id: concurrent-import-grant
title: Concurrent import grant
kind: grant
status: accepted
owner: operator:test
reviewed_by: operator:test
scope: autonomy-evidence-threshold
auto_admit_class: job_health
min_terminal_samples: 2
min_window_days: 1
max_failures: 0
---

# Concurrent import grant
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	if err := homepkg.WithAutonomyAdmissionLock(home, func() error {
		go func() {
			close(started)
			_, err := ImportPolicyDecision(store, path, time.Now())
			done <- err
		}()
		<-started
		select {
		case err := <-done:
			t.Fatalf("decision import crossed the admission critical section: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestRecordRequiresKnownKind proves --kind is restricted to grant|freeze|ruling.
func TestRecordRequiresKnownKind(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Record(store, home, RecordInput{
		Kind:       "policy",
		Title:      "Not a grant/freeze/ruling",
		ReviewedBy: "operator:test",
	})
	if err == nil {
		t.Fatal("record with an unknown --kind must be refused")
	}
}

// TestRecordWritesUnderOperatorDirAndImportsThroughDecisionsStore is the
// headline Slice 1 proof: a grant record lands under
// $COMPOUND_HOME/decisions/operator/ (never the repo, never ~/.compound —
// tests use temp homes only) and is imported through the SAME
// ImportPolicyDecision path every other policy decision uses, landing in the
// store with real lifecycle qualifiers (kind, status, scope, reviewed_by).
func TestRecordWritesUnderOperatorDirAndImportsThroughDecisionsStore(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	report, err := Record(store, home, RecordInput{
		Kind:       KindGrant,
		ID:         "director-apply-grant-2026-07-10",
		Title:      "Grant director --apply",
		ReviewedBy: "operator:test",
		Scope:      "director-apply",
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(report.Path, filepath.Join(home, "decisions", "operator")) {
		t.Fatalf("record path = %q, want under decisions/operator/", report.Path)
	}
	if _, statErr := os.Stat(report.Path); statErr != nil {
		t.Fatalf("record file not written: %v", statErr)
	}
	if report.Import.ClaimID == "" {
		t.Fatalf("grant record should promote to a current claim: %+v", report.Import)
	}
	l := report.Import.Lifecycle
	if l.ID != "director-apply-grant-2026-07-10" || l.Kind != KindGrant || l.Status != StatusAccepted {
		t.Fatalf("lifecycle = %+v", l)
	}
	if l.Scope != "director-apply" || l.ReviewedBy != "operator:test" {
		t.Fatalf("scope/reviewed_by not preserved: %+v", l)
	}
	if l.Falsifier == "" || l.ReviewTrigger == "" {
		t.Fatalf("record must synthesize lifecycle proof so it can promote: %+v", l)
	}
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range claims {
		if c.ID == report.Import.ClaimID {
			found = true
		}
	}
	if !found {
		t.Fatalf("claim %s not visible via AllCurrent()", report.Import.ClaimID)
	}
}

// TestRecordFreezeScopeQualifiers proves a freeze record's explicit scope
// (owner globs / predicate keywords) round-trips through the store.
func TestRecordFreezeScopeQualifiers(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Record(store, home, RecordInput{
		Kind:       KindFreeze,
		ID:         "direction-freeze-2026-07-10",
		Title:      "Freeze new-organ/absorption imports",
		ReviewedBy: "operator:test",
		OwnerGlobs: []string{"compound-authority"},
		Keywords:   []string{"absorption_debt", "harness_edit"},
		Now:        time.Date(2026, 7, 10, 12, 5, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	l := report.Import.Lifecycle
	if len(l.FreezeOwnerGlobs) != 1 || l.FreezeOwnerGlobs[0] != "compound-authority" {
		t.Fatalf("freeze_owner_globs = %v", l.FreezeOwnerGlobs)
	}
	if len(l.FreezeKeywords) != 2 {
		t.Fatalf("freeze_keywords = %v", l.FreezeKeywords)
	}
}

func TestRecordAutonomyGrantThresholdsRoundTrip(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Record(store, home, RecordInput{
		Kind:               KindGrant,
		ID:                 "job-health-autonomy-test",
		Title:              "Permit evidence-threshold admission for job health",
		ReviewedBy:         "operator:test",
		Scope:              "autonomy-evidence-threshold",
		AutoAdmitClass:     "job_health",
		MinTerminalSamples: 5,
		MinWindowDays:      7,
		MaxFailures:        0,
		Now:                time.Date(2026, 7, 10, 12, 10, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	l := report.Import.Lifecycle
	if l.AutoAdmitClass != "job_health" || l.MinTerminalSamples != 5 || l.MinWindowDays != 7 || l.MaxFailures != 0 {
		t.Fatalf("autonomy thresholds did not round-trip: %+v", l)
	}
}

func TestRecordAutonomyGrantRequiresCompleteSafeThreshold(t *testing.T) {
	base := RecordInput{
		Kind:               KindGrant,
		Title:              "Permit evidence-threshold admission for job health",
		ReviewedBy:         "operator:test",
		Scope:              "autonomy-evidence-threshold",
		AutoAdmitClass:     "job_health",
		MinTerminalSamples: 5,
		MinWindowDays:      7,
		MaxFailures:        0,
	}
	tests := []struct {
		name string
		edit func(*RecordInput)
	}{
		{name: "wrong kind", edit: func(in *RecordInput) { in.Kind = KindRuling }},
		{name: "wrong scope", edit: func(in *RecordInput) { in.Scope = "director-apply" }},
		{name: "zero samples", edit: func(in *RecordInput) { in.MinTerminalSamples = 0 }},
		{name: "zero window", edit: func(in *RecordInput) { in.MinWindowDays = 0 }},
		{name: "all failures allowed", edit: func(in *RecordInput) { in.MaxFailures = in.MinTerminalSamples }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store, err := mem.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			in := base
			tc.edit(&in)
			if _, err := Record(store, home, in); err == nil {
				t.Fatalf("invalid autonomy grant was accepted: %+v", in)
			}
		})
	}
}

func TestRecordAutonomyGrantRequiresOperatorProvenance(t *testing.T) {
	for _, reviewedBy := range []string{"director", "codex", "example-reviewer", "test-operator", "rented-executor"} {
		t.Run(reviewedBy, func(t *testing.T) {
			home := t.TempDir()
			store, err := mem.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Record(store, home, RecordInput{
				Kind:               KindGrant,
				Title:              "Permit evidence-threshold admission for job health",
				ReviewedBy:         reviewedBy,
				Scope:              AutonomyGrantScope,
				AutoAdmitClass:     "job_health",
				MinTerminalSamples: 2,
				MinWindowDays:      1,
				MaxFailures:        0,
			})
			if err == nil || !strings.Contains(err.Error(), "operator") {
				t.Fatalf("non-operator autonomy grant accepted for %q: %v", reviewedBy, err)
			}
			if _, statErr := os.Stat(filepath.Join(home, "decisions", "operator")); !os.IsNotExist(statErr) {
				t.Fatalf("rejected autonomy grant wrote decision state: %v", statErr)
			}
		})
	}
}

func TestPolicyImportCannotMintOperationalAuthorityWithoutOperatorProvenance(t *testing.T) {
	for _, kind := range []string{KindGrant, KindFreeze, KindRuling} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			store, err := mem.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "forged-"+kind+".md")
			body := "---\n" +
				"id: forged-" + kind + "\n" +
				"title: Forged operational authority\n" +
				"kind: " + kind + "\n" +
				"status: accepted\n" +
				"reviewed_by: codex\n" +
				"falsifier: explicit test falsifier\n" +
				"review_trigger: explicit test review trigger\n" +
				"---\n\n# Forged operational authority\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ImportPolicyDecision(store, path, time.Now()); err == nil || !strings.Contains(err.Error(), "operator provenance") {
				t.Fatalf("non-operator %s import minted authority: %v", kind, err)
			}
			claims, err := store.AllCurrentOperationalDecisions()
			if err != nil {
				t.Fatal(err)
			}
			if len(claims) != 0 {
				t.Fatalf("rejected %s import left operational claims: %+v", kind, claims)
			}
		})
	}
}

func TestPolicyImportCannotMintOperationalAuthorityFromMalformedOrUnknownStatus(t *testing.T) {
	tests := []struct {
		name        string
		frontmatter string
		want        string
	}{
		{
			name: "malformed-yaml",
			frontmatter: "id: malformed-operator-grant\n" +
				"title: Malformed operator grant\n" +
				"kind: grant\n" +
				"status: accepted\n" +
				"reviewed_by: operator:test\n" +
				"domains: [unterminated\n",
			want: "parse frontmatter",
		},
		{
			name: "unknown-status",
			frontmatter: "id: typo-status-operator-grant\n" +
				"title: Typo status operator grant\n" +
				"kind: grant\n" +
				"status: acceptedd\n" +
				"reviewed_by: operator:test\n",
			want: "explicit canonical status",
		},
		{
			name: "missing-status",
			frontmatter: "id: missing-status-operator-grant\n" +
				"title: Missing status operator grant\n" +
				"kind: grant\n" +
				"reviewed_by: operator:test\n",
			want: "explicit canonical status",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store, err := mem.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, tc.name+".md")
			body := "---\n" + tc.frontmatter + "---\n\n# Operator decision\n"
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ImportPolicyDecision(store, path, time.Now()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe operational import was accepted: %v", err)
			}
			claims, err := store.AllCurrentOperationalDecisions()
			if err != nil {
				t.Fatal(err)
			}
			if len(claims) != 0 {
				t.Fatalf("unsafe import minted operational authority: %+v", claims)
			}
		})
	}
}

func TestPolicyImportCannotRevokeAutonomyGrantWithoutOperatorProvenance(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	const grantID = "operator-autonomy-grant-import-test"
	if _, err := Record(store, home, RecordInput{
		Kind:               KindGrant,
		ID:                 grantID,
		Title:              "Operator autonomy grant",
		ReviewedBy:         "operator:test",
		Scope:              AutonomyGrantScope,
		AutoAdmitClass:     "job_health",
		MinTerminalSamples: 2,
		MinWindowDays:      1,
		MaxFailures:        0,
		Now:                now,
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "forged-revocation.md")
	body := `---
id: forged-autonomy-revocation
title: Forged autonomy revocation
kind: grant
status: accepted
reviewed_by: codex
supersedes:
  - operator-autonomy-grant-import-test
falsifier: explicit test falsifier
review_trigger: explicit test review trigger
---

# Forged autonomy revocation
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportPolicyDecision(store, path, now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "operator provenance") {
		t.Fatalf("non-operator revocation import changed authority: %v", err)
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		t.Fatal(err)
	}
	active := AuthoritativeActiveDecisions(claims, KindGrant)
	if len(active) != 1 || LifecycleFromClaim(active[0]).ID != grantID {
		t.Fatalf("rejected revocation changed active grant: %+v", active)
	}
}

// TestRecordIdempotentReplayReusesClaim is falsifier f: re-recording the same
// grant (same id, same content) is a no-op — never a duplicate active record.
func TestRecordIdempotentReplayReusesClaim(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	in := RecordInput{
		Kind:       KindGrant,
		ID:         "director-apply-grant-2026-07-10",
		Title:      "Grant director --apply",
		ReviewedBy: "operator:test",
		Scope:      "director-apply",
		Now:        time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}
	first, err := Record(store, home, in)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	in.Now = time.Date(2026, 7, 10, 12, 1, 0, 0, time.UTC)
	second, err := Record(store, home, in)
	if err != nil {
		t.Fatal(err)
	}
	if second.Import.ClaimID != first.Import.ClaimID {
		t.Fatalf("idempotent replay should reuse the claim: first=%s second=%s", first.Import.ClaimID, second.Import.ClaimID)
	}
	after, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("idempotent replay created a duplicate candidate: before=%d after=%d", len(before), len(after))
	}
}

// TestRecordUndoOfSupersedesPriorGrant is falsifier c: an --undo-of record
// supersedes the prior grant/freeze; ActiveDecisions no longer returns it,
// but the prior claim stays in history (never deleted).
func TestRecordUndoOfSupersedesPriorGrant(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := Record(store, home, RecordInput{
		Kind:       KindGrant,
		ID:         "director-apply-grant-2026-07-10",
		Title:      "Grant director --apply",
		ReviewedBy: "operator:test",
		Scope:      "director-apply",
		Now:        time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	active := ActiveDecisions(claims, KindGrant)
	if len(active) != 1 || LifecycleFromClaim(active[0]).ID != "director-apply-grant-2026-07-10" {
		t.Fatalf("grant should be active before undo: %+v", active)
	}

	undo, err := Record(store, home, RecordInput{
		Kind:       KindGrant,
		ID:         "director-apply-grant-2026-07-10-revoke",
		Title:      "Revoke director --apply grant",
		ReviewedBy: "operator:test",
		UndoOf:     "director-apply-grant-2026-07-10",
		Now:        time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if undo.Import.Lifecycle.Supersedes[0] != "director-apply-grant-2026-07-10" {
		t.Fatalf("undo record should carry supersedes: %+v", undo.Import.Lifecycle)
	}

	claimsAfter, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	activeAfter := ActiveDecisions(claimsAfter, KindGrant)
	for _, c := range activeAfter {
		if LifecycleFromClaim(c).ID == "director-apply-grant-2026-07-10" {
			t.Fatalf("undo must flip the prior grant inactive: %+v", activeAfter)
		}
	}
	// Falsifier c: prior record stays in history — its claim is not deleted.
	foundPrior := false
	for _, c := range claimsAfter {
		if c.ID == grant.Import.ClaimID {
			foundPrior = true
		}
	}
	if !foundPrior {
		t.Fatalf("prior grant claim %s must stay in history", grant.Import.ClaimID)
	}
}

// TestActiveDecisionsFiltersKindStatusAndSupersession is the unit-level proof
// for the read helper both the director grant consumer and the freeze admit
// guard share.
func TestActiveDecisionsFiltersKindStatusAndSupersession(t *testing.T) {
	claims := []mem.Claim{
		{
			ID:         "claim_grant_active",
			ClaimType:  "decision",
			Qualifiers: operationalDecisionQualifiers(lifecycleQualifiers(KindGrant, "decisions-repo", "grant-active", StatusAccepted)),
		},
		{
			ID:         "claim_grant_superseded",
			ClaimType:  "decision",
			Qualifiers: operationalDecisionQualifiers(lifecycleQualifiers(KindGrant, "decisions-repo", "grant-old", StatusAccepted)),
		},
		{
			ID:         "claim_grant_undo",
			ClaimType:  "decision",
			Qualifiers: mapWithSupersedes(operationalDecisionQualifiers(lifecycleQualifiers(KindGrant, "decisions-repo", "grant-undo", StatusAccepted)), "grant-old"),
		},
		{
			ID:         "claim_freeze_active",
			ClaimType:  "decision",
			Qualifiers: operationalDecisionQualifiers(lifecycleQualifiers(KindFreeze, "decisions-repo", "freeze-active", StatusAccepted)),
		},
		{
			ID:        "claim_memory",
			ClaimType: "memory",
		},
	}
	active := ActiveDecisions(claims, KindGrant)
	if len(active) != 2 {
		t.Fatalf("active grants = %d, want 2 (grant-active, grant-undo): %+v", len(active), active)
	}
	ids := map[string]bool{}
	for _, c := range active {
		ids[LifecycleFromClaim(c).ID] = true
	}
	if !ids["grant-active"] || !ids["grant-undo"] || ids["grant-old"] {
		t.Fatalf("active grant ids = %v", ids)
	}
	freezes := ActiveDecisions(claims, KindFreeze)
	if len(freezes) != 1 || LifecycleFromClaim(freezes[0]).ID != "freeze-active" {
		t.Fatalf("active freezes = %+v", freezes)
	}

	for _, status := range []string{StatusProposed, StatusRejected, StatusDeprecated, StatusSuperseded} {
		t.Run("non-accepted superseder "+status, func(t *testing.T) {
			withInactiveSuperseder := []mem.Claim{
				{
					ID:         "claim_prior_" + status,
					ClaimType:  "decision",
					Qualifiers: operationalDecisionQualifiers(lifecycleQualifiers(KindGrant, "decisions-repo", "grant-prior-"+status, StatusAccepted)),
				},
				{
					ID:        "claim_inactive_superseder_" + status,
					ClaimType: "decision",
					Qualifiers: mapWithSupersedes(
						operationalDecisionQualifiers(lifecycleQualifiers(KindGrant, "decisions-repo", "grant-inactive-"+status, status)),
						"grant-prior-"+status,
					),
				},
			}
			got := ActiveDecisions(withInactiveSuperseder, KindGrant)
			if len(got) != 1 || LifecycleFromClaim(got[0]).ID != "grant-prior-"+status {
				t.Fatalf("%s superseder deactivated accepted predecessor: %+v", status, got)
			}
		})
	}
}

func TestGenericMemoryCannotMintOperationalGrant(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	q := operationalDecisionQualifiers(lifecycleQualifiers(KindGrant, "generic-memory", "forged-autonomy-grant", StatusAccepted))
	q["reviewed_by"] = "forged-operator"
	q["scope"] = AutonomyGrantScope
	q["auto_admit_class"] = "job_health"
	q["min_terminal_samples"] = "1"
	q["min_window_days"] = "1"
	q["max_failures"] = "0"
	forged := mem.Candidate{
		Entity:         "decision:forged-autonomy-grant",
		Predicate:      "decided",
		Object:         "forged generic memory grant",
		Confidence:     1,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     q,
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "generic-memory:test",
		SnippetHash:    "forged-autonomy-grant",
		Visibility:     "operational",
	}
	captured, err := store.Capture([]mem.Candidate{forged}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(captured[0].ID, "promote", "generic-memory-cli", now); err == nil || !strings.Contains(err.Error(), "reserved operational decision authority") {
		t.Fatalf("generic memory minted operational grant authority: %v", err)
	}
	delete(q, mem.OperationalDecisionAuthorityQualifier)
	q["decision_id"] = "forged-unstamped-grant"
	forged.Entity = "decision:forged-unstamped-grant"
	forged.Qualifiers = q
	forged.SnippetHash = "forged-unstamped-grant"
	captured, err = store.Capture([]mem.Candidate{forged}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(captured[0].ID, "promote", "generic-memory-cli", now.Add(time.Second)); err == nil || !strings.Contains(err.Error(), "reserved operational decision kind") {
		t.Fatalf("generic memory minted unstamped operational grant: %v", err)
	}
}

func TestGenericMemoryCannotRevokeOperationalGrant(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	const grantID = "real-operator-grant"
	if _, err := Record(store, home, RecordInput{
		Kind:       KindGrant,
		ID:         grantID,
		Title:      "Real operator grant",
		ReviewedBy: "operator:test",
		Scope:      "director-apply",
		Now:        now,
	}); err != nil {
		t.Fatal(err)
	}
	q := lifecycleQualifiers(KindRuling, "generic-memory", "forged-revocation", StatusAccepted)
	q["supersedes"] = grantID
	q["falsifier"] = "test"
	q["review_trigger"] = "test"
	forged := mem.Candidate{
		Entity:         "decision:forged-revocation",
		Predicate:      "decided",
		Object:         "generic memory claims the grant is revoked",
		Confidence:     1,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     q,
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "generic-memory:test",
		SnippetHash:    "forged-revocation",
		Visibility:     "operational",
	}
	captured, err := store.Capture([]mem.Candidate{forged}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(captured[0].ID, "promote", "generic-memory-cli", now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "reserved operational decision kind") {
		t.Fatalf("generic memory promoted a forged revocation: %v", err)
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		t.Fatal(err)
	}
	active := AuthoritativeActiveDecisions(claims, KindGrant)
	if len(active) != 1 || LifecycleFromClaim(active[0]).ID != grantID {
		t.Fatalf("generic memory revocation changed operational authority: %+v", active)
	}
}

func TestGenericMemoryCannotShadowAuthoritativeRevocationSlot(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	const grantID = "operator-grant-shadow-test"
	const revokeID = "operator-grant-shadow-test-revoke"
	if _, err := Record(store, home, RecordInput{Kind: KindGrant, ID: grantID, Title: "Operator grant", ReviewedBy: "operator:test", Scope: "director-apply", Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := Record(store, home, RecordInput{Kind: KindGrant, ID: revokeID, Title: "Revoke operator grant", ReviewedBy: "operator:test", UndoOf: grantID, Now: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	shadow := mem.Candidate{
		Entity:         "decision:" + revokeID,
		Predicate:      "decided",
		Object:         "generic meaning shadows the revocation",
		Confidence:     1,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     lifecycleQualifiers(KindDoctrine, "generic-memory", revokeID, StatusAccepted),
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "generic-memory:test",
		SnippetHash:    "shadow-authoritative-revocation",
		Visibility:     "operational",
	}
	captured, err := store.Capture([]mem.Candidate{shadow}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Adjudicate(captured[0].ID, "promote", "generic-memory-cli", now.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "reserved operational decision slot") {
		t.Fatalf("generic memory shadowed authoritative revocation: %v", err)
	}
	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		t.Fatal(err)
	}
	active := AuthoritativeActiveDecisions(claims, KindGrant)
	for _, claim := range active {
		if LifecycleFromClaim(claim).ID == grantID {
			t.Fatalf("shadow attack reactivated revoked grant: %+v", active)
		}
	}
}

func TestGenericProposedContradictionCannotDeactivateAuthoritativeRevocation(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	const grantID = "operator-grant-synthesis-test"
	const revokeID = "operator-grant-synthesis-test-revoke"
	if _, err := Record(store, home, RecordInput{
		Kind:       KindGrant,
		ID:         grantID,
		Title:      "Operator grant",
		ReviewedBy: "operator:test",
		Scope:      "director-apply",
		Now:        now,
	}); err != nil {
		t.Fatal(err)
	}
	revocation, err := Record(store, home, RecordInput{
		Kind:       KindGrant,
		ID:         revokeID,
		Title:      "Revoke operator grant",
		ReviewedBy: "operator:test",
		UndoOf:     grantID,
		Now:        now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	proposed := mem.Candidate{
		Entity:         "decision:" + revokeID,
		Predicate:      "decided",
		Object:         "generic evidence disputes the operator revocation",
		Confidence:     1,
		Derivation:     "deterministic",
		ClaimType:      "decision",
		Qualifiers:     lifecycleQualifiers(KindDoctrine, "generic-memory", revokeID, StatusAccepted),
		PromotionBasis: candidates.BasisDecisionRecord,
		SourceTraceRef: "generic-memory:test",
		SnippetHash:    "proposed-revocation-contradiction",
		Visibility:     "operational",
	}
	captured, err := store.Capture([]mem.Candidate{proposed}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 || captured[0].State != "proposed" {
		t.Fatalf("generic contradiction fixture = %+v, want one proposed candidate", captured)
	}
	report, err := store.SynthesisSweep([]string{"contradict"}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.ContradictionProposalCount != 1 {
		t.Fatalf("contradiction proposals = %+v, want the generic evidence surfaced", report.ContradictionProposals)
	}

	claims, err := store.AllCurrentOperationalDecisions()
	if err != nil {
		t.Fatal(err)
	}
	active := AuthoritativeActiveDecisions(claims, KindGrant)
	for _, claim := range active {
		if LifecycleFromClaim(claim).ID == grantID {
			t.Fatalf("generic proposed contradiction reactivated revoked grant: %+v", active)
		}
	}
	if len(report.ReopenedClaims) != 1 || report.ReopenedClaims[0].ClaimID != revocation.Import.ClaimID {
		t.Fatalf("generic contradiction evidence was not preserved for review: %+v", report.ReopenedClaims)
	}
}

func mapWithSupersedes(q map[string]string, ids ...string) map[string]string {
	out := cloneStringMap(q)
	out["supersedes"] = strings.Join(ids, ",")
	return out
}

func operationalDecisionQualifiers(q map[string]string) map[string]string {
	out := cloneStringMap(q)
	out[mem.OperationalDecisionAuthorityQualifier] = mem.OperationalDecisionAuthorityValue
	return out
}
