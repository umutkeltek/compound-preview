package capgraph

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"compound/internal/eventstore"
)

func canonEventFiles(t *testing.T, homePath string) []string {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Scan(eventstore.ScanOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name)
	}
	return out
}

func assertCanonEventBytes(t *testing.T, homePath string, transition any, id string) {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	got := record.Bytes
	want, err := json.Marshal(transition)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if !bytes.Equal(got, want) {
		t.Fatalf("canon transition bytes changed\ngot:  %q\nwant: %q", got, want)
	}
}

// F2 (collision): the same canon file drifting A->B then B->C under the same cause must mint two
// DISTINCT transitions whose event files both survive, and two distinct source-authority revisions.
// The two drifts share node refs, cause class, and changed paths; only their evidence refs differ
// (candidate ids carry the live file hash). Before content identity lands in the id, the two mints
// share a tr_canon id and the second os.WriteFile clobbers the first event -> RED.
func TestRecordCanonEvolutionDistinctEvidenceMintsDistinctTransitions(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

	base := CanonEvolutionInput{
		NodeRefs:     []string{"harness:example"},
		DeltaCause:   "accept",
		CausedByRefs: []string{"work_canon_test"},
		NextConsumer: "capgraph",
		ChangedRefs:  []string{"settings.json"},
	}
	// Drift A->B: candidate evidence derived from the B live hash.
	inAB := base
	inAB.EvidenceRefs = []string{"src_cand_hashB", "source:settings.json#hashB"}
	// Drift B->C: same file, same cause, distinct content -> distinct evidence.
	inBC := base
	inBC.EvidenceRefs = []string{"src_cand_hashC", "source:settings.json#hashC"}

	trAB, repAB, err := RecordCanonEvolution(homePath, inAB, now)
	if err != nil {
		t.Fatalf("mint A->B: %v", err)
	}
	trBC, repBC, err := RecordCanonEvolution(homePath, inBC, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("mint B->C: %v", err)
	}

	if trAB.ID == "" || trBC.ID == "" {
		t.Fatalf("both mints must produce a transition id: %q %q", trAB.ID, trBC.ID)
	}
	if trAB.ID == trBC.ID {
		t.Fatalf("distinct-content drifts collided on transition id %q — same-file/same-cause drifts must not share identity", trAB.ID)
	}
	if repAB.TransitionID != trAB.ID || repBC.TransitionID != trBC.ID {
		t.Fatalf("report ids must match minted transitions: %q/%q vs %q/%q", repAB.TransitionID, trAB.ID, repBC.TransitionID, trBC.ID)
	}

	events := canonEventFiles(t, homePath)
	if len(events) != 2 {
		t.Fatalf("both transition event files must survive, got %d: %v (second mint overwrote the first)", len(events), events)
	}
	assertCanonEventBytes(t, homePath, &trAB, trAB.ID)
	assertCanonEventBytes(t, homePath, &trBC, trBC.ID)

	ensured := []string{"harness:example"}
	revAB := canonSourceRevision(inAB, ensured)
	revBC := canonSourceRevision(inBC, ensured)
	if revAB == revBC {
		t.Fatalf("distinct-content drifts produced identical source-authority revision %q", revAB)
	}

	// The work_canon id must also carry content identity so re-derived work ids stay distinct.
	if trAB.IDs.WorkID == "" || trBC.IDs.WorkID == "" {
		t.Fatalf("both mints must carry a work id: %q %q", trAB.IDs.WorkID, trBC.IDs.WorkID)
	}
	if trAB.IDs.WorkID == trBC.IDs.WorkID {
		t.Fatalf("distinct-content drifts collided on work id %q", trAB.IDs.WorkID)
	}
}

// A byte-identical re-run of the same failed/succeeded mint must reproduce the SAME transition id
// (deterministic replay is a feature — no timestamps in identity). This guards D3 against the naive
// "just add time" fix that would defeat re-detectable transaction safety.
func TestRecordCanonEvolutionIdenticalInputIsDeterministic(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	in := CanonEvolutionInput{
		NodeRefs:     []string{"harness:example"},
		DeltaCause:   "repair",
		CausedByRefs: []string{"work_canon_test"},
		ChangedRefs:  []string{"settings.json"},
		EvidenceRefs: []string{"src_cand_hashB"},
	}

	home1 := t.TempDir()
	tr1, _, err := RecordCanonEvolution(home1, in, now)
	if err != nil {
		t.Fatal(err)
	}
	home2 := t.TempDir()
	tr2, _, err := RecordCanonEvolution(home2, in, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if tr1.ID != tr2.ID {
		t.Fatalf("identical input must mint a deterministic transition id, got %q vs %q", tr1.ID, tr2.ID)
	}
	if tr1.IDs.WorkID != tr2.IDs.WorkID {
		t.Fatalf("identical input must mint a deterministic work id, got %q vs %q", tr1.IDs.WorkID, tr2.IDs.WorkID)
	}
}
