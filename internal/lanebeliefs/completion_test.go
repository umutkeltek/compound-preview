package lanebeliefs

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
)

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// foldCompletionWorks folds `completed`+`failed` works for one lane (completions completing, the
// rest failing) and returns the resolved node id so a test can query the posterior. One receipt per
// work, so receipt volume equals the attempt count.
func foldCompletionWorks(t *testing.T, homePath, shortLane string, completed, failed int) string {
	t.Helper()
	nodeID := seedLaneNode(t, homePath, shortLane)
	i := 0
	for ; i < completed; i++ {
		writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_done_%02d", i),
			WorkID:    fmt.Sprintf("work_done_%02d", i),
			LaneID:    shortLane,
			MoveType:  "control_plane_repair",
			Outcome:   "completed",
			Changed:   true,
			To:        "completed",
		})
	}
	for j := 0; j < failed; j++ {
		writeLaneReceiptEvent(t, homePath, laneReceiptFixture{
			ReceiptID: fmt.Sprintf("rec_fail_%02d", j),
			WorkID:    fmt.Sprintf("work_fail_%02d", j),
			LaneID:    shortLane,
			MoveType:  "outward_delta",
			Outcome:   "failed",
			Changed:   false,
			To:        "failed",
		})
	}
	if _, err := Fold(homePath, FoldOptions{Now: foldTestTime}); err != nil {
		t.Fatal(err)
	}
	return nodeID
}

// The real fold path records one completion trial per terminal work attempt — completions as hits,
// failures as misses — so the posterior is P(completed) with honest evidence counts. Reds against a
// completion kind that only counts completions (a denominator that ignores failures reads as 1.0).
func TestFoldCompletionPosteriorCountsHitsAndMisses(t *testing.T) {
	homePath := t.TempDir()
	nodeID := foldCompletionWorks(t, homePath, "fixture/judge", 3, 2)

	post, err := CompletionPosterior(homePath, nodeID, capabilityClassUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if post.Attempts != 5 || post.Completions != 3 || post.Failures != 2 {
		t.Fatalf("completion counts = %d/%d/%d, want 5 attempts / 3 completions / 2 failures: %+v", post.Attempts, post.Completions, post.Failures, post)
	}
	if !approxEqual(post.PosteriorMean, float64(BETA_PRIOR_ALPHA+3)/float64(BETA_PRIOR_ALPHA+BETA_PRIOR_BETA+5)) {
		t.Fatalf("posterior mean = %v, want (2+3)/(4+5)", post.PosteriorMean)
	}
	if !post.MeetsFloor || post.PriorOnly {
		t.Fatalf("5 receipts should clear the floor and not read as prior-only: %+v", post)
	}
	if len(post.ObservationRefs) != 5 {
		t.Fatalf("observation refs = %v, want one per attempt", post.ObservationRefs)
	}
}

// F-floor for the completion kind: a lane with only 4 receipts yields NO posterior — MeetsFloor is
// false below MIN_LANE_RECEIPTS and true at it. Reds against a completion posterior that priced a
// lane on 4 attempts.
func TestFoldCompletionFloorGatesPosterior(t *testing.T) {
	below := t.TempDir()
	belowNode := foldCompletionWorks(t, below, "fixture/judge", MIN_LANE_RECEIPTS-1, 0)
	belowPost, err := CompletionPosterior(below, belowNode, capabilityClassUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if belowPost.Attempts != MIN_LANE_RECEIPTS-1 || belowPost.MeetsFloor {
		t.Fatalf("lane with %d receipts must not clear the floor: %+v", MIN_LANE_RECEIPTS-1, belowPost)
	}

	at := t.TempDir()
	atNode := foldCompletionWorks(t, at, "fixture/judge", MIN_LANE_RECEIPTS, 0)
	atPost, err := CompletionPosterior(at, atNode, capabilityClassUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if atPost.Attempts != MIN_LANE_RECEIPTS || !atPost.MeetsFloor {
		t.Fatalf("lane at the floor (%d receipts) must clear it: %+v", MIN_LANE_RECEIPTS, atPost)
	}
}

// The prior-only case is VISIBLY prior-only: an unobserved (lane, class) cell returns the bare 2/2
// prior with zero evidence counts, so a consumer can never mistake the prior mean for knowledge.
func TestCompletionPriorOnlyCellIsVisiblyPrior(t *testing.T) {
	homePath := t.TempDir()
	// Fold some evidence for ONE class, then query a DIFFERENT class on the same lane.
	nodeID := foldCompletionWorks(t, homePath, "fixture/judge", MIN_LANE_RECEIPTS, 0)

	post, err := CompletionPosterior(homePath, nodeID, "judge-critique")
	if err != nil {
		t.Fatal(err)
	}
	if !post.PriorOnly || post.Attempts != 0 || post.Completions != 0 || post.MeetsFloor {
		t.Fatalf("unobserved class cell must be prior-only with zero evidence: %+v", post)
	}
	if !approxEqual(post.PosteriorMean, float64(BETA_PRIOR_ALPHA)/float64(BETA_PRIOR_ALPHA+BETA_PRIOR_BETA)) {
		t.Fatalf("prior-only mean = %v, want alpha/(alpha+beta)=0.5", post.PosteriorMean)
	}
	if len(post.ObservationRefs) != 0 {
		t.Fatalf("prior-only cell cites no observations, got %v", post.ObservationRefs)
	}
}

// recordCompletionTrials writes `attempts` completion trials for (lane, class), `completions` of them
// hits — through the fold's OWN writer, the exact records a future class-resolving producer emits.
func recordCompletionTrials(t *testing.T, homePath, laneID, class string, attempts, completions int) {
	t.Helper()
	recs := make([]observationRecord, 0, attempts)
	for i := 0; i < attempts; i++ {
		hit := 0
		if i < completions {
			hit = 1
		}
		recs = append(recs, observationRecord{
			GroupKey:          hashParts(laneID, beliefKindCompletion, class, fmt.Sprintf("w_%s_%02d", class, i)),
			SubjectLaneNodeID: laneID,
			Domain:            class,
			Kind:              beliefKindCompletion,
			Object:            completionObjectAttempt,
			WorkID:            fmt.Sprintf("w_%s_%02d", class, i),
			EvidenceEventID:   fmt.Sprintf("rec_%s_%02d", class, i),
			Hits:              hit,
			Opportunities:     1,
			ReceiptCount:      1,
		})
	}
	if err := appendCompletionObservations(homePath, recs); err != nil {
		t.Fatal(err)
	}
}

// Two DIFFERENT known capability classes never pool into one posterior, and "unknown" is its own
// bucket — each cell reports only its own attempts/completions. Reds against a read surface that
// dropped the class (Domain) filter and summed every class together.
func TestCompletionKnownClassesNeverPool(t *testing.T) {
	homePath := t.TempDir()
	const laneX = "agent-lane:x"
	const laneY = "agent-lane:y"
	recordCompletionTrials(t, homePath, laneX, "judge-critique", 6, 4)
	recordCompletionTrials(t, homePath, laneX, "build-implement", 5, 5)
	recordCompletionTrials(t, homePath, laneX, capabilityClassUnknown, 3, 0)
	recordCompletionTrials(t, homePath, laneY, "judge-critique", 8, 1) // same class, different lane

	judge, err := CompletionPosterior(homePath, laneX, "judge-critique")
	if err != nil {
		t.Fatal(err)
	}
	build, err := CompletionPosterior(homePath, laneX, "build-implement")
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := CompletionPosterior(homePath, laneX, capabilityClassUnknown)
	if err != nil {
		t.Fatal(err)
	}
	// If any class pooled, its Attempts would exceed its own 6/5/3 (e.g. 14 for laneX pooled).
	if judge.Attempts != 6 || judge.Completions != 4 {
		t.Fatalf("judge-critique pooled other classes: %+v", judge)
	}
	if build.Attempts != 5 || build.Completions != 5 {
		t.Fatalf("build-implement pooled other classes: %+v", build)
	}
	if unknown.Attempts != 3 || unknown.Completions != 0 {
		t.Fatalf("unknown bucket pooled a known class: %+v", unknown)
	}
	if approxEqual(judge.PosteriorMean, build.PosteriorMean) || approxEqual(judge.PosteriorMean, unknown.PosteriorMean) {
		t.Fatalf("distinct classes collapsed to one posterior: judge=%v build=%v unknown=%v", judge.PosteriorMean, build.PosteriorMean, unknown.PosteriorMean)
	}
	// Same class on a different lane must not bleed across the lane axis either.
	laneYJudge, err := CompletionPosterior(homePath, laneY, "judge-critique")
	if err != nil {
		t.Fatal(err)
	}
	if laneYJudge.Attempts != 8 || laneYJudge.Completions != 1 {
		t.Fatalf("lane axis pooled: %+v", laneYJudge)
	}
}

// Determinism: the same attempts discovered in permuted event/receipt order fold to byte-identical
// output — report, belief observations, AND completion trials. This bites the completion reducers:
// the AND-reduced hit and the smallest-receipt-id evidence pin must not depend on discovery order.
func TestFoldCompletionDeterministicUnderPermutedDiscoveryOrder(t *testing.T) {
	// work_c mixes a completed and a failed terminal (the reopen edge) and work_d is a triple-receipt
	// failure, so both the AND-hit reducer and the min-evidence-id reducer are exercised.
	fixtures := []laneReceiptFixture{
		{ReceiptID: "rec_a1", WorkID: "work_a", LaneID: "fixture/judge", MoveType: "control_plane_repair", Outcome: "completed", Changed: true, To: "completed"},
		{ReceiptID: "rec_b1", WorkID: "work_b", LaneID: "fixture/judge", MoveType: "outward_delta", Outcome: "failed", Changed: false, To: "failed"},
		{ReceiptID: "rec_c1", WorkID: "work_c", LaneID: "fixture/judge", MoveType: "control_plane_repair", Outcome: "completed", Changed: true, To: "completed"},
		{ReceiptID: "rec_c2", WorkID: "work_c", LaneID: "fixture/judge", MoveType: "outward_delta", Outcome: "failed", Changed: false, To: "failed"},
		{ReceiptID: "rec_d1", WorkID: "work_d", LaneID: "fixture/judge", MoveType: "outward_delta", Outcome: "failed", Changed: false, To: "failed"},
		{ReceiptID: "rec_d2", WorkID: "work_d", LaneID: "fixture/judge", MoveType: "outward_delta", Outcome: "failed", Changed: false, To: "failed"},
		{ReceiptID: "rec_d3", WorkID: "work_d", LaneID: "fixture/judge", MoveType: "outward_delta", Outcome: "failed", Changed: false, To: "failed"},
	}
	permuted := []laneReceiptFixture{fixtures[6], fixtures[3], fixtures[0], fixtures[4], fixtures[1], fixtures[5], fixtures[2]}

	runFold := func(order []laneReceiptFixture) (FoldReport, []byte, []byte) {
		home := t.TempDir()
		seedLaneNode(t, home, "fixture/judge")
		for _, fx := range order {
			writeLaneReceiptEvent(t, home, fx)
		}
		report, err := Fold(home, FoldOptions{Now: foldTestTime})
		if err != nil {
			t.Fatal(err)
		}
		obs, err := os.ReadFile(observationsPath(home))
		if err != nil {
			t.Fatal(err)
		}
		comp, err := os.ReadFile(completionsPath(home))
		if err != nil {
			t.Fatal(err)
		}
		return report, obs, comp
	}

	reportA, obsA, compA := runFold(fixtures)
	reportB, obsB, compB := runFold(permuted)

	jsonA, _ := json.Marshal(reportA)
	jsonB, _ := json.Marshal(reportB)
	if string(jsonA) != string(jsonB) {
		t.Fatalf("fold report differs under permuted discovery order:\n a=%s\n b=%s", jsonA, jsonB)
	}
	if string(obsA) != string(obsB) {
		t.Fatalf("belief observations differ under permuted discovery order:\n a=%s\n b=%s", obsA, obsB)
	}
	if string(compA) != string(compB) {
		t.Fatalf("completion trials differ under permuted discovery order:\n a=%s\n b=%s", compA, compB)
	}
	// Depth check on the reduced result: four attempts (a,b,c,d), and only work_a completed —
	// work_c's completed receipt is AND-reduced to a miss by its failed sibling.
	if reportA.NewCompletionObservationCount != 4 {
		t.Fatalf("want four completion attempts (a,b,c,d), got %d", reportA.NewCompletionObservationCount)
	}
}
