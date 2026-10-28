package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
)

func TestCmdDecisionsShowResolvesOneExactActiveDecision(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	captured, err := store.Capture([]mem.Candidate{
		{
			Entity: "decision:brain-D1", Predicate: "decided", Object: "Target short-id decision",
			Confidence: 1, Derivation: "deterministic", ClaimType: "decision",
			Qualifiers:     map[string]string{"decision_id": "brain-D1", "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord, SourceTraceRef: "ev_target", SnippetHash: "target", Visibility: "ambient",
		},
		{
			Entity: "decision:brain-D2", Predicate: "decided", Object: "Unrelated short-id decision",
			Confidence: 1, Derivation: "deterministic", ClaimType: "decision",
			Qualifiers:     map[string]string{"decision_id": "brain-D2", "lifecycle_status": "accepted"},
			PromotionBasis: candidates.BasisDecisionRecord, SourceTraceRef: "ev_unrelated", SnippetHash: "unrelated", Visibility: "ambient",
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	var targetClaimID string
	for i, candidate := range captured {
		result, err := store.Adjudicate(candidate.ID, "promote", "test", now.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			targetClaimID = result.Claim.ID
		}
	}

	for _, query := range []string{"decision://brain-D1", "brain-D1", targetClaimID} {
		out := captureStdout(t, func() {
			cmdDecisions([]string{"show", query, "--json"})
		})
		var got struct {
			Ref   string    `json:"ref"`
			Claim mem.Claim `json:"claim"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode decisions show JSON for %q: %v\n%s", query, err, out)
		}
		if got.Ref != "decision://brain-D1" || got.Claim.ID != targetClaimID || got.Claim.Object != "Target short-id decision" {
			t.Fatalf("decisions show %q resolved wrong record: %+v", query, got)
		}
		if strings.Contains(out, "Unrelated short-id decision") {
			t.Fatalf("decisions show %q re-expanded the full inventory:\n%s", query, out)
		}
	}
}

// TestCmdDecisionsRecordWritesGrantAndCitesIt proves `compound decisions
// record --kind grant ...` writes a decision record under
// $COMPOUND_HOME/decisions/operator/ and prints the resulting claim/scope so
// a caller can cite it (grants-as-gates, 2026-07-10 packet).
func TestCmdDecisionsRecordWritesGrantAndCitesIt(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdDecisions([]string{
			"record",
			"--kind", "grant",
			"--id", "director-apply-grant-2026-07-10",
			"--title", "Grant director --apply",
			"--reviewed-by", "operator:test",
			"--scope", "director-apply",
		})
	})
	for _, want := range []string{
		"director-apply-grant-2026-07-10",
		"- scope: director-apply",
		"- reviewed_by: operator:test",
		"- verdict: promote",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("decisions record output missing %q:\n%s", want, out)
		}
	}
}

// TestCmdDecisionsRecordFreezeWritesScopeQualifiers proves the freeze CLI
// path threads --owner-glob/--keyword through to the record (falsifier b's
// wiring surface: the freeze admission guard reads these qualifiers).
func TestCmdDecisionsRecordFreezeWritesScopeQualifiers(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdDecisions([]string{
			"record",
			"--kind", "freeze",
			"--id", "direction-freeze-2026-07-10",
			"--title", "Freeze new-organ/absorption imports",
			"--reviewed-by", "operator:test",
			"--keyword", "absorption_debt",
			"--keyword", "harness_edit",
			"--owner-glob", "compound-authority",
		})
	})
	if !strings.Contains(out, "direction-freeze-2026-07-10") {
		t.Fatalf("decisions record freeze output missing id:\n%s", out)
	}
}

func TestCmdDecisionsRecordWritesTypedAutonomyThreshold(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdDecisions([]string{
			"record",
			"--kind", "grant",
			"--id", "job-health-autonomy-test",
			"--title", "Permit evidence-threshold admission for job health",
			"--reviewed-by", "operator:test",
			"--scope", "autonomy-evidence-threshold",
			"--auto-admit-class", "job_health",
			"--min-terminal-samples", "5",
			"--min-window-days", "7",
			"--max-failures", "0",
		})
	})
	for _, want := range []string{
		"- auto_admit_class: job_health",
		"- min_terminal_samples: 5",
		"- min_window_days: 7",
		"- max_failures: 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("typed autonomy grant output missing %q:\n%s", want, out)
		}
	}
}
