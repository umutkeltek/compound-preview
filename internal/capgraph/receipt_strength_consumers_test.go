package capgraph

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestReceiptUsageDoesNotClaimValidationOrObservedExecution(t *testing.T) {
	home := t.TempDir()
	const ref = "capability://skill:declared"
	rec := testReceipt("rec_usage_declared", "completed", true, []string{ref}, nil, []string{ref})
	writeReceipt(t, home, rec)
	proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	use := findCap(proj, ref)
	if use == nil || use.ValidatedCount != 0 || use.FiredCount != 0 {
		t.Fatalf("receipt declaration minted validation/observed execution: %+v", use)
	}
	data, err := json.Marshal(use)
	if err != nil || !strings.Contains(string(data), `"declared_use_count":1`) || !strings.Contains(string(data), `"accepted_use_declaration_count":1`) {
		t.Fatalf("JSON usage consumer lost evidence strength: %s %v", data, err)
	}
	var report bytes.Buffer
	if err := WriteCapabilityUsageReport(&report, "generated-projection", proj, 10); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "declared_use=1") || !strings.Contains(report.String(), "observed_skill_calls=0") {
		t.Fatalf("operator usage consumer did not expose declared versus observed: %s", report.String())
	}
}

func TestReceiptDeclarationsStaySeparateFromObservedCallsAndAcceptance(t *testing.T) {
	for _, outcome := range []string{"completed", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			home := t.TempDir()
			const observed = "capability://skill:observed"
			const declaredOnly = "capability://skill:declared-only"
			writeReceipt(t, home, testReceipt("rec_two_declarations", outcome, true, []string{observed, declaredOnly}, nil, nil))
			writeUsageHookEvent(t, home, envelope.HookEvent{
				Header: envelope.Header{
					SchemaVersion: envelope.SVHookEvent, ID: "hook_one_observation", CreatedAt: t0.Format(time.RFC3339),
					Producer:  envelope.Producer{Kind: "test", Name: "compound"},
					Authority: envelope.Authority{SourceClass: "runtime", Status: "current"},
					Privacy:   "private", Risk: "tier1", EvidenceRefs: []string{}, Uncertainty: envelope.Uncertainty{Level: "known"},
				},
				Harness: "codex", HookName: "PostToolUse", HookCapability: "telemetry", SideEffects: []string{"Skill"}, Status: "ok",
				CapabilityUse: &envelope.CapabilityUse{Ref: observed, Skill: "observed", Lane: "codex", Outcome: "used"},
			})
			proj, err := BuildCapabilityUsage(home, UsageBuildOptions{GeneratedAt: t0})
			if err != nil {
				t.Fatal(err)
			}
			accepted := 0
			if outcome == "completed" {
				accepted = 1
			}
			for _, ref := range []string{observed, declaredOnly} {
				use := findCap(proj, ref)
				fired := 0
				if ref == observed {
					fired = 1
				}
				if use == nil || use.DeclaredUseCount != 1 || use.UsedCount != 1 || use.AcceptedUseDeclarationCount != accepted || use.FiredCount != fired || use.ValidatedCount != 0 {
					t.Fatalf("%s %s conflated declaration, observation, or acceptance: %+v", outcome, ref, use)
				}
			}
		})
	}
}

func TestSourceLessReceiptTargetsAreWeakDeclarations(t *testing.T) {
	for _, target := range []string{"rec_legacy", "compound://events/rec_legacy", "events/rec_legacy.json"} {
		t.Run(target, func(t *testing.T) {
			_, s := newStore(t)
			if err := s.Add(Node{ID: "skill:legacy", Kind: "skill", Name: "Legacy", Triggers: []string{"receipt"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.AddEdge(Edge{Kind: EdValidatedBy, From: "skill:legacy", To: target,
				Confidence: 1, Weight: 2, Provenance: "verified_lane"}, t0); err != nil {
				t.Fatal(err)
			}
			edges, err := s.Edges()
			if err != nil || len(edges) != 1 || edges[0].Kind != EdDeclaredUsedBy || edges[0].Weight > 1 || edges[0].SourceReceipt != "rec_legacy" || edges[0].To != "rec_legacy" {
				t.Fatalf("source-less receipt target kept strong labels: %+v %v", edges, err)
			}
		})
	}
}

func TestSourceLessTypedEvaluationAndUnknownContextStayDistinct(t *testing.T) {
	_, s := newStore(t)
	for _, e := range []Edge{
		{Kind: EdValidatedBy, From: "skill:reviewed", To: "eval:reviewed", Confidence: 1, Weight: 2},
		{Kind: EdValidatedBy, From: "context:reviewed", To: "skill:reviewed", Confidence: 1, Weight: 2},
	} {
		if err := s.AddEdge(e, t0); err != nil {
			t.Fatal(err)
		}
	}
	edges, err := s.Edges()
	if err != nil || len(edges) != 2 {
		t.Fatalf("typed evaluation/unknown context lost: %+v %v", edges, err)
	}
	for _, e := range edges {
		if e.Kind != EdValidatedBy || e.Weight != 2 || e.SourceReceipt != "" {
			t.Fatalf("ambiguous context incorrectly declared receipt use: %+v", e)
		}
	}
}
