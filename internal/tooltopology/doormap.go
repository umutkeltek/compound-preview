package tooltopology

// Door map: one cached, evidence-class snapshot of "what doors, tools and MCP
// servers does this machine offer a harness, and what changed lately".
//
// The snapshot is OBSERVED, never executed: it reads files that other owners
// already publish (a door-doctor JSON report, harness MCP configs, harness
// binaries on PATH, the launch owner's lane projection) and never starts a
// door, an MCP server or a harness. It carries names and short recipes, never
// credential values: MCP configs are read for server NAMES only.
//
// The snapshot lives at COMPOUND_HOME/observed/door-map/ and is served to a
// harness at session start as a size-capped block. A stale or missing snapshot
// is served as a visible notice, never as silence and never as confident
// door status.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	// DoorMapSchema names the snapshot shape.
	DoorMapSchema = "compound.door_map.v0.1"
	// DefaultDoorMapTTL is how long door status stays trustworthy enough to
	// serve. Doors flip on login expiry and device unplugging, so a day is the
	// outer bound; beyond it the block stops asserting statuses.
	DefaultDoorMapTTL = 24 * time.Hour
	// MaxServedBytes is the hard cap on the block injected at session start.
	// It leaves room for every door recipe of a ~15-door machine uncut: a
	// recipe cut by "…" is a door the harness must rediscover by hand.
	MaxServedBytes = 3000

	doorMapDir        = "observed/door-map"
	doorMapCurrent    = "snapshot.json"
	doorMapPrevious   = "previous.json"
	maxDoorMapRead    = 1 << 20
	recentChangeTTL   = 72 * time.Hour
	maxRecentChanges  = 12
	maxFieldBytes     = 240
	StatusPass        = "pass"
	StatusFail        = "fail"
	StatusHuman       = "human"
	StatusUnknown     = "unknown"
	servedRefreshHint = "compound doors refresh"
)

// Door is one standard way a harness acts on the machine, as last observed.
type Door struct {
	Name   string `json:"door"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Use    string `json:"use,omitempty"`
}

// MCPSet is the MCP server NAMES one harness (or bridge) config declares.
type MCPSet struct {
	Harness string   `json:"harness"`
	Source  string   `json:"source"`
	Servers []string `json:"servers"`
}

// HarnessVersion is a harness binary version inferred WITHOUT running it.
type HarnessVersion struct {
	Harness string `json:"harness"`
	Binary  string `json:"binary"`
	Version string `json:"version"`
}

// ModelLane is one launcher's route (vendor/model) from the launch owner's projection.
type ModelLane struct {
	Launcher string `json:"launcher"`
	Route    string `json:"route"`
	Harness  string `json:"harness,omitempty"`
}

// Change is one observed difference, stamped with when it was first seen.
type Change struct {
	What string `json:"what"`
	At   string `json:"at"`
}

// DoorMap is the whole snapshot.
type DoorMap struct {
	SchemaVersion   string           `json:"schema_version"`
	GeneratedAt     string           `json:"generated_at"`
	DoorsObservedAt string           `json:"doors_observed_at,omitempty"`
	Doors           []Door           `json:"doors"`
	MCP             []MCPSet         `json:"mcp"`
	Harnesses       []HarnessVersion `json:"harnesses"`
	Models          []ModelLane      `json:"models"`
	// ChangedSinceLast is the pure diff against the immediately previous snapshot.
	ChangedSinceLast []string `json:"changed_since_last"`
	// RecentChanges rolls ChangedSinceLast forward for 72h so a refresh that
	// observes nothing new does not erase the fact that something changed.
	RecentChanges []Change `json:"recent_changes,omitempty"`
	// Gaps names what this snapshot could not observe.
	Gaps []string `json:"gaps,omitempty"`
}

// ParseDoorDoctor adapts a door-doctor JSON report (an array of
// {door,status,detail,use}) into Doors. Unknown statuses become "unknown"
// rather than being guessed. An empty or malformed report is an error: a
// snapshot must not claim "no doors" when the source simply failed.
func ParseDoorDoctor(data []byte) ([]Door, error) {
	var rows []struct {
		Door   string `json:"door"`
		Status string `json:"status"`
		Detail string `json:"detail"`
		Use    string `json:"use"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parse door report: %w", err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("door report has no doors")
	}
	doors := make([]Door, 0, len(rows))
	for _, r := range rows {
		name := cleanField(r.Door, 48)
		if name == "" {
			return nil, fmt.Errorf("door report row without a door name")
		}
		doors = append(doors, Door{
			Name:   name,
			Status: normalizeStatus(r.Status),
			Detail: cleanField(r.Detail, maxFieldBytes),
			Use:    cleanField(r.Use, maxFieldBytes),
		})
	}
	return doors, nil
}

func normalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pass", "ok":
		return StatusPass
	case "fail":
		return StatusFail
	case "human":
		return StatusHuman
	}
	return StatusUnknown
}

// cleanField removes control characters, collapses whitespace, and caps bytes
// on a rune boundary so a hostile or noisy report cannot reshape the block.
func cleanField(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return truncBytes(s, max)
}

func truncBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	const ell = "…"
	if max <= len(ell) {
		return ""
	}
	cut := max - len(ell)
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return strings.TrimRight(s[:cut], " ") + ell
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Diff lists human-readable differences between two snapshots. prev may be nil.
func Diff(prev *DoorMap, next DoorMap) []string {
	if prev == nil {
		return nil
	}
	var out []string
	pd := map[string]Door{}
	for _, d := range prev.Doors {
		pd[d.Name] = d
	}
	nd := map[string]Door{}
	for _, d := range next.Doors {
		nd[d.Name] = d
		if old, ok := pd[d.Name]; !ok {
			out = append(out, fmt.Sprintf("door %s appeared (%s)", d.Name, d.Status))
		} else if old.Status != d.Status {
			out = append(out, fmt.Sprintf("door %s %s->%s", d.Name, old.Status, d.Status))
		}
	}
	for _, d := range prev.Doors {
		if _, ok := nd[d.Name]; !ok {
			out = append(out, fmt.Sprintf("door %s disappeared", d.Name))
		}
	}
	out = append(out, diffMCP(prev.MCP, next.MCP)...)
	pv := map[string]string{}
	for _, h := range prev.Harnesses {
		pv[h.Harness] = h.Version
	}
	for _, h := range next.Harnesses {
		if old, ok := pv[h.Harness]; ok && old != h.Version {
			out = append(out, fmt.Sprintf("%s version %s->%s", h.Harness, old, h.Version))
		}
	}
	pm := map[string]string{}
	for _, m := range prev.Models {
		pm[m.Launcher] = m.Route
	}
	nm := map[string]string{}
	for _, m := range next.Models {
		nm[m.Launcher] = m.Route
		if old, ok := pm[m.Launcher]; !ok {
			out = append(out, fmt.Sprintf("launcher %s appeared (%s)", m.Launcher, m.Route))
		} else if old != m.Route {
			out = append(out, fmt.Sprintf("launcher %s model %s->%s", m.Launcher, old, m.Route))
		}
	}
	for l := range pm {
		if _, ok := nm[l]; !ok {
			out = append(out, fmt.Sprintf("launcher %s disappeared", l))
		}
	}
	sort.Strings(out)
	return out
}

func diffMCP(prev, next []MCPSet) []string {
	index := func(sets []MCPSet) map[string]map[string]bool {
		m := map[string]map[string]bool{}
		for _, s := range sets {
			set := map[string]bool{}
			for _, n := range s.Servers {
				set[n] = true
			}
			m[s.Harness] = set
		}
		return m
	}
	p, n := index(prev), index(next)
	var out []string
	for h, set := range n {
		for name := range set {
			if !p[h][name] {
				out = append(out, fmt.Sprintf("mcp %s added to %s", name, h))
			}
		}
	}
	for h, set := range p {
		for name := range set {
			if !n[h][name] {
				out = append(out, fmt.Sprintf("mcp %s removed from %s", name, h))
			}
		}
	}
	return out
}

// MergeRecent carries recent changes forward and stamps new ones.
func MergeRecent(prev *DoorMap, changed []string, now time.Time) []Change {
	seen := map[string]bool{}
	var out []Change
	stamp := now.UTC().Format(time.RFC3339)
	for _, w := range changed {
		seen[w] = true
		out = append(out, Change{What: w, At: stamp})
	}
	if prev != nil {
		for _, c := range prev.RecentChanges {
			t, err := time.Parse(time.RFC3339, c.At)
			if err != nil || now.Sub(t) > recentChangeTTL || seen[c.What] {
				continue
			}
			seen[c.What] = true
			out = append(out, c)
		}
	}
	if len(out) > maxRecentChanges {
		out = out[:maxRecentChanges]
	}
	return out
}

// Dir returns the snapshot directory under a Compound home.
func doorMapPath(home, name string) string {
	return filepath.Join(home, filepath.FromSlash(doorMapDir), name)
}

// SnapshotPath is the drilldown path served in the block.
func SnapshotPath(home string) string { return doorMapPath(home, doorMapCurrent) }

// LoadDoorMap reads the current snapshot. os.ErrNotExist is returned (wrapped)
// when none was produced yet.
func LoadDoorMap(home string) (DoorMap, error) {
	return readDoorMap(doorMapPath(home, doorMapCurrent))
}

func readDoorMap(path string) (DoorMap, error) {
	f, err := os.Open(path)
	if err != nil {
		return DoorMap{}, err
	}
	defer f.Close()
	var m DoorMap
	dec := json.NewDecoder(&limitedReader{r: f, n: maxDoorMapRead})
	if err := dec.Decode(&m); err != nil {
		return DoorMap{}, fmt.Errorf("parse door map %s: %w", path, err)
	}
	if m.SchemaVersion != DoorMapSchema {
		return DoorMap{}, fmt.Errorf("door map %s: unsupported schema %q", path, m.SchemaVersion)
	}
	return m, nil
}

type limitedReader struct {
	r interface{ Read([]byte) (int, error) }
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, fmt.Errorf("door map exceeds %d bytes", maxDoorMapRead)
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

// WriteDoorMap rotates the current snapshot to previous.json and replaces it
// atomically (temp file + rename), so a concurrent session-start never reads a
// torn file.
func WriteDoorMap(home string, m DoorMap) error {
	dir := filepath.Join(home, filepath.FromSlash(doorMapDir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	cur := doorMapPath(home, doorMapCurrent)
	if _, err := os.Stat(cur); err == nil {
		_ = os.Rename(cur, doorMapPath(home, doorMapPrevious))
	}
	if err := os.Rename(tmpName, cur); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Inputs are everything Refresh observes. All of it is data already published
// by another owner; nothing here is executed.
type Inputs struct {
	// DoorsJSON is a door-doctor report; nil carries the previous doors forward
	// with their ORIGINAL observation time (so staleness keeps counting).
	DoorsJSON       []byte
	DoorsObservedAt time.Time
	MCPSources      []MCPSource
	Binaries        []BinarySpec
	PathDirs        []string
	HomeDir         string
	Models          []ModelLane
	// ModelsObserved is false when no lane projection was available, so the
	// snapshot records a gap instead of an empty fleet.
	ModelsObserved bool
}

// Refresh observes Inputs, diffs against the stored snapshot, and writes the result.
func Refresh(home string, in Inputs, now time.Time) (DoorMap, error) {
	var prev *DoorMap
	if p, err := LoadDoorMap(home); err == nil {
		prev = &p
	}
	m := DoorMap{
		SchemaVersion: DoorMapSchema,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
	}
	switch {
	case in.DoorsJSON != nil:
		doors, err := ParseDoorDoctor(in.DoorsJSON)
		if err != nil {
			return DoorMap{}, err
		}
		m.Doors = doors
		observed := in.DoorsObservedAt
		if observed.IsZero() {
			observed = now
		}
		m.DoorsObservedAt = observed.UTC().Format(time.RFC3339)
	case prev != nil && len(prev.Doors) > 0:
		m.Doors, m.DoorsObservedAt = prev.Doors, prev.DoorsObservedAt
	default:
		m.Gaps = append(m.Gaps, "doors: no door report supplied")
	}
	var mcpGaps []string
	m.MCP, mcpGaps = ScanMCP(in.MCPSources)
	m.Gaps = append(m.Gaps, mcpGaps...)
	m.Harnesses = ResolveVersions(in.Binaries, in.PathDirs, in.HomeDir)
	if in.ModelsObserved {
		m.Models = append(m.Models, in.Models...)
		sort.Slice(m.Models, func(i, j int) bool { return m.Models[i].Launcher < m.Models[j].Launcher })
	} else if prev != nil && len(prev.Models) > 0 {
		m.Models = prev.Models
	} else {
		m.Gaps = append(m.Gaps, "models: no lane projection supplied")
	}
	m.ChangedSinceLast = Diff(prev, m)
	if m.ChangedSinceLast == nil {
		m.ChangedSinceLast = []string{}
	}
	m.RecentChanges = MergeRecent(prev, m.ChangedSinceLast, now)
	if err := WriteDoorMap(home, m); err != nil {
		return DoorMap{}, err
	}
	return m, nil
}

// semverRe finds a version-looking path component or file name.
var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.]+)?`)
