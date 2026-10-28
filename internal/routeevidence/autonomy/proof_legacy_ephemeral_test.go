package autonomy_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"
)

// A legacy work_live_ record persisted in a terminal state is historical debris:
// it can never validate as a durable envelope, so eligibility compilation must
// exclude it visibly instead of failing closed over the entire class. A
// non-legacy invalid durable record must still fail the compilation.
func TestCompileExcludesTerminalLegacyEphemeralWorkRecords(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "genuine", proofNow.Add(-48*time.Hour), sampleOptions{})

	writeRawWork(t, home, "work_live_deadbeef0001", map[string]any{
		"schema_version":    "compound.work.v0.1",
		"id":                "work_live_deadbeef0001",
		"created_at":        proofNow.Add(-240 * time.Hour).UTC().Format(time.RFC3339),
		"state":             "completed",
		"owner":             "agent:legacy-hook",
		"success_predicate": "unattributed live-hook mutation (legacy)",
	})

	artifact := compile(t, home, plan, 1)
	if artifact.ExcludedUnverifiableCount != 1 {
		t.Fatalf("legacy ephemeral record not visibly excluded: %+v", artifact)
	}
	for _, sample := range artifact.Samples {
		if strings.HasPrefix(sample.WorkID, "work_live_") {
			t.Fatalf("legacy ephemeral record leaked into samples: %+v", sample)
		}
	}

	writeRawWork(t, home, "work_badd00d0000001", map[string]any{
		"schema_version": "compound.work.v0.1",
		"id":             "work_badd00d0000001",
		"created_at":     proofNow.Add(-240 * time.Hour).UTC().Format(time.RFC3339),
		"state":          "completed",
	})
	if _, err := compileArtifact(home, plan, 1); err == nil {
		t.Fatal("non-legacy invalid durable record must still fail compilation closed")
	}
}

func writeRawWork(t *testing.T, home, id string, doc map[string]any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "work", "work", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Custody strictness is class-scoped: an OFF-class recent-terminal record with a
// dirty custody trail is excluded visibly; the SAME dirt on a class-relevant
// record still fails the compilation closed.
func TestCompileExcludesOffClassDirtyTerminalHistory(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "class-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	foreignDup := seedSample(t, home, plan, "foreign-dup", proofNow.Add(-24*time.Hour), sampleOptions{autonomyClass: "other_class"})
	duplicateTerminalWorkEvent(t, home, foreignDup.WorkID)

	artifact := compile(t, home, plan, 1)
	if artifact.ExcludedUnverifiableCount != 1 {
		t.Fatalf("off-class dirty terminal history not visibly excluded: %+v", artifact)
	}
	for _, sample := range artifact.Samples {
		if sample.WorkID == foreignDup.WorkID {
			t.Fatalf("off-class dirty record entered cohort: %+v", artifact.Samples)
		}
	}
}

func TestCompileFailsClosedOnClassDirtyTerminalHistory(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "class-clean-2", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "class-dup", proofNow.Add(-24*time.Hour), sampleOptions{})
	duplicateTerminalWorkEvent(t, home, victim.WorkID)

	if _, err := compileArtifact(home, plan, 1); err == nil || !strings.Contains(err.Error(), "multiple terminal events") {
		t.Fatalf("class-relevant dirty terminal history did not fail closed: %v", err)
	}
}

func duplicateTerminalWorkEvent(t *testing.T, home, workID string) {
	t.Helper()
	path := filepath.Join(home, "events", "work.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dup := ""
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event struct {
			WorkID string `json:"work_id"`
			To     string `json:"to"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.WorkID == workID && (event.To == "completed" || event.To == "failed" || event.To == "cancelled") {
			dup = line
		}
	}
	if dup == "" {
		t.Fatalf("no terminal event found for %s", workID)
	}
	if err := os.WriteFile(path, append(raw, []byte(dup+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Same-state rows (from == to) are annotations on already-closed work -- delegate
// steer and exit notes write them -- not closes. Counting them as terminal events
// made a steered work look like it had several conflicting closes and no custody
// hash, failing the whole compilation closed.
func TestCompileIgnoresSameStateAnnotationsOnClosedWork(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "annotated-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	annotated := seedSample(t, home, plan, "annotated", proofNow.Add(-24*time.Hour), sampleOptions{})
	appendWorkEvent(t, home, map[string]any{
		"ts":      proofNow.Add(-1 * time.Hour).Format(time.RFC3339),
		"work_id": annotated.WorkID,
		"from":    "completed",
		"to":      "completed",
		"by":      "claude-fable",
		"note":    "delegate steered session",
	})

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "eligible" {
		t.Fatalf("same-state annotation blocked eligibility: %+v", artifact)
	}
	found := false
	for _, sample := range artifact.Samples {
		if sample.WorkID == annotated.WorkID {
			found = true
		}
	}
	if !found {
		t.Fatalf("annotated work dropped out of the cohort: %+v", artifact.Samples)
	}
}
