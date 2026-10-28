package mem

// Task-shaped recall: instead of flat top-k, seed-match entities, walk bounded
// typed edges around seeds, return a budget-trimmed subgraph plus a coverage
// verdict. On gaps it names what is missing rather than fabricating.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RelationKinds are entity-level edge kinds task-shaped recall traverses.
// Claim-level version/review edges are stored in the same log but excluded.
var RelationKinds = []string{"relates", "contradicts", "depends_on", "refines", "supports", "mentions", "owns", "part_of", "narrows_to"}

// RelationHint is an extracted relation between two entities.
type RelationHint struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Kind       string  `json:"kind"`
	Confidence float64 `json:"confidence"`
}

// Extraction is what an Extractor turns a trace into: candidate facts plus relation hints.
type Extraction struct {
	Candidates []Candidate    `json:"candidates"`
	Relations  []RelationHint `json:"relations"`
}

// Extractor turns a raw trace into an Extraction.
type Extractor interface {
	Extract(trace []byte) (Extraction, error)
}

// StructuredExtractor parses either a bare []Candidate for compatibility or a
// {candidates, relations} object. It is the JSON-parsing boundary the CLI's
// raw-intake path uses (cmd/compound/memory_cli.go: "compound memory candidate
// <trace.json>"). Per en.work_2742cffbeb5f, system-derived promotion_basis
// values arriving through this parser are stripped at THIS seam — the only
// door raw intake actually uses — so a model-supplied basis never reaches
// Capture or Adjudicate. Direct callers of Capture (system adapters, tests
// that legitimately seed candidates) are unaffected: their Candidate literals
// never visit the parser.
type StructuredExtractor struct{}

func (StructuredExtractor) Extract(trace []byte) (Extraction, error) {
	var ext Extraction
	if err := json.Unmarshal(trace, &ext); err == nil && (len(ext.Candidates) > 0 || len(ext.Relations) > 0) {
		for i := range ext.Candidates {
			stripIntakePromotionBasis(&ext.Candidates[i])
		}
		return ext, nil
	}
	var cands []Candidate
	if err := json.Unmarshal(trace, &cands); err != nil {
		return Extraction{}, fmt.Errorf("trace is neither {candidates,relations} nor [candidates]: %w", err)
	}
	for i := range cands {
		stripIntakePromotionBasis(&cands[i])
	}
	return Extraction{Candidates: cands}, nil
}

// Relate records an entity-level relation edge. Relations are structural, not
// claims, so they are not candidate-adjudicated; recall confidence-gates them.
func (s *Store) Relate(from, kind, to string, conf float64, now time.Time) (Edge, error) {
	return s.relate(from, kind, to, conf, nil, now)
}

func (s *Store) relate(from, kind, to string, conf float64, evidenceRefs []string, now time.Time) (Edge, error) {
	if !oneOf(kind, RelationKinds) {
		return Edge{}, fmt.Errorf("relation kind must be one of %v, got %q", RelationKinds, kind)
	}
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return Edge{}, fmt.Errorf("relate needs entity")
	}
	var e Edge
	err := s.withLock(func() error {
		seq, err := s.nextSeq()
		if err != nil {
			return err
		}
		e = Edge{Seq: seq, Kind: kind, From: from, To: to, Confidence: conf, EvidenceRefs: normalizeStringRefs(evidenceRefs), CreatedAt: rfc3339(now)}
		return appendJSONL(s.edgesLog(), &e)
	})
	return e, err
}

// loadEdges reads every edge in the append-only edge log.
func (s *Store) loadEdges() ([]Edge, error) {
	var out []Edge
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

func (s *Store) loadRelations(minConf float64) ([]Edge, error) {
	var out []Edge
	err := readJSONL(s.edgesLog(), func(line []byte) error {
		var e Edge
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Kind != EdgeKindSupersedes && e.Kind != EdgeKindReinforces && e.Kind != EdgeKindNeedsReview && e.Confidence >= minConf {
			out = append(out, e)
		}
		return nil
	})
	return out, err
}

// Subgraph is the result of task-shaped recall: seed-matched entities, bounded
// reached entities, their current claims, relations among them, and coverage.
type Subgraph struct {
	Seed         string   `json:"seed"`
	SeedEntities []string `json:"seed_entities"`
	Entities     []string `json:"entities"`
	Claims       []Claim  `json:"claims"`
	Relations    []Edge   `json:"relations"`
	DepthReached int      `json:"depth_reached"`
	BudgetHit    bool     `json:"budget_hit"`
	Coverage     string   `json:"coverage"`
}

// GraphOpts controls bounded graph recall. Direction is empty for the legacy
// undirected walk, "up" for From->To traversal, and "down" for To->From.
type GraphOpts struct {
	Depth     int
	Budget    int
	MinConf   float64
	Direction string
	Kinds     []string
}

// RecallGraph keeps the original undirected graph recall API.
func (s *Store) RecallGraph(seed string, depth, budget int, minConf float64) (Subgraph, error) {
	return s.RecallGraphOpts(seed, GraphOpts{Depth: depth, Budget: budget, MinConf: minConf})
}

// RecallGraphOpts does task-shaped recall with optional relation-kind and
// direction filters. It never fabricates: a seed that matches nothing says so.
func (s *Store) RecallGraphOpts(seed string, opts GraphOpts) (Subgraph, error) {
	res := Subgraph{Seed: seed}
	cur, err := s.currentVisible()
	if err != nil {
		return res, err
	}
	res.SeedEntities = matchSeed(cur, seed)
	if len(res.SeedEntities) == 0 {
		res.Coverage = fmt.Sprintf("seed %q matched no entity - not fabricating", seed)
		return res, nil
	}
	if opts.Direction != "" && opts.Direction != "up" && opts.Direction != "down" {
		return res, fmt.Errorf("graph direction must be empty, up, or down; got %q", opts.Direction)
	}
	kindSet := map[string]bool{}
	for _, kind := range opts.Kinds {
		if !oneOf(kind, RelationKinds) {
			return res, fmt.Errorf("graph relation kind must be one of %v, got %q", RelationKinds, kind)
		}
		kindSet[kind] = true
	}
	rels, err := s.loadRelations(opts.MinConf)
	if err != nil {
		return res, err
	}
	if len(kindSet) > 0 {
		filtered := make([]Edge, 0, len(rels))
		for _, e := range rels {
			if kindSet[e.Kind] {
				filtered = append(filtered, e)
			}
		}
		rels = filtered
	}

	adj := map[string][]Edge{}
	for _, e := range rels {
		switch opts.Direction {
		case "up":
			adj[e.From] = append(adj[e.From], e)
		case "down":
			adj[e.To] = append(adj[e.To], e)
		default:
			adj[e.From] = append(adj[e.From], e)
			adj[e.To] = append(adj[e.To], e)
		}
	}

	seen := map[string]bool{}
	frontier := append([]string{}, res.SeedEntities...)
	for _, e := range frontier {
		seen[e] = true
	}
	for d := 0; d < opts.Depth && len(frontier) > 0; d++ {
		var next []string
		for _, ent := range frontier {
			for _, edge := range adj[ent] {
				other := graphOther(ent, edge, opts.Direction)
				if other == "" || seen[other] {
					continue
				}
				if opts.Budget > 0 && len(seen) >= opts.Budget {
					res.BudgetHit = true
					continue
				}
				seen[other] = true
				next = append(next, other)
			}
		}
		if len(next) > 0 {
			res.DepthReached = d + 1
		}
		frontier = next
	}

	for e := range seen {
		res.Entities = append(res.Entities, e)
	}
	sort.Strings(res.Entities)

	entSet := seen
	for _, c := range cur {
		if entSet[c.Entity] {
			res.Claims = append(res.Claims, c)
		}
	}
	sort.Slice(res.Claims, func(i, j int) bool { return res.Claims[i].Seq < res.Claims[j].Seq })

	emitted := map[string]bool{}
	for _, e := range rels {
		if entSet[e.From] && entSet[e.To] {
			if k := relKey(e); !emitted[k] {
				emitted[k] = true
				res.Relations = append(res.Relations, e)
			}
		}
	}
	sort.Slice(res.Relations, func(i, j int) bool { return res.Relations[i].Seq < res.Relations[j].Seq })

	budgetNote := ""
	if res.BudgetHit {
		budgetNote = fmt.Sprintf(" (budget %d hit - subgraph truncated)", opts.Budget)
	}
	res.Coverage = fmt.Sprintf("subgraph from %d seed(s): %d entities, %d claims, %d relations, depth %d%s",
		len(res.SeedEntities), len(res.Entities), len(res.Claims), len(res.Relations), res.DepthReached, budgetNote)
	return res, nil
}

func graphOther(ent string, edge Edge, direction string) string {
	switch direction {
	case "up":
		if edge.From == ent {
			return edge.To
		}
	case "down":
		if edge.To == ent {
			return edge.From
		}
	default:
		if edge.From == ent {
			return edge.To
		}
		if edge.To == ent {
			return edge.From
		}
	}
	return ""
}

// matchSeed returns entities whose name or current-claim text contains the seed token.
func matchSeed(cur []Claim, seed string) []string {
	terms := strings.Fields(strings.ToLower(seed))
	if len(terms) == 0 {
		return nil
	}
	hit := map[string]bool{}
	for _, c := range cur {
		hay := strings.ToLower(c.Entity + " " + c.Predicate + " " + c.Object)
		for _, t := range terms {
			if strings.Contains(hay, t) {
				hit[c.Entity] = true
				break
			}
		}
	}
	out := make([]string, 0, len(hit))
	for e := range hit {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

func relKey(e Edge) string {
	return e.From + "|" + e.Kind + "|" + e.To
}
