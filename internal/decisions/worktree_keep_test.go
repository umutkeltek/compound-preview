package decisions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
)

// keepClaim builds a decision claim carrying worktree-keep qualifiers, mirroring
// how a recorded keep folds into the operational-decision store, so the reader's
// exclusion rules are unit-testable without a real home.
func keepClaim(id, branchRef, reason, revisit, reviewedBy string) mem.Claim {
	q := operationalDecisionQualifiers(lifecycleQualifiers(KindGrant, "decisions-repo", id, StatusAccepted))
	q["scope"] = WorktreeKeepScope
	q["worktree_branch_ref"] = branchRef
	q["review_trigger"] = revisit
	if reviewedBy != "" {
		q["reviewed_by"] = reviewedBy
	}
	return mem.Claim{ID: "claim_" + id, ClaimType: "decision", Object: reason, Qualifiers: q}
}

// K2 (no-self-keep law): worktreeKeepsFromClaims includes an operator-reviewed
// keep and EXCLUDES a keep lacking operator provenance — an agent-authored keep
// exists as a record but is inert. It also ignores non-keep scopes and invalid
// branch refs so the map only ever carries real, pinnable branches.
func TestWorktreeKeepsFromClaimsExcludesNonOperatorAndBadScope(t *testing.T) {
	claims := []mem.Claim{
		keepClaim("keep-a", "refs/heads/lane-a", "actively iterating auth refactor", "auth refactor lands or is abandoned", "operator:test"),
		keepClaim("keep-b", "refs/heads/lane-b", "agent wants to keep its own lane", "someday", "codex"), // no operator provenance ⇒ inert
		keepClaim("keep-c", "refs/heads/lane-c", "agent wants to keep its own lane", "someday", ""),      // empty reviewed_by ⇒ inert
		keepClaim("keep-d", "not-a-ref", "malformed branch", "revisit", "operator:test"),                 // invalid branch ref ⇒ skipped
		func() mem.Claim {
			c := keepClaim("keep-e", "refs/heads/lane-e", "reason", "revisit", "operator:test")
			c.Qualifiers["scope"] = "director-apply"
			return c
		}(), // wrong scope ⇒ skipped
	}

	keeps := worktreeKeepsFromClaims(claims)

	if len(keeps) != 1 {
		t.Fatalf("expected exactly one active keep (lane-a), got %d: %+v", len(keeps), keeps)
	}
	a, ok := keeps["refs/heads/lane-a"]
	if !ok {
		t.Fatalf("operator-reviewed keep for lane-a missing: %+v", keeps)
	}
	if a.DecisionID != "keep-a" || a.Reason != "actively iterating auth refactor" ||
		a.Revisit != "auth refactor lands or is abandoned" || a.ReviewedBy != "operator:test" {
		t.Fatalf("keep fields wrong: %+v", a)
	}
	for _, badBranch := range []string{"refs/heads/lane-b", "refs/heads/lane-c", "refs/heads/lane-e", "not-a-ref"} {
		if _, present := keeps[badBranch]; present {
			t.Fatalf("inert/invalid keep leaked into the map: %q", badBranch)
		}
	}
}

// K4: `decisions record` with worktree-keep scope rejects a branch ref that is
// not refs/heads/, an empty revisit condition, an empty reason, and the wrong
// keyword arity — writing NOTHING on refusal.
func TestRecordWorktreeKeepAdmissionValidation(t *testing.T) {
	valid := RecordInput{
		Kind:          KindGrant,
		Title:         "actively iterating on the auth refactor lane",
		ReviewedBy:    "operator:test",
		Scope:         WorktreeKeepScope,
		Keywords:      []string{"refs/heads/lane-a"},
		ReviewTrigger: "auth refactor lands or is abandoned",
	}
	cases := []struct {
		name string
		edit func(*RecordInput)
	}{
		{"branch ref not refs/heads", func(in *RecordInput) { in.Keywords = []string{"lane-a"} }},
		{"empty revisit", func(in *RecordInput) { in.ReviewTrigger = "" }},
		{"empty reason", func(in *RecordInput) { in.Title = "" }},
		{"no keyword branch ref", func(in *RecordInput) { in.Keywords = nil }},
		{"two keyword branch refs", func(in *RecordInput) { in.Keywords = []string{"refs/heads/a", "refs/heads/b"} }},
		{"empty branch ref after prefix", func(in *RecordInput) { in.Keywords = []string{"refs/heads/"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			store, err := mem.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			in := valid
			tc.edit(&in)
			if _, err := Record(store, home, in); err == nil {
				t.Fatalf("invalid worktree-keep was accepted: %+v", in)
			}
			entries, readErr := os.ReadDir(filepath.Join(home, "decisions", "operator"))
			if readErr == nil && len(entries) != 0 {
				t.Fatalf("rejected worktree-keep wrote a record: %v", entries)
			}
		})
	}
}

// K4 happy path + reader round-trip: a valid worktree-keep records, promotes,
// stores the branch ref in worktree_branch_ref (NOT the freeze keyword scope),
// and is read back by ActiveWorktreeKeeps keyed on the branch ref.
func TestRecordWorktreeKeepRoundTripsThroughReader(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Record(store, home, RecordInput{
		Kind:          KindGrant,
		Title:         "actively iterating on the auth refactor lane",
		ReviewedBy:    "operator:test",
		Scope:         WorktreeKeepScope,
		Keywords:      []string{"refs/heads/lane-a"},
		ReviewTrigger: "auth refactor lands or is abandoned",
		Now:           time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	l := report.Import.Lifecycle
	if l.Scope != WorktreeKeepScope || l.WorktreeBranchRef != "refs/heads/lane-a" {
		t.Fatalf("keep lifecycle wrong: %+v", l)
	}
	if len(l.FreezeKeywords) != 0 || len(l.FreezeOwnerGlobs) != 0 {
		t.Fatalf("branch ref must not land in freeze scope: %+v", l)
	}
	if report.Import.ClaimID == "" {
		t.Fatalf("operator-reviewed keep should promote to a current claim: %+v", report.Import)
	}

	keeps, ok := ActiveWorktreeKeeps(home)
	if !ok {
		t.Fatal("reader reported keeps unreadable over a freshly recorded keep")
	}
	got, present := keeps["refs/heads/lane-a"]
	if !present {
		t.Fatalf("recorded keep not read back: %+v", keeps)
	}
	if got.Reason != "actively iterating on the auth refactor lane" ||
		got.Revisit != "auth refactor lands or is abandoned" ||
		!strings.HasPrefix(got.ReviewedBy, "operator:") {
		t.Fatalf("read-back keep fields wrong: %+v", got)
	}
}

// The reader over an empty (but readable) home returns an empty map and ok=true
// — absence of keeps is provable there, distinct from an unreadable home.
func TestActiveWorktreeKeepsEmptyHomeIsReadableAndEmpty(t *testing.T) {
	home := t.TempDir()
	if _, err := mem.Open(home); err != nil {
		t.Fatal(err)
	}
	keeps, ok := ActiveWorktreeKeeps(home)
	if !ok {
		t.Fatal("empty readable home must report ok=true")
	}
	if len(keeps) != 0 {
		t.Fatalf("empty home should have no keeps: %+v", keeps)
	}
}
