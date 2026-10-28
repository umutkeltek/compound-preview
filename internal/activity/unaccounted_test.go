package activity

import (
	"testing"
	"time"
)

func span(startMin, endMin int, bundle string, cat Category, coverage string) ActivitySpan {
	st := time.Date(2026, 7, 15, 9, startMin, 0, 0, time.UTC)
	en := time.Date(2026, 7, 15, 9, endMin, 0, 0, time.UTC)
	return ActivitySpan{
		Start:     st.Format(time.RFC3339),
		End:       en.Format(time.RFC3339),
		ActiveSec: int64(en.Sub(st).Seconds()),
		BundleID:  bundle,
		Category:  cat,
		Coverage:  coverage,
	}
}

func workAt(min int) WorkActivity {
	return WorkActivity{Ref: "rec_x", At: time.Date(2026, 7, 15, 9, min, 0, 0, time.UTC)}
}

// TestFalsifier8_TwoUnaccountedBlocksCountedOnce: a day with two >=20-min unaccounted blocks and one
// accounted block yields exactly ONE attention-eligible inference whose summary counts BOTH
// unaccounted blocks.
func TestFalsifier8_TwoUnaccountedBlocksCountedOnce(t *testing.T) {
	spans := []ActivitySpan{
		// Block A: browsing 0-25 (25 min, unaccounted)
		span(0, 25, "com.google.Chrome", CategoryBrowsing, CoverageComplete),
		// Block B: design 40-65 (25 min) but a work timestamp at 50 accounts for it
		span(40, 65, "com.figma.Desktop", CategoryDesign, CoverageComplete),
		// Block C: coding 90-115 (25 min, unaccounted)
		span(90, 115, "com.apple.dt.Xcode", CategoryCoding, CoverageComplete),
	}
	work := []WorkActivity{workAt(50)} // accounts only for block B
	inf := InferDay("2026-07-15", spans, work)
	if len(inf.UnaccountedBlocks) != 2 {
		t.Fatalf("want exactly 2 unaccounted blocks (A and C), got %d: %+v", len(inf.UnaccountedBlocks), inf.UnaccountedBlocks)
	}
	// Largest-first ordering (both 25 min -> earlier start wins -> block A first).
	if inf.UnaccountedBlocks[0].Start != span(0, 25, "", "", "").Start {
		t.Fatalf("largest/earliest unaccounted block should be block A, got %+v", inf.UnaccountedBlocks[0])
	}
	if inf.TotalUnaccountedMin != 50 {
		t.Fatalf("total unaccounted should be 50 min (both blocks), got %d", inf.TotalUnaccountedMin)
	}
	summary := inf.OperatorSummary()
	if summary == "" {
		t.Fatal("expected a non-empty operator summary")
	}
	// The summary must acknowledge the extra block ("+1 more").
	if !contains(summary, "+1 more") {
		t.Fatalf("summary must count the second unaccounted block: %q", summary)
	}
}

// TestFalsifier8_AccountedOnlyDayHasZeroBlocks: a day where every block overlaps a work timestamp
// yields no unaccounted blocks (no attention group).
func TestFalsifier8_AccountedOnlyDayHasZeroBlocks(t *testing.T) {
	spans := []ActivitySpan{span(0, 30, "com.google.Chrome", CategoryBrowsing, CoverageComplete)}
	inf := InferDay("2026-07-15", spans, []WorkActivity{workAt(15)})
	if len(inf.UnaccountedBlocks) != 0 {
		t.Fatalf("accounted day must yield zero unaccounted blocks, got %+v", inf.UnaccountedBlocks)
	}
	if inf.OperatorSummary() != "" {
		t.Fatalf("accounted day must have empty summary, got %q", inf.OperatorSummary())
	}
}

// TestFalsifier11_IncompleteCoverageExcludedFromBlocks: an unaccounted-shaped block made of
// incomplete-coverage spans must NOT produce an unaccounted block.
func TestFalsifier11_IncompleteCoverageExcludedFromBlocks(t *testing.T) {
	spans := []ActivitySpan{span(0, 40, "com.google.Chrome", CategoryBrowsing, CoverageIncomplete)}
	inf := InferDay("2026-07-15", spans, nil)
	if len(inf.UnaccountedBlocks) != 0 {
		t.Fatalf("incomplete-coverage window must be excluded, got %+v", inf.UnaccountedBlocks)
	}
}

// TestInferBelowFloorNotSurfaced: a <20-min block is never unaccounted-surfaced.
func TestInferBelowFloorNotSurfaced(t *testing.T) {
	spans := []ActivitySpan{span(0, 15, "com.google.Chrome", CategoryBrowsing, CoverageComplete)}
	inf := InferDay("2026-07-15", spans, nil)
	if len(inf.UnaccountedBlocks) != 0 {
		t.Fatalf("sub-20-min block must not surface, got %+v", inf.UnaccountedBlocks)
	}
}

// TestInferSystemCategoryExcluded: system chrome is not "work outside agent sessions".
func TestInferSystemCategoryExcluded(t *testing.T) {
	spans := []ActivitySpan{span(0, 40, "com.apple.finder", CategorySystem, CoverageComplete)}
	inf := InferDay("2026-07-15", spans, nil)
	if len(inf.UnaccountedBlocks) != 0 {
		t.Fatalf("system category must be excluded, got %+v", inf.UnaccountedBlocks)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
