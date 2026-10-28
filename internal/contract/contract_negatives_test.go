package contract

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/negatives"
)

// writeNegativesFixture seeds the negatives projection directly (the read-side join
// under test here is contract.go's; internal/negatives' own package tests already
// cover Build()'s read of the raw producers).
func writeNegativesFixture(t *testing.T, home string, records ...negatives.Record) {
	t.Helper()
	proj := negatives.Projection{
		SchemaVersion: negatives.SchemaVersion,
		ProjectionRef: negatives.ProjectionRef,
		ComputedAt:    fixedNow.UTC().Format(time.RFC3339),
		Records:       records,
	}
	if _, err := negatives.WriteProjection(home, proj); err != nil {
		t.Fatal(err)
	}
}

// Falsifier (a): scope match, not dump, through the real orient seam — an Intent
// whose task overlaps the recorded failure's scope gets the negative card; a
// disjoint Intent gets ZERO.
func TestPackWithCapabilitiesNegativesScopeMatchNotDump(t *testing.T) {
	home := t.TempDir()
	writeNegativesFixture(t, home, negatives.Record{
		Source:       negatives.SourceFailurePattern,
		RefID:        "fp_orient_scope",
		TaskClass:    "fold-target-mismatch",
		ApproachKey:  "wrong-fold-target-merge",
		WhatWasTried: "folded new content into the wrong merge target",
		WhyItFailed:  "fold-target-mismatch",
		Ref:          "candidate://failure_pattern/fp_orient_scope",
		Confidence:   0.8,
	})

	matching := Input{Harness: "claude_code", Cwd: "/repo/x", Task: "fold the new section into the merge target again", RepoKind: "product", RepoKindVia: "flag", Mode: "execute"}
	pack, err := PackWithCapabilities(matching, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Negatives) != 1 || pack.Negatives[0].Ref != "candidate://failure_pattern/fp_orient_scope" {
		t.Fatalf("expected matching negative card, got %+v", pack.Negatives)
	}
	if _, ok := findIncludedRef(pack, "candidate://failure_pattern/fp_orient_scope"); !ok {
		t.Fatalf("expected included ref for surfaced negative: %+v", pack.Included)
	}

	disjoint := Input{Harness: "claude_code", Cwd: "/repo/x", Task: "generate ios app icon assets for the store listing", RepoKind: "product", RepoKindVia: "flag", Mode: "execute"}
	packDisjoint, err := PackWithCapabilities(disjoint, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(packDisjoint.Negatives) != 0 {
		t.Fatalf("expected ZERO negatives for disjoint task scope, got %+v", packDisjoint.Negatives)
	}
}

func TestPackWithCapabilitiesNegativesExcludeCorruptionWithoutHidingValidSibling(t *testing.T) {
	home := t.TempDir()
	const validID = "fp_valid_sibling"
	if err := candidates.WriteCandidate(home, candidates.FailurePattern, validID, map[string]any{
		"id":         validID,
		"subject":    "later-valid-negative",
		"statement":  "the valid sibling remains visible after canonical custody excludes corruption",
		"detail":     "later-valid-negative",
		"confidence": 0.9,
		"verdict":    candidates.Park,
	}); err != nil {
		t.Fatal(err)
	}
	inbox := filepath.Join(home, candidates.InboxDir(candidates.FailurePattern))
	const corruptName = "operator-controlled-name.json"
	if err := os.WriteFile(filepath.Join(inbox, corruptName), []byte(`{"id":"different-id"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	in := Input{Harness: "codex", Cwd: "/repo/x", Task: "review a later valid negative signal", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Negatives) != 1 || pack.Negatives[0].Ref != "candidate://failure_pattern/"+validID {
		t.Fatalf("valid sibling negative = %+v", pack.Negatives)
	}
	wantRef := negatives.ProjectionRef + "/source/" + negatives.SourceFailurePattern + "/" + string(candidates.CorruptionIDMismatch)
	reason := exclusionReason(pack.Excluded, wantRef)
	if !strings.Contains(reason, "excluded by canonical custody") {
		t.Fatalf("negative corruption exclusion = %q in %+v", reason, pack.Excluded)
	}
	if strings.Contains(reason, corruptName) {
		t.Fatalf("negative exclusion leaked operator-controlled filename: %q", reason)
	}
}

// Falsifier (c) at the orient seam: a failure-patterns-sourced record and a
// route-remorse-sourced record that both match scope surface through the SAME
// ContextPack.negatives section.
func TestPackWithCapabilitiesNegativesProducerAgnostic(t *testing.T) {
	home := t.TempDir()
	writeNegativesFixture(t, home,
		negatives.Record{
			Source:       negatives.SourceFailurePattern,
			RefID:        "fp_orient_dual",
			TaskClass:    "repeated-policy-hold",
			ApproachKey:  "dirty-source-control-hold",
			WhatWasTried: "mutated a dirty repo without a cleanup review",
			WhyItFailed:  "repeated-policy-hold",
			Ref:          "candidate://failure_pattern/fp_orient_dual",
			Confidence:   0.9,
		},
		negatives.Record{
			Source:       negatives.SourceRouteRemorse,
			RefID:        "ror_orient_dual",
			TaskClass:    "repeated-policy-hold",
			ApproachKey:  "policyhold-route-x",
			WhatWasTried: "routed to policyhold/route-x",
			WhyItFailed:  "route repeatedly held for the same dirty-hold policy reason",
			Ref:          "route-remorse://ror_orient_dual",
			Confidence:   1,
		},
	)
	in := Input{Harness: "claude_code", Cwd: "/repo/x", Task: "hold policy again for repeated dirty state", RepoKind: "product", RepoKindVia: "flag", Mode: "execute"}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Negatives) != 2 {
		t.Fatalf("expected both producers to surface through the same section, got %+v", pack.Negatives)
	}
	var sawFailurePattern, sawRouteRemorse bool
	for _, card := range pack.Negatives {
		switch card.Source {
		case negatives.SourceFailurePattern:
			sawFailurePattern = true
		case negatives.SourceRouteRemorse:
			sawRouteRemorse = true
		}
	}
	if !sawFailurePattern || !sawRouteRemorse {
		t.Fatalf("expected one card from each producer, got %+v", pack.Negatives)
	}
}

// Falsifier (b): read-only. A planted high-severity negative must not change any
// route/dispatch/claim-adjacent field of the pack — display only.
func TestPackWithCapabilitiesNegativesAreReadOnly(t *testing.T) {
	home := t.TempDir()
	writeNegativesFixture(t, home, negatives.Record{
		Source:       negatives.SourceRouteRemorse,
		RefID:        "ror_orient_readonly",
		TaskClass:    "high-severity-remorse",
		ApproachKey:  "high-severity-route-remorse",
		WhatWasTried: "routed to a high severity remorse target",
		WhyItFailed:  "planted high-severity negative for the read-only falsifier",
		Ref:          "route-remorse://ror_orient_readonly",
		Confidence:   1,
	})
	in := Input{Harness: "claude_code", Cwd: "/repo/x", Task: "high severity route remorse retry", RepoKind: "product", RepoKindVia: "flag", Mode: "execute"}

	withNegatives, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(withNegatives.Negatives) == 0 {
		t.Fatalf("expected the planted negative to surface, got %+v", withNegatives.Negatives)
	}

	emptyHome := t.TempDir()
	withoutNegatives, err := PackWithCapabilities(in, DefaultPolicy(), emptyHome, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(withNegatives.ActiveHooks, withoutNegatives.ActiveHooks) ||
		!reflect.DeepEqual(withNegatives.ActiveDecisions, withoutNegatives.ActiveDecisions) ||
		!reflect.DeepEqual(withNegatives.RequiredChecks, withoutNegatives.RequiredChecks) ||
		!reflect.DeepEqual(withNegatives.Avoid, withoutNegatives.Avoid) {
		t.Fatalf("a planted high-severity negative must not change any route/dispatch/claim-adjacent field: with=%+v without=%+v", withNegatives, withoutNegatives)
	}
}

// Falsifier (e) / Slice 3 — the lived-instance proof: the T-1 wrong-fold-target
// correction (Weng harness-RSI map item 6: "manually corrected in the handoff so no
// session re-walks it") becomes a fixture. Before this projection existed, the
// handoff prose was the ONLY record of the burned path; a matching Intent now
// receives the negative card directly from ContextPack. Naming stays generic/
// synthetic (fold = merge content into a target) — no private data, per the repo's
// public/private boundary.
func TestPackWithCapabilitiesNegativesLivedInstanceWrongFoldTarget(t *testing.T) {
	home := t.TempDir()
	writeNegativesFixture(t, home, negatives.Record{
		Source:       negatives.SourceFailurePattern,
		RefID:        "fp_t1_wrong_fold_target",
		TaskClass:    "wrong-fold-target",
		ApproachKey:  "wrong-fold-target",
		WhatWasTried: "folded new material directly into the wrong merge target without checking the intended destination first",
		WhyItFailed:  "the fold landed on the wrong target and had to be manually corrected before it reached the record",
		Ref:          "candidate://failure_pattern/fp_t1_wrong_fold_target",
		Confidence:   0.85,
	})
	in := Input{Harness: "codex", Cwd: "/repo/x", Task: "fold this update into the target section of the doc", RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Negatives) != 1 || pack.Negatives[0].ApproachKey != "wrong-fold-target" {
		t.Fatalf("expected the wrong-fold-target lived instance to surface for a matching fold/target intent, got %+v", pack.Negatives)
	}
}
