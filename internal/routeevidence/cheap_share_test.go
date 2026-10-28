package routeevidence

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cheapShareRow(t *testing.T, requestID string, costMicros int64, totalTokens int) Evidence {
	t.Helper()
	ev, err := Build(BuildInput{
		CreatedAt:     time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC),
		RequestID:     requestID,
		ClientProfile: "provider-ledger:test",
		Protocol:      "chat_completions",
		Provider:      "fixture-provider",
		Model:         "fixture-model",
		Usage:         Usage{TotalTokens: totalTokens},
		CostMicros:    costMicros,
		Status:        "completed",
	})
	if err != nil {
		t.Fatalf("build evidence %s: %v", requestID, err)
	}
	return ev
}

// The reducer must split rows spend-first: cost_micros>0 is paid, otherwise
// cheap. It fails if the classification or either sum (calls, tokens) is wrong,
// and if the ledger fields the ratio depends on change shape silently (the
// literal rows below would then miscount or fail to build).
func TestComputeCheapShareCountsCallsAndTokens(t *testing.T) {
	rows := []Evidence{
		cheapShareRow(t, "req_free_1", 0, 100),   // cheap
		cheapShareRow(t, "req_free_2", 0, 50),    // cheap
		cheapShareRow(t, "req_free_3", 0, 0),     // cheap, no tokens
		cheapShareRow(t, "req_paid_1", 2500, 40), // paid
	}
	got := ComputeCheapShare(rows)

	if !got.Present {
		t.Fatalf("present=false over non-empty rows")
	}
	if got.Basis != CheapShareBasis {
		t.Fatalf("basis=%q, want %q", got.Basis, CheapShareBasis)
	}
	if got.TotalCalls != 4 || got.CheapCalls != 3 || got.PaidCalls != 1 {
		t.Fatalf("call split wrong: %+v", got)
	}
	if got.TotalTokens != 190 || got.CheapTokens != 150 || got.PaidTokens != 40 {
		t.Fatalf("token split wrong: %+v", got)
	}
}

// Empty rows must report absent so a renderer omits the beat (never a fake 0%).
func TestComputeCheapShareEmptyIsAbsent(t *testing.T) {
	got := ComputeCheapShare(nil)
	if got.Present || got.TotalCalls != 0 {
		t.Fatalf("empty rows should be absent: %+v", got)
	}
}

// A real all-paid ledger is a genuine 0% cheap share, not "absent". The beat
// then honestly reports zero, distinct from the no-ledger case above.
func TestComputeCheapShareAllPaidIsRealZero(t *testing.T) {
	got := ComputeCheapShare([]Evidence{cheapShareRow(t, "req_paid_only", 1000, 10)})
	if !got.Present {
		t.Fatalf("present=false over a real (paid) call")
	}
	if got.CheapCalls != 0 || got.PaidCalls != 1 {
		t.Fatalf("all-paid split wrong: %+v", got)
	}
}

// The loader distinguishes an absent ledger from an imported one and decodes
// every recorded row.
func TestLoadRouteEvidenceLedgerRoundTrip(t *testing.T) {
	home := t.TempDir()

	rows, exists, err := LoadRouteEvidenceLedger(home)
	if err != nil {
		t.Fatalf("load absent ledger: %v", err)
	}
	if exists || len(rows) != 0 {
		t.Fatalf("absent ledger should report exists=false, got exists=%v rows=%d", exists, len(rows))
	}

	if _, err := RecordJoined(home, cheapShareRow(t, "req_free_1", 0, 100)); err != nil {
		t.Fatalf("record free row: %v", err)
	}
	if _, err := RecordJoined(home, cheapShareRow(t, "req_paid_1", 2500, 40)); err != nil {
		t.Fatalf("record paid row: %v", err)
	}

	rows, exists, err = LoadRouteEvidenceLedger(home)
	if err != nil {
		t.Fatalf("load recorded ledger: %v", err)
	}
	if !exists || len(rows) != 2 {
		t.Fatalf("recorded ledger should report exists=true with 2 rows, got exists=%v rows=%d", exists, len(rows))
	}
	share := ComputeCheapShare(rows)
	if share.CheapCalls != 1 || share.PaidCalls != 1 || share.CheapTokens != 100 || share.PaidTokens != 40 {
		t.Fatalf("round-trip share wrong: %+v", share)
	}
}

// A silent projection-shape drift (an unexpected field in a persisted row) must
// surface as a load error, not a silently wrong ratio.
func TestLoadRouteEvidenceLedgerRejectsShapeDrift(t *testing.T) {
	home := t.TempDir()
	ledgerDir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(ledgerDir, 0o755); err != nil {
		t.Fatalf("mkdir ledger: %v", err)
	}
	drifted := `{"schema_version":"compound.execution_route_evidence.v0.1","id":"route_x","created_at":"2026-07-15T08:00:00Z","request_id":"req_x","client_profile":"p","protocol":"chat_completions","selected_target":{"provider":"a","model":"b","protocol":"chat_completions"},"status":"completed","recorded_via":"generic","surprise_new_field":true}` + "\n"
	if err := os.WriteFile(filepath.Join(ledgerDir, executionRouteEvidenceLedger), []byte(drifted), 0o644); err != nil {
		t.Fatalf("write drifted ledger: %v", err)
	}
	if _, _, err := LoadRouteEvidenceLedger(home); err == nil {
		t.Fatalf("loader accepted an unknown field (silent shape drift)")
	}
}
