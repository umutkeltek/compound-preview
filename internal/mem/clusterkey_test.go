package mem

import (
	"fmt"
	"os"
	"testing"
)

func TestClusterKeyIsPure(t *testing.T) {
	got := ClusterKey(" Project:API ", " Languages ", " Preference ")
	want := "project:api\x1flanguages\x1fpreference"
	if got != want {
		t.Fatalf("ClusterKey = %q, want %q", got, want)
	}
}

func TestStoreClusterKeyResolvesEntityWhenUnambiguous(t *testing.T) {
	s := newStore(t)
	if _, err := s.RegisterEntity(Entity{Slug: "project:api", Kind: "project", Aliases: []string{"API"}}, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClusterKey("api", "language", "decision")
	if err != nil {
		t.Fatal(err)
	}
	want := "project:api\x1flanguage\x1fdecision"
	if got != want {
		t.Fatalf("store ClusterKey = %q, want %q", got, want)
	}
}

func TestStoreClusterKeyDoesNotGuessAmbiguousEntity(t *testing.T) {
	s := newStore(t)
	if _, err := s.RegisterEntity(Entity{Slug: "project:api", Kind: "project", Aliases: []string{"shared"}}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterEntity(Entity{Slug: "project:web", Kind: "project", Aliases: []string{"shared"}}, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClusterKey(" Shared ", "language", "decision")
	if err != nil {
		t.Fatal(err)
	}
	want := "shared\x1flanguage\x1fdecision"
	if got != want {
		t.Fatalf("ambiguous entity ClusterKey = %q, want raw fallback %q", got, want)
	}
}

func TestCurrentClusterLookupsLoadEntitySnapshotOnce(t *testing.T) {
	s := newStore(t)
	if _, err := s.RegisterEntity(Entity{Slug: "project:api", Kind: "project", Aliases: []string{"API"}}, t0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		claim := Claim{
			ID:         fmt.Sprintf("claim_%02d", i),
			Seq:        int64(i + 1),
			Entity:     "project:api",
			Predicate:  fmt.Sprintf("fact_%02d", i),
			Object:     "same value",
			ClaimType:  "status",
			Qualifiers: map[string]string{"domain": "runtime"},
		}
		if err := appendJSONL(s.claimsLog(), &claim); err != nil {
			t.Fatal(err)
		}
	}

	loads := 0
	s.entityLoader = func() ([]Entity, error) {
		loads++
		return s.loadEntities()
	}
	cand := Candidate{
		Entity:     "API",
		Predicate:  "fact_63",
		Object:     "same value",
		ClaimType:  "status",
		Qualifiers: map[string]string{"domain": "runtime"},
	}

	if got, found, err := s.currentClusterClaim(cand, 0); err != nil {
		t.Fatal(err)
	} else if !found || got.ID != "claim_63" {
		t.Fatalf("current cluster claim = %+v found=%v, want claim_63", got, found)
	}
	if loads != 1 {
		t.Fatalf("current cluster entity-registry loads = %d, want 1 regardless of claim count", loads)
	}

	loads = 0
	if got, found, err := s.currentEchoClaim(cand, 0); err != nil {
		t.Fatal(err)
	} else if !found || got.ID != "claim_63" {
		t.Fatalf("current echo claim = %+v found=%v, want claim_63", got, found)
	}
	if loads != 1 {
		t.Fatalf("current echo entity-registry loads = %d, want 1 regardless of claim count", loads)
	}

	loads = 0
	echo := cand
	echo.Predicate = "new_fact"
	reinforced, err := s.reinforceIfEcho(echo, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !reinforced {
		t.Fatal("broad echo should reinforce the current claim after an exact-slot miss")
	}
	if loads != 1 {
		t.Fatalf("exact-miss plus broad-echo entity-registry loads = %d, want one shared snapshot", loads)
	}
}

func TestCaptureEchoReinforcesCurrentClaimWithoutNewCandidate(t *testing.T) {
	s := newStore(t)
	initial := cand("project:api", "runtime", "go", "")
	initial.Qualifiers = map[string]string{"domain": "language"}
	first, err := s.Capture([]Candidate{initial}, t0)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := promoteForTest(t, s, first[0].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	before := memoryCandidateFileCount(t, s)

	echo := cand("project:api", "implementation_language", " GO ", "")
	echo.Qualifiers = map[string]string{"domain": "language"}
	echo.SourceTraceRef = "trace_echo"
	got, err := s.Capture([]Candidate{echo}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("echo capture returned %d candidate(s), want 0", len(got))
	}
	if after := memoryCandidateFileCount(t, s); after != before {
		t.Fatalf("candidate files after echo = %d, want %d", after, before)
	}

	relations, err := s.loadEdges()
	if err != nil {
		t.Fatal(err)
	}
	var reinforces []Edge
	for _, edge := range relations {
		if edge.Kind == "reinforces" {
			reinforces = append(reinforces, edge)
		}
	}
	if len(reinforces) != 1 {
		t.Fatalf("reinforces edges = %+v, want exactly one", reinforces)
	}
	if reinforces[0].To != promoted.Claim.ID {
		t.Fatalf("reinforces target = %q, want %q", reinforces[0].To, promoted.Claim.ID)
	}
	if reinforces[0].From == "" || reinforces[0].From == first[0].ID {
		t.Fatalf("reinforces source should be new evidence id, got %+v", reinforces[0])
	}
}

func TestCaptureChangedExactSlotWinsOverBroadEcho(t *testing.T) {
	s := newStore(t)
	qualifiers := map[string]string{"domain": "language"}

	runtime := cand("project:api", "runtime", "node", "")
	runtime.Qualifiers = qualifiers
	runtimeCandidates, err := s.Capture([]Candidate{runtime}, t0)
	if err != nil {
		t.Fatal(err)
	}
	runtimePrior, err := promoteForTest(t, s, runtimeCandidates[0].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}

	alias := cand("project:api", "implementation_language", "go", "")
	alias.Qualifiers = qualifiers
	aliasCandidates, err := s.Capture([]Candidate{alias}, t0)
	if err != nil {
		t.Fatal(err)
	}
	aliasCurrent, err := promoteForTest(t, s, aliasCandidates[0].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}

	changed := cand("project:api", "runtime", "go", "")
	changed.Qualifiers = qualifiers
	changedCandidates, err := s.Capture([]Candidate{changed}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changedCandidates) != 1 {
		t.Fatalf("changed exact slot returned %d candidate(s), want 1", len(changedCandidates))
	}
	promoted, err := promoteForTest(t, s, changedCandidates[0].ID, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Superseded != runtimePrior.Claim.ID {
		t.Fatalf("changed runtime superseded %q, want exact prior %q", promoted.Superseded, runtimePrior.Claim.ID)
	}
	if promoted.Superseded == aliasCurrent.Claim.ID {
		t.Fatalf("changed runtime superseded broad alias %+v", aliasCurrent.Claim)
	}
}

func memoryCandidateFileCount(t *testing.T, s *Store) int {
	t.Helper()
	entries, err := os.ReadDir(s.candDir())
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
