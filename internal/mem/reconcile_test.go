package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestSynthesisReconcileStageRegistered(t *testing.T) {
	_, stages, err := normalizeSynthesisStages([]string{"reconcile"})
	if err != nil {
		t.Fatalf("reconcile stage should be registered: %v", err)
	}
	if !stages["reconcile"] {
		t.Fatalf("reconcile stage missing from normalized stage set: %+v", stages)
	}
}

func TestLoadReconcileEventsFailsClosedOnMalformedRecord(t *testing.T) {
	s := newStore(t)
	path := filepath.Join(s.home, "events", "rec_malformed.json")
	if err := os.WriteFile(path, []byte("{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.loadReconcileEvents(reconcileCheckpoint{}, t0); err == nil ||
		!strings.Contains(err.Error(), "load event rec_malformed.json") {
		t.Fatalf("malformed reconcile event error = %v", err)
	}
}

func TestSynthesisReconcileDismissesEchoAndScopeDisjointWithoutMutation(t *testing.T) {
	s := newStore(t)
	now := t0.Add(2 * time.Hour)
	current := reconcileClaim("claim_current", 1, "project:api", "runtime", "Go", rfc3339(t0))
	appendReconcileClaim(t, s, current)
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_scope_disjoint",
		CreatedAt: rfc3339(now),
		Scope:     "project:other",
		ClaimRefs: []string{current.ID},
	})
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_echo",
		CreatedAt: rfc3339(now.Add(time.Minute)),
		Scope:     current.Entity,
		ClaimRefs: []string{current.ID},
	})
	beforeClaims := readFileIfExists(t, s.claimsLog())
	beforeEdges := readFileIfExists(t, s.edgesLog())
	beforeCandidates := snapshotDirFiles(t, s.candDir())

	report, err := s.SynthesisSweep([]string{"reconcile"}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileDismissedCount != 2 {
		t.Fatalf("dismissed = %d, want 2; report=%+v", report.ReconcileDismissedCount, report)
	}
	if report.ReconcileReopenedCount != 0 || report.ReconcileRaisedCount != 0 || report.ReconcileParkedCount != 0 {
		t.Fatalf("dismiss-only evidence mutated dispositions: report=%+v", report)
	}
	assertFileEqual(t, s.claimsLog(), beforeClaims)
	assertFileEqual(t, s.edgesLog(), beforeEdges)
	assertDirSnapshotEqual(t, s.candDir(), beforeCandidates)
	if unexpected := reviewFilesExceptReconcile(t, s.home); len(unexpected) > 0 {
		t.Fatalf("reconcile must not create review cards: %v", unexpected)
	}
}

func TestSynthesisReconcileReopensDeterministicContradictionOnce(t *testing.T) {
	s := newStore(t)
	base := reconcileClaim("claim_go", 1, "project:api", "runtime", "Go", rfc3339(t0))
	evidence := reconcileClaim("claim_rust", 2, "project:api", "runtime", "Rust", rfc3339(t0.AddDate(0, 0, 1)))
	appendReconcileClaim(t, s, base)
	appendReconcileClaim(t, s, evidence)
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_rust",
		CreatedAt: rfc3339(t0.AddDate(0, 0, 1).Add(time.Hour)),
		Scope:     evidence.Entity,
		ClaimRefs: []string{evidence.ID},
	})

	report, err := s.SynthesisSweep([]string{"reconcile"}, t0.AddDate(0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileReopenedCount != 1 || report.ReviewDebtCount != 1 || len(report.ReopenedClaims) != 1 {
		t.Fatalf("reopen counts = reconcile:%d debt:%d reopened:%+v", report.ReconcileReopenedCount, report.ReviewDebtCount, report.ReopenedClaims)
	}
	if report.ReopenedClaims[0].ClaimID != base.ID || !strings.HasPrefix(report.ReopenedClaims[0].ReviewRef, "synth_reconcile_contradiction_") {
		t.Fatalf("wrong reopened marker: %+v", report.ReopenedClaims[0])
	}
	claims, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}
	got := claimByID(claims, base.ID)
	if got.State != ClaimStateNeedsReview || len(got.ReviewRefs) != 1 || got.ReviewRefs[0] != report.ReopenedClaims[0].ReviewRef {
		t.Fatalf("base claim review state = %+v", got)
	}

	again, err := s.SynthesisSweep([]string{"reconcile"}, t0.AddDate(0, 0, 2).Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.ReconcileReopenedCount != 0 || again.ReviewDebtCount != 0 || len(again.ReopenedClaims) != 0 {
		t.Fatalf("second reconcile should be idempotent, got %+v", again)
	}
	claimsAgain, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}
	got = claimByID(claimsAgain, base.ID)
	if len(got.ReviewRefs) != 1 {
		t.Fatalf("duplicate review refs after second run: %+v", got.ReviewRefs)
	}
}

func TestSynthesisReconcileDecisiveEvidenceDoesNotCreateContestedCard(t *testing.T) {
	s := newStore(t)
	base := reconcileClaim("claim_decision_old", 1, "project:api", "runtime", "Go", rfc3339(t0))
	base.CanonClass = "decision"
	evidence := reconcileClaim("claim_decision_new", 2, "project:api", "runtime", "Rust", rfc3339(t0.AddDate(0, 0, 1)))
	evidence.CanonClass = "decision"
	appendReconcileClaim(t, s, base)
	appendReconcileClaim(t, s, evidence)
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_decisive_reconcile",
		CreatedAt: rfc3339(t0.AddDate(0, 0, 1).Add(time.Hour)),
		Scope:     evidence.Entity,
		ClaimRefs: []string{evidence.ID},
	})

	report, err := s.SynthesisSweep([]string{"reconcile"}, t0.AddDate(0, 0, 2))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileReopenedCount != 1 {
		t.Fatalf("decisive evidence should reopen existing claim, got report=%+v", report)
	}
	if report.ReconcileContestedCount != 0 || len(report.ReconcileContestedCards) != 0 {
		t.Fatalf("decisive evidence must not create contested cards: report=%+v", report)
	}
	if files := reconcileContestedReviewFiles(t, s); len(files) != 0 {
		t.Fatalf("decisive evidence wrote contested review files: %v", files)
	}
}

func TestSynthesisReconcileCredibleSourceSplitWritesOneContestedCard(t *testing.T) {
	s := newStore(t)
	memory := reconcileClaim("claim_decision_memory", 1, "project:api", "runtime", "Go", rfc3339(t0))
	memory.CanonClass = "decision"
	memory.EvidenceRefs = []string{"source:trace-a"}
	evidence := reconcileClaim("claim_decision_evidence", 2, "project:api", "runtime", "Rust", rfc3339(t0))
	evidence.CanonClass = "decision"
	evidence.EvidenceRefs = []string{"source:trace-b"}
	appendReconcileClaim(t, s, memory)
	appendReconcileClaim(t, s, evidence)
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_split_reconcile",
		CreatedAt: rfc3339(t0.Add(time.Hour)),
		Scope:     evidence.Entity,
		ClaimRefs: []string{evidence.ID},
	})

	report, err := s.SynthesisSweep([]string{"reconcile"}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileContestedCount != 1 || len(report.ReconcileContestedCards) != 1 {
		t.Fatalf("contested cards = count:%d cards:%+v", report.ReconcileContestedCount, report.ReconcileContestedCards)
	}
	if report.ReconcileReopenedCount != 0 {
		t.Fatalf("split tie must not reopen decisively: %+v", report)
	}
	files := reconcileContestedReviewFiles(t, s)
	if len(files) != 2 {
		t.Fatalf("contested review should write json+md, got %v", files)
	}
	artifact := readReconcileContestedArtifact(t, s)
	if artifact["status"] != "review_required" || artifact["readiness"] != "needs_review" {
		t.Fatalf("contested artifact should require review: %+v", artifact)
	}
	summary, _ := artifact["operator_summary"].(string)
	if !strings.HasPrefix(summary, "In project api, in runtime, memory holds ") ||
		!strings.Contains(summary, "Go (") ||
		!strings.Contains(summary, "Rust (") ||
		!strings.Contains(summary, "representation correct?") {
		t.Fatalf("operator summary not in split-card sentence shape: %q", summary)
	}

	again, err := s.SynthesisSweep([]string{"reconcile"}, t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.ReconcileContestedCount != 0 || len(again.ReconcileContestedCards) != 0 {
		t.Fatalf("second reconcile should not create duplicate contested cards: %+v", again)
	}
	if after := reconcileContestedReviewFiles(t, s); strings.Join(after, "\n") != strings.Join(files, "\n") {
		t.Fatalf("second reconcile changed contested review files: before=%v after=%v", files, after)
	}
}
func TestSynthesisReconcileThreeSplitTensionsCollapseIntoOneCard(t *testing.T) {
	s := newStore(t)
	claims := []Claim{
		reconcileClaim("claim_split_go", 1, "project:api", "runtime", "Go", rfc3339(t0)),
		reconcileClaim("claim_split_rust", 2, "project:api", "runtime", "Rust", rfc3339(t0)),
		reconcileClaim("claim_split_zig", 3, "project:api", "runtime", "Zig", rfc3339(t0)),
	}
	for i := range claims {
		claims[i].CanonClass = "decision"
		claims[i].EvidenceRefs = []string{fmt.Sprintf("source:trace-%d", i+1)}
		appendReconcileClaim(t, s, claims[i])
	}
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_three_way_split",
		CreatedAt: rfc3339(t0.Add(time.Hour)),
		Scope:     "project:api",
		ClaimRefs: []string{"claim_split_rust", "claim_split_zig"},
	})

	report, err := s.SynthesisSweep([]string{"reconcile"}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileContestedCount != 1 || len(report.ReconcileContestedCards) != 1 {
		t.Fatalf("three-way split should create one card, got %+v", report.ReconcileContestedCards)
	}
	card := report.ReconcileContestedCards[0]
	if len(card.Claims) != 3 {
		t.Fatalf("one card should list all three representations: %+v", card.Claims)
	}
	markdown := readReconcileContestedMarkdown(t, s)
	if !strings.Contains(markdown, "## Representations") {
		t.Fatalf("card body should expose a representations section:\n%s", markdown)
	}
	for _, want := range []string{"Go (", "Rust (", "Zig ("} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("card body missing %q in:\n%s", want, markdown)
		}
	}
	if files := reconcileContestedReviewFiles(t, s); len(files) != 2 {
		t.Fatalf("three-way split should still write one json+md pair, got %v", files)
	}
}

func TestSynthesisReconcileUsageOnlyRoutesToDecayWithoutReopen(t *testing.T) {
	s := newStore(t)
	claim := reconcileClaim("claim_used", 1, "project:api", "runtime", "Go", rfc3339(t0))
	appendReconcileClaim(t, s, claim)
	writeReconcileCloseReceipt(t, s, "rec_usage", "tr_usage", rfc3339(t0.Add(time.Hour)), []string{"claim://" + claim.ID})

	report, err := s.SynthesisSweep([]string{"reconcile"}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileDecayRoutedCount != 1 {
		t.Fatalf("decay-routed = %d, want 1; report=%+v", report.ReconcileDecayRoutedCount, report)
	}
	if report.ReconcileReopenedCount != 0 || report.ReviewDebtCount != 0 {
		t.Fatalf("usage-only must not reopen: %+v", report)
	}
	got := claimByID(mustLoadClaims(t, s), claim.ID)
	if got.State == ClaimStateNeedsReview {
		t.Fatalf("usage-only claim was reopened: %+v", got)
	}
}

func TestSynthesisReconcileParksMissingDateWithoutMutation(t *testing.T) {
	s := newStore(t)
	base := reconcileClaim("claim_go", 1, "project:api", "runtime", "Go", rfc3339(t0))
	evidence := reconcileClaim("claim_rust", 2, "project:api", "runtime", "Rust", "")
	appendReconcileClaim(t, s, base)
	appendReconcileClaim(t, s, evidence)
	writeReconcileTransition(t, s, reconcileTransitionInput{
		ID:        "tr_missing_date",
		CreatedAt: rfc3339(t0.Add(time.Hour)),
		Scope:     evidence.Entity,
		ClaimRefs: []string{evidence.ID},
	})
	beforeClaims := readFileIfExists(t, s.claimsLog())
	beforeEdges := readFileIfExists(t, s.edgesLog())
	beforeCandidates := snapshotDirFiles(t, s.candDir())

	report, err := s.SynthesisSweep([]string{"reconcile"}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.ReconcileParkedCount != 1 {
		t.Fatalf("parked = %d, want 1; report=%+v", report.ReconcileParkedCount, report)
	}
	if !hasReconcileParkedReason(report.ReconcileParkedReasons, reconcileParkMissingDate) {
		t.Fatalf("missing parked reason %q in %+v", reconcileParkMissingDate, report.ReconcileParkedReasons)
	}
	assertFileEqual(t, s.claimsLog(), beforeClaims)
	assertFileEqual(t, s.edgesLog(), beforeEdges)
	assertDirSnapshotEqual(t, s.candDir(), beforeCandidates)
}

func TestSynthesisReconcileHasNoTruthInstallingDisposition(t *testing.T) {
	for _, disposition := range reconcileDispositions {
		switch string(disposition) {
		case "supersede", "promote", "retire":
			t.Fatalf("reconcile disposition must not install truth: %q", disposition)
		}
	}
	allowedActed := map[reconcileDisposition]bool{
		reconcileDispositionReopen:     true,
		reconcileDispositionRaise:      true,
		reconcileDispositionDecayRoute: true,
	}
	for _, disposition := range reconcileActedDispositions {
		if !allowedActed[disposition] {
			t.Fatalf("acted-on reconcile disposition %q outside non-assertive set", disposition)
		}
	}
}

type reconcileTransitionInput struct {
	ID        string
	CreatedAt string
	Scope     string
	ClaimRefs []string
}

func reconcileClaim(id string, seq int64, entity, predicate, object, validFrom string) Claim {
	return Claim{
		ID:             id,
		Seq:            seq,
		Entity:         entity,
		Predicate:      predicate,
		Object:         object,
		ValidFrom:      validFrom,
		Confidence:     0.9,
		Derivation:     "extracted",
		PromotionBasis: "explicit_confirmation",
		EvidenceRefs:   []string{"trace_" + id},
		Visibility:     "ambient",
		CreatedAt:      firstNonEmpty(validFrom, rfc3339(t0)),
	}
}

func appendReconcileClaim(t *testing.T, s *Store, claim Claim) {
	t.Helper()
	if err := appendJSONL(s.claimsLog(), &claim); err != nil {
		t.Fatal(err)
	}
}

func writeReconcileTransition(t *testing.T, s *Store, in reconcileTransitionInput) {
	t.Helper()
	typed := make([]envelope.ProjectionRef, 0, len(in.ClaimRefs))
	for _, ref := range in.ClaimRefs {
		typed = append(typed, envelope.ProjectionRef{Kind: "memory", Role: "promoted", Ref: ref, EvidenceRef: "trace_" + ref})
	}
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            in.ID,
			CreatedAt:     in.CreatedAt,
			Producer:      envelope.Producer{Kind: "test", Name: "compound-test"},
			IDs:           envelope.IDs{WorkID: "work_" + in.ID},
			Scope:         envelope.Scope{EntityID: in.Scope},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  in.ClaimRefs,
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		BeforeRefs:  []string{},
		ActionRefs:  []string{},
		AfterRefs:   append([]string(nil), in.ClaimRefs...),
		StateDelta:  "test transition",
		Projections: envelope.Projections{Memory: append([]string(nil), in.ClaimRefs...), Typed: typed},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.eventStore.PutJSON(tr.ID, &tr, "  "); err != nil {
		t.Fatal(err)
	}
}

func writeReconcileCloseReceipt(t *testing.T, s *Store, receiptID, transitionID, createdAt string, used []string) {
	t.Helper()
	rec := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            receiptID,
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "test", Name: "compound-test"},
			IDs:           envelope.IDs{WorkID: "work_" + transitionID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    "completed",
		MoveType:   "audit_only",
		StateDelta: envelope.StateDelta{Changed: false, Summary: "usage-only close"},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Used: used, UsedDeclared: true, ApplicationMethod: "explicit_declaration"},
		Memory:     envelope.ReceiptMemory{},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
	}
	if err := rec.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.eventStore.PutJSON(rec.ID, &rec, "  "); err != nil {
		t.Fatal(err)
	}
	tr := envelope.Transition{
		Header: envelope.Header{
			SchemaVersion: envelope.SVTransition,
			ID:            transitionID,
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "test", Name: "compound-test"},
			IDs:           envelope.IDs{WorkID: "work_" + transitionID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		BeforeRefs:  []string{},
		ActionRefs:  []string{},
		AfterRefs:   []string{receiptID},
		StateDelta:  "closed",
		Projections: envelope.Projections{},
		SecondOrder: envelope.SecondOrder{},
	}
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.eventStore.PutJSON(tr.ID, &tr, "  "); err != nil {
		t.Fatal(err)
	}
}

func mustLoadClaims(t *testing.T, s *Store) []Claim {
	t.Helper()
	claims, err := s.loadClaims()
	if err != nil {
		t.Fatal(err)
	}
	return claims
}

func claimByID(claims []Claim, id string) Claim {
	for _, claim := range claims {
		if claim.ID == id {
			return claim
		}
	}
	return Claim{}
}

func readFileIfExists(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return data
}

func assertFileEqual(t *testing.T, path string, before []byte) {
	t.Helper()
	after := readFileIfExists(t, path)
	if string(after) != string(before) {
		t.Fatalf("%s changed\nbefore:\n%s\nafter:\n%s", path, string(before), string(after))
	}
}

func snapshotDirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data := readFileIfExists(t, filepath.Join(dir, entry.Name()))
		out[entry.Name()] = string(data)
	}
	return out
}

func assertDirSnapshotEqual(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := snapshotDirFiles(t, dir)
	if len(after) != len(before) {
		t.Fatalf("%s file count changed: before=%d after=%d", dir, len(before), len(after))
	}
	for name, beforeData := range before {
		if after[name] != beforeData {
			t.Fatalf("%s changed in %s", name, dir)
		}
	}
}

func reviewFilesExceptReconcile(t *testing.T, home string) []string {
	t.Helper()
	var out []string
	root := filepath.Join(home, "reviews")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "reconcile" {
			continue
		}
		out = append(out, entry.Name())
	}
	return out
}

func reconcileContestedReviewFiles(t *testing.T, s *Store) []string {
	t.Helper()
	root := filepath.Join(s.home, "reviews", reconcileContestedArea)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		out = append(out, entry.Name())
	}
	return out
}

func readReconcileContestedArtifact(t *testing.T, s *Store) map[string]any {
	t.Helper()
	root := filepath.Join(s.home, "reviews", reconcileContestedArea)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data := readFileIfExists(t, filepath.Join(root, entry.Name()))
		var artifact map[string]any
		if err := json.Unmarshal(data, &artifact); err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	t.Fatalf("no contested review artifact under %s", root)
	return nil
}

func readReconcileContestedMarkdown(t *testing.T, s *Store) string {
	t.Helper()
	root := filepath.Join(s.home, "reviews", reconcileContestedArea)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		return string(readFileIfExists(t, filepath.Join(root, entry.Name())))
	}
	t.Fatalf("no contested review markdown under %s", root)
	return ""
}

func hasReconcileParkedReason(reasons []SynthesisReconcileParkedReason, reason string) bool {
	for _, got := range reasons {
		if got.Reason == reason && got.Count > 0 {
			return true
		}
	}
	return false
}

func TestReadReconcileEventLegacyMentionIsNotApplication(t *testing.T) {
	rec := envelope.Receipt{
		Header:  envelope.Header{SchemaVersion: envelope.SVReceipt, ID: "rec_legacy_mention", CreatedAt: time.Now().UTC().Format(time.RFC3339), Producer: envelope.Producer{Kind: "test", Name: "compound-test"}, IDs: envelope.IDs{WorkID: "work_legacy_mention"}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1", EvidenceRefs: []string{}, Uncertainty: envelope.Uncertainty{Level: "known"}},
		Outcome: "completed", MoveType: "audit_only", StateDelta: envelope.StateDelta{Changed: false, Summary: "legacy close"}, Cost: envelope.ReceiptCost{ModelCalls: []string{}},
		Skills: envelope.ReceiptSkills{Used: []string{"claim://claim_mentioned"}, UsedDeclared: true}, Memory: envelope.ReceiptMemory{}, Next: envelope.ReceiptNext{SuggestedWork: []string{}},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	_, included, err := readReconcileEvent("legacy.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if included {
		t.Fatal("legacy transcript mention entered reconcile as application")
	}
}
