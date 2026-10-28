package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

// validBranchTransition is a minimal Transition that validates with no
// opens_branches, so each falsifier can attach branch refs and observe only
// the branch rules.
func validBranchTransition() Transition {
	return Transition{
		Header: Header{
			SchemaVersion: SVTransition,
			ID:            "tr_branch_test",
			CreatedAt:     "2026-07-23T00:00:00Z",
			Producer:      Producer{Kind: "cli", Name: "compound"},
			IDs:           IDs{WorkID: "work_branch_test"},
			Authority:     Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   Uncertainty{Level: "known"},
		},
		BeforeRefs:  []string{},
		ActionRefs:  []string{},
		AfterRefs:   []string{},
		Projections: Projections{},
		SecondOrder: SecondOrder{},
	}
}

// Falsifier 1: a bogus branch kind is rejected; all six enum values pass.
func TestTransitionBranchRejectsBadKind(t *testing.T) {
	tr := validBranchTransition()
	tr.OpensBranches = []BranchRef{{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: "frobnicate"}}
	if err := tr.Validate(); err == nil {
		t.Fatalf("expected validation error for bogus branch kind, got nil")
	}
	for _, kind := range BranchKinds {
		tr.OpensBranches = []BranchRef{{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: kind}}
		if err := tr.Validate(); err != nil {
			t.Fatalf("branch kind %q should validate: %v", kind, err)
		}
	}
}

// Falsifier 2: a prose or empty branch slug is rejected (slugs are machine refs,
// never prose); dotted/dashed lowercase slugs pass.
func TestTransitionBranchRejectsProseSlug(t *testing.T) {
	tr := validBranchTransition()
	for _, bad := range []string{"", "   ", "Freeze the flow", "Uppercase", "-leading-dash"} {
		tr.OpensBranches = []BranchRef{{Branch: bad, Owner: "claude:orchestrator", Kind: "opens"}}
		if err := tr.Validate(); err == nil {
			t.Fatalf("expected validation error for branch slug %q, got nil", bad)
		}
	}
	for _, good := range []string{"branch-lifecycle", "route_promotion.holds", "a"} {
		tr.OpensBranches = []BranchRef{{Branch: good, Owner: "claude:orchestrator", Kind: "opens"}}
		if err := tr.Validate(); err != nil {
			t.Fatalf("branch slug %q should validate: %v", good, err)
		}
	}
}

// Falsifier 3: an empty or prose owner is rejected; consumer refs pass.
func TestTransitionBranchRejectsProseOwner(t *testing.T) {
	tr := validBranchTransition()
	for _, bad := range []string{"", "   ", "the operator"} {
		tr.OpensBranches = []BranchRef{{Branch: "branch-lifecycle", Owner: bad, Kind: "opens"}}
		if err := tr.Validate(); err == nil {
			t.Fatalf("expected validation error for owner %q, got nil", bad)
		}
	}
	for _, good := range []string{"claude:orchestrator", "operator:example", "workbench"} {
		tr.OpensBranches = []BranchRef{{Branch: "branch-lifecycle", Owner: good, Kind: "opens"}}
		if err := tr.Validate(); err != nil {
			t.Fatalf("owner %q should validate: %v", good, err)
		}
	}
}

// Falsifier 4: one statement per branch per transition — a duplicate slug in a
// single opens_branches list is rejected.
func TestTransitionBranchRejectsDuplicateSlug(t *testing.T) {
	tr := validBranchTransition()
	tr.OpensBranches = []BranchRef{
		{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: "opens"},
		{Branch: "branch-lifecycle", Owner: "workbench", Kind: "blocks"},
	}
	if err := tr.Validate(); err == nil {
		t.Fatalf("expected validation error for duplicate branch slug, got nil")
	}
	tr.OpensBranches = []BranchRef{
		{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: "opens"},
		{Branch: "route-promotion", Owner: "workbench", Kind: "blocks"},
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("distinct branch slugs should validate: %v", err)
	}
}

// Falsifier 5: omitted opens_branches is backcompat — every pre-existing
// Transition (nil list) still validates, and the field stays absent from JSON.
func TestTransitionBranchesOmittedIsBackcompat(t *testing.T) {
	tr := validBranchTransition()
	if err := tr.Validate(); err != nil {
		t.Fatalf("nil opens_branches should validate: %v", err)
	}
	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "opens_branches") {
		t.Fatalf("omitempty violated: nil opens_branches leaked into JSON: %s", data)
	}
}

// Falsifier 6: codec round-trip — a Transition carrying opens_branches survives
// marshal -> ValidateBytes -> unmarshal with every field intact.
func TestTransitionBranchesCodecRoundTrip(t *testing.T) {
	tr := validBranchTransition()
	tr.OpensBranches = []BranchRef{
		{Branch: "branch-lifecycle", Owner: "claude:orchestrator", Kind: "opens"},
	}
	data, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	kind, err := ValidateBytes(data)
	if err != nil {
		t.Fatalf("ValidateBytes rejected a valid branched transition: %v", err)
	}
	if kind != "transition" {
		t.Fatalf("ValidateBytes kind = %q, want transition", kind)
	}
	var back Transition
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.OpensBranches) != 1 || back.OpensBranches[0] != tr.OpensBranches[0] {
		t.Fatalf("round trip lost opens_branches: %+v", back.OpensBranches)
	}
}
