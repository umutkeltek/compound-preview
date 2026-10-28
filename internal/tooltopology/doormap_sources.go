package tooltopology

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MCP source kinds. A kind names a config SHAPE, not a product: the reader
// extracts server names only and never touches env, args, headers or URLs, so
// no credential can reach the snapshot.
const (
	MCPKindJSONServers = "json-mcpServers"  // top-level "mcpServers" object
	MCPKindJSONMCP     = "json-mcp"         // top-level "mcp" object
	MCPKindTOMLServers = "toml-mcp_servers" // [mcp_servers.<name>] tables
)

// MCPSource is one harness config that declares MCP servers.
type MCPSource struct {
	Harness string
	Kind    string
	Path    string
	// Display is the home-relative label shown to readers ("~/..."), so the
	// snapshot says where to look without embedding an absolute home path.
	Display string
}

// DefaultMCPSources lists the conventional config locations of the harnesses
// Compound serves, relative to a user home. Absent files are skipped.
func DefaultMCPSources(homeDir string) []MCPSource {
	mk := func(harness, kind, rel string) MCPSource {
		return MCPSource{Harness: harness, Kind: kind, Path: filepath.Join(homeDir, filepath.FromSlash(rel)), Display: "~/" + rel}
	}
	return []MCPSource{
		mk("claude_code", MCPKindJSONServers, ".claude.json"),
		mk("codex", MCPKindTOMLServers, ".codex/config.toml"),
		mk("opencode", MCPKindJSONMCP, ".config/opencode/opencode.json"),
		mk("mcporter", MCPKindJSONServers, ".mcporter/mcporter.json"),
	}
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ScanMCP reads server names from each source. A missing file is a quiet skip
// (the harness may not be installed); an unreadable or unparseable one is a gap.
func ScanMCP(sources []MCPSource) ([]MCPSet, []string) {
	var sets []MCPSet
	var gaps []string
	for _, src := range sources {
		label := src.Display
		if label == "" {
			label = filepath.Base(src.Path)
		}
		names, extra, err := readMCPNames(src)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			gaps = append(gaps, fmt.Sprintf("mcp %s: unreadable (%s)", src.Harness, truncBytes(cleanField(err.Error(), 80), 80)))
			continue
		}
		if extra != "" {
			gaps = append(gaps, fmt.Sprintf("mcp %s: %s", src.Harness, extra))
		}
		sets = append(sets, MCPSet{Harness: src.Harness, Source: label, Servers: names})
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].Harness < sets[j].Harness })
	return sets, gaps
}

func readMCPNames(src MCPSource) (names []string, note string, err error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	switch src.Kind {
	case MCPKindTOMLServers:
		return scanTOMLServers(f), "", nil
	case MCPKindJSONServers, MCPKindJSONMCP:
		key := "mcpServers"
		if src.Kind == MCPKindJSONMCP {
			key = "mcp"
		}
		var doc map[string]json.RawMessage
		if err := json.NewDecoder(f).Decode(&doc); err != nil {
			return nil, "", fmt.Errorf("parse json: %w", err)
		}
		var servers map[string]json.RawMessage
		if raw, ok := doc[key]; ok {
			if err := json.Unmarshal(raw, &servers); err != nil {
				return nil, "", fmt.Errorf("%s is not an object", key)
			}
		}
		for name := range servers {
			if safeName.MatchString(name) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		if raw, ok := doc["projects"]; ok && key == "mcpServers" {
			var projects map[string]struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			}
			if json.Unmarshal(raw, &projects) == nil {
				n := 0
				for _, p := range projects {
					if len(p.MCPServers) > 0 {
						n++
					}
				}
				if n > 0 {
					note = strconv.Itoa(n) + " project-scoped config(s) with servers not listed"
				}
			}
		}
		return names, note, nil
	}
	return nil, "", fmt.Errorf("unknown source kind %q", src.Kind)
}

var tomlServerHeader = regexp.MustCompile(`^\[mcp_servers\.(?:"([^"]+)"|([A-Za-z0-9_-]+))\]\s*(?:#.*)?$`)

func scanTOMLServers(f *os.File) []string {
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		m := tomlServerHeader.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if safeName.MatchString(name) {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// BinarySpec maps a harness name to the executable that launches it.
type BinarySpec struct {
	Harness string
	Binary  string
}

// DefaultBinaries are the harness executables Compound serves.
var DefaultBinaries = []BinarySpec{
	{"claude_code", "claude"},
	{"codex", "codex"},
	{"opencode", "opencode"},
	{"oh_my_pi", "omp"},
}

// vendorVersionDirs are conventional version-pin directories, relative to a
// user home, used when the executable on PATH is a wrapper rather than a
// versioned install.
var vendorVersionDirs = map[string]string{
	"claude": ".local/share/claude/versions",
}

// ResolveVersions infers each harness version WITHOUT executing the binary:
// a version in the resolved install path, a nearby package.json, a vendor
// version-pin directory, else an "unversioned@<mtime>:<size>" fingerprint that
// still changes when the binary is replaced.
func ResolveVersions(specs []BinarySpec, pathDirs []string, homeDir string) []HarnessVersion {
	out := make([]HarnessVersion, 0, len(specs))
	for _, s := range specs {
		out = append(out, HarnessVersion{Harness: s.Harness, Binary: s.Binary, Version: resolveVersion(s.Binary, pathDirs, homeDir)})
	}
	return out
}

func resolveVersion(bin string, pathDirs []string, homeDir string) string {
	var found string
	for _, d := range pathDirs {
		p := filepath.Join(d, bin)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			found = p
			break
		}
	}
	if found == "" {
		return "not-found"
	}
	real, err := filepath.EvalSymlinks(found)
	if err != nil {
		real = found
	}
	if v := semverInPath(real); v != "" {
		return v
	}
	if v := packageJSONVersion(real); v != "" {
		return v
	}
	if rel, ok := vendorVersionDirs[bin]; ok && homeDir != "" {
		if v := highestSemverEntry(filepath.Join(homeDir, filepath.FromSlash(rel))); v != "" {
			return v
		}
	}
	if fi, err := os.Stat(real); err == nil {
		return fmt.Sprintf("unversioned@%s:%d", fi.ModTime().UTC().Format("2006-01-02"), fi.Size())
	}
	return "unknown"
}

func semverInPath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if m := semverRe.FindString(parts[i]); m != "" {
			return m
		}
	}
	return ""
}

func packageJSONVersion(real string) string {
	dir := filepath.Dir(real)
	for i := 0; i < 4 && dir != "/" && dir != "."; i++ {
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err == nil && len(data) < 1<<20 {
			var pkg struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &pkg) == nil && semverRe.MatchString(pkg.Version) {
				return pkg.Version
			}
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func highestSemverEntry(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestKey := "", [3]int{-1, -1, -1}
	for _, e := range entries {
		v := semverRe.FindString(e.Name())
		if v == "" {
			continue
		}
		if k := semverKey(v); k[0] > bestKey[0] || (k[0] == bestKey[0] && (k[1] > bestKey[1] || (k[1] == bestKey[1] && k[2] > bestKey[2]))) {
			best, bestKey = v, k
		}
	}
	return best
}

func semverKey(v string) [3]int {
	var k [3]int
	core := strings.SplitN(strings.SplitN(v, "-", 2)[0], "+", 2)[0]
	for i, p := range strings.SplitN(core, ".", 3) {
		k[i], _ = strconv.Atoi(p)
	}
	return k
}
