package mem

import "testing"

func TestEngineCompareMatchesProtocolSidecar(t *testing.T) {
	s := storeWith(t, "project:api", "backend_language", "node", "go")
	eng := NewProtocolEngine(&fakeTransport{embedder: "reference:test:0:0", failAfter: -1}, "reference:test:0:0")

	report, err := s.EngineCompare("backend go", 10, eng)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Match || !report.LedgerUnchanged {
		t.Fatalf("engine compare should match: %+v", report)
	}
	if !report.Acceptable || !report.ReferenceCovered || report.CompareMode != "exact" {
		t.Fatalf("exact match should be acceptable and covered: %+v", report)
	}
	if report.Reference.Engine.Name != "lexical" || report.Candidate.Engine.Name != "fake" {
		t.Fatalf("engine names = ref %q candidate %q", report.Reference.Engine.Name, report.Candidate.Engine.Name)
	}
	if len(report.ReferenceClaimIDs) != 1 || len(report.CandidateClaimIDs) != 1 || report.ReferenceClaimIDs[0] != report.CandidateClaimIDs[0] {
		t.Fatalf("claim ids ref=%v candidate=%v", report.ReferenceClaimIDs, report.CandidateClaimIDs)
	}
}

func TestEngineCompareReportsDivergentCandidate(t *testing.T) {
	s := storeWith(t, "project:api", "backend_language", "node", "go")
	report, err := s.EngineCompare("backend go", 10, emptyEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Match {
		t.Fatalf("divergent engine should not match: %+v", report)
	}
	if report.Acceptable || report.ReferenceCovered {
		t.Fatalf("missing reference hit should not be acceptable/covered: %+v", report)
	}
	if len(report.MissingClaimIDs) != 1 || len(report.ExtraClaimIDs) != 0 {
		t.Fatalf("missing/extra = %v/%v", report.MissingClaimIDs, report.ExtraClaimIDs)
	}
	if !report.LedgerUnchanged {
		t.Fatalf("ledger should remain unchanged: %+v", report)
	}
}

func TestEngineCompareAllowsCoveredExpansion(t *testing.T) {
	s := newStore(t)
	c1, _ := s.Capture([]Candidate{cand("project:api", "backend_language", "go", "")}, t0)
	r1, err := promoteForTest(t, s, c1[0].ID, "", t0)
	if err != nil || r1.Claim == nil {
		t.Fatalf("promote backend claim: %v", err)
	}
	c2, _ := s.Capture([]Candidate{cand("project:api", "cache", "redis", "")}, t0)
	r2, err := promoteForTest(t, s, c2[0].ID, "", t0)
	if err != nil || r2.Claim == nil {
		t.Fatalf("promote cache claim: %v", err)
	}
	eng := staticHitsEngine{hits: []Hit{
		{ClaimID: r1.Claim.ID, Score: 2},
		{ClaimID: r2.Claim.ID, Score: 1},
	}}

	exact, err := s.EngineCompare("backend go", 10, eng)
	if err != nil {
		t.Fatal(err)
	}
	if exact.Acceptable || exact.Match {
		t.Fatalf("exact mode should reject expansion extras: %+v", exact)
	}
	if !exact.ReferenceCovered || len(exact.ExtraClaimIDs) != 1 {
		t.Fatalf("exact report should still expose covered expansion: %+v", exact)
	}

	expanded, err := s.EngineCompareWithOptions("backend go", 10, eng, EngineCompareOptions{AllowExpansion: true})
	if err != nil {
		t.Fatal(err)
	}
	if expanded.Match {
		t.Fatalf("allow-expansion mode should not claim exact match: %+v", expanded)
	}
	if !expanded.Acceptable || !expanded.ReferenceCovered || !expanded.LedgerUnchanged {
		t.Fatalf("covered expansion should be acceptable: %+v", expanded)
	}
	if expanded.CompareMode != "allow_expansion" {
		t.Fatalf("compare mode = %q", expanded.CompareMode)
	}
	if len(expanded.ExpansionClaimIDs) != 1 || expanded.ExpansionClaimIDs[0] != r2.Claim.ID {
		t.Fatalf("expansion ids = %v, want %s", expanded.ExpansionClaimIDs, r2.Claim.ID)
	}
}

type emptyEngine struct{}

func (emptyEngine) Capabilities() EngineCapabilities {
	return EngineCapabilities{Name: "empty", Breadth: true}
}

func (emptyEngine) Index([]Claim, int64) error {
	return nil
}

func (emptyEngine) Query(string, string, []string, int) ([]Hit, error) {
	return nil, nil
}

type staticHitsEngine struct {
	hits []Hit
}

func (staticHitsEngine) Capabilities() EngineCapabilities {
	return EngineCapabilities{Name: "static-expansion", Breadth: true}
}

func (staticHitsEngine) Index([]Claim, int64) error {
	return nil
}

func (e staticHitsEngine) Query(_ string, gate string, allowlist []string, k int) ([]Hit, error) {
	allow := map[string]bool{}
	for _, id := range allowlist {
		allow[id] = true
	}
	out := make([]Hit, 0, len(e.hits))
	for _, h := range e.hits {
		if gate != GateAll && !allow[h.ClaimID] {
			continue
		}
		out = append(out, h)
	}
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, nil
}
