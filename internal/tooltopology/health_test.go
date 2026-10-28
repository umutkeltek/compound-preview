package tooltopology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/capgraph"
)

// writeHealthFixture writes a synthetic catalog with one healthy server and one
// that was DOWN at probe time. Synthetic hosts and paths only -- this repo ships
// public, and a fixture is where a real name gets typed by reflex.
func writeHealthFixture(t *testing.T, downStatus string) string {
	t.Helper()
	body := `{
  "mode": "list",
  "servers": [
    {
      "name": "steady-svc",
      "status": "ok",
      "transport": "STDIO /opt/synthetic/bin/steady serve",
      "source": {"kind": "local", "path": "/opt/synthetic/conf/steady.json"},
      "tools": [{"name": "do_thing", "description": "does a thing"}]
    },
    {
      "name": "flaky-svc",
      "status": "` + downStatus + `",
      "transport": "STDIO /opt/synthetic/bin/flaky serve",
      "source": {"kind": "local", "path": "/opt/synthetic/conf/flaky.json"},
      "tools": []
    }
  ]
}`
	path := filepath.Join(t.TempDir(), "mcporter-list.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func nodeByID(t *testing.T, store *capgraph.Store, id string) capgraph.Node {
	t.Helper()
	nodes, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %s not in graph", id)
	return capgraph.Node{}
}

// FALSIFIER (the defect this slice removes): a server that was merely DOWN when
// mcporter probed must not be retired. Retirement removes a node from surfacing,
// so deriving it from a health probe made the graph forget a capability over a
// transient blip -- and once it is gone from every offer, nothing downstream can
// report that it is missing. Restore `Retired: status != "on_demand"` in the
// adapter and every subtest here goes red.
func TestATransientlyDownServerIsDegradedNotRetired(t *testing.T) {
	for _, downStatus := range []string{"offline", "error", "auth"} {
		t.Run(downStatus, func(t *testing.T) {
			home := t.TempDir()
			topo, err := LoadMCPorterCatalog(writeHealthFixture(t, downStatus), tTopo)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Apply(home, topo, tTopo); err != nil {
				t.Fatal(err)
			}
			store, err := capgraph.Open(home)
			if err != nil {
				t.Fatal(err)
			}

			down := nodeByID(t, store, "mcp:flaky-svc")
			if down.Retired {
				t.Error("a server that was down at probe time was RETIRED; health is not existence")
			}
			if down.Health == "" {
				t.Error("a server that was down carries no Health; the degradation is invisible")
			}

			healthy := nodeByID(t, store, "mcp:steady-svc")
			if healthy.Health != "" {
				t.Errorf("healthy server carries Health %q, want empty", healthy.Health)
			}
			if healthy.Retired {
				t.Error("healthy server was retired")
			}
		})
	}
}

// FALSIFIER (the exact line between the two states): a broken capability is NOT
// offered for a task -- an agent handed a dead tool burns a turn finding out --
// but it must still be NAMED, counted, and listed with its condition.
//
// That pair is the whole point of the split, and testing either half alone would
// miss the bug. Asserting only "not offered" passes under the old retirement
// behavior too. Asserting only "still named" would pass a version that offers
// broken tools. The regression this file exists to prevent lives between them.
func TestADegradedCapabilityIsNamedButNotOffered(t *testing.T) {
	home := t.TempDir()
	topo, err := LoadMCPorterCatalog(writeHealthFixture(t, "offline"), tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, topo, tTopo); err != nil {
		t.Fatal(err)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}

	// NOT offered: a task that matches it by name must not get it back while it
	// is down. Remove the Health skip in Surface and this half goes red.
	surfaced, err := store.Surface("flaky svc")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range surfaced {
		if s.Node.ID == "mcp:flaky-svc" {
			t.Errorf("a broken capability was offered for a task: %+v", s.Node)
		}
	}
	// Sanity that the query itself works -- without this the "not offered" check
	// above would pass just as well on a task that matches nothing at all, which
	// is the vacuous-green shape this file is written to avoid.
	var sawHealthy bool
	for _, s := range surfaced {
		if s.Node.ID == "mcp:steady-svc" || s.Node.ID == "tool:steady-svc.do-thing" {
			sawHealthy = true
		}
	}
	if !sawHealthy {
		t.Fatal("no healthy capability surfaced either; the query proves nothing about the skip")
	}

	// STILL NAMED: this is the half retirement destroyed. The node is present,
	// carries its condition, and keeps the access facts needed to fix it.
	node := nodeByID(t, store, "mcp:flaky-svc")
	if node.Health != "offline" {
		t.Errorf("degraded node Health = %q, want offline", node.Health)
	}
	if node.Retired {
		t.Error("degraded node was retired; being down is not being withdrawn")
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	var keptAccess bool
	for _, cs := range surfaces {
		if cs.NodeID == "mcp:flaky-svc" && len(cs.ConfigPaths) > 0 {
			keptAccess = true
		}
	}
	if !keptAccess {
		t.Error("degraded node lost its config paths — the facts needed to FIX it are exactly what must survive an outage")
	}
}

// FALSIFIER (the other direction -- a split that makes retirement impossible is
// just as wrong): a source that says RETIRED or DEPRECATED still retires the
// node. Retirement is a decision the source states, not an inference from a
// failed probe, and it must keep working.
func TestAnExplicitlyWithdrawnEntryStillRetires(t *testing.T) {
	home := t.TempDir()
	topo := validTopology()
	topo.Entries[0].Status = "deprecated"
	if _, err := Apply(home, topo, tTopo); err != nil {
		t.Fatal(err)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if !nodeByID(t, store, topo.Entries[0].ID).Retired {
		t.Error("an explicitly deprecated entry was NOT retired; the split removed real retirement")
	}
}

// FALSIFIER: degradation must be COUNTED, not just stored. Before this slice a
// degraded node was excluded from the tally entirely (it was retired), so "down"
// and "never existed" produced identical reports. Degraded nodes must appear in
// Nodes AND in Degraded.
func TestReachabilityCountsDegradedWithoutHidingIt(t *testing.T) {
	home := t.TempDir()
	topo, err := LoadMCPorterCatalog(writeHealthFixture(t, "offline"), tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, topo, tTopo); err != nil {
		t.Fatal(err)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	report, err := store.ComputeReachability()
	if err != nil {
		t.Fatal(err)
	}
	var mcp *capgraph.KindReachability
	for i := range report.Kinds {
		if report.Kinds[i].Kind == "mcp" {
			mcp = &report.Kinds[i]
		}
	}
	if mcp == nil {
		t.Fatal("no mcp kind in reachability at all")
	}
	if mcp.Nodes != 2 {
		t.Errorf("mcp nodes = %d, want 2 — a degraded node must still be COUNTED", mcp.Nodes)
	}
	if mcp.Degraded != 1 {
		t.Errorf("mcp degraded = %d, want 1", mcp.Degraded)
	}
	if report.Total.Degraded != mcp.Degraded {
		t.Errorf("total degraded %d does not aggregate kind degraded %d", report.Total.Degraded, mcp.Degraded)
	}
}

// FALSIFIER (recovery must be observable, or health is write-once noise): after a
// server comes back, re-importing clears its Health. If a health change were
// treated as "no change" by any dedupe path, the node would stay marked down
// forever and the operator would chase a phantom outage.
func TestRecoveryClearsHealthOnReimport(t *testing.T) {
	home := t.TempDir()

	downTopo, err := LoadMCPorterCatalog(writeHealthFixture(t, "offline"), tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, downTopo, tTopo); err != nil {
		t.Fatal(err)
	}
	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if nodeByID(t, store, "mcp:flaky-svc").Health == "" {
		t.Fatal("precondition broken: node not marked degraded on the first import")
	}

	upTopo, err := LoadMCPorterCatalog(writeHealthFixture(t, "ok"), tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, upTopo, tTopo); err != nil {
		t.Fatal(err)
	}
	if got := nodeByID(t, store, "mcp:flaky-svc").Health; got != "" {
		t.Errorf("Health = %q after recovery, want cleared — a stale outage marker is a phantom bug report", got)
	}
}

// FALSIFIER: "on_demand" is mcporter's HEALTHY state for a server started when
// needed -- the normal condition for stdio servers. Treating anything-but-
// on_demand as a problem is what made the old rule wrong in both directions, so
// pin that the healthy mapping produces no health string.
func TestOnDemandIsHealthNotDegradation(t *testing.T) {
	if got := mcporterHealth("on_demand"); got != "" {
		t.Errorf("mcporterHealth(on_demand) = %q, want empty — on_demand IS healthy", got)
	}
	for _, bad := range []string{"offline", "error", "auth_required"} {
		if got := mcporterHealth(bad); !strings.EqualFold(got, bad) {
			t.Errorf("mcporterHealth(%q) = %q, want it reported", bad, got)
		}
	}
}
