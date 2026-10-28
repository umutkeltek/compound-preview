package capgraph

// harness_nodes_test.go — falsifier F5.
//
// F5: after minting harness nodes + surfaces_with edges for every enrolled
// harness, the orient offer filter includes `skill:browser` for each one.
// The mint goes through the governed ApplyNodesAndEdgesGoverned door — a
// re-run with the same inputs is a no-op.

import (
	"path/filepath"
	"testing"
	"time"
)

// newCapgraphStore mirrors the pattern reachability_test.go:29 uses —
// open a Store on a temp home with the standard layout.
func newCapgraphStore(t *testing.T) (string, *Store) {
	t.Helper()
	home := t.TempDir()
	store, err := Open(home)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return home, store
}

func fixedCapgraphNow() time.Time {
	return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
}

// F5: surfaces_with edges land; orient filter surfaces skill:browser for
// each enrolled harness. Pre-mint: only claude_code sees it (the existing
// state). Post-mint: every enrolled harness id surfaces it.
func TestSkillBrowserSurfacesWithEveryEnrolledHarness(t *testing.T) {
	home, store := newCapgraphStore(t)
	now := fixedCapgraphNow()

	// Seed the pre-mint state: skill:browser node + ONE surfaces_with edge
	// to claude_code (the packet's "base" measurement). Triggers include
	// "browser" + "automation" so a task that names those terms matches.
	if err := store.Add(Node{
		ID: "skill:browser", Kind: "skill", Name: "Browser",
		Triggers: []string{"browser", "skill", "automation", "adapter", "login"},
		Weight:   1,
	}); err != nil {
		t.Fatalf("seed skill:browser node: %v", err)
	}
	if err := store.AddEdge(Edge{
		Kind: EdSurfacesWith, From: "skill:browser", To: "harness:claude_code",
		Confidence: 1, CreatedAt: now.UTC().Format(time.RFC3339),
	}, now); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	// Task that matches at least one trigger so surfaceTriggerScore > 0.
	const task = "browser automation login website screenshot"

	// Pre-mint: orient for codex must NOT see skill:browser (no edge).
	pre, err := store.SurfaceForHarness(task, "codex")
	if err != nil {
		t.Fatalf("pre-mint surface: %v", err)
	}
	if hasSkillBrowser(pre) {
		t.Errorf("F5 pre-mint: skill:browser already surfacing for codex (it shouldn't be)")
	}

	// Mint for the four enrolled harnesses the packet names. claude_code's
	// edge already exists (seeded above as the "base" state) but its node
	// was not seeded — so all 5 nodes get minted and 4 edges are new.
	enrolled := []string{"claude_code", "codex", "hermes", "oh_my_pi", "opencode"}
	nodesAdded, edgesAdded, err := MintHarnessNodesForBrowser(home, enrolled, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if nodesAdded != len(enrolled) {
		t.Errorf("F5 mint: nodesAdded=%d want %d", nodesAdded, len(enrolled))
	}
	if edgesAdded != len(enrolled)-1 {
		t.Errorf("F5 mint: edgesAdded=%d want %d (claude_code edge already exists)", edgesAdded, len(enrolled)-1)
	}

	// Post-mint: orient for codex, hermes, oh_my_pi must see skill:browser.
	for _, h := range []string{"codex", "hermes", "oh_my_pi"} {
		hits, err := store.SurfaceForHarness(task, h)
		if err != nil {
			t.Fatalf("post-mint surface for %s: %v", h, err)
		}
		if !hasSkillBrowser(hits) {
			t.Errorf("F5 FAIL: orient for harness=%s does not surface skill:browser", h)
		}
	}

	// Idempotent: re-mint with same inputs adds zero rows.
	n2, e2, err := MintHarnessNodesForBrowser(home, enrolled, now)
	if err != nil {
		t.Fatalf("re-mint: %v", err)
	}
	if n2 != 0 || e2 != 0 {
		t.Errorf("F5 idempotent: re-mint appended %d nodes / %d edges, want 0/0", n2, e2)
	}
}

// hasSkillBrowser is the offer-side assertion. SurfaceForHarness returns
// Surfaced values whose Node.ID is what we want.
func hasSkillBrowser(hits []Surfaced) bool {
	for _, h := range hits {
		if h.Node.ID == "skill:browser" {
			return true
		}
	}
	return false
}

// F5 extension: BrowserSurfacesWithHarness is the explicit-graph assertion
// the CLI doctor can use without going through the orient filter.
func TestBrowserSurfacesWithHarnessHelper(t *testing.T) {
	home, _ := newCapgraphStore(t)
	now := fixedCapgraphNow()

	if _, _, err := MintHarnessNodesForBrowser(home, []string{"claude_code", "codex", "hermes"}, now); err != nil {
		t.Fatalf("mint: %v", err)
	}

	for _, h := range []string{"claude_code", "codex", "hermes"} {
		got, err := BrowserSurfacesWithHarness(home, h)
		if err != nil {
			t.Fatalf("check %s: %v", h, err)
		}
		if !got {
			t.Errorf("F5 helper: skill:browser does NOT surface_with %s", h)
		}
	}
	// A harness that was never minted returns false.
	got, err := BrowserSurfacesWithHarness(home, "no_such_harness")
	if err != nil {
		t.Fatalf("check no_such: %v", err)
	}
	if got {
		t.Errorf("F5 helper: no_such_harness should not surface_with browser")
	}
}

// ensure test references filepath to avoid unused-import errors if the
// test above is the only file referencing it.
var _ = filepath.Join
