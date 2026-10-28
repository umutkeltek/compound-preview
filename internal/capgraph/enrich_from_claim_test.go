// enrich_from_claim_test.go — Falsifier-first tests for loop piece 2/4
// (work_57f80593eb2d).
//
// The envelope (work_57f80593eb2d) requires that promoting a candidate via
// `compound memory adjudicate` automatically writes / strengthens matching
// capability-graph node(s) + edge evidence refs (validated_by chain), with
// the enrichment visible in the next compound orient offer scoring.
//
// These tests are RED today: the `capgraph.EnrichFromPromotedClaim` seam
// does not exist yet, so the test file fails to compile (run
// `go test ./internal/capgraph/...`) and exposes the gap. Once the seam is
// implemented, the same tests turn GREEN — the bridge from a promoted
// memory Claim to a capability-graph node's validated_by edge is wired and
// surface-observable.
package capgraph

import (
	"strings"
	"testing"
	"time"
)

// TestEnrichFromPromotedClaim_WritesValidatedByEdge is the head-line red
// test: pre-seed a capgraph node that names "execute ceremony" and a
// freshly-promoted Claim about that topic; expect the seam to mint one
// validated_by edge whose From is context:token, To is the resolved node,
// SourceReceipt is the Claim ID, and Provenance points at memory adjudicate.
func TestEnrichFromPromotedClaim_WritesValidatedByEdge(t *testing.T) {
	home := t.TempDir()
	if err := initGraphHome(t, home, []Node{
		mustNode("procedure:compound-execute-ceremony", "Compound Execute Ceremony", []string{"execute-ceremony", "envelope"}),
	}); err != nil {
		t.Fatalf("init graph: %v", err)
	}

	now := time.Date(2026, 8, 14, 7, 30, 0, 0, time.UTC)
	edge, wrote, err := EnrichFromPromotedClaim(home,
		"claim_test_001",
		"compound/memory.adjudicate",
		"compound-execute-ceremony",
		"execute-ceremony",
		now,
	)
	if err != nil {
		t.Fatalf("EnrichFromPromotedClaim: %v", err)
	}
	if !wrote {
		t.Fatalf("expected write=true, got false")
	}
	if edge.Kind != EdValidatedBy {
		t.Fatalf("edge kind = %q, want %q", edge.Kind, EdValidatedBy)
	}
	// The from token is normalized the same way Surface indexes task tokens
	// (alphanumeric-only, lowercase, dash-stripped) so a future orient
	// surface scoring round can find the edge and lift the matched node's
	// score via the validated_by contribution.
	if edge.From != "context:executeceremony" {
		t.Fatalf("edge from = %q, want context:executeceremony", edge.From)
	}
	if edge.To != "procedure:compound-execute-ceremony" {
		t.Fatalf("edge to = %q, want procedure:compound-execute-ceremony", edge.To)
	}
	if edge.SourceReceipt != "claim_test_001" {
		t.Fatalf("edge source_receipt = %q, want claim_test_001", edge.SourceReceipt)
	}
	if edge.Provenance != "compound/memory.adjudicate" {
		t.Fatalf("edge provenance = %q, want compound/memory.adjudicate", edge.Provenance)
	}
	if got := edge.CreatedAt; got != now.UTC().Format(time.RFC3339) {
		t.Fatalf("edge created_at = %q, want %q", got, now.UTC().Format(time.RFC3339))
	}

	// Verify the edge is actually persisted in the log and survives reload.
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	edges, err := store.Edges()
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	found := false
	for _, e := range edges {
		if e.Kind == EdValidatedBy && e.From == "context:executeceremony" && e.To == "procedure:compound-execute-ceremony" && e.SourceReceipt == "claim_test_001" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("validated_by edge not found in %d persisted edges", len(edges))
	}
}

// TestEnrichFromPromotedClaim_ReappearsInSurfaceScore pins the orient-side
// proof: a token that matches a validated_by edge must lift the matched
// node's score in SurfaceForContext and surface the validated_by bump in
// the offer's "why" / matched evidence trail.
//
// The trigger list uses a single lowercase, dash-merged token
// ("executeceremony") that matches BOTH the surface tokenizer (which
// strips dashes from task text and from trigger tokens alike) AND the
// normalized context token EnrichFromPromotedClaim writes to the
// validated_by edge's From field. A mismatch in any of those three forms
// would surface as offers=[] here.
func TestEnrichFromPromotedClaim_ReappearsInSurfaceScore(t *testing.T) {
	home := t.TempDir()
	if err := initGraphHome(t, home, []Node{
		mustNode("procedure:compound-execute-ceremony", "Compound Execute Ceremony", []string{"executeceremony"}),
	}); err != nil {
		t.Fatalf("init graph: %v", err)
	}

	now := time.Date(2026, 8, 14, 7, 30, 0, 0, time.UTC)
	if _, wrote, err := EnrichFromPromotedClaim(home,
		"claim_test_002",
		"compound/memory.adjudicate",
		"compound-execute-ceremony",
		"executeceremony",
		now,
	); err != nil || !wrote {
		t.Fatalf("EnrichFromPromotedClaim: wrote=%v err=%v", wrote, err)
	}

	store, err := Open(home)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	offers, err := store.SurfaceForContext(SurfaceContext{Task: "do an executeceremony rollup"})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	var bumped *Surfaced
	for i := range offers {
		if offers[i].Node.ID == "procedure:compound-execute-ceremony" {
			bumped = &offers[i]
			break
		}
	}
	if bumped == nil {
		t.Fatalf("node procedure:compound-execute-ceremony missing from offers; offers=%+v", offers)
	}
	if !strings.Contains(bumped.Why, "validated_by") {
		t.Fatalf("offer why does not name validated_by bump; why=%q", bumped.Why)
	}
}

// TestEnrichFromPromotedClaim_NoMatchReturnsCleanSkip pins the false-positive
// guard: a Claim that names no resolvable capability must NOT mint a phantom
// node / edge. The seam returns false cleanly so callers can carry on.
func TestEnrichFromPromotedClaim_NoMatchReturnsCleanSkip(t *testing.T) {
	home := t.TempDir()
	if err := initGraphHome(t, home, []Node{
		mustNode("procedure:compound-execute-ceremony", "Compound Execute Ceremony", []string{"execute-ceremony"}),
	}); err != nil {
		t.Fatalf("init graph: %v", err)
	}

	now := time.Date(2026, 8, 14, 7, 30, 0, 0, time.UTC)
	_, wrote, err := EnrichFromPromotedClaim(home,
		"claim_test_003",
		"compound/memory.adjudicate",
		"totally-unrelated-capability-that-does-not-exist",
		"some-token",
		now,
	)
	if err != nil {
		t.Fatalf("EnrichFromPromotedClaim: %v", err)
	}
	if wrote {
		t.Fatalf("expected clean skip (wrote=false) for unmatched capability hint")
	}
}

// TestEnrichFromPromotedClaim_RefusesEmptyInputs pins input hygiene: empty
// home / no hint / no token must return a loud error rather than silently
// mint a meaningless edge.
func TestEnrichFromPromotedClaim_RefusesEmptyInputs(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 8, 14, 7, 30, 0, 0, time.UTC)
	if _, _, err := EnrichFromPromotedClaim("", "c", "p", "hint", "tok", now); err == nil {
		t.Fatalf("expected error for empty home path")
	}
	if _, _, err := EnrichFromPromotedClaim(home, "c", "p", "", "", now); err == nil {
		t.Fatalf("expected error when both hint and token are empty")
	}
}

// initGraphHome creates the capgraph home wiring (logs + nodes) under
// homePath with the provided nodes pre-seeded via the governed Apply path.
func initGraphHome(t *testing.T, homePath string, nodes []Node) error {
	t.Helper()
	store, err := Open(homePath)
	if err != nil {
		return err
	}
	if err := ApplyNodesAndEdgesGoverned(homePath, nodes, nil, time.Now()); err != nil {
		return err
	}
	_ = store
	return nil
}

func mustNode(id, name string, triggers []string) Node {
	return Node{
		ID:       id,
		Kind:     "procedure",
		Name:     name,
		Triggers: triggers,
		Weight:   1.0,
	}
}
