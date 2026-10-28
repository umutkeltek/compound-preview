// Red-first contract for the thin expressive-intent coordinator.
package intent_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/intent"
	"compound/internal/mem"
	"compound/internal/work"
)

var intentNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

func TestExpressivePlanPreservesBytesCitesRecallAndStaysProposeOnly(t *testing.T) {
	homePath := t.TempDir()
	promoteDirective(t, homePath, "BLUE-ARC", "shape the blue arc after review", intentNow)
	pack := recordPack(t, homePath, intentNow.Add(time.Minute))
	raw := []byte("  let the blue arc breathe\nthen make it inevitable\x00")
	report, err := intent.Plan(homePath, intent.PlanInput{
		Source: "fixture-gesture", Utterance: raw, ContextPackID: pack.ID,
		RecalledRefs: pack.ActiveDecisions, Actor: "codex", Owner: "compound-core",
		CanonicalSurface: "compound://intent/fixture",
		Proposal: intent.Proposal{
			DesiredOutcome: "expressive direction becomes inspectable work", SuccessPredicate: "the blue arc is shaped only after review",
			NonGoals: []string{"do not execute during planning"}, ActionSummary: "shape the blue arc after review",
			EvidenceRefs: pack.ActiveDecisions, Uncertainty: envelope.Uncertainty{Level: "inferred", Reason: "derived from expressive speech"},
		},
	}, intentNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != intent.StatusPlanProposed || report.Plan == nil || report.PlanPath == "" {
		t.Fatalf("report = %+v", report)
	}
	ms, _ := mem.Open(homePath)
	got, err := ms.ReadCapture(report.CaptureCID)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("capture got=%q err=%v want=%q", got, err, raw)
	}
	if report.Plan.MeaningResolution != "resolved" || report.Plan.ActionContext == nil || report.Plan.ActionContext.ActionSummary != "shape the blue arc after review" {
		t.Fatalf("meaning = %+v", report.Plan)
	}
	if report.Plan.Work.Uncertainty.Level != "inferred" {
		t.Fatalf("uncertainty = %+v", report.Plan.Work.Uncertainty)
	}
	requireHas(t, report.Plan.Work.EvidenceRefs, report.CaptureCID)
	for _, ref := range pack.ActiveDecisions {
		requireHas(t, report.Plan.Work.EvidenceRefs, ref)
	}
	for _, ref := range report.Plan.ActionContext.EvidenceRefs {
		requireHas(t, report.Plan.Work.EvidenceRefs, ref)
	}
	ws, _ := work.Open(homePath)
	if _, err := ws.Get(report.Plan.WorkID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proposal admitted work: %v", err)
	}
	if got := lifecycleRows(t, homePath); len(got) != 0 {
		t.Fatalf("proposal wrote lifecycle rows: %v", got)
	}
}

func TestExpressivePlanNoneCapturesOnlyAndAmbiguityParksOnly(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		homePath := t.TempDir()
		pack := recordPack(t, homePath, intentNow)
		report, err := intent.Plan(homePath, intent.PlanInput{Source: "fixture-gesture", Utterance: []byte("keep the quiet edge"), ContextPackID: pack.ID, Actor: "codex", Owner: "compound-core", Proposal: intent.Proposal{DesiredOutcome: "keep the edge", SuccessPredicate: "source remains captured"}}, intentNow.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if report.Status != intent.StatusCaptureOnly || report.Plan != nil || report.Park != nil {
			t.Fatalf("capture-only = %+v", report)
		}
		requireNoPlansParksOrWork(t, homePath)
	})
	t.Run("ambiguous", func(t *testing.T) {
		homePath := t.TempDir()
		promoteDirective(t, homePath, "DIRECTION-B", "shape direction B", intentNow)
		promoteDirective(t, homePath, "DIRECTION-A", "shape direction A", intentNow.Add(time.Second))
		pack := recordPack(t, homePath, intentNow.Add(time.Minute))
		report, err := intent.Plan(homePath, intent.PlanInput{Source: "fixture-gesture", Utterance: []byte("make it feel inevitable"), ContextPackID: pack.ID, RecalledRefs: pack.ActiveDecisions, Actor: "codex", Owner: "compound-core", Proposal: intent.Proposal{DesiredOutcome: "choose direction", SuccessPredicate: "ambiguity stays visible"}}, intentNow.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"decision://DIRECTION-A", "decision://DIRECTION-B"}
		if report.Status != intent.StatusAmbiguityParked || report.Park == nil || report.ParkPath == "" || !reflect.DeepEqual(report.Park.AmbiguousRefs, want) {
			t.Fatalf("park = %+v want_refs=%v", report, want)
		}
		if plans, _ := filepath.Glob(filepath.Join(homePath, "create-plans", "*.json")); len(plans) != 0 {
			t.Fatalf("ambiguity wrote plans: %v", plans)
		}
		if works, _ := filepath.Glob(filepath.Join(homePath, "work", "work", "*.json")); len(works) != 0 {
			t.Fatalf("ambiguity wrote work: %v", works)
		}
		if got := lifecycleRows(t, homePath); len(got) != 0 {
			t.Fatalf("ambiguity wrote lifecycle rows: %v", got)
		}
		if _, err := intent.Apply(homePath, report.ParkPath, "operator-fixture", intentNow.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "park") {
			t.Fatalf("park apply err = %v", err)
		}
	})
}

func TestProposalStrictlyRejectsAuthorityFieldsEvidenceEscapeAndActionOverride(t *testing.T) {
	for _, key := range []string{"work_id", "context_pack_id", "policy_decision_id"} {
		body := []byte(`{"desired_outcome":"x","success_predicate":"y","` + key + `":"forged"}`)
		if _, err := intent.DecodeProposal(body); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s accepted: %v", key, err)
		}
	}
	homePath := t.TempDir()
	promoteDirective(t, homePath, "CONTAINED", "shape contained action", intentNow)
	pack := recordPack(t, homePath, intentNow.Add(time.Minute))
	base := intent.PlanInput{Source: "fixture-gesture", Utterance: []byte("hold the thread"), ContextPackID: pack.ID, RecalledRefs: pack.ActiveDecisions, Actor: "codex", Owner: "compound-core", Proposal: intent.Proposal{DesiredOutcome: "hold thread", SuccessPredicate: "authority stays contained", ActionSummary: "shape contained action"}}
	escape := base
	escape.Proposal.EvidenceRefs = []string{"decision://FORGED"}
	if _, err := intent.Plan(homePath, escape, intentNow.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "outside capture and recalled refs") {
		t.Fatalf("evidence escape err = %v", err)
	}
	override := base
	override.Proposal.ActionSummary = "run a model-invented action"
	if _, err := intent.Plan(homePath, override, intentNow.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "does not match promoted exact action") {
		t.Fatalf("action override err = %v", err)
	}
}

func TestReviewIsReadOnlyAndApplyRequiresReviewerThenReplaysExactlyOnce(t *testing.T) {
	homePath := t.TempDir()
	promoteDirective(t, homePath, "REVIEWED", "shape reviewed action", intentNow)
	pack := recordPack(t, homePath, intentNow.Add(time.Minute))
	planned, err := intent.Plan(homePath, intent.PlanInput{Source: "fixture-gesture", Utterance: []byte("give motion a memory"), ContextPackID: pack.ID, RecalledRefs: pack.ActiveDecisions, Actor: "codex", Owner: "compound-core", Proposal: intent.Proposal{DesiredOutcome: "motion carries memory", SuccessPredicate: "reviewed plan admits once", ActionSummary: "shape reviewed action"}}, intentNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, homePath)
	review, err := intent.Review(planned.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if review.Kind != intent.ReviewKindPlan || review.Plan == nil || review.Plan.ID != planned.Plan.ID {
		t.Fatalf("review = %+v", review)
	}
	if after := snapshotTree(t, homePath); !reflect.DeepEqual(before, after) {
		t.Fatalf("review mutated home")
	}
	if _, err := intent.Apply(homePath, planned.PlanPath, "", intentNow.Add(3*time.Minute)); err == nil || !strings.Contains(err.Error(), "reviewed-by") {
		t.Fatalf("unreviewed err = %v", err)
	}
	first, err := intent.Apply(homePath, planned.PlanPath, "operator-fixture", intentNow.Add(4*time.Minute))
	if err != nil || first.Status != "admitted" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := intent.Apply(homePath, planned.PlanPath, "operator-fixture", intentNow.Add(5*time.Minute))
	if err != nil || second.Status != "already_admitted" || second.WorkID != first.WorkID {
		t.Fatalf("second=%+v err=%v first=%+v", second, err, first)
	}
}

func promoteDirective(t *testing.T, homePath, decisionID, directive string, now time.Time) {
	t.Helper()
	ms, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ms.Capture([]mem.Candidate{{Entity: "loop:expressive-fixture", Predicate: "action_directive", Object: directive, ObservationDate: now.Format(time.RFC3339), Confidence: .99, Derivation: "deterministic", ClaimType: "decision", Qualifiers: map[string]string{"decision_id": decisionID, "lifecycle_status": "accepted"}, PromotionBasis: candidates.BasisDecisionRecord, SourceTraceRef: "fixture://expressive-intent/" + strings.ToLower(decisionID), SnippetHash: "sha256:" + strings.ToLower(decisionID), Visibility: "operational"}}, now)
	if err != nil || len(c) != 1 {
		t.Fatalf("capture count=%d err=%v", len(c), err)
	}
	if _, err := ms.Adjudicate(c[0].ID, "promote", "intent_fixture_promote", now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := ms.FoldPositions(mem.PositionFoldOptions{}, now.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
}

func recordPack(t *testing.T, homePath string, now time.Time) envelope.ContextPack {
	t.Helper()
	in := contract.Input{Harness: "codex", Cwd: "/repo/expressive-fixture", Task: "shape expressive action directive after review", RepoKind: "control-plane", RepoKindVia: "fixture", Mode: "plan"}
	runtime, err := contract.MaterializeRuntime(in, contract.DefaultPolicy(), homePath, now)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err = contract.SealRuntimeMaterialization(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.RecordRuntimeMaterialization(homePath, runtime); err != nil {
		t.Fatal(err)
	}
	return runtime.ContextPack
}

func requireNoPlansParksOrWork(t *testing.T, homePath string) {
	t.Helper()
	for _, p := range []string{filepath.Join(homePath, "create-plans", "*"), filepath.Join(homePath, "reviews", "intent-ambiguity", "*"), filepath.Join(homePath, "work", "work", "*.json")} {
		if got, _ := filepath.Glob(p); len(got) != 0 {
			t.Fatalf("capture-only wrote %s: %v", p, got)
		}
	}
	if got := lifecycleRows(t, homePath); len(got) != 0 {
		t.Fatalf("capture-only wrote lifecycle rows: %v", got)
	}
}

func lifecycleRows(t *testing.T, homePath string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(homePath, "events", "segments", "work"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func requireHas(t *testing.T, values []string, want string) {
	t.Helper()
	for _, v := range values {
		if v == want {
			return
		}
	}
	t.Fatalf("%q missing from %v", want, values)
}
