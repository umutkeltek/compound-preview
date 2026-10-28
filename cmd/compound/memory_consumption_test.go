package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/home"
	"compound/internal/mem"
)

// TestCmdMemoryConsumptionEndToEndViaRealMintPath exercises the ONE real production mint site
// (compound source canon-observe / capgraph.RecordCanonEvolution) end to end through the CLI
// door: mint one Cause-carrying transition citing an unknown (never-adjudicated) ref, then prove
// `compound memory consumption --json` reports it bucketed as observed_raw with a non-zero
// thesis ratio.
func TestCmdMemoryConsumptionEndToEndViaRealMintPath(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}

	if _, _, err := capgraph.RecordCanonEvolution(homePath, capgraph.CanonEvolutionInput{
		NodeRefs:     []string{"harness:consumption-fixture"},
		DeltaCause:   "accept",
		CausedByRefs: []string{"cand_never_adjudicated_consumption_fixture"},
		NextConsumer: "capgraph",
		ChangedRefs:  []string{"fixture.json"},
	}, now); err != nil {
		t.Fatalf("mint canon evolution: %v", err)
	}

	rep, err := mem.BuildCauseConsumptionReport(homePath, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rep.TransitionsWithCause != 1 {
		t.Fatalf("transitions_with_cause = %d, want 1: %+v", rep.TransitionsWithCause, rep)
	}
	if rep.RefTierCounts[mem.CauseConsumptionTierObservedRaw] != 1 {
		t.Fatalf("observed_raw bucket = %d, want 1 (never-adjudicated ref): %+v", rep.RefTierCounts[mem.CauseConsumptionTierObservedRaw], rep.RefTierCounts)
	}
	if rep.ThesisRatio != 1.0 {
		t.Fatalf("thesis ratio = %v, want 1.0 (100%% unpromoted so far)", rep.ThesisRatio)
	}

	var stdout, stderr bytes.Buffer
	if err := mem.RunCauseConsumptionCLI(homePath, nil, &stdout, &stderr, func() time.Time {
		return now.Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "observed_raw") || !strings.Contains(out, "motion_from_unpromoted_ratio") {
		t.Fatalf("human summary missing bucket/ratio lines:\n%s", out)
	}
}

// TestCmdMemoryConsumptionRecordWritesOnlyUnderStateRuns is F3 at the CLI layer: --record's
// artifact must be the only new file, under state/runs/cause-consumption/ only.
func TestCmdMemoryConsumptionRecordWritesOnlyUnderStateRuns(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}

	before := snapshotDirFiles(t, homePath)
	var stdout, stderr bytes.Buffer
	if err := mem.RunCauseConsumptionCLI(homePath, []string{"--record"}, &stdout, &stderr, func() time.Time {
		return now
	}); err != nil {
		t.Fatal(err)
	}
	after := snapshotDirFiles(t, homePath)

	var added []string
	for f := range after {
		if !before[f] {
			added = append(added, f)
		}
	}
	if len(added) != 1 {
		t.Fatalf("unexpected new files: %v", added)
	}
	rel, err := filepath.Rel(homePath, added[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rel, filepath.Join("state", "runs", "cause-consumption")) {
		t.Fatalf("--record artifact landed outside state/runs/cause-consumption: %s", rel)
	}
}

// TestCmdMemoryConsumptionJSONZeroIsHonestAndExitsZero is F4 at the CLI layer: an empty home
// (zero Cause-transitions) exits 0 and prints an honest zero report, no --record artifact.
func TestCmdMemoryConsumptionJSONZeroIsHonestAndExitsZero(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}

	before := snapshotDirFiles(t, homePath)
	var stdout, stderr bytes.Buffer
	if err := mem.RunCauseConsumptionCLI(homePath, []string{"--json"}, &stdout, &stderr, func() time.Time {
		return now
	}); err != nil {
		t.Fatal(err)
	}
	out := stdout.String()
	after := snapshotDirFiles(t, homePath)

	if !strings.Contains(out, `"transitions_scanned": 0`) {
		t.Fatalf("json output missing zero transitions_scanned:\n%s", out)
	}
	if len(after) != len(before) {
		t.Fatalf("no --record flag must never write a new file; before=%d after=%d", len(before), len(after))
	}
}

func TestCmdMemoryConsumptionWithoutRecordBypassesMutableStoreOpen(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	for _, rel := range []string{"events", candidates.InboxDir(candidates.Memory)} {
		if err := os.RemoveAll(filepath.Join(homePath, rel)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COMPOUND_HOME", homePath)
	before := snapshotMemoryConsumptionTree(t, homePath)

	out := captureStdout(t, func() { cmdMemory([]string{"consumption", "--json"}) })
	if !strings.Contains(out, `"transitions_scanned": 0`) {
		t.Fatalf("json output missing zero report:\n%s", out)
	}
	after := snapshotMemoryConsumptionTree(t, homePath)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("non-record CLI mutated home\nbefore=%v\nafter=%v", before, after)
	}
}

func snapshotDirFiles(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		out[path] = true
		return nil
	})
	return out
}

func snapshotMemoryConsumptionTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			out[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = "file:" + string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
