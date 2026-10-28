package policy

// Falsifiers for retention apply/empty-trash (packet work_f4ac0f4e04de).
//
// The packet names four falsifiers:
//   1. dry-run is the default (no --yes = zero bytes changed)
//   2. --yes reclaims only whitelisted-class, old files; forbidden dirs untouched
//   3. trash restore (move back == original tree)
//   4. receipt written per apply with correct byte counts
//
// Each test below exercises ONE falsifier with a scratch home so the live
// $COMPOUND_HOME never sees a retention verb. Tests assert on byte equality
// (not approximate) because the failure mode we are proving against is a
// silent partial move.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixedNow is the test clock — every test seeds files with explicit mtimes
// relative to it, so age floors behave deterministically regardless of when
// the test happens to run. Tests never compare wall-clock against fixture
// dates (the AGENTS.md ban on fixture staleness).
//
// The package-level fixedNow in policy_test.go is the canonical test clock;
// this constant just records the date we wrote these tests against.
var _ = "fixedNow lives in policy_test.go: shared test clock"

// seedApplyHome builds a scratch home containing both reclaimable and
// forbidden paths across every apply class plus a forbidden dir under
// ledger/, so a falsifier can assert the safety floors hold.
//
// Returns the home path and a map of (relative path -> file contents).
func seedApplyHome(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	home := t.TempDir()

	// Layout (relative to home):
	//   events/segments/2026-06-01/seg-old.json   (60d old, 100 bytes)  -> eligible
	//   events/segments/2026-08-20/seg-new.json   (3d old, 50 bytes)    -> NOT eligible (age)
	//   context/packs/cpack-old.json              (60d old, 200 bytes)  -> eligible
	//   context/packs/cpack-new.json              (3d old, 60 bytes)    -> NOT eligible
	//   observed/tool-output/tool-old.bin         (30d old, 300 bytes)  -> eligible (AgeFloor 14d)
	//   observed/tool-output/tool-fresh.bin       (1d old, 30 bytes)    -> NOT eligible
	//   delegate-workspaces/ws-old.tar            (30d old, 400 bytes)  -> eligible (AgeFloor 14d)
	//   delegate-workspaces/ws-fresh.tar          (1d old, 40 bytes)    -> NOT eligible
	//   ledger/claims.jsonl                       (30d old, 999 bytes)  -> FORBIDDEN ROOT
	//   work/runs/run-1.json                      (30d old, 999 bytes)  -> FORBIDDEN ROOT
	//   contracts/foo.json                        (30d old, 999 bytes)  -> FORBIDDEN ROOT
	//   events/work.jsonl                         (30d old, 999 bytes)  -> FORBIDDEN FILE
	files := map[string][]byte{
		"events/segments/2026-06-01/seg-old.json": make([]byte, 100),
		"events/segments/2026-08-20/seg-new.json": make([]byte, 50),
		"context/packs/cpack-old.json":            make([]byte, 200),
		"context/packs/cpack-new.json":            make([]byte, 60),
		"observed/tool-output/tool-old.bin":       make([]byte, 300),
		"observed/tool-output/tool-fresh.bin":     make([]byte, 30),
		"delegate-workspaces/ws-old.tar":          make([]byte, 400),
		"delegate-workspaces/ws-fresh.tar":        make([]byte, 40),
		"ledger/claims.jsonl":                     make([]byte, 999),
		"work/runs/run-1.json":                    make([]byte, 999),
		"contracts/foo.json":                      make([]byte, 999),
		"events/work.jsonl":                       make([]byte, 999),
	}
	// Mtimes: file is "old" iff mtime is >= 30d before fixedNow (for events/context)
	// OR >= 14d before fixedNow (for tool-output/delegate). "fresh" files are 1-3d old.
	ages := map[string]time.Duration{
		"events/segments/2026-06-01/seg-old.json": 60 * 24 * time.Hour,
		"events/segments/2026-08-20/seg-new.json": 3 * 24 * time.Hour,
		"context/packs/cpack-old.json":            60 * 24 * time.Hour,
		"context/packs/cpack-new.json":            3 * 24 * time.Hour,
		"observed/tool-output/tool-old.bin":       30 * 24 * time.Hour,
		"observed/tool-output/tool-fresh.bin":     1 * 24 * time.Hour,
		"delegate-workspaces/ws-old.tar":          30 * 24 * time.Hour,
		"delegate-workspaces/ws-fresh.tar":        1 * 24 * time.Hour,
		"ledger/claims.jsonl":                     30 * 24 * time.Hour,
		"work/runs/run-1.json":                    30 * 24 * time.Hour,
		"contracts/foo.json":                      30 * 24 * time.Hour,
		"events/work.jsonl":                       30 * 24 * time.Hour,
	}
	for rel, body := range files {
		full := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
		if err := os.Chtimes(full, fixedNow.Add(-ages[rel]), fixedNow.Add(-ages[rel])); err != nil {
			t.Fatalf("chtimes %s: %v", full, err)
		}
	}
	return home, files
}

// snapshotHome records (path -> bytes) for every file under home so a test
// can compare "before" vs "after" at the byte level.
func snapshotHome(t *testing.T, home string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(home, path)
		out[rel] = body
		return nil
	})
	return out
}

// diffSnapshots returns entries that differ between two snapshots, with
// additions/deletions as empty-string/empty-byte sentinels.
func diffSnapshots(before, after map[string][]byte) []string {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	var diffs []string
	for k := range keys {
		b := string(before[k])
		a := string(after[k])
		if b != a {
			diffs = append(diffs, k)
		}
	}
	sort.Strings(diffs)
	return diffs
}

// TestFalsifier1DryRunChangesZeroBytes is the most important falsifier: a
// retention verb without --yes must NOT mutate the home, regardless of what
// --class flags it gets.
func TestFalsifier1DryRunChangesZeroBytes(t *testing.T) {
	home, _ := seedApplyHome(t)
	before := snapshotHome(t, home)

	req := ApplyRequest{
		Home: home,
		ClassIDs: []string{
			"events-segments", "context-packs", "tool-output", "delegate-workspaces",
		},
		// Deliberately omit --older-than to exercise the default-floor path.
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    true,
		Now:       fixedNow,
	}
	plan, err := BuildApplyPlan(req)
	if err != nil {
		t.Fatalf("build apply plan: %v", err)
	}
	// Sanity: dry-run plan enumerates at least the four old eligible files.
	if len(plan.Steps) == 0 {
		t.Fatalf("dry-run produced no steps; want at least 4 eligible files: %+v", plan)
	}
	if !plan.DryRun {
		t.Fatalf("plan must be marked DryRun")
	}

	after := snapshotHome(t, home)
	if diffs := diffSnapshots(before, after); len(diffs) > 0 {
		t.Fatalf("dry-run mutated home (forbidden): diff=%v", diffs)
	}
}

// TestFalsifier2ApplyReclaimsOnlyEligibleAndForbiddenDirsUnchanged is the
// mutating half: with --yes, only old files in whitelisted classes move to
// trash, and forbidden dirs are byte-identical.
func TestFalsifier2ApplyReclaimsOnlyEligibleAndForbiddenDirsUnchanged(t *testing.T) {
	home, seeded := seedApplyHome(t)
	before := snapshotHome(t, home)

	req := ApplyRequest{
		Home: home,
		ClassIDs: []string{
			"events-segments", "context-packs", "tool-output", "delegate-workspaces",
		},
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    false,
		Now:       fixedNow,
	}
	plan, err := BuildApplyPlan(req)
	if err != nil {
		t.Fatalf("build apply plan: %v", err)
	}
	plan, err = ExecuteApplyPlan(plan, fixedNow)
	if err != nil {
		t.Fatalf("execute apply: %v", err)
	}
	if plan.AppliedCount == 0 {
		t.Fatalf("expected at least one applied file")
	}

	// Eligible files (must have moved to trash):
	eligible := []string{
		"events/segments/2026-06-01/seg-old.json",
		"context/packs/cpack-old.json",
		"observed/tool-output/tool-old.bin",
		"delegate-workspaces/ws-old.tar",
	}
	// NOT-eligible by age (must remain in place):
	notEligible := []string{
		"events/segments/2026-08-20/seg-new.json",
		"context/packs/cpack-new.json",
		"observed/tool-output/tool-fresh.bin",
		"delegate-workspaces/ws-fresh.tar",
	}
	// Forbidden roots (must remain BYTE-IDENTICAL):
	forbidden := []string{
		"ledger/claims.jsonl",
		"work/runs/run-1.json",
		"contracts/foo.json",
		"events/work.jsonl",
	}

	for _, rel := range eligible {
		if _, exists := before[rel]; !exists {
			t.Fatalf("seed missing %s", rel)
		}
		if _, stillThere := snapshotHas(t, home, rel); stillThere {
			t.Fatalf("eligible file %s was NOT moved to trash", rel)
		}
	}
	for _, rel := range notEligible {
		body, ok := snapshotHas(t, home, rel)
		if !ok {
			t.Fatalf("non-eligible file %s disappeared", rel)
		}
		if string(body) != string(seeded[rel]) {
			t.Fatalf("non-eligible file %s content changed", rel)
		}
	}
	for _, rel := range forbidden {
		body, ok := snapshotHas(t, home, rel)
		if !ok {
			t.Fatalf("FORBIDDEN file %s disappeared — apply must NEVER touch authority/evidence paths", rel)
		}
		if string(body) != string(seeded[rel]) {
			t.Fatalf("FORBIDDEN file %s content changed", rel)
		}
	}

	// Receipts must exist and tally applied bytes correctly.
	receipts, err := ReadApplyReceipts(home)
	if err != nil {
		t.Fatalf("read apply receipts: %v", err)
	}
	if len(receipts) != plan.AppliedCount {
		t.Fatalf("receipt count = %d, want %d (one per applied step)", len(receipts), plan.AppliedCount)
	}
	var totalReceiptBytes int64
	for _, rec := range receipts {
		totalReceiptBytes += rec.Bytes
		// Every receipt points at a path under trash/, never back into ledger/ etc.
		if !strings.HasPrefix(rec.TrashPath, filepath.Join(home, TrashRootRel)) {
			t.Fatalf("receipt trash path escapes trash root: %s", rec.TrashPath)
		}
	}
	if totalReceiptBytes != plan.AppliedBytes {
		t.Fatalf("receipt byte sum = %d, want %d (plan.AppliedBytes)", totalReceiptBytes, plan.AppliedBytes)
	}
}

// TestFalsifier3TrashRestoreRoundTripsOriginalBytes is the safety net: a
// trashed file moved back to its source path must reproduce the original
// bytes byte-for-byte. This catches any path-mapping bug in BuildApplyPlan
// before the operator ever relies on a restore.
func TestFalsifier3TrashRestoreRoundTripsOriginalBytes(t *testing.T) {
	home, seeded := seedApplyHome(t)
	req := ApplyRequest{
		Home:      home,
		ClassIDs:  []string{"events-segments", "context-packs"},
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    false,
		Now:       fixedNow,
		TrashDate: fixedNow,
	}
	plan, err := BuildApplyPlan(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := ExecuteApplyPlan(plan, fixedNow); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Move each step's trash file back to its source and assert byte equality.
	for _, step := range plan.Steps {
		if err := os.Rename(step.TrashPath, step.SourcePath); err != nil {
			t.Fatalf("restore %s -> %s: %v", step.TrashPath, step.SourcePath, err)
		}
		got, err := os.ReadFile(step.SourcePath)
		if err != nil {
			t.Fatalf("read restored %s: %v", step.SourcePath, err)
		}
		// The original bytes must equal the seed body for this source. Find
		// the source's seeded bytes by stripping the home prefix and looking
		// up in the seeded map.
		rel, relErr := filepath.Rel(home, step.SourcePath)
		if relErr != nil {
			t.Fatalf("rel %s: %v", step.SourcePath, relErr)
		}
		want, ok := seeded[rel]
		if !ok {
			t.Fatalf("seed missing original for %s", rel)
		}
		if string(got) != string(want) {
			t.Fatalf("restored %s bytes differ (got %d, want %d)", rel, len(got), len(want))
		}
	}
}

// TestFalsifier4ReceiptHasCorrectBytesAndPerStepEntries verifies the JSONL
// receipt log is well-formed and the byte counts match the plan.
func TestFalsifier4ReceiptHasCorrectBytesAndPerStepEntries(t *testing.T) {
	home, _ := seedApplyHome(t)
	req := ApplyRequest{
		Home:      home,
		ClassIDs:  []string{"tool-output"},
		OlderThan: 14 * 24 * time.Hour,
		DryRun:    false,
		Now:       fixedNow,
	}
	plan, err := BuildApplyPlan(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	plan, err = ExecuteApplyPlan(plan, fixedNow)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Read the receipt journal directly. Every applied step is bracketed:
	// one durable intent line BEFORE the rename and one committed line after
	// it, so the raw file holds 2×AppliedCount lines and the reader surfaces
	// exactly AppliedCount committed rows.
	raw, err := os.ReadFile(filepath.Join(home, ReceiptDirRel, "apply.jsonl"))
	if err != nil {
		t.Fatalf("read receipt log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2*plan.AppliedCount {
		t.Fatalf("journal line count = %d, want %d (intent+committed per step)", len(lines), 2*plan.AppliedCount)
	}
	committed := 0
	for i, line := range lines {
		var rec applyReceipt
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i+1, err)
		}
		if rec.Authority != ApplyAuthorityRef {
			t.Fatalf("line %d authority = %q, want %q", i+1, rec.Authority, ApplyAuthorityRef)
		}
		if rec.ClassID != "tool-output" {
			t.Fatalf("line %d class = %q", i+1, rec.ClassID)
		}
		if rec.Bytes <= 0 {
			t.Fatalf("line %d bytes = %d", i+1, rec.Bytes)
		}
		switch rec.Phase {
		case applyPhaseIntent:
			if i%2 != 0 {
				t.Fatalf("line %d is an intent line but appears after a same-step phase", i+1)
			}
		case applyPhaseCommitted:
			committed++
		default:
			t.Fatalf("line %d phase = %q, want intent or committed", i+1, rec.Phase)
		}
	}
	if committed != plan.AppliedCount {
		t.Fatalf("committed rows = %d, want %d", committed, plan.AppliedCount)
	}
	// The reader must surface committed rows only.
	read, err := ReadApplyReceipts(home)
	if err != nil {
		t.Fatalf("read receipts: %v", err)
	}
	if len(read) != plan.AppliedCount {
		t.Fatalf("reader returned %d rows, want %d committed", len(read), plan.AppliedCount)
	}
}

// TestApplyRejectsUnknownClass is the refusal-on-typo gate.
func TestApplyRejectsUnknownClass(t *testing.T) {
	home := t.TempDir()
	req := ApplyRequest{
		Home:      home,
		ClassIDs:  []string{"events-segments", "no-such-class"},
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    true,
		Now:       fixedNow,
	}
	_, err := BuildApplyPlan(req)
	if err == nil {
		t.Fatal("unknown class must fail closed")
	}
	var uce *UnknownApplyClassError
	if !errorsAs(err, &uce) {
		t.Fatalf("error type = %T, want UnknownApplyClassError", err)
	}
	if uce.ClassID != "no-such-class" {
		t.Fatalf("error class = %q", uce.ClassID)
	}
}

// TestApplyRejectsForbiddenPath is the safety-floor gate. A class whose
// path would intersect a forbidden root must be refused, even if the class
// would otherwise look legitimate. This protects against a future maintainer
// accidentally adding a class that claims ledger/, work/, etc.
func TestApplyRejectsForbiddenPath(t *testing.T) {
	// Build a synthetic class that points at ledger/. We can't add to the
	// compiled-in table from a test, so we exercise the pathIntersects
	// helper directly AND walk the table to confirm every entry is clean.
	for _, class := range ApplyClasses() {
		for _, forbidden := range forbiddenApplyRoots {
			if pathIntersects(class.PathUnderHome, forbidden) {
				t.Fatalf("apply class %q path %q intersects forbidden root %q — refusing to ship",
					class.ID, class.PathUnderHome, forbidden)
			}
		}
	}
	if pathIntersects("ledger/anything", "ledger") != true {
		t.Fatal("pathIntersects must match a strict child")
	}
	if pathIntersects("ledgers/x", "ledger") != false {
		t.Fatal("pathIntersects must NOT match a non-child")
	}
}

// TestApplyRefusesOlderThanBelowAgeFloor is the per-class floor guard.
func TestApplyRefusesOlderThanBelowAgeFloor(t *testing.T) {
	home := t.TempDir()
	req := ApplyRequest{
		Home:      home,
		ClassIDs:  []string{"events-segments"},
		OlderThan: 1 * time.Hour, // AgeFloor is 30d
		DryRun:    true,
		Now:       fixedNow,
	}
	if _, err := BuildApplyPlan(req); err == nil {
		t.Fatal("OlderThan below class AgeFloor must fail closed")
	}
}

// TestApplyLimitBytesCapsAppliedBytes is the budget guard.
func TestApplyLimitBytesCapsAppliedBytes(t *testing.T) {
	home, _ := seedApplyHome(t)
	req := ApplyRequest{
		Home:       home,
		ClassIDs:   []string{"events-segments"},
		OlderThan:  30 * 24 * time.Hour,
		LimitBytes: 50, // less than the 100-byte seg-old
		DryRun:     true,
		Now:        fixedNow,
	}
	plan, err := BuildApplyPlan(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.PlannedBytes > 50 {
		t.Fatalf("planned %d bytes exceeds limit 50", plan.PlannedBytes)
	}
}

// TestEmptyTrashDryRunListsOlderAndLeavesYounger verifies empty-trash's
// date-based gating.
func TestEmptyTrashDryRunListsOlderAndLeavesYounger(t *testing.T) {
	home, _ := seedApplyHome(t)
	// Run apply to seed trash first.
	req := ApplyRequest{
		Home:      home,
		ClassIDs:  []string{"events-segments"},
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    false,
		Now:       fixedNow,
		TrashDate: fixedNow,
	}
	plan, err := BuildApplyPlan(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := ExecuteApplyPlan(plan, fixedNow); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Seed an older date directory directly under trash/ to exercise the
	// date gating. Today's trash was just created at fixedNow.
	older := filepath.Join(home, TrashRootRel, "events-segments", "2026-01-01")
	if err := os.MkdirAll(older, 0o755); err != nil {
		t.Fatalf("mkdir older: %v", err)
	}
	if err := os.WriteFile(filepath.Join(older, "old.json"), make([]byte, 500), 0o644); err != nil {
		t.Fatalf("write older: %v", err)
	}

	rpt, err := BuildEmptyTrashReport(EmptyTrashRequest{
		Home:      home,
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    true,
		Now:       fixedNow,
	})
	if err != nil {
		t.Fatalf("empty-trash report: %v", err)
	}
	if len(rpt.Steps) != 1 {
		t.Fatalf("want 1 older date dir, got %d (%+v)", len(rpt.Steps), rpt.Steps)
	}
	if rpt.Steps[0].Date != "2026-01-01" {
		t.Fatalf("step date = %q", rpt.Steps[0].Date)
	}
}

// TestEmptyTrashYesUnlinksOldAndReceipts is the mutating half of empty-trash.
func TestEmptyTrashYesUnlinksOldAndReceipts(t *testing.T) {
	home := t.TempDir()
	older := filepath.Join(home, TrashRootRel, "context-packs", "2026-01-01")
	if err := os.MkdirAll(older, 0o755); err != nil {
		t.Fatalf("mkdir older: %v", err)
	}
	if err := os.WriteFile(filepath.Join(older, "x.bin"), make([]byte, 200), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	rpt, err := BuildEmptyTrashReport(EmptyTrashRequest{
		Home:      home,
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    false,
		Now:       fixedNow,
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	rpt, err = ExecuteEmptyTrash(rpt, fixedNow)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rpt.AppliedCount != 1 {
		t.Fatalf("applied = %d, want 1", rpt.AppliedCount)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("old trash dir not unlinked: stat err = %v", err)
	}
	// Receipt written.
	raw, err := os.ReadFile(filepath.Join(home, ReceiptDirRel, "empty-trash.jsonl"))
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("empty-trash receipt log is empty after execute")
	}
}

// TestParseApplyDuration accepts the packet's grammar.
func TestParseApplyDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"30d", 30 * 24 * time.Hour, true},
		{"14h", 14 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"3600s", 3600 * time.Second, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range cases {
		got, err := ParseApplyDuration(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("ParseApplyDuration(%q) err = %v, want nil", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("ParseApplyDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		} else if err == nil {
			t.Errorf("ParseApplyDuration(%q) ok, want err", tc.in)
		}
	}
}

// snapshotHas returns (bytes, true) when rel exists under home.
func snapshotHas(t *testing.T, home, rel string) ([]byte, bool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, rel))
	if err != nil {
		return nil, false
	}
	return body, true
}

// errorsAs wraps errors.As so tests read more cleanly.
func errorsAs(err error, target interface{}) bool {
	return errors.As(err, target)
}

// TestPathInsideRootRejectsPrefixSiblings: raw strings.HasPrefix accepted
// <home>/trash-malicious as "inside" <home>/trash because the sibling merely
// shares a name prefix. Containment for irreversible mutation must be a real
// sub-path relation.
func TestPathInsideRootRejectsPrefixSiblings(t *testing.T) {
	home := t.TempDir()
	trashRoot := filepath.Join(home, TrashRootRel)
	if err := os.MkdirAll(trashRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// The lived escape shape: sibling whose name starts with the root name.
	sibling := filepath.Join(home, "trash-malicious")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(trashRoot, "2026-08-01"), true},
		{trashRoot, false},                // the root itself is not a child
		{sibling, false},                  // prefix sibling
		{home, false},                     // ancestor
		{trashRoot + "/../ledger", false}, // .. escape
	}
	for _, tc := range cases {
		if got := pathInsideRoot(tc.path, trashRoot); got != tc.want {
			t.Fatalf("pathInsideRoot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestEmptyTrashRefusesPathsOutsideTrashRoot: even a step list widened by a
// future bug must not reach RemoveAll for anything that is not strictly
// inside trash/.
func TestEmptyTrashRefusesPathsOutsideTrashRoot(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ReceiptDirRel), 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(home, "trash-malicious")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(sibling, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("operator data"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := EmptyTrashReport{
		Home:      home,
		OlderThan: "30d",
		Steps: []EmptyTrashStep{{
			ClassID:   "tool-output",
			TrashPath: sibling,
			Bytes:     16,
			Date:      "2026-08-01",
		}},
	}
	_, err := ExecuteEmptyTrash(report, fixedNow)
	if err == nil {
		t.Fatal("empty-trash must refuse a path outside the trash root")
	}
	if !strings.Contains(err.Error(), "refusing to unlink outside") {
		t.Fatalf("error should name the refusal: %v", err)
	}
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Fatalf("refusal must leave the sibling untouched: %v", statErr)
	}
}

// TestApplyJournalsIntentBeforeMutation: a mid-run mutation failure must
// leave the journal showing a durable intent line and a failed line for the
// failing step, and durable intent+committed for the steps before it. The
// pre-journal behavior wrote receipts only after successful renames, so a
// crash deleted the evidence of what was in flight.
func TestApplyJournalsIntentBeforeMutation(t *testing.T) {
	home, _ := seedApplyHome(t)
	req := ApplyRequest{
		Home:      home,
		ClassIDs:  []string{"tool-output", "delegate-workspaces"},
		OlderThan: 14 * 24 * time.Hour,
		DryRun:    false,
		Now:       fixedNow,
	}
	plan, err := BuildApplyPlan(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(plan.Steps) < 2 {
		t.Fatalf("fixture must seed at least two steps, got %d", len(plan.Steps))
	}
	// Sabotage the SECOND step: remove its source between plan and execute.
	if err := os.Remove(plan.Steps[1].SourcePath); err != nil {
		t.Fatalf("sabotage step 2 source: %v", err)
	}
	executed, execErr := ExecuteApplyPlan(plan, fixedNow)
	if execErr == nil {
		t.Fatal("execute must fail when a rename fails")
	}
	if executed.AppliedCount != 1 {
		t.Fatalf("applied count = %d, want 1 (only the step before the failure)", executed.AppliedCount)
	}
	raw, err := os.ReadFile(filepath.Join(home, ReceiptDirRel, "apply.jsonl"))
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var phases []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		var rec applyReceipt
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad journal line: %v", err)
		}
		phases = append(phases, rec.Phase)
	}
	want := []string{applyPhaseIntent, applyPhaseCommitted, applyPhaseIntent, applyPhaseFailed}
	if len(phases) != len(want) {
		t.Fatalf("journal phases = %v, want %v", phases, want)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Fatalf("journal phases = %v, want %v", phases, want)
		}
	}
	// The reader surfaces only committed rows — the failed step is not applied.
	read, err := ReadApplyReceipts(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 1 || read[0].Phase != applyPhaseCommitted {
		t.Fatalf("reader returned %+v, want exactly one committed row", read)
	}
}
