package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/runtimebinding"
)

const SystemMapSchemaVersion = "compound.system_map_projection.v0.1"
const SystemStatsSchemaVersion = "compound.system_stats_projection.v0.1"

type SystemMapProjection struct {
	SchemaVersion string                    `json:"schema_version"`
	GeneratedAt   string                    `json:"generated_at"`
	SourceRefs    []string                  `json:"source_refs"`
	Nodes         []SystemMapNode           `json:"nodes"`
	Edges         []SystemMapEdge           `json:"edges"`
	Stats         map[string]SystemMapStats `json:"stats"`
}

type SystemMapNode struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Lane         string   `json:"lane"`
	Accent       string   `json:"accent,omitempty"`
	Glyph        string   `json:"glyph,omitempty"`
	Status       string   `json:"status"`
	ReviewID     string   `json:"reviewId,omitempty"`
	Projection   string   `json:"projection"`
	Volume       string   `json:"volume,omitempty"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
	// Target binds a node to the live source that hydrates it. For the
	// "host-reachability" projection it is "<bundlePath>[#<probeID>]": the bundle
	// path resolves against COMPOUND_HOME (or an absolute / ~-prefixed path), and
	// an optional "#probeID" fragment selects one probe (per-host node); with no
	// fragment the whole bundle is aggregated (e.g. "6/7 reachable"). It stays
	// product-generic: any future projection may reuse it to point at its source.
	Target string `json:"target,omitempty"`
}

type SystemMapEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Label  string `json:"label"`
	Status string `json:"status"`
}

type SystemMapStats struct {
	Live    int `json:"live"`
	NotLive int `json:"notLive"`
}

type SystemStatsProjection struct {
	SchemaVersion        string   `json:"schema_version"`
	GeneratedAt          string   `json:"generated_at"`
	Entities             int      `json:"entities"`
	Claims               int      `json:"claims"`
	Edges                int      `json:"edges"`
	Evidence             int      `json:"evidence"`
	RouteEvidence        int      `json:"route_evidence"`
	NoPromote            int      `json:"no_promote"`
	KnowledgeImports     int      `json:"knowledge_imports"`
	KnowledgeSources     int      `json:"knowledge_sources"`
	KnowledgeSourceKinds []string `json:"knowledge_source_kinds,omitempty"`
	IntakeRepos          int      `json:"intake_repos"`
}

type systemMapFile struct {
	Nodes []SystemMapNode `json:"nodes"`
	Edges []SystemMapEdge `json:"edges"`
}

func BuildSystemMap(homePath, repoPath string, now time.Time) (SystemMapProjection, error) {
	if now.IsZero() {
		now = time.Now()
	}
	file, sourceRefs, err := loadSystemMapFile(homePath, repoPath)
	if err != nil {
		return SystemMapProjection{}, err
	}
	hosts := newHostReachability(homePath, now)
	nodes := make([]SystemMapNode, 0, len(file.Nodes))
	for _, node := range file.Nodes {
		hydrateSystemMapNode(homePath, &node, hosts)
		nodes = append(nodes, node)
	}
	stats := map[string]SystemMapStats{}
	for _, lane := range []string{"capture", "storage", "meaning", "outward", "infra"} {
		stats[lane] = SystemMapStats{}
	}
	for _, node := range nodes {
		lane := strings.TrimSpace(node.Lane)
		if lane == "" {
			lane = "unknown"
		}
		s := stats[lane]
		if strings.TrimSpace(node.Status) == "live" {
			s.Live++
		} else {
			s.NotLive++
		}
		stats[lane] = s
	}
	return SystemMapProjection{
		SchemaVersion: SystemMapSchemaVersion,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		SourceRefs:    sourceRefs,
		Nodes:         nodes,
		Edges:         file.Edges,
		Stats:         stats,
	}, nil
}

func BuildSystemStats(homePath string, now time.Time) SystemStatsProjection {
	if now.IsZero() {
		now = time.Now()
	}
	ledger := filepath.Join(homePath, "ledger")
	knowledgeSources := filepath.Join(homePath, "registries", "knowledge-sources.jsonl")
	return SystemStatsProjection{
		SchemaVersion:        SystemStatsSchemaVersion,
		GeneratedAt:          now.UTC().Format(time.RFC3339),
		Entities:             countJSONLLines(filepath.Join(ledger, "entities.jsonl")),
		Claims:               countJSONLLines(filepath.Join(ledger, "claims.jsonl")),
		Edges:                countJSONLLines(filepath.Join(ledger, "edges.jsonl")),
		Evidence:             countJSONLLines(filepath.Join(ledger, "evidence.jsonl")),
		RouteEvidence:        countJSONLLines(filepath.Join(ledger, "execution-route-evidence.jsonl")),
		NoPromote:            countJSONLLines(filepath.Join(ledger, "no-promote-decisions.jsonl")),
		KnowledgeImports:     countJSONLLines(filepath.Join(ledger, "knowledge-imports.jsonl")),
		KnowledgeSources:     countJSONLLines(knowledgeSources),
		KnowledgeSourceKinds: knowledgeSourceKinds(knowledgeSources),
		IntakeRepos:          countDirs(filepath.Join(homePath, "inbox", "external-repo-intake")),
	}
}

func loadSystemMapFile(homePath, repoPath string) (systemMapFile, []string, error) {
	candidates := []string{}
	if strings.TrimSpace(homePath) != "" {
		candidates = append(candidates, filepath.Join(homePath, "config", "system-topology.json"))
	}
	if strings.TrimSpace(repoPath) != "" {
		candidates = append(candidates, filepath.Join(repoPath, "fixtures", "map", "topology.json"))
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return systemMapFile{}, nil, err
		}
		var file systemMapFile
		if err := json.Unmarshal(data, &file); err != nil {
			return systemMapFile{}, nil, err
		}
		if file.Nodes == nil {
			file.Nodes = []SystemMapNode{}
		}
		if file.Edges == nil {
			file.Edges = []SystemMapEdge{}
		}
		return file, []string{path}, nil
	}
	return systemMapFile{Nodes: []SystemMapNode{}, Edges: []SystemMapEdge{}}, []string{}, nil
}

func hydrateSystemMapNode(homePath string, node *SystemMapNode, hosts *hostReachability) {
	switch strings.TrimSpace(node.Projection) {
	case "host-reachability":
		hydrateHostReachability(node, hosts)
	case "capability-authority":
		nodes := countJSONLLines(filepath.Join(homePath, "registries", "capability-nodes.jsonl"))
		edges := countJSONLLines(filepath.Join(homePath, "registries", "capability-edges.jsonl"))
		node.Volume = countPair(nodes, "nodes", edges, "edges")
	case "product-authority":
		signals := countJSONLLines(filepath.Join(homePath, "ledger", "product-signals.jsonl"))
		reviews := countJSONLLines(filepath.Join(homePath, "ledger", "product-reviews.jsonl"))
		node.Volume = countPair(signals, "signals", reviews, "reviews")
	case "route-authority":
		rows := countJSONLLines(filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl"))
		node.Volume = countLabel(rows, "route evidence")
	case "ledger-stats":
		claims := countJSONLLines(filepath.Join(homePath, "ledger", "claims.jsonl"))
		entities := countJSONLLines(filepath.Join(homePath, "ledger", "entities.jsonl"))
		evidence := countJSONLLines(filepath.Join(homePath, "ledger", "evidence.jsonl"))
		node.Volume = countTriple(claims, "claims", entities, "entities", evidence, "evidence")
	case "knowledge-sources":
		path := filepath.Join(homePath, "registries", "knowledge-sources.jsonl")
		node.Volume = countLabel(countJSONLLines(path), "sources")
		node.EvidenceRefs = knowledgeSourceKinds(path)
	case "external-intake-map":
		node.Volume = countLabel(countDirs(filepath.Join(homePath, "inbox", "external-repo-intake")), "repos")
		if strings.TrimSpace(node.ReviewID) != "" {
			node.EvidenceRefs = append(node.EvidenceRefs, node.ReviewID)
		}
	}
}

// hostReachability memoizes live probe reads per bundle for one BuildSystemMap
// call: a bundle referenced by many host nodes (per-host probes + an aggregate)
// is loaded and probed exactly once, so a down host is dialed once, not per node.
type hostReachability struct {
	homePath string
	now      time.Time
	cache    map[string]map[string]runtimebinding.Check // resolved bundle path -> probe id -> check (nil = load failed)
}

func newHostReachability(homePath string, now time.Time) *hostReachability {
	return &hostReachability{homePath: homePath, now: now, cache: map[string]map[string]runtimebinding.Check{}}
}

// results loads + probes a bundle once, memoizing by resolved path. ok is false
// when the bundle could not be loaded (missing/invalid) so callers degrade to an
// honest "unverified" status instead of claiming reachability they never proved.
func (h *hostReachability) results(bundlePath string) (map[string]runtimebinding.Check, bool) {
	if cached, seen := h.cache[bundlePath]; seen {
		return cached, cached != nil
	}
	bundle, err := runtimebinding.Load(bundlePath)
	if err != nil {
		h.cache[bundlePath] = nil
		return nil, false
	}
	res := runtimebinding.ProbeResults(context.Background(), bundle, h.now)
	h.cache[bundlePath] = res
	return res, true
}

// hydrateHostReachability turns a live probe read into a topology node's status
// + volume. A passing probe is "live"; anything else (unreachable, missing
// bundle, unknown probe) is "divergent" — the observed-truth signal that a host
// the map expects to be up is not answering. Status is a free string here; the
// console maps "live" -> green and everything else -> not-live.
func hydrateHostReachability(node *SystemMapNode, hosts *hostReachability) {
	bundlePath, probeID := parseHostTarget(node.Target, hosts.homePath)
	if bundlePath == "" {
		node.Status = "divergent"
		node.Volume = "no probe bound"
		return
	}
	results, ok := hosts.results(bundlePath)
	if !ok {
		node.Status = "divergent"
		node.Volume = "probe bundle unavailable"
		return
	}
	if probeID != "" {
		check, found := results[probeID]
		if !found {
			node.Status = "divergent"
			node.Volume = "probe " + probeID + " not found"
			return
		}
		if check.Status == "ok" {
			node.Status = "live"
			node.Volume = "reachable"
		} else {
			node.Status = "divergent"
			node.Volume = "unreachable"
		}
		node.EvidenceRefs = append(node.EvidenceRefs, "probe:"+probeID+":"+check.Status)
		return
	}
	// Aggregate node: the whole bundle. Live only when every host answers.
	total := len(results)
	okCount := 0
	down := make([]string, 0, total)
	for id, check := range results {
		if check.Status == "ok" {
			okCount++
		} else {
			down = append(down, id)
		}
	}
	sort.Strings(down)
	if total > 0 && okCount == total {
		node.Status = "live"
	} else {
		node.Status = "divergent"
	}
	node.Volume = fmt.Sprintf("%d/%d reachable", okCount, total)
	for _, id := range down {
		node.EvidenceRefs = append(node.EvidenceRefs, "unreachable:"+id)
	}
}

// parseHostTarget splits a host-reachability target "<bundlePath>[#probeID]" and
// resolves the bundle path: ~/ expands to the home dir, absolute paths pass
// through, and a relative path resolves against COMPOUND_HOME. Probe/bundle
// paths carry no host data into the public repo — the bundle itself is private
// runtime state under COMPOUND_HOME.
func parseHostTarget(target, homePath string) (bundlePath, probeID string) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", ""
	}
	path := target
	if idx := strings.LastIndex(target, "#"); idx >= 0 {
		path = strings.TrimSpace(target[:idx])
		probeID = strings.TrimSpace(target[idx+1:])
	}
	if path == "" {
		return "", probeID
	}
	switch {
	case strings.HasPrefix(path, "~/"):
		if h, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(h, strings.TrimPrefix(path, "~/"))
		}
	case !filepath.IsAbs(path) && strings.TrimSpace(homePath) != "":
		path = filepath.Join(homePath, path)
	}
	return path, probeID
}

func countJSONLLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func countDirs(path string) int {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

func knowledgeSourceKinds(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		for _, key := range []string{"source_kind", "kind"} {
			if value, ok := row[key].(string); ok && strings.TrimSpace(value) != "" {
				seen[strings.TrimSpace(value)] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func countLabel(count int, label string) string {
	return strings.TrimSpace(strings.Join([]string{itoa(count), label}, " "))
}

func countPair(a int, alabel string, b int, blabel string) string {
	return countLabel(a, alabel) + " · " + countLabel(b, blabel)
}

func countTriple(a int, alabel string, b int, blabel string, c int, clabel string) string {
	return countPair(a, alabel, b, blabel) + " · " + countLabel(c, clabel)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
