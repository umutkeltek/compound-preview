package contract

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/mem"
)

const mutableWorkbenchEvidenceRefForTest = "compound://workbench"

type positionFreshnessFixture struct {
	home      string
	store     *mem.Store
	pack      envelope.ContextPack
	positions map[string]mem.Position
	claims    map[string]mem.Claim
	refs      map[string]string
}

func TestCurrentStateStatusPositionRejectsMutableWorkbenchEvidence(t *testing.T) {
	fixture := buildPositionFreshnessFixture(t)
	positionRef := fixture.refs["mutable"]
	claimRef := "claim://" + fixture.claims["mutable"].ID
	if contains(fixture.pack.ActiveMemory, positionRef) {
		t.Errorf("mutable Workbench position remained in active_memory: %v", fixture.pack.ActiveMemory)
	}
	if hasActivePositionRef(fixture.pack, positionRef) {
		t.Errorf("mutable Workbench position remained in active_positions: %+v", fixture.pack.ActivePositions)
	}
	if hasIncluded(fixture.pack, positionRef) {
		t.Errorf("mutable Workbench position remained in included[]: %+v", fixture.pack.Included)
	}
	reasons := positionExclusionReasons(fixture.pack, positionRef)
	if len(reasons) != 1 {
		t.Fatalf("mutable Workbench position exclusions = %v, want exactly one loud exclusion", reasons)
	}
	for _, want := range []string{
		"current-state position",
		claimRef,
		mutableWorkbenchEvidenceRefForTest,
		"mutable",
		"unversioned",
		"compound workbench --json",
	} {
		if !strings.Contains(reasons[0], want) {
			t.Errorf("exclusion reason %q does not contain %q", reasons[0], want)
		}
	}
}

func TestCurrentStatePositionImmutableSuccessorIsNotTaintedByMutableHistory(t *testing.T) {
	fixture := buildPositionFreshnessFixture(t)
	position := fixture.positions["successor"]
	positionRef := fixture.refs["successor"]
	oldClaim := fixture.claims["successor_old"]
	activeClaim := fixture.claims["successor"]

	if !contains(position.EvidenceRefs, mutableWorkbenchEvidenceRefForTest) {
		t.Fatalf("regression fixture does not preserve old mutable evidence in folded history: %+v", position)
	}
	if contains(activeClaim.EvidenceRefs, mutableWorkbenchEvidenceRefForTest) {
		t.Fatalf("immutable successor unexpectedly cites mutable Workbench evidence: %+v", activeClaim.EvidenceRefs)
	}
	if position.ActiveClaimID != activeClaim.ID || !contains(position.ClaimChain, oldClaim.ID) || !contains(position.ClaimChain, activeClaim.ID) {
		t.Fatalf("regression fixture is not a real same-slot supersession chain: %+v", position)
	}
	if !contains(fixture.pack.ActiveMemory, positionRef) || !hasActivePositionRef(fixture.pack, positionRef) || !hasIncluded(fixture.pack, positionRef) {
		t.Errorf("immutable successor was fossilized by mutable historical evidence: active_memory=%v active_positions=%+v included=%+v",
			fixture.pack.ActiveMemory, fixture.pack.ActivePositions, fixture.pack.Included)
	}
	if reasons := positionExclusionReasons(fixture.pack, positionRef); len(reasons) != 0 {
		t.Errorf("immutable successor was freshness-excluded by folded historical evidence: %v", reasons)
	}
}

func TestCurrentStateConstraintPositionPreservesMutableWorkbenchEvidence(t *testing.T) {
	fixture := buildPositionFreshnessFixture(t)
	positionRef := fixture.refs["constraint"]

	if !contains(fixture.pack.ActiveMemory, positionRef) {
		t.Fatalf("constraint position missing from active_memory: %v", fixture.pack.ActiveMemory)
	}
	if !hasActivePositionRef(fixture.pack, positionRef) {
		t.Fatalf("constraint position missing from active_positions: %+v", fixture.pack.ActivePositions)
	}
	if !hasIncluded(fixture.pack, positionRef) {
		t.Fatalf("constraint position missing from included[]: %+v", fixture.pack.Included)
	}
	if reasons := positionExclusionReasons(fixture.pack, positionRef); len(reasons) != 0 {
		t.Fatalf("constraint position was freshness-excluded: %v", reasons)
	}
}

func TestCurrentStateStatusPositionWithoutBareMutableWorkbenchStaysCurrent(t *testing.T) {
	fixture := buildPositionFreshnessFixture(t)

	for _, name := range []string{"unrelated"} {
		positionRef := fixture.refs[name]
		if !contains(fixture.pack.ActiveMemory, positionRef) || !hasActivePositionRef(fixture.pack, positionRef) || !hasIncluded(fixture.pack, positionRef) {
			t.Errorf("%s status position should remain current: active_memory=%v active_positions=%+v included=%+v",
				name, fixture.pack.ActiveMemory, fixture.pack.ActivePositions, fixture.pack.Included)
		}
		if reasons := positionExclusionReasons(fixture.pack, positionRef); len(reasons) != 0 {
			t.Errorf("%s status position was freshness-excluded: %v", name, reasons)
		}
	}
}

func TestCurrentStatePositionExclusionPreservesHistoricalFoldAndLedger(t *testing.T) {
	home := t.TempDir()
	fixture := buildPositionFreshnessFixtureAtHome(t, home)
	claimsPath := filepath.Join(home, "ledger", "claims.jsonl")
	positionsPath := filepath.Join(home, "ledger", "positions.jsonl")
	claimsBefore := mustReadPositionFreshnessFile(t, claimsPath)
	positionsBefore := mustReadPositionFreshnessFile(t, positionsPath)

	if _, err := PackWithCapabilities(positionFreshnessInput(), DefaultPolicy(), home, fixedNow); err != nil {
		t.Fatal(err)
	}

	if got := mustReadPositionFreshnessFile(t, claimsPath); !bytes.Equal(got, claimsBefore) {
		t.Fatal("ContextPack construction mutated the immutable claim ledger")
	}
	if got := mustReadPositionFreshnessFile(t, positionsPath); !bytes.Equal(got, positionsBefore) {
		t.Fatal("ContextPack construction mutated the folded position checkpoint")
	}
	folded, err := fixture.store.LoadFoldedPositions()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mutable", "successor"} {
		if !hasFoldedPosition(folded, fixture.positions[name].ID, fixture.claims[name].ID) {
			t.Errorf("historical fold lost %s status position/claim: positions=%+v", name, folded)
		}
	}
	detail, err := fixture.store.Position("project:compound", mem.PositionFoldOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mutable", "successor_old", "successor"} {
		if !positionDetailHasClaim(detail, fixture.claims[name].ID) {
			t.Errorf("historical position drilldown lost %s claim %s: %+v", name, fixture.claims[name].ID, detail.Positions)
		}
	}
}

func buildPositionFreshnessFixture(t *testing.T) positionFreshnessFixture {
	t.Helper()
	return buildPositionFreshnessFixtureAtHome(t, t.TempDir())
}

func buildPositionFreshnessFixtureAtHome(t *testing.T, home string) positionFreshnessFixture {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	type seed struct {
		name         string
		predicate    string
		object       string
		claimType    string
		evidenceRefs []string
	}
	seeds := []seed{
		{name: "mutable", predicate: "workbench_active_snapshot", object: "Workbench snapshot is mutable", claimType: "status", evidenceRefs: []string{mutableWorkbenchEvidenceRefForTest}},
		{name: "successor_old", predicate: "workbench_replaced_snapshot", object: "Old Workbench snapshot was mutable", claimType: "status", evidenceRefs: []string{mutableWorkbenchEvidenceRefForTest}},
		{name: "successor", predicate: "workbench_replaced_snapshot", object: "Current Workbench snapshot is revision-attested", claimType: "status", evidenceRefs: []string{"artifact://workbench-snapshot@sha256:abc"}},
		{name: "constraint", predicate: "workbench_operating_constraint", object: "Workbench evidence supports durable operator law", claimType: "constraint", evidenceRefs: []string{mutableWorkbenchEvidenceRefForTest}},
		{name: "unrelated", predicate: "release_status_snapshot", object: "Release status is backed by another source and immutable Workbench revision", claimType: "status", evidenceRefs: []string{"compound://release-status", "compound://workbench?rev=sha256:abc"}},
	}
	candidateRows := make([]mem.Candidate, 0, len(seeds))
	for _, seed := range seeds {
		basis := candidates.BasisRuntimeProof
		if seed.claimType == "constraint" {
			basis = candidates.BasisCuratedPage
		}
		candidateRows = append(candidateRows, mem.Candidate{
			Entity:          "project:compound",
			Predicate:       seed.predicate,
			Object:          seed.object,
			ObservationDate: "2026-07-16T00:00:00Z",
			Confidence:      0.93,
			Derivation:      "deterministic",
			ClaimType:       seed.claimType,
			Qualifiers:      map[string]string{"domain": "current_state_freshness"},
			PromotionBasis:  basis,
			SourceTraceRef:  "trace_" + seed.name,
			EvidenceRefs:    seed.evidenceRefs,
			SnippetHash:     "sha256:" + seed.name,
			Visibility:      "contextual",
		})
	}
	captured, err := store.Capture(candidateRows, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claims := make(map[string]mem.Claim, len(seeds))
	for i, candidate := range captured {
		promoted, err := store.Adjudicate(candidate.ID, "promote", "test", fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		claims[seeds[i].name] = *promoted.Claim
	}
	fold, err := store.FoldPositions(mem.PositionFoldOptions{}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	positions := make(map[string]mem.Position, len(seeds))
	refs := make(map[string]string, len(seeds))
	nameByClaimID := make(map[string]string, len(seeds))
	for name, claim := range claims {
		nameByClaimID[claim.ID] = name
	}
	for i := range fold.Positions {
		position := fold.Positions[i]
		name, ok := nameByClaimID[position.ActiveClaimID]
		if !ok {
			t.Fatalf("folded position has unknown active claim: %+v", position)
		}
		positions[name] = position
		refs[name] = "position://" + position.ID
	}

	pack, err := PackWithCapabilities(positionFreshnessInput(), DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return positionFreshnessFixture{home: home, store: store, pack: pack, positions: positions, claims: claims, refs: refs}
}

func positionFreshnessInput() Input {
	return Input{
		Harness:     "codex",
		Cwd:         "/repo/compound",
		Task:        "report current Compound Workbench mutable replaced release and revision-attested status while preserving operator constraints",
		RepoKind:    "control-plane",
		RepoKindVia: "flag",
		Mode:        "execute",
	}
}

func mustReadPositionFreshnessFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func hasActivePositionRef(pack envelope.ContextPack, ref string) bool {
	for _, position := range pack.ActivePositions {
		if position.Ref == ref {
			return true
		}
	}
	return false
}

func positionExclusionReasons(pack envelope.ContextPack, ref string) []string {
	var reasons []string
	for _, excluded := range pack.Excluded {
		if excluded.Ref == ref {
			reasons = append(reasons, excluded.Reason)
		}
	}
	return reasons
}

func hasFoldedPosition(positions []mem.Position, positionID, claimID string) bool {
	for _, position := range positions {
		if position.ID == positionID && position.ActiveClaimID == claimID {
			return true
		}
	}
	return false
}

func positionDetailHasClaim(result mem.PositionQueryResult, claimID string) bool {
	for _, position := range result.Positions {
		for _, claim := range position.Claims {
			if claim.ID == claimID {
				return true
			}
		}
	}
	return false
}
