package lanebeliefs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/mem"
)

var foldTestTime = time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)

// seedLaneNode imports one agent-lane node so a receipt declaring shortID attributes to it
// through the B0 store door. It mirrors capgraph's own resolve_lane_ref_test seeding: the
// exported mapper only COMPUTES the id to import here; it is not a resolution path.
func seedLaneNode(t *testing.T, homePath, shortID string) string {
	t.Helper()
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	nodeID, ok := capgraph.AgentLaneNodeIDFromShortLaneID(shortID)
	if !ok {
		t.Fatalf("short lane id %q has no node mapping", shortID)
	}
	if err := store.Add(capgraph.Node{ID: nodeID, Kind: "agent-lane", Name: nodeID, Weight: 0.25, EvidenceOnly: true}); err != nil {
		t.Fatalf("seed lane node %q: %v", nodeID, err)
	}
	return nodeID
}

func aggregateFor(report FoldReport, kind, domain string) (FoldAggregate, bool) {
	for _, agg := range report.Aggregates {
		if agg.Kind == kind && agg.Domain == domain {
			return agg, true
		}
	}
	return FoldAggregate{}, false
}

// F-j (spam) + F-f (idempotence): 20 receipts in ONE work for one lane collapse to n=1
// observation PER belief domain (receipt volume never inflates the Beta trial count), stay in the
// park band (one work < the corroboration floor), and a second fold over unchanged events adds
// nothing (cursor stable).
func TestFoldOneObservationPerWorkAndIdempotent(t *testing.T) {
	homePath := t.TempDir()
	seedLaneNode(t, homePath, "fixture/judge")
	for i := 0; i < 20; i++ {
		writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_spam_%02d", i),
			WorkID:    "work_one",
			LaneID:    "fixture/judge",
			MoveType:  "control_plane_repair",
			Outcome:   "completed",
			Changed:   true,
		})
	}

	report, err := Fold(homePath, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	// Each completed proof-bearing receipt yields two signals (capability + tendency); 20 spam
	// receipts in one work → exactly two observation trials, one per domain.
	if report.RawReceiptCount != 20 || report.NewObservationCount != 2 || report.CandidateCount != 2 {
		t.Fatalf("counts = raw %d observations %d candidates %d, want 20/2/2: %+v", report.RawReceiptCount, report.NewObservationCount, report.CandidateCount, report)
	}
	if report.ReinforcementCount != 0 {
		t.Fatalf("propose-only fold must not reinforce, got %d", report.ReinforcementCount)
	}
	tend, ok := aggregateFor(report, beliefKindTendency, tendencyReceiptFaithful)
	if !ok {
		t.Fatalf("missing receipt-faithful tendency aggregate: %+v", report.Aggregates)
	}
	if tend.HitCount != 1 || tend.OpportunityCount != 1 || tend.WorkCount != 1 {
		t.Fatalf("tendency Beta inflated by receipt spam: %+v", tend)
	}
	if tend.ReceiptCount != 20 {
		t.Fatalf("tendency receipt volume = %d, want 20 (volume recorded, n not inflated)", tend.ReceiptCount)
	}
	if tend.AbovePromotionFloor || tend.PosteriorBand != "park" {
		t.Fatalf("one-work spam must stay in the park band, no promotion: %+v", tend)
	}

	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored candidates = %d, want two proposals", len(stored))
	}
	for _, c := range stored {
		if c.State != "parked" {
			t.Fatalf("candidate %s state = %q, want parked", c.ID, c.State)
		}
	}

	again, err := Fold(homePath, FoldOptions{Now: foldTestTime.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if again.RawReceiptCount != 0 || again.NewObservationCount != 0 || again.CandidateCount != 0 || again.ReinforcementCount != 0 {
		t.Fatalf("second fold over unchanged events must be empty (idempotent), got %+v", again)
	}
	if again.CursorAfter != report.CursorAfter {
		t.Fatalf("cursor moved on an empty re-fold: %d -> %d", report.CursorAfter, again.CursorAfter)
	}
}

// F-k (privacy): operator-personal text planted in a receipt's free-text fields
// (StateDelta.Summary, FailureCause) never appears in any belief field — the extractor reads only
// closed structured fields and the valve refuses free text.
func TestFoldNeverCopiesReceiptFreeTextIntoBeliefFields(t *testing.T) {
	homePath := t.TempDir()
	seedLaneNode(t, homePath, "fixture/judge")
	secret := "synthetic-secret-token-do-not-copy"
	failText := "synthetic-failure-note-do-not-copy"
	for i := 0; i < MIN_LANE_RECEIPTS; i++ {
		writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
			ReceiptID:   fmt.Sprintf("rec_private_%02d", i),
			WorkID:      "work_private",
			LaneID:      "fixture/judge",
			MoveType:    "control_plane_repair",
			Outcome:     "completed",
			Changed:     true,
			Summary:     secret,
			FailureNote: failText,
		})
	}
	if _, err := Fold(homePath, FoldOptions{Now: foldTestTime}); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 {
		t.Fatal("expected proposed candidates to inspect for leaks")
	}
	for _, c := range stored {
		encoded, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), failText) {
			t.Fatalf("belief candidate leaked receipt free text: %s", encoded)
		}
	}
}

// F-l (identity): a receipt whose declared lane names no imported node contributes zero evidence
// and is counted unresolved (here a wildcard lane; the store-membership variant is
// TestFoldResolvesOnlyImportedLaneNodes).
func TestFoldUnresolvedLaneYieldsZeroEvidence(t *testing.T) {
	homePath := t.TempDir()
	for i := 0; i < MIN_LANE_RECEIPTS; i++ {
		writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_unresolved_%02d", i),
			WorkID:    "work_unresolved",
			LaneID:    "*",
			MoveType:  "control_plane_repair",
			Outcome:   "completed",
			Changed:   true,
		})
	}
	report, err := Fold(homePath, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if report.UnresolvedReceiptCount != MIN_LANE_RECEIPTS || report.NewObservationCount != 0 || report.CandidateCount != 0 {
		t.Fatalf("unresolved report = %+v, want unresolved receipts and zero evidence", report)
	}
}

func TestFoldIgnoresNonterminalActionReceipt(t *testing.T) {
	homePath := t.TempDir()
	seedLaneNode(t, homePath, "fixture/judge")
	writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
		ReceiptID: "rec_intermediate_action",
		WorkID:    "work_parent_still_running",
		LaneID:    "fixture/judge",
		MoveType:  "control_plane_repair",
		Outcome:   "completed",
		Changed:   true,
		To:        "running",
	})

	report, err := Fold(homePath, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if report.RawReceiptCount != 0 || report.NewObservationCount != 0 || report.CandidateCount != 0 || len(report.Aggregates) != 0 {
		t.Fatalf("nonterminal work event became lane outcome evidence: %+v", report)
	}
}

// A cancelled/no_op terminal receipt yields no BELIEF signal (the fold does not infer a capability
// or a raising tendency from a non-completion), but it IS one completion attempt — a first-class
// miss, not an ignored receipt: a lane's completion rate must see its cancellations, or the
// denominator lies. This reds against silently dropping non-completions from the completion kind.
func TestFoldCancelledTerminalReceiptIsCompletionMissNotBelief(t *testing.T) {
	homePath := t.TempDir()
	nodeID := seedLaneNode(t, homePath, "fixture/judge")
	writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
		ReceiptID: "rec_cancelled_terminal",
		WorkID:    "work_cancelled_terminal",
		LaneID:    "fixture/judge",
		MoveType:  "outward_delta",
		Outcome:   "no_op",
		Changed:   false,
		To:        "cancelled",
	})

	report, err := Fold(homePath, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	// No belief, and NOT ignored — it is one recorded completion miss.
	if report.RawReceiptCount != 1 || report.IgnoredReceiptCount != 0 || report.NewObservationCount != 0 || report.CandidateCount != 0 {
		t.Fatalf("cancelled terminal receipt formed a belief or was ignored: %+v", report)
	}
	if report.NewCompletionObservationCount != 1 {
		t.Fatalf("cancelled terminal receipt did not record a completion attempt: %+v", report)
	}
	post, err := CompletionPosterior(homePath, nodeID, capabilityClassUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if post.Attempts != 1 || post.Completions != 0 || post.Failures != 1 {
		t.Fatalf("completion posterior did not record the cancellation as a miss: %+v", post)
	}
}

// F-floor (noise floor): a lane below MIN_LANE_RECEIPTS yields no candidate; at the floor it is
// eligible. Below-floor lanes are counted, not silently dropped.
func TestFoldNoiseFloorGatesCandidates(t *testing.T) {
	below := t.TempDir()
	seedLaneNode(t, below, "fixture/judge")
	for i := 0; i < MIN_LANE_RECEIPTS-1; i++ {
		writeLaneReceiptEvent(t, below, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_below_%02d", i),
			WorkID:    fmt.Sprintf("work_below_%02d", i),
			LaneID:    "fixture/judge",
			MoveType:  "control_plane_repair",
			Outcome:   "completed",
			Changed:   true,
		})
	}
	belowReport, err := Fold(below, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if belowReport.CandidateCount != 0 {
		t.Fatalf("lane with %d receipts proposed candidates: %+v", MIN_LANE_RECEIPTS-1, belowReport)
	}
	if belowReport.BelowFloorLaneCount == 0 {
		t.Fatalf("below-floor lane/domains not counted: %+v", belowReport)
	}

	at := t.TempDir()
	seedLaneNode(t, at, "fixture/judge")
	for i := 0; i < MIN_LANE_RECEIPTS; i++ {
		writeLaneReceiptEvent(t, at, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_at_%02d", i),
			WorkID:    fmt.Sprintf("work_at_%02d", i),
			LaneID:    "fixture/judge",
			MoveType:  "control_plane_repair",
			Outcome:   "completed",
			Changed:   true,
		})
	}
	atReport, err := Fold(at, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if atReport.CandidateCount == 0 {
		t.Fatalf("lane at the floor (%d receipts) proposed nothing: %+v", MIN_LANE_RECEIPTS, atReport)
	}
}

// F-valve: a draft the B1 valve refuses is DROPPED and counted with its reason, never proposed —
// and the drop is per-draft (a sibling valid draft still proposes). Here a receipt whose move_type
// slugs to an empty task-class domain makes the capability draft invalid while the tendency draft
// stays valid.
func TestFoldValveDropsInvalidDraftAndCounts(t *testing.T) {
	homePath := t.TempDir()
	seedLaneNode(t, homePath, "fixture/judge")
	for i := 0; i < MIN_LANE_RECEIPTS; i++ {
		writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_badmove_%02d", i),
			WorkID:    "work_badmove",
			LaneID:    "fixture/judge",
			MoveType:  "///", // slugs to "" → invalid capability domain the valve refuses
			Outcome:   "completed",
			Changed:   true,
		})
	}
	report, err := Fold(homePath, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if report.ValveDroppedCount != 1 || len(report.ValveDrops) != 1 {
		t.Fatalf("valve drops = %d, want exactly one refused capability draft: %+v", report.ValveDroppedCount, report)
	}
	drop := report.ValveDrops[0]
	if drop.Kind != beliefKindCapability || !strings.Contains(drop.Reason, "domain") {
		t.Fatalf("valve drop = %+v, want a capability draft refused for its domain", drop)
	}
	// The sibling tendency draft (receipt-faithful) still proposes — the drop is per-draft.
	if report.CandidateCount != 1 {
		t.Fatalf("candidates = %d, want the one valid tendency belief proposed alongside the drop: %+v", report.CandidateCount, report)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range stored {
		if c.Predicate == mem.BeliefPredicateCapableOf {
			t.Fatalf("an invalid capability draft reached the store: %+v", c)
		}
	}
}

// F-green (end-to-end): a seeded capgraph lane + post-B0 completed proof-bearing receipts →
// the fold PROPOSES valid belief drafts (the valve accepts them, zero drops), visible via List,
// each carrying the two ratified shapes.
func TestFoldProposesValidBeliefsVisibleViaList(t *testing.T) {
	homePath := t.TempDir()
	nodeID := seedLaneNode(t, homePath, "fixture/judge")
	for i := 0; i < MIN_LANE_RECEIPTS; i++ {
		writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_green_%02d", i),
			WorkID:    fmt.Sprintf("work_green_%02d", i),
			LaneID:    "fixture/judge",
			MoveType:  "control_plane_repair",
			Outcome:   "completed",
			Changed:   true,
		})
	}
	report, err := Fold(homePath, FoldOptions{Now: foldTestTime})
	if err != nil {
		t.Fatal(err)
	}
	if report.CandidateCount != 2 || report.ValveDroppedCount != 0 {
		t.Fatalf("fold report = %+v, want two proposed candidates, zero valve drops", report)
	}

	list, err := List(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if list.CandidateCount != 2 {
		t.Fatalf("list candidates = %d, want two: %+v", list.CandidateCount, list)
	}
	sawTendency, sawCapability := false, false
	for _, c := range list.Candidates {
		if c.Entity != nodeID {
			t.Fatalf("candidate entity = %q, want the resolved node id %q", c.Entity, nodeID)
		}
		if c.ProvenanceLane != foldProvenanceTag {
			t.Fatalf("candidate provenance = %q, want the fold tag", c.ProvenanceLane)
		}
		if len(c.Tags) != 0 {
			t.Fatalf("belief candidate carries tags %v (the valve refuses tags)", c.Tags)
		}
		switch c.Predicate {
		case mem.BeliefPredicateTendsTo:
			sawTendency = true
			if c.Qualifiers["domain"] != tendencyReceiptFaithful {
				t.Fatalf("tendency domain = %q, want %q", c.Qualifiers["domain"], tendencyReceiptFaithful)
			}
		case mem.BeliefPredicateCapableOf:
			sawCapability = true
			if c.Qualifiers["domain"] != "control-plane-repair" {
				t.Fatalf("capability domain = %q, want control-plane-repair", c.Qualifiers["domain"])
			}
		default:
			t.Fatalf("non-belief predicate in fold output: %q", c.Predicate)
		}
	}
	if !sawTendency || !sawCapability {
		t.Fatalf("want one tendency and one capability belief, got tendency=%v capability=%v", sawTendency, sawCapability)
	}
}

func TestListEmptyHomeDoesNotScaffoldState(t *testing.T) {
	homePath := t.TempDir()
	report, err := List(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if report.CandidateCount != 0 {
		t.Fatalf("empty list = %+v", report)
	}
	entries, err := os.ReadDir(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("read-only list scaffolded runtime state: %+v", entries)
	}
}

func TestFoldConstantsMatchBinding(t *testing.T) {
	if MIN_LANE_RECEIPTS != 5 || MIN_CORROBORATION_WORKS != 2 || BETA_PRIOR_ALPHA != 2 || BETA_PRIOR_BETA != 2 || PROMOTE_POSTERIOR_FLOOR != 0.66 || COOL_AFTER_FOLDS != 3 || PER_CYCLE_CANDIDATE_CAP <= 0 {
		t.Fatalf("fold constants drifted")
	}
}

type laneReceiptFixture struct {
	ReceiptID   string
	WorkID      string
	LaneID      string
	MoveType    string
	Outcome     string
	Changed     bool
	Summary     string
	FailureNote string
	NoProof     bool
	To          string
}

func writeLaneReceiptEvent(t *testing.T, homePath string, fx laneReceiptFixture) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(homePath, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	created := foldTestTime.Format(time.RFC3339)
	proofRefs := []string{"proof:test"}
	if fx.NoProof {
		proofRefs = nil
	}
	receipt := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            fx.ReceiptID,
			CreatedAt:     created,
			Producer:      envelope.Producer{Kind: "test", Name: "fixture", LaneID: fx.LaneID},
			IDs:           envelope.IDs{WorkID: fx.WorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier0",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    fx.Outcome,
		MoveType:   fx.MoveType,
		StateDelta: envelope.StateDelta{Changed: fx.Changed, Summary: fx.Summary, FilesChanged: []string{"a.go", "b.go"}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Candidate: []string{}},
		Memory:     envelope.ReceiptMemory{Candidates: []string{}},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs:  proofRefs,
	}
	if fx.FailureNote != "" {
		receipt.FailureCause = &envelope.FailureCause{SurfaceOutcome: fx.FailureNote}
	}
	writeJSONFile(t, filepath.Join(homePath, "events", fx.ReceiptID+".json"), receipt)
	to := fx.To
	if to == "" {
		to = "completed"
	}
	line := map[string]any{
		"ts":       created,
		"work_id":  fx.WorkID,
		"from":     "running",
		"to":       to,
		"attached": []string{fx.ReceiptID},
	}
	b, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(homePath, "events", "work.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
