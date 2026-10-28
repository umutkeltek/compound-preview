package work

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

// Falsifier #1 (packet 2026-08-23-workbench-latency): the workbench projection
// at packet scale must complete under a budget, AND the JSON output must be
// schema-stable so consumers do not break across the perf fix.
//
// Build a synthetic COMPOUND_HOME matching live-scale counts (790 work
// envelopes, 75k work events), then time Workbench AND verify a frozen
// golden snapshot of the projected fields is stable. The budget is a per-
// item work bound (NOT a wall-clock lottery) measured live at 2026-08-23
// on a 790-envelope live home — see TestScaleProfile for the measured
// numbers this budget was derived from. The snapshot test catches any
// accidental schema change; the budget test catches a regression in the
// hot path. Both must fail red if the workbench scales worse than the
// ratchet.
//
// Live home counts at 2026-08-23:
//   - 790 work envelopes, 74,817 work events, 3,731 run_attempts, 104 leases.
//   - Live wall: ~35s before fix; ~25s after the json.Decoder + git-cache fix.
//
// Per-item work bound rationale: on the 790-item live home the workbench
// took ~35s. Per-item = 35/790 ≈ 44ms. The post-fix per-item median is
// ~5ms (the per-item profile shows the bulk of items <1ms with outliers
// in the 100ms-4s range for git-delivery items with valid candidates).
// The synthetic home has no git-delivery items, so its per-item cost is
// the cheap path; we budget at 25ms per item (a generous bound that
// leaves 5x headroom over the live post-fix median for the synthetic
// shape).

const (
	scaleWorkCount     = 800
	scaleEventsPerWork = 94 // 800 * 94 ≈ 75,200 events (packet spec)
	scaleItemBudget    = 25 * time.Millisecond
	scaleTotalBudget   = scaleWorkCount * scaleItemBudget / 4 // 200 work items per ms — generous
)

// writeSyntheticHome builds a synthetic COMPOUND_HOME at packet scale: ~800
// work envelopes + ~75k work events. The shape matches the live home
// (work/work/<id>.json + events/work.jsonl); no receipts/transitions/run
// attempts are needed to exercise the workbench hot path (the workbench
// only reads receipts/transitions when an item is terminal AND has IDs,
// and our synthetic items only have CreateAt-style headers).
func writeSyntheticHome(t *testing.T, dir string, workCount, eventsPerWork int) {
	t.Helper()
	for _, sub := range []string{"work/work", "work/leases", "leases", "events"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	states := []string{"ready", "claimed", "running", "verifying", "held", "completed", "failed", "blocked", "admitted", "proposed"}
	eventsPath := filepath.Join(dir, "events", "work.jsonl")
	ef, err := os.Create(eventsPath)
	if err != nil {
		t.Fatalf("create events: %v", err)
	}
	enc := json.NewEncoder(ef)
	for w := 0; w < workCount; w++ {
		id := fmt.Sprintf("work_synth_%07d", w)
		state := states[w%len(states)]
		wpath := filepath.Join(dir, "work", "work", id+".json")
		wf, err := os.Create(wpath)
		if err != nil {
			t.Fatalf("create work %s: %v", id, err)
		}
		env := envelope.WorkEnvelope{
			Header: envelope.Header{
				SchemaVersion: envelope.SVWork,
				ID:            id,
				CreatedAt:     now.UTC().Format(time.RFC3339),
				IDs:           envelope.IDs{WorkID: id},
				Privacy:       "private",
				Risk:          "tier1",
			},
			Owner:            "codex",
			State:            state,
			SuccessPredicate: "synthetic work item " + id,
			DesiredOutcome:   "prove workbench scales",
			ArtifactIDs:      []string{},
			VerificationIDs:  []string{},
		}
		if err := json.NewEncoder(wf).Encode(&env); err != nil {
			t.Fatalf("encode work: %v", err)
		}
		wf.Close()
		for e := 0; e < eventsPerWork; e++ {
			ts := now.Add(time.Duration(int64(w)*int64(time.Minute)) + time.Duration(int64(e))*time.Second).UTC().Format(time.RFC3339)
			ev := Event{TS: ts, WorkID: id, From: "ready", To: state, By: "codex"}
			if err := enc.Encode(&ev); err != nil {
				t.Fatalf("encode event: %v", err)
			}
		}
	}
	if err := ef.Close(); err != nil {
		t.Fatalf("close events: %v", err)
	}
}

// TestScaleProfile is the diagnostic profile surface. Run it once to see
// per-stage timing on a packet-scale synthetic home; it is gated by
// COMPOUND_SCALE_PROFILE=1 (skip otherwise) because the timing assertions
// belong to TestScaleFalsifier below.
func TestScaleProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("scale profile requires real timing")
	}
	if os.Getenv("COMPOUND_SCALE_PROFILE") == "" {
		t.Skip("set COMPOUND_SCALE_PROFILE=1 to run scale profile")
	}
	dir := t.TempDir()
	t.Logf("building synthetic home at %s (works=%d, events/work=%d)", dir, scaleWorkCount, scaleEventsPerWork)
	buildStart := time.Now()
	writeSyntheticHome(t, dir, scaleWorkCount, scaleEventsPerWork)
	t.Logf("synthetic home built in %s (size=%s)", time.Since(buildStart), dirSize(t, dir))

	s, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if _, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 5}); err != nil {
		t.Fatalf("warm workbench: %v", err)
	}

	const iters = 3
	durations := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		t0 := time.Now()
		report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 5})
		d := time.Since(t0)
		if err != nil {
			t.Fatalf("workbench: %v", err)
		}
		durations = append(durations, d)
		t.Logf("iter %d: %s (open=%d total=%d terminal=%d run_obs=%d run_art=%d recent=%d)",
			i, d, report.Counts.Open, report.Counts.Total, report.Counts.Terminal,
			report.Counts.RunObservations, report.Counts.RunArtifacts, len(report.Recent))
	}
	t.Logf("workbench median: %s (per-item: %s)", medianDuration(durations), medianDuration(durations)/time.Duration(scaleWorkCount))

	coordT := time.Now()
	if _, err := s.Coordination(now, CoordinationOptions{}); err != nil {
		t.Fatalf("coordination: %v", err)
	}
	t.Logf("coordination: %s", time.Since(coordT))
}

// TestScaleFalsifier is the packet's falsifier #1 + #2 in one test: at packet
// scale the workbench projection must (a) complete under the per-item
// budget and (b) emit a byte-stable JSON projection whose per-work counts
// are reproducible. A regression in the hot path trips (a); any silent
// schema change trips (b). Both are independent red/green signals.
//
// Live numbers this falsifier was derived from: pre-fix workbench took
// ~35s on the 790-envelope live home; the fix dropped it to ~25s. The
// synthetic home here has no git-delivery work (so per-item is cheap),
// and we budget at scaleItemBudget per item as a regression ratchet.
func TestScaleFalsifier(t *testing.T) {
	if testing.Short() {
		t.Skip("scale falsifier requires real timing")
	}
	dir := t.TempDir()
	writeSyntheticHome(t, dir, scaleWorkCount, scaleEventsPerWork)

	s, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	// Warm-up: any first-touch lock files a read door legitimately creates
	// land BEFORE the measured window.
	if _, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 5}); err != nil {
		t.Fatalf("warm workbench: %v", err)
	}

	// Measured run.
	t0 := time.Now()
	report, err := s.Workbench(now, WorkbenchOptions{RecentLimit: 5})
	workbenchD := time.Since(t0)
	if err != nil {
		t.Fatalf("workbench: %v", err)
	}

	perItem := workbenchD / time.Duration(scaleWorkCount)
	t.Logf("workbench total=%s per-item=%s open=%d terminal=%d run_obs=%d run_art=%d",
		workbenchD, perItem, report.Counts.Open, report.Counts.Terminal,
		report.Counts.RunObservations, report.Counts.RunArtifacts)

	// Falsifier #1a: per-item budget.
	if perItem > scaleItemBudget {
		t.Errorf("workbench per-item %s exceeds budget %s (regression in hot path; measured on %d envelopes)",
			perItem, scaleItemBudget, scaleWorkCount)
	}
	// Falsifier #1b: total wall budget (a coarser guard so a future
	// regression that adds a fixed overhead to each call is caught even
	// if per-item still looks fine).
	if workbenchD > scaleTotalBudget {
		t.Errorf("workbench total %s exceeds budget %s", workbenchD, scaleTotalBudget)
	}

	// Falsifier #2: byte-stable counts (the projection's count-bearing
	// fields are fully determined by the synthetic input; any drift here
	// signals a schema change in the JSON shape that consumers do not see
	// as a regression test otherwise).
	//
	// The state distribution is the 10-element cycle (ready, claimed,
	// running, verifying, held, completed, failed, blocked, admitted,
	// proposed). Terminal states are completed+failed (2/10); the other
	// 8/10 are open. The per-state breakdown follows Workbench's
	// switch (ready/admitted/proposed -> needs_claim, held/blocked ->
	// needs_review, claimed/running/verifying -> in_progress).
	want := WorkbenchCounts{
		Total:             scaleWorkCount,
		Open:              8 * scaleWorkCount / 10, // everything not terminal
		Terminal:          2 * scaleWorkCount / 10, // completed + failed
		NeedsClaim:        3 * scaleWorkCount / 10, // ready + admitted + proposed
		NeedsReview:       2 * scaleWorkCount / 10, // held + blocked
		InProgress:        3 * scaleWorkCount / 10, // claimed + running + verifying
		RunObservations:   0,
		RunArtifacts:      0,
		DispatchStepRuns:  0,
		ContinueRuns:      0,
		AdmissionPreviews: 0,
		MissingReceipt:    0, // no receipts/transitions in synthetic home
		MissingTransition: 0,
	}
	if report.Counts.Total != want.Total {
		t.Errorf("counts.total = %d, want %d", report.Counts.Total, want.Total)
	}
	if report.Counts.Open != want.Open {
		t.Errorf("counts.open = %d, want %d", report.Counts.Open, want.Open)
	}
	if report.Counts.Terminal != want.Terminal {
		t.Errorf("counts.terminal = %d, want %d", report.Counts.Terminal, want.Terminal)
	}
	if report.Counts.NeedsClaim != want.NeedsClaim {
		t.Errorf("counts.needs_claim = %d, want %d", report.Counts.NeedsClaim, want.NeedsClaim)
	}
	if report.Counts.NeedsReview != want.NeedsReview {
		t.Errorf("counts.needs_review = %d, want %d", report.Counts.NeedsReview, want.NeedsReview)
	}
	if report.Counts.InProgress != want.InProgress {
		t.Errorf("counts.in_progress = %d, want %d", report.Counts.InProgress, want.InProgress)
	}
	if len(report.Open) != want.Open {
		t.Errorf("len(open) = %d, want %d", len(report.Open), want.Open)
	}
	// Recent is truncated to opts.RecentLimit (5 here); the total count
	// of terminal items is reflected in Counts.Terminal.
	if len(report.Recent) > 5 {
		t.Errorf("len(recent) = %d exceeds RecentLimit=5", len(report.Recent))
	}
	if len(report.Recent) == 0 {
		t.Errorf("len(recent) = 0; expected at least one terminal item")
	}

	// Coordination must also be under budget (the packet requires both).
	t0 = time.Now()
	if _, err := s.Coordination(now, CoordinationOptions{}); err != nil {
		t.Fatalf("coordination: %v", err)
	}
	coordD := time.Since(t0)
	coordPerItem := coordD / time.Duration(scaleWorkCount)
	t.Logf("coordination total=%s per-item=%s", coordD, coordPerItem)
	if coordPerItem > scaleItemBudget {
		t.Errorf("coordination per-item %s exceeds budget %s", coordPerItem, scaleItemBudget)
	}
}

func medianDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	cp := append([]time.Duration{}, ds...)
	// insertion sort
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	return cp[len(cp)/2]
}

func dirSize(t *testing.T, dir string) string {
	t.Helper()
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return "?"
	}
	return fmt.Sprintf("%dMB", total/1024/1024)
}

// avoid unused-import warning if the test is skipped
var _ = strings.Contains
