package mem

import "testing"

func TestCaptureSameFactDifferentProvenancePersistsDistinctCandidates(t *testing.T) {
	s := newStore(t)

	first := cand("project:api", "backend_language", "go", "2026-07-23T00:00:00Z")
	first.Confidence = 0.8
	first.ProvenanceLane = "lane:first"
	first.SourceTraceRef = "trace:first"
	first.SnippetHash = "snippet:first"

	second := first
	second.Confidence = 0.95
	second.ProvenanceLane = "lane:second"
	second.SourceTraceRef = "trace:second"
	second.SnippetHash = "snippet:second"

	captured, err := s.Capture([]Candidate{first, second}, t0)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("captured %d candidates, want 2", len(captured))
	}
	if captured[0].ID == captured[1].ID {
		t.Fatalf("different provenance produced the same candidate ID %q", captured[0].ID)
	}

	persisted, err := s.CandidatesByID([]string{captured[0].ID, captured[1].ID})
	if err != nil {
		t.Fatalf("read captured candidates: %v", err)
	}
	if len(persisted) != 2 {
		t.Fatalf("persisted %d candidates, want 2", len(persisted))
	}
	if persisted[captured[0].ID].SourceTraceRef != "trace:first" {
		t.Fatalf("first persisted source trace = %q, want trace:first", persisted[captured[0].ID].SourceTraceRef)
	}
	if persisted[captured[1].ID].SourceTraceRef != "trace:second" {
		t.Fatalf("second persisted source trace = %q, want trace:second", persisted[captured[1].ID].SourceTraceRef)
	}
}
