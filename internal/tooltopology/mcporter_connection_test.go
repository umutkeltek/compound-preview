package tooltopology

import (
	"os"
	"path/filepath"
	"testing"

	"compound/internal/capgraph"
)

// writeMCPorterFixture writes a synthetic `mcporter list --json` catalog.
//
// Every host and path here is invented. Fixtures are where a real private path
// or product name gets typed by reflex, and this repo ships public -- so the
// catalog below deliberately looks nothing like a real machine while keeping the
// SHAPES that matter: a compound transport string, a path containing a space,
// an http server, a stdio server, and a server with tools.
func writeMCPorterFixture(t *testing.T) string {
	t.Helper()
	body := `{
  "mode": "list",
  "servers": [
    {
      "name": "remote-svc",
      "status": "ok",
      "transport": "HTTP https://mcp.example.test/mcp",
      "source": {"kind": "local", "path": "/opt/synthetic/conf/remote.json"},
      "tools": [{"name": "fetch_thing", "description": "fetch a thing"}]
    },
    {
      "name": "local-svc",
      "status": "ok",
      "transport": "STDIO /opt/synthetic/Some App.app/Contents/MacOS/helper --stdio serve",
      "source": {"kind": "import", "path": "/opt/synthetic/conf/local.json"},
      "sources": [{"kind": "import", "path": "/opt/synthetic/conf/extra.json"}],
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

func entryByID(t *testing.T, topo Topology, id string) Entry {
	t.Helper()
	for _, e := range topo.Entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("entry %s not in topology (%d entries)", id, len(topo.Entries))
	return Entry{}
}

// FALSIFIER (the whole point of the door): mcporter reports transport as one
// compound string whose first field is the CLASS and whose remainder is the
// ADDRESS. Keeping only the class is what left every MCP node named-but-
// unreachable. Revert mcporterConnection to the old strings.Fields(...)[0]
// behavior and both halves of this test go red.
func TestMCPorterImportKeepsTheTransportAddressNotJustItsClass(t *testing.T) {
	topo, err := LoadMCPorterCatalog(writeMCPorterFixture(t), tTopo)
	if err != nil {
		t.Fatal(err)
	}

	remote := entryByID(t, topo, "mcp:remote-svc")
	if remote.Connection == nil {
		t.Fatal("http server imported with no connection facts at all")
	}
	if got := remote.Connection.Endpoint; got != "https://mcp.example.test/mcp" {
		t.Errorf("http endpoint = %q, want the URL from the transport string", got)
	}
	if remote.Connection.LaunchCommand != "" {
		t.Errorf("http server got a launch command %q; an address is dialed, not run", remote.Connection.LaunchCommand)
	}

	// The stdio command contains BOTH arguments and a path with a space in it.
	// Any split greedier than "first whitespace run" truncates it, which is the
	// silent-corruption failure: the field is populated, so it reads as fixed,
	// and the command it holds does not run.
	local := entryByID(t, topo, "mcp:local-svc")
	if local.Connection == nil {
		t.Fatal("stdio server imported with no connection facts at all")
	}
	want := "/opt/synthetic/Some App.app/Contents/MacOS/helper --stdio serve"
	if got := local.Connection.LaunchCommand; got != want {
		t.Errorf("stdio launch command = %q, want %q", got, want)
	}
	if local.Connection.Endpoint != "" {
		t.Errorf("stdio server got endpoint %q; a command is run, not dialed", local.Connection.Endpoint)
	}
}

// FALSIFIER: "where is this configured" is what makes a broken server fixable
// without a filesystem hunt. Both the primary source and the additional sources
// list must survive, deduped.
func TestMCPorterImportKeepsConfigPathsFromEverySource(t *testing.T) {
	topo, err := LoadMCPorterCatalog(writeMCPorterFixture(t), tTopo)
	if err != nil {
		t.Fatal(err)
	}
	local := entryByID(t, topo, "mcp:local-svc")
	got := local.Connection.ConfigPaths
	want := []string{"/opt/synthetic/conf/local.json", "/opt/synthetic/conf/extra.json"}
	if len(got) != len(want) {
		t.Fatalf("config paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("config paths = %v, want %v", got, want)
		}
	}
}

// FALSIFIER (a wrong answer is worse than no answer): a tool is reached THROUGH
// the gateway, not by dialing its server's endpoint. If the tool inherited
// Endpoint, a harness would be told to dial an address that does not serve that
// tool -- confidently wrong, and it would still look like progress on the
// reachability number. Delete the comment-guarded split in the tool Entry (copy
// serverConn wholesale) and this goes red.
func TestMCPToolDoesNotInheritItsServersEndpoint(t *testing.T) {
	topo, err := LoadMCPorterCatalog(writeMCPorterFixture(t), tTopo)
	if err != nil {
		t.Fatal(err)
	}
	// The node ID is slugged (fetch_thing -> fetch-thing) because ids are graph
	// identifiers. The INVOCATION is not: mcporter is called with the server and
	// tool's real names. Asserting both in one place pins the distinction, because
	// deriving the command from the id would produce a command that does not run
	// and would look correct in every report.
	tool := entryByID(t, topo, "tool:remote-svc.fetch-thing")
	if tool.Connection == nil {
		t.Fatal("mcp tool imported with no connection facts")
	}
	if tool.Connection.Endpoint != "" {
		t.Errorf("tool inherited endpoint %q from its server; it is not separately dialable", tool.Connection.Endpoint)
	}
	if got, want := tool.Connection.LaunchCommand, "mcporter call remote-svc.fetch_thing"; got != want {
		t.Errorf("tool launch command = %q, want the gateway invocation %q (real name, not the slugged id)", got, want)
	}
	// Config paths DO belong to the tool: they describe the server it lives in,
	// which is where you go when the tool is missing.
	if len(tool.Connection.ConfigPaths) != 1 || tool.Connection.ConfigPaths[0] != "/opt/synthetic/conf/remote.json" {
		t.Errorf("tool config paths = %v, want its server's", tool.Connection.ConfigPaths)
	}
}

// FALSIFIER (end to end, against the projection that actually reports the gap):
// importing a catalog must move `mcp` and `tool` OFF zero reachability. This is
// the guarantee the whole slice exists to produce, and it is asserted through
// ComputeReachability rather than by reading the sidecar, because the sidecar
// being written is not the same claim as the report changing. Drop the
// AddConnectionSurface call in Apply and this goes red while every unit test
// above stays green.
func TestImportMovesMCPAndToolOffZeroReachability(t *testing.T) {
	home := t.TempDir()
	topo, err := LoadMCPorterCatalog(writeMCPorterFixture(t), tTopo)
	if err != nil {
		t.Fatal(err)
	}

	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ComputeReachability()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range before.Kinds {
		if (k.Kind == "mcp" || k.Kind == "tool") && k.Reachable != 0 {
			t.Fatalf("precondition broken: %s already reachable=%d on an empty home", k.Kind, k.Reachable)
		}
	}

	report, err := Apply(home, topo, tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if report.ConnectionSurfacesAdded != 3 {
		t.Errorf("connection surfaces added = %d, want 3 (2 servers + 1 tool)", report.ConnectionSurfacesAdded)
	}

	after, err := store.ComputeReachability()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, k := range after.Kinds {
		seen[k.Kind] = k.Reachable
	}
	if seen["mcp"] != 2 {
		t.Errorf("mcp reachable = %d, want 2 — the import door did not move the number it exists to move", seen["mcp"])
	}
	if seen["tool"] != 1 {
		t.Errorf("tool reachable = %d, want 1", seen["tool"])
	}
}

// FALSIFIER: a re-import with unchanged sources must add nothing while still
// reporting every id it covers. Reporting only one of those numbers makes
// "idempotent" and "did nothing" indistinguishable.
func TestReimportWithUnchangedSourcesAddsNoDuplicateSurfaces(t *testing.T) {
	home := t.TempDir()
	topo, err := LoadMCPorterCatalog(writeMCPorterFixture(t), tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(home, topo, tTopo); err != nil {
		t.Fatal(err)
	}
	second, err := Apply(home, topo, tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if second.ConnectionSurfacesAdded != 0 {
		t.Errorf("second import added %d surfaces, want 0", second.ConnectionSurfacesAdded)
	}
	if len(second.ConnectionSurfaceIDs) != 3 {
		t.Errorf("second import covered %d ids, want 3 — coverage must still be reported", len(second.ConnectionSurfaceIDs))
	}

	store, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(surfaces) != 3 {
		t.Errorf("sidecar holds %d surfaces after two imports, want 3", len(surfaces))
	}
}

// FALSIFIER (no free reachability): an entry carrying no access facts must mint
// nothing. Minting an empty record would raise the reachability percentage
// without making one capability more reachable -- the map becoming the work, and
// the exact way this metric could be gamed by its own import door.
func TestEntryWithoutAccessFactsMintsNoSurface(t *testing.T) {
	home := t.TempDir()
	topo := validTopology()
	for i := range topo.Entries {
		topo.Entries[i].Connection = nil
	}
	report, err := Apply(home, topo, tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if report.ConnectionSurfacesAdded != 0 || len(report.ConnectionSurfaceIDs) != 0 {
		t.Fatalf("factless entries minted surfaces: added=%d ids=%v", report.ConnectionSurfacesAdded, report.ConnectionSurfaceIDs)
	}

	// An all-empty connection struct is the subtler version of the same bug: it
	// is non-nil, so a nil check alone would let it through.
	topo.Entries[0].Connection = &EntryConnection{}
	empty, err := Apply(home, topo, tTopo)
	if err != nil {
		t.Fatal(err)
	}
	if empty.ConnectionSurfacesAdded != 0 {
		t.Fatalf("an empty (non-nil) connection minted a surface: added=%d", empty.ConnectionSurfacesAdded)
	}
}
