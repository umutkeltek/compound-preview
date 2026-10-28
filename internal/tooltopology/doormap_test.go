package tooltopology

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var doorNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// syntheticDoorReport mirrors the door-doctor report shape with invented doors.
const syntheticDoorReport = `[
 {"door":"web","status":"PASS","detail":"results returned","use":"websearch \"<query>\"; webfetch <url>"},
 {"door":"store-api","status":"PASS","detail":"key answers","use":"cd <app repo> && env -u HTTPS_PROXY store-cli apps list"},
 {"door":"gui","status":"HUMAN","detail":"helper lacks a grant: Accessibility","use":"Operator: grant it in system settings"},
 {"door":"devices","status":"FAIL","detail":"2 paired device(s), all unavailable","use":"plug in and unlock"}
]`

func dmWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseDoorDoctorNormalizesAndRejectsEmpty(t *testing.T) {
	doors, err := ParseDoorDoctor([]byte(syntheticDoorReport))
	if err != nil {
		t.Fatal(err)
	}
	if len(doors) != 4 || doors[0].Status != StatusPass || doors[2].Status != StatusHuman || doors[3].Status != StatusFail {
		t.Fatalf("doors = %+v", doors)
	}
	odd, _ := ParseDoorDoctor([]byte(`[{"door":"x","status":"MAYBE"}]`))
	if odd[0].Status != StatusUnknown {
		t.Fatalf("unrecognized status must become unknown, got %q", odd[0].Status)
	}
	for _, bad := range []string{`[]`, `{}`, `not json`, `[{"status":"PASS"}]`} {
		if _, err := ParseDoorDoctor([]byte(bad)); err == nil {
			t.Fatalf("report %q must be rejected, a failed source must not read as 'no doors'", bad)
		}
	}
}

func TestScanMCPReadsNamesOnlyNeverCredentials(t *testing.T) {
	dir := t.TempDir()
	dmWriteFile(t, filepath.Join(dir, "a.json"), `{"mcpServers":{"alpha":{"command":"x","env":{"API_TOKEN":"sekret-value-1"}},"beta":{"url":"https://h.example/mcp?key=sekret-value-2"}},"projects":{"/p":{"mcpServers":{"gamma":{}}}}}`)
	dmWriteFile(t, filepath.Join(dir, "c.toml"), "model = \"m\"\n[mcp_servers.delta]\ncommand = \"d\"\n[mcp_servers.delta.env]\nTOKEN = \"sekret-value-3\"\n[mcp_servers.\"eps-1\"]\nurl = \"https://h.example\"\n")
	dmWriteFile(t, filepath.Join(dir, "o.json"), `{"mcp":{"zeta":{"type":"local"}}}`)
	sets, gaps := ScanMCP([]MCPSource{
		{Harness: "h1", Kind: MCPKindJSONServers, Path: filepath.Join(dir, "a.json"), Display: "~/a.json"},
		{Harness: "h2", Kind: MCPKindTOMLServers, Path: filepath.Join(dir, "c.toml"), Display: "~/c.toml"},
		{Harness: "h3", Kind: MCPKindJSONMCP, Path: filepath.Join(dir, "o.json"), Display: "~/o.json"},
		{Harness: "h4", Kind: MCPKindJSONServers, Path: filepath.Join(dir, "absent.json")},
	})
	got := map[string]string{}
	for _, s := range sets {
		got[s.Harness] = strings.Join(s.Servers, ",")
	}
	if got["h1"] != "alpha,beta" || got["h2"] != "delta,eps-1" || got["h3"] != "zeta" {
		t.Fatalf("servers = %v", got)
	}
	if _, ok := got["h4"]; ok {
		t.Fatal("a missing config is a quiet skip, not an empty server set")
	}
	if len(gaps) != 1 || !strings.Contains(gaps[0], "project-scoped") {
		t.Fatalf("project-scoped servers must be reported as a gap, got %v", gaps)
	}
	blob := fmt.Sprintf("%+v %v", sets, gaps)
	for _, secret := range []string{"sekret-value-1", "sekret-value-2", "sekret-value-3", "API_TOKEN"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("snapshot leaked %q", secret)
		}
	}
}

func TestScanMCPMalformedSourceIsAGapNotEmpty(t *testing.T) {
	dir := t.TempDir()
	dmWriteFile(t, filepath.Join(dir, "bad.json"), `{"mcpServers": [`)
	sets, gaps := ScanMCP([]MCPSource{{Harness: "h", Kind: MCPKindJSONServers, Path: filepath.Join(dir, "bad.json")}})
	if len(sets) != 0 || len(gaps) != 1 {
		t.Fatalf("sets=%v gaps=%v", sets, gaps)
	}
}

func TestResolveVersionsWithoutExecuting(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	// A symlink into a versioned install dir.
	real := filepath.Join(root, "cask", "tool-a", "1.4.2", "bin", "tool-a")
	dmWriteFile(t, real, "#!/bin/sh\ntouch "+filepath.Join(root, "EXECUTED")+"\n")
	os.Chmod(real, 0o755)
	os.MkdirAll(bin, 0o755)
	if err := os.Symlink(real, filepath.Join(bin, "tool-a")); err != nil {
		t.Fatal(err)
	}
	// A wrapper script resolved through a vendor version-pin directory.
	dmWriteFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\ntouch "+filepath.Join(root, "EXECUTED")+"\n")
	os.Chmod(filepath.Join(bin, "claude"), 0o755)
	for _, v := range []string{"2.9.0", "2.10.1", "2.10.0"} {
		dmWriteFile(t, filepath.Join(root, ".local/share/claude/versions", v), "x")
	}
	// An unversioned binary falls back to a fingerprint.
	dmWriteFile(t, filepath.Join(bin, "tool-b"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(bin, "tool-b"), 0o755)

	hs := ResolveVersions([]BinarySpec{{"a", "tool-a"}, {"claude_code", "claude"}, {"b", "tool-b"}, {"c", "tool-missing"}}, []string{bin}, root)
	want := []string{"1.4.2", "2.10.1", "", "not-found"}
	for i, h := range hs {
		if want[i] != "" && h.Version != want[i] {
			t.Fatalf("%s version = %q, want %q", h.Harness, h.Version, want[i])
		}
	}
	if !strings.HasPrefix(hs[2].Version, "unversioned@") {
		t.Fatalf("unversioned binary should fingerprint, got %q", hs[2].Version)
	}
	if _, err := os.Stat(filepath.Join(root, "EXECUTED")); err == nil {
		t.Fatal("probe executed a binary; it must only observe")
	}
}

func baseInputs(t *testing.T, root string) Inputs {
	t.Helper()
	cfg := filepath.Join(root, "cfg.json")
	dmWriteFile(t, cfg, `{"mcpServers":{"alpha":{}}}`)
	bin := filepath.Join(root, "bin")
	real := filepath.Join(root, "cask", "tool-a", "1.0.0", "bin", "tool-a")
	dmWriteFile(t, real, "x")
	os.Chmod(real, 0o755)
	os.MkdirAll(bin, 0o755)
	os.Symlink(real, filepath.Join(bin, "tool-a"))
	return Inputs{
		DoorsJSON:       []byte(syntheticDoorReport),
		DoorsObservedAt: doorNow.Add(-time.Hour),
		MCPSources:      []MCPSource{{Harness: "h1", Kind: MCPKindJSONServers, Path: cfg, Display: "~/cfg.json"}},
		Binaries:        []BinarySpec{{"a", "tool-a"}},
		PathDirs:        []string{bin},
		HomeDir:         root,
		Models:          []ModelLane{{Launcher: "lane-one", Route: "vendor/model-1", Harness: "a"}},
		ModelsObserved:  true,
	}
}

func TestRefreshChangedSinceLastTracksVersionAndMCPAndDoorChanges(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	in := baseInputs(t, root)
	first, err := Refresh(home, in, doorNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ChangedSinceLast) != 0 {
		t.Fatalf("first snapshot has nothing to diff against, got %v", first.ChangedSinceLast)
	}
	// Second snapshot: version string changes, MCP server added, door flips, model moves.
	if err := os.Rename(filepath.Join(root, "cask", "tool-a", "1.0.0"), filepath.Join(root, "cask", "tool-a", "1.1.0")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(root, "bin", "tool-a"))
	os.Symlink(filepath.Join(root, "cask", "tool-a", "1.1.0", "bin", "tool-a"), filepath.Join(root, "bin", "tool-a"))
	dmWriteFile(t, filepath.Join(root, "cfg.json"), `{"mcpServers":{"alpha":{},"beta":{}}}`)
	in.DoorsJSON = []byte(strings.Replace(syntheticDoorReport, `"door":"web","status":"PASS"`, `"door":"web","status":"FAIL"`, 1))
	in.Models = []ModelLane{{Launcher: "lane-one", Route: "vendor/model-2", Harness: "a"}}
	second, err := Refresh(home, in, doorNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a version 1.0.0->1.1.0", "mcp beta added to h1", "door web pass->fail", "launcher lane-one model vendor/model-1->vendor/model-2"}
	joined := strings.Join(second.ChangedSinceLast, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Fatalf("changed_since_last missing %q in:\n%s", w, joined)
		}
	}
	// A third refresh that observes nothing new keeps the diff empty but remembers recent changes.
	third, err := Refresh(home, in, doorNow.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(third.ChangedSinceLast) != 0 {
		t.Fatalf("no change since second snapshot, got %v", third.ChangedSinceLast)
	}
	if len(third.RecentChanges) != len(want) {
		t.Fatalf("recent changes must roll forward 72h, got %+v", third.RecentChanges)
	}
	// ...and forgets them after 72h.
	fourth, _ := Refresh(home, in, doorNow.Add(80*time.Hour))
	if len(fourth.RecentChanges) != 0 {
		t.Fatalf("changes older than 72h must age out, got %+v", fourth.RecentChanges)
	}
	if _, err := os.Stat(doorMapPath(home, doorMapPrevious)); err != nil {
		t.Fatalf("previous snapshot must be kept: %v", err)
	}
}

func TestRefreshWithoutDoorReportCarriesDoorsWithOriginalObservationTime(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	in := baseInputs(t, root)
	if _, err := Refresh(home, in, doorNow); err != nil {
		t.Fatal(err)
	}
	in.DoorsJSON, in.DoorsObservedAt = nil, time.Time{}
	in.ModelsObserved = false
	m, err := Refresh(home, in, doorNow.Add(30*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Doors) != 4 || m.DoorsObservedAt != doorNow.Add(-time.Hour).Format(time.RFC3339) {
		t.Fatalf("doors must carry forward unchanged with their original time: %+v %s", m.Doors, m.DoorsObservedAt)
	}
	if len(m.Models) != 1 {
		t.Fatalf("models must carry forward, got %v", m.Models)
	}
	if got := ServeDoorMap(home, doorNow.Add(30*time.Hour), DefaultDoorMapTTL); !strings.Contains(got, "STALE") {
		t.Fatalf("a refresh must not launder old door statuses into fresh ones:\n%s", got)
	}
}

func TestServeDoorMapMissingIsAVisibleNotice(t *testing.T) {
	got := ServeDoorMap(t.TempDir(), doorNow, DefaultDoorMapTTL)
	if !strings.Contains(got, "Door map") || !strings.Contains(got, "none produced yet") {
		t.Fatalf("missing snapshot must be announced, got %q", got)
	}
}

func TestServeDoorMapCorruptIsAVisibleNotice(t *testing.T) {
	home := t.TempDir()
	dmWriteFile(t, SnapshotPath(home), `{"schema_version":"other"}`)
	if got := ServeDoorMap(home, doorNow, DefaultDoorMapTTL); !strings.Contains(got, "unreadable") {
		t.Fatalf("got %q", got)
	}
}

func TestServeDoorMapFreshContainsDoorsRecipesAndChanges(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	in := baseInputs(t, root)
	if _, err := Refresh(home, in, doorNow); err != nil {
		t.Fatal(err)
	}
	dmWriteFile(t, filepath.Join(root, "cfg.json"), `{"mcpServers":{"alpha":{},"beta":{}}}`)
	if _, err := Refresh(home, in, doorNow.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := ServeDoorMap(home, doorNow.Add(2*time.Hour), DefaultDoorMapTTL)
	for _, want := range []string{"## Door map", "doors checked 3h ago", "PASS web: websearch", "HUMAN gui:", "FAIL devices:", "MCP: h1[alpha,beta]", "Models: lane-one=vendor/model-1", "Changed: mcp beta added to h1 (1h)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("served block missing %q:\n%s", want, got)
		}
	}
	// Harness versions stay in `compound doors show`; the served block is for acting.
	if strings.Contains(got, "Versions:") {
		t.Fatalf("served block must not carry the harness Versions line:\n%s", got)
	}
	if len(got) > MaxServedBytes {
		t.Fatalf("served block is %d bytes, cap %d", len(got), MaxServedBytes)
	}
}

func TestServeDoorMapStaleBeyondTTLWithholdsStatuses(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if _, err := Refresh(home, baseInputs(t, root), doorNow); err != nil {
		t.Fatal(err)
	}
	// doors observed 1h before doorNow; TTL 24h.
	fresh := ServeDoorMap(home, doorNow.Add(22*time.Hour), DefaultDoorMapTTL)
	if strings.Contains(fresh, "STALE") || !strings.Contains(fresh, "PASS web") {
		t.Fatalf("inside TTL must serve statuses:\n%s", fresh)
	}
	stale := ServeDoorMap(home, doorNow.Add(26*time.Hour), DefaultDoorMapTTL)
	if !strings.Contains(stale, "STALE") || strings.Contains(stale, "PASS web") || !strings.Contains(stale, "compound doors refresh") {
		t.Fatalf("beyond TTL must withhold statuses and say how to refresh:\n%s", stale)
	}
}

func TestRenderDoorMapNeverExceedsCapUnderWorstCase(t *testing.T) {
	long := strings.Repeat("longword ", 60)
	m := DoorMap{SchemaVersion: DoorMapSchema, GeneratedAt: doorNow.Format(time.RFC3339)}
	for i := 0; i < 40; i++ {
		st := StatusPass
		if i%5 == 0 {
			st = StatusFail
		}
		m.Doors = append(m.Doors, Door{Name: fmt.Sprintf("door-%02d", i), Status: st, Detail: long, Use: long})
	}
	for h := 0; h < 6; h++ {
		var names []string
		for i := 0; i < 40; i++ {
			names = append(names, fmt.Sprintf("server-%d-%02d", h, i))
		}
		m.MCP = append(m.MCP, MCPSet{Harness: fmt.Sprintf("harness-%d", h), Servers: names})
		m.Harnesses = append(m.Harnesses, HarnessVersion{Harness: fmt.Sprintf("harness-%d", h), Version: "10.20.30-beta.1"})
	}
	for i := 0; i < 50; i++ {
		m.Models = append(m.Models, ModelLane{Launcher: fmt.Sprintf("launcher-%d", i), Route: "vendor/model-name-long"})
		m.RecentChanges = append(m.RecentChanges, Change{What: long, At: doorNow.Format(time.RFC3339)})
	}
	got := RenderDoorMap(m, doorNow, doorNow)
	if len(got) > MaxServedBytes {
		t.Fatalf("worst case rendered %d bytes, cap %d:\n%s", len(got), MaxServedBytes, got)
	}
	if !strings.Contains(got, "FAIL door-00") || !strings.Contains(got, "+") {
		t.Fatalf("failing doors must survive truncation and omissions must be counted:\n%s", got)
	}
	// Multi-byte content must never be cut mid-rune.
	m.Doors[0].Detail = strings.Repeat("é", 400)
	if out := RenderDoorMap(m, doorNow, doorNow); !strings.HasSuffix(out, "") || strings.ToValidUTF8(out, "?") != out {
		t.Fatal("served block is not valid UTF-8 after truncation")
	}
}

func TestRealisticSnapshotFitsWithMostRecipesVisible(t *testing.T) {
	var doors []Door
	for _, n := range []string{"disk", "web", "asc-api", "asc-web", "applescript", "gui", "simulator", "storekit", "devices", "gcloud", "browser", "browser-auth", "images", "helpers"} {
		doors = append(doors, Door{Name: n, Status: StatusPass, Detail: "d", Use: "run the standard command for " + n + " with the usual arguments and flags"})
	}
	m := DoorMap{SchemaVersion: DoorMapSchema, GeneratedAt: doorNow.Format(time.RFC3339), Doors: doors,
		MCP: []MCPSet{{Harness: "claude_code", Servers: []string{"a", "b"}}, {Harness: "codex", Servers: []string{"c", "d", "e"}}}}
	got := RenderDoorMap(m, doorNow, doorNow)
	if len(got) > MaxServedBytes || strings.Count(got, "PASS ") < 10 {
		t.Fatalf("len=%d recipes=%d\n%s", len(got), strings.Count(got, "PASS "), got)
	}
}

// A machine shaped like the operator's: three non-passing doors, twelve
// passing doors whose recipes run up to ~200 bytes, several MCP sets, harness
// versions, fifteen model lanes and recent changes. Every recipe must reach the
// harness whole — a recipe cut by "…" is a door the harness rediscovers by hand.
func TestLiveShapedSnapshotServesEveryRecipeWhole(t *testing.T) {
	recipe := func(name string, n int) string {
		s := "tool-" + name + " --flag <value>"
		for len(s) < n {
			s += " then step"
		}
		return s[:n]
	}
	m := DoorMap{SchemaVersion: DoorMapSchema, GeneratedAt: doorNow.Format(time.RFC3339),
		Doors: []Door{
			{Name: "disk", Status: StatusFail, Detail: strings.Repeat("d", 78)},
			{Name: "store-web", Status: StatusFail, Detail: strings.Repeat("d", 67)},
			{Name: "surface", Status: StatusUnknown, Detail: strings.Repeat("d", 160)},
		}}
	for i, n := range []int{75, 110, 100, 175, 120, 185, 70, 100, 125, 55, 80, 35} {
		name := fmt.Sprintf("door-%02d", i)
		m.Doors = append(m.Doors, Door{Name: name, Status: StatusPass, Detail: "ok", Use: recipe(name, n)})
	}
	m.MCP = []MCPSet{{Harness: "h1", Servers: []string{"a", "b"}}, {Harness: "h2", Servers: []string{"c", "d", "e", "f"}}, {Harness: "bridge", Servers: strings.Fields(strings.Repeat("server ", 23))}}
	for _, h := range []string{"h1", "h2", "h3", "h4"} {
		m.Harnesses = append(m.Harnesses, HarnessVersion{Harness: h, Version: "2.1.309"})
	}
	for i := 0; i < 15; i++ {
		m.Models = append(m.Models, ModelLane{Launcher: fmt.Sprintf("lane-%02d", i), Route: "vendor/model-name"})
		m.RecentChanges = append(m.RecentChanges, Change{What: fmt.Sprintf("lane-%02d route changed", i), At: doorNow.Format(time.RFC3339)})
	}
	got := RenderDoorMap(m, doorNow, doorNow)
	if len(got) > MaxServedBytes {
		t.Fatalf("served %d bytes, cap %d:\n%s", len(got), MaxServedBytes, got)
	}
	for _, d := range m.Doors {
		if d.Status == StatusPass && !strings.Contains(got, "PASS "+d.Name+": "+d.Use+"\n") {
			t.Fatalf("recipe for %s not served whole:\n%s", d.Name, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "PASS") && strings.HasSuffix(line, "…") {
			t.Fatalf("PASS line cut: %q", line)
		}
	}
	if strings.Contains(got, "Versions:") || !strings.Contains(got, "FAIL disk:") || !strings.Contains(got, "UNKNOWN surface:") {
		t.Fatalf("block must keep non-passing doors and drop harness versions:\n%s", got)
	}
}

// TestNonPassLineIsNotCutWhileBudgetRemains: a non-PASS line carries the cause
// the reader must act on. It may be cut only when the served block has reached
// its cap; a cut line next to unused bytes is a cause hidden for nothing.
func TestNonPassLineIsNotCutWhileBudgetRemains(t *testing.T) {
	cases := map[string]DoorMap{
		"short block": {SchemaVersion: DoorMapSchema, GeneratedAt: doorNow.Format(time.RFC3339), Doors: []Door{
			{Name: "surface", Status: StatusUnknown, Detail: strings.Repeat("cause ", 40)},
			{Name: "web", Status: StatusPass, Use: "search-cli <query>"},
		}},
		"long cause in a full block": {SchemaVersion: DoorMapSchema, GeneratedAt: doorNow.Format(time.RFC3339), Doors: []Door{
			{Name: "disk", Status: StatusFail, Detail: strings.Repeat("d", 78)},
			{Name: "surface", Status: StatusUnknown, Detail: strings.Repeat("cause ", 400)},
		}},
	}
	for i := 0; i < 12; i++ {
		m := cases["long cause in a full block"]
		m.Doors = append(m.Doors, Door{Name: fmt.Sprintf("door-%02d", i), Status: StatusPass, Use: strings.Repeat("step ", 24)})
		cases["long cause in a full block"] = m
	}
	// Several cut lines of uneven length near the cap: one that needs a few
	// bytes must not strand the share the longer ones could still use.
	for pass := 8; pass <= 16; pass += 4 {
		m := DoorMap{SchemaVersion: DoorMapSchema, GeneratedAt: doorNow.Format(time.RFC3339), Doors: []Door{
			{Name: "near", Status: StatusFail, Detail: strings.Repeat("n", 92)},
			{Name: "mid", Status: StatusHuman, Detail: strings.Repeat("m", 130)},
			{Name: "long-a", Status: StatusUnknown, Detail: strings.Repeat("a ", 300)},
			{Name: "long-b", Status: StatusUnknown, Detail: strings.Repeat("b ", 300)},
		}}
		for i := 0; i < pass; i++ {
			m.Doors = append(m.Doors, Door{Name: fmt.Sprintf("door-%02d", i), Status: StatusPass, Use: strings.Repeat("step ", 26)})
		}
		cases[fmt.Sprintf("uneven cuts with %d recipes", pass)] = m
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			got := RenderDoorMap(m, doorNow, doorNow)
			if len(got) > MaxServedBytes {
				t.Fatalf("served %d bytes, cap %d", len(got), MaxServedBytes)
			}
			for _, line := range strings.Split(got, "\n") {
				if strings.HasPrefix(line, "PASS") || !strings.HasSuffix(line, "…") {
					continue
				}
				if unused := MaxServedBytes - len(got); unused > 8 {
					t.Fatalf("non-PASS line cut to %d bytes while %d of %d bytes are unused: %q", len(line), unused, MaxServedBytes, line)
				}
			}
		})
	}
}
