package capgraph

// harness_nodes.go — mint missing harness nodes + surfaces_with edges so the
// orient offer filter sees `skill:browser` for every enrolled harness, not
// only claude_code.
//
// This is the small helper the packet calls for: it lives in a NEW file
// (sibling-lane files in internal/capgraph — capgraph.go, skillmanifest.go,
// skill_source_doctor.go, reachability.go — are off-limits). The helper uses
// the existing ApplyNodesAndEdgesGoverned door so the mutation crosses the
// ActionIntent + PolicyDecision seam.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// HarnessNodeKind is the kind tag for the harness nodes this helper mints.
// Matches what compound harness list ids surface in the registry.
const HarnessNodeKind = "harness"

// MintHarnessNodesForBrowser adds (idempotent) harness nodes for every id
// the caller asks for, plus `surfaces_with` edges from `skill:browser` to
// each one. The function returns the number of new node + edge rows
// appended through the governed door. Existing nodes/edges are skipped
// here (the door's Add() does dedupe identical writes too, but reading
// first lets us count accurately and short-circuit when nothing to do).
//
// The caller passes the harness ids. Production callers source from
// `compound harness list`; tests pass a fixture.
func MintHarnessNodesForBrowser(homePath string, harnessIDs []string, now time.Time) (nodesAdded, edgesAdded int, err error) {
	if strings.TrimSpace(homePath) == "" {
		return 0, 0, fmt.Errorf("mint harness nodes: home path is empty")
	}
	if len(harnessIDs) == 0 {
		return 0, 0, nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	store, err := Open(homePath)
	if err != nil {
		return 0, 0, err
	}

	// Stable order so re-runs are byte-identical at the door.
	ids := append([]string(nil), harnessIDs...)
	sort.Strings(ids)

	existingNodes, err := store.loadNodes()
	if err != nil {
		return 0, 0, err
	}
	haveNode := map[string]bool{}
	for _, n := range existingNodes {
		haveNode[n.ID] = true
	}
	existingEdges, err := store.loadEdges()
	if err != nil {
		return 0, 0, err
	}
	haveEdge := map[string]bool{}
	for _, e := range existingEdges {
		if e.Kind != EdSurfacesWith {
			continue
		}
		haveEdge[e.From+"|"+e.To] = true
	}

	var nodes []Node
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		nodeID := "harness:" + id
		if haveNode[nodeID] {
			continue
		}
		nodes = append(nodes, Node{
			ID:       nodeID,
			Kind:     HarnessNodeKind,
			Name:     nodeID,
			Triggers: []string{id},
			Weight:   1,
		})
	}

	created := now.UTC().Format(time.RFC3339)
	var edges []Edge
	for _, n := range nodes {
		key := "skill:browser|" + n.ID
		if haveEdge[key] {
			continue
		}
		edges = append(edges, Edge{
			Kind:       EdSurfacesWith,
			From:       "skill:browser",
			To:         n.ID,
			Confidence: 1,
			CreatedAt:  created,
		})
	}

	if len(nodes) == 0 && len(edges) == 0 {
		return 0, 0, nil
	}
	if err := ApplyNodesAndEdgesGoverned(homePath, nodes, edges, now); err != nil {
		return 0, 0, fmt.Errorf("mint harness nodes for browser: %w", err)
	}
	return len(nodes), len(edges), nil
}

// BrowserSurfacesWithHarness returns true iff `skill:browser` has a
// `surfaces_with` edge to the harness id. Reads the live graph (last-write-
// wins on load). The harness id may be passed with or without the
// `harness:` prefix; the helper normalizes.
func BrowserSurfacesWithHarness(homePath, harness string) (bool, error) {
	if strings.TrimSpace(homePath) == "" {
		return false, fmt.Errorf("check browser surfaces_with: home path is empty")
	}
	store, err := Open(homePath)
	if err != nil {
		return false, err
	}
	edges, err := store.loadEdges()
	if err != nil {
		return false, err
	}
	want := harnessID(strings.TrimSpace(harness))
	for _, e := range edges {
		if e.Kind != EdSurfacesWith {
			continue
		}
		if e.From != "skill:browser" {
			continue
		}
		if harnessID(e.To) == want {
			return true, nil
		}
	}
	return false, nil
}
