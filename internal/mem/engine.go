package mem

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
)

// Engine errors are EXPLICIT and distinct so a caller can tell apart "this engine has no
// breadth" (degrade to ledger), "the engine process is down" (degrade to ledger, say so), and
// "the engine's embedder does not match what we indexed with" (HARD refuse — a silent model
// swap degrades recall invisibly). None of these is ever a silent nil.
var (
	ErrUnsupported       = errors.New("capability not supported by this engine")
	ErrEngineUnavailable = errors.New("breadth engine unavailable (transport down) — ledger answers exact/as-of")
	ErrEmbedderMismatch  = errors.New("engine embedder does not match the indexed embedder (refusing breadth, not degrading)")
	ErrProtocolVersion   = errors.New("engine protocol version skew (refusing breadth, not degrading)")
	ErrEngineAhead       = errors.New("breadth engine generation is ahead of owned ledger (refusing backward rebuild)")
	// ErrEngineServedUnownedClaim fires when the rented engine emits an index_hit into a claim
	// the owned ledger does not hold — the engine serving authority it does not have. Like
	// ErrEmbedderMismatch it is fail-closed (it refuses the WHOLE query, never a silent drop),
	// because an engine that can smuggle unowned pointers into recall has become an authority.
	ErrEngineServedUnownedClaim = errors.New("engine returned an index_hit not in the owned ledger allowlist (serving authority it does not have)")
)

// Gate decouples the visibility decision from the allowlist CONTENTS. GateList honors the
// allowlist and an EMPTY allowlist matches NOTHING (fail-closed); GateAll is admin/debug only
// and the core never emits it.
const (
	GateList = "list"
	GateAll  = "all"
)

// EngineCapabilities advertises what an engine can do so callers adapt instead of assuming.
type EngineCapabilities struct {
	Name      string `json:"name"`
	Breadth   bool   `json:"breadth"`    // semantic/lexical Query over many claims
	Embedder  string `json:"embedder"`   // "<provider:model:dim:bits>" — for the identity guard; "" = none
	CorpusGen int64  `json:"corpus_gen"` // the generation this engine is indexed at (ledger maxSeq)
}

// HitKind types a breadth-query result by the AUTHORITY it claims. There are exactly two
// legitimate shapes: an index_hit is a pointer into owned canon (it carries a ClaimID that MUST
// resolve to a current ledger claim), and an observed_fact is a candidate-shaped proposal
// surfaced over rented breadth that Compound does NOT own (no ClaimID, no authority). The zero
// value ("") is index_hit, so a legacy engine that only emits resolvable pointers is unaffected.
type HitKind string

const (
	KindIndexHit     HitKind = "index_hit"
	KindObservedFact HitKind = "observed_fact"
)

// Hit is a breadth-query result. Its Kind declares STRUCTURALLY what authority it claims, so an
// engine can never smuggle authority it lacks through an ambiguous drop: an index_hit that does
// not resolve to an owned claim is a hard ErrEngineServedUnownedClaim (never a silent drop), and
// an observed_fact carries no ClaimID and reaches canon only as an adjudicated candidate.
type Hit struct {
	Kind     HitKind       `json:"kind,omitempty"` // "" == index_hit (backward compat)
	ClaimID  string        `json:"claim_id"`
	Score    float64       `json:"score"`
	Observed *ObservedFact `json:"observed,omitempty"` // non-nil iff Kind == observed_fact
}

// ObservedFact is a candidate-shaped proposal the rented engine SURFACED over breadth that
// Compound does not own. It carries no ClaimID and no authority; it reaches canon ONLY as an
// `observed`-derivation candidate in the inbox, adjudicated by code, never auto-promoted, and
// never in ambient recall.
type ObservedFact struct {
	Entity    string `json:"entity"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
	Snippet   string `json:"snippet,omitempty"` // bounded evidence text, not a raw body
}

// Engine is the RENTED breadth index. Recall (exact / as-of) stays the ledger's job; the engine
// only answers breadth Query, honoring the ledger-computed allowlist (the relevance gate). Index
// is idempotent on the corpus generation, so an out-of-process engine never re-embeds per query.
// This is the P6 seam the turbovec sidecar plugs into (behind ProtocolEngine); the legacy
// markdown engine (internal/engine) retires with the legacy memory object model.
type Engine interface {
	Capabilities() EngineCapabilities
	Index(claims []Claim, gen int64) error
	Query(text, gate string, allowlist []string, k int) ([]Hit, error)
}

// DiagnosticEngine returns query-scoped degradation with the hits it produced.
// Engines without diagnostics continue through the Engine interface.
type DiagnosticEngine interface {
	Engine
	QueryWithDiagnostics(text, gate string, allowlist []string, k int) ([]Hit, []string, error)
}

type contextIndexEngine interface {
	IndexWithContext(claims []Claim, edges []WireEdge, gen int64) error
}

// NoEngine is the default: no breadth. Its ops return ErrUnsupported EXPLICITLY — never nil,nil —
// so a missing index cannot masquerade as an empty result.
type NoEngine struct{}

func (NoEngine) Capabilities() EngineCapabilities                   { return EngineCapabilities{Name: "none"} }
func (NoEngine) Index([]Claim, int64) error                         { return ErrUnsupported }
func (NoEngine) Query(string, string, []string, int) ([]Hit, error) { return nil, ErrUnsupported }

// LexicalEngine is a dependency-free, in-process breadth engine: case-insensitive token match
// over claim text, honoring the allowlist. It proves the seam end-to-end with real results and
// is the recall body the pure-Go mock sidecar reuses behind the wire. A semantic ANN engine
// (turbovec) is a later swap behind this same interface, via ProtocolEngine.
type LexicalEngine struct {
	claims []Claim
	gen    int64
}

func (e *LexicalEngine) Capabilities() EngineCapabilities {
	return EngineCapabilities{Name: "lexical", Breadth: true, CorpusGen: e.gen}
}

// Index is idempotent on gen: re-indexing the same generation is a no-op (no re-embed storm).
func (e *LexicalEngine) Index(claims []Claim, gen int64) error {
	if e.claims != nil && gen == e.gen {
		return nil
	}
	e.claims, e.gen = claims, gen
	return nil
}

func (e *LexicalEngine) Query(text, gate string, allowlist []string, k int) ([]Hit, error) {
	allow := map[string]bool{}
	for _, id := range allowlist {
		allow[id] = true
	}
	terms := strings.Fields(strings.ToLower(text))
	var hits []Hit
	for _, c := range e.claims {
		// GateList: an empty allowlist matches NOTHING (fail-closed). GateAll skips the gate.
		if gate != GateAll && !allow[c.ID] {
			continue
		}
		hay := strings.ToLower(c.Entity + " " + c.Predicate + " " + c.Object)
		score := 0.0
		for _, t := range terms {
			if strings.Contains(hay, t) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, Hit{ClaimID: c.ID, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

// QueryBreadth answers a breadth query via the RENTED engine, relevance-gated: the LEDGER
// computes the allowlist (current, non-retired claim ids) and passes it under GateList, so the
// gate is honored at the engine (cheap), not as a post-filter. It re-Indexes ONLY when the
// engine's generation lags the ledger's (no re-embed per query). Returns ErrUnsupported /
// ErrEngineUnavailable / ErrEmbedderMismatch explicitly — callers degrade or refuse, never
// silently empty.
func (s *Store) QueryBreadth(text string, k int, eng Engine) ([]Claim, error) {
	view, err := s.CurrentView()
	if err != nil {
		return nil, err
	}
	result, err := s.queryBreadthFromView(text, k, eng, true, view, nil)
	return result.Claims, err
}

func (s *Store) QueryBreadthResult(text string, k int, eng Engine) (ReadOnlyBreadthQueryResult, error) {
	view, err := s.CurrentView()
	if err != nil {
		return ReadOnlyBreadthQueryResult{}, err
	}
	return s.queryBreadthFromView(text, k, eng, true, view, nil)
}

// QueryBreadthReadOnly shares QueryBreadth's validation and ranking path but does not capture
// valid observed_fact proposals. Live prompt retrieval uses it so an engine echo cannot turn a
// transient query into a durable candidate. Malformed hits still fail closed.
func (s *Store) QueryBreadthReadOnly(text string, k int, eng Engine) ([]Claim, error) {
	view, err := s.CurrentView()
	if err != nil {
		return nil, err
	}
	result, err := s.queryBreadthFromView(text, k, eng, false, view, nil)
	return result.Claims, err
}

func (s *Store) QueryBreadthReadOnlyResult(text string, k int, eng Engine) (ReadOnlyBreadthQueryResult, error) {
	view, err := s.CurrentView()
	if err != nil {
		return ReadOnlyBreadthQueryResult{}, err
	}
	return s.queryBreadthFromView(text, k, eng, false, view, nil)
}

// QueryBreadthReadOnlyFromView preserves the normal read-only query ingress,
// ranking, alias expansion, and fail-closed authority checks while reusing an
// invocation's already-stabilized memory view.
func (s *Store) QueryBreadthReadOnlyFromView(text string, k int, eng Engine, view *CurrentView) ([]Claim, error) {
	result, err := s.queryBreadthFromView(text, k, eng, false, view, nil)
	return result.Claims, err
}

func (s *Store) QueryBreadthReadOnlyFromViewResult(text string, k int, eng Engine, view *CurrentView) (ReadOnlyBreadthQueryResult, error) {
	return s.queryBreadthFromView(text, k, eng, false, view, nil)
}

// QueryBreadthReadOnlyFromViewExpanded is the invocation-snapshot variant used
// when the caller has already captured deterministic recall expansion. It
// prevents the breadth engine and the caller's revision from observing different
// alias-registry generations.
func (s *Store) QueryBreadthReadOnlyFromViewExpanded(text string, k int, eng Engine, view *CurrentView, expanded ExpandedQuery) ([]Claim, error) {
	result, err := s.queryBreadthFromView(text, k, eng, false, view, &expanded)
	return result.Claims, err
}

// ReadOnlyBreadthQueryResult distinguishes a captured alias expansion from one
// that the rented breadth path actually consumed successfully. Callers may bind
// ConsumedExpansion into freshness only when this result is returned without an
// error; early exits and failed queries return no dependency.
type ReadOnlyBreadthQueryResult struct {
	Claims            []Claim
	ConsumedExpansion *ExpandedQuery
	DegradedLanes     []string
}

func (s *Store) QueryBreadthReadOnlyFromViewExpandedResult(text string, k int, eng Engine, view *CurrentView, expanded ExpandedQuery) (ReadOnlyBreadthQueryResult, error) {
	return s.queryBreadthFromView(text, k, eng, false, view, &expanded)
}

func (s *Store) queryBreadthFromView(text string, k int, eng Engine, captureObserved bool, view *CurrentView, capturedExpansion *ExpandedQuery) (ReadOnlyBreadthQueryResult, error) {
	if capturedExpansion != nil && expandedQueryProvided(*capturedExpansion) && capturedExpansion.Original != text {
		return ReadOnlyBreadthQueryResult{}, fmt.Errorf("captured recall expansion mismatch: original does not match query text")
	}
	caps := eng.Capabilities()
	if !caps.Breadth {
		return ReadOnlyBreadthQueryResult{}, ErrUnsupported
	}
	if view == nil || view.owner != s || view.revision == "" {
		return ReadOnlyBreadthQueryResult{}, fmt.Errorf("current memory view is required and must belong to this Store instance")
	}
	cur := view.claims
	gen := view.generation
	engineGen := caps.CorpusGen
	if engineGen > gen {
		return ReadOnlyBreadthQueryResult{}, fmt.Errorf("%w: engine_gen=%d ledger_gen=%d", ErrEngineAhead, engineGen, gen)
	}
	if engineGen < gen {
		if ceng, ok := eng.(contextIndexEngine); ok {
			if err := ceng.IndexWithContext(view.Claims(), view.wireEdges(0.5), gen); err != nil {
				return ReadOnlyBreadthQueryResult{}, err
			}
		} else if err := eng.Index(view.Claims(), gen); err != nil {
			return ReadOnlyBreadthQueryResult{}, err
		}
	}
	allow := make([]string, 0, len(cur))
	for _, c := range cur {
		allow = append(allow, c.ID)
	}
	// E1 deterministic query expansion: widen the QUERY only (never the gate).
	// Absent/empty registry → original text only (fail-open, byte-identical path).
	expanded := ExpandedQuery{Original: text, Text: text}
	if capturedExpansion != nil && expandedQueryProvided(*capturedExpansion) {
		expanded = cloneExpandedQuery(*capturedExpansion)
	} else {
		reg, _ := LoadRecallAliases(s.home)
		expanded = ExpandQuery(text, reg)
	}
	fetchK := k
	if fetchK > 0 && expanded.Active {
		// Pull a wider pool so discounted expansion hits can still fill top-k.
		fetchK = k * 2
	}
	query := func(queryText string) ([]Hit, []string, error) {
		if diagnostic, ok := eng.(DiagnosticEngine); ok {
			return diagnostic.QueryWithDiagnostics(queryText, GateList, allow, fetchK)
		}
		hits, err := eng.Query(queryText, GateList, allow, fetchK)
		return hits, nil, err
	}
	hits, degradedLanes, err := query(text)
	if err != nil {
		return ReadOnlyBreadthQueryResult{}, err
	}
	degradedLanes = appendDiagnosticLanes(nil, degradedLanes...)
	if expanded.Active && len(expanded.Added) > 0 {
		expHits, expLanes, expErr := query(strings.Join(expanded.Added, " "))
		if expErr != nil {
			return ReadOnlyBreadthQueryResult{}, expErr
		}
		degradedLanes = appendDiagnosticLanes(degradedLanes, expLanes...)
		hits = mergeHitsWithExpansionDiscount(hits, expHits, k)
	} else if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	// ROUTER — the single choke point (the engine-ingress face) that types each rented-engine hit
	// by the authority it claims. An index_hit is a pointer into owned canon and MUST resolve to a
	// current ledger claim; an unresolved one is the engine serving authority it does not have —
	// a hard refusal (fail-closed, like ErrEmbedderMismatch), never the old silent drop. An
	// observed_fact owns nothing and is routed to the adjudicated candidate inbox here; it is
	// deliberately NOT appended to the returned claims, so it can never enter ambient recall.
	engineName := eng.Capabilities().Name
	out := make([]Claim, 0, len(hits))
	for _, h := range hits {
		switch h.Kind {
		case "", KindIndexHit:
			if h.Observed != nil {
				return ReadOnlyBreadthQueryResult{}, fmt.Errorf("%w: index_hit %q carried an observed payload", ErrEngineServedUnownedClaim, h.ClaimID)
			}
			c, ok := view.byID[h.ClaimID]
			if !ok {
				return ReadOnlyBreadthQueryResult{}, fmt.Errorf("%w: %q", ErrEngineServedUnownedClaim, h.ClaimID) // was: silent drop
			}
			out = append(out, cloneCurrentViewClaim(c))
		case KindObservedFact:
			if h.ClaimID != "" || h.Observed == nil || h.Observed.Entity == "" {
				return ReadOnlyBreadthQueryResult{}, fmt.Errorf("%w: malformed observed_fact (claim_id=%q)", ErrEngineServedUnownedClaim, h.ClaimID)
			}
			if captureObserved {
				if err := s.captureObservedFact(engineName, *h.Observed, time.Now()); err != nil {
					return ReadOnlyBreadthQueryResult{}, err
				}
			}
		default:
			return ReadOnlyBreadthQueryResult{}, fmt.Errorf("%w: unknown hit kind %q", ErrEngineServedUnownedClaim, h.Kind)
		}
	}
	consumed := cloneExpandedQuery(expanded)
	return ReadOnlyBreadthQueryResult{Claims: out, ConsumedExpansion: &consumed, DegradedLanes: degradedLanes}, nil
}

func appendDiagnosticLanes(dst []string, lanes ...string) []string {
	for _, lane := range lanes {
		lane = strings.TrimSpace(lane)
		if len(dst) >= 8 {
			continue
		}
		if lane == "" || len(lane) > 32 {
			lane = "unknown"
		} else {
			for i := 0; i < len(lane); i++ {
				ch := lane[i]
				if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
					lane = "unknown"
					break
				}
			}
		}
		found := false
		for _, existing := range dst {
			if existing == lane {
				found = true
				break
			}
		}
		if !found {
			dst = append(dst, lane)
		}
	}
	return dst
}

func expandedQueryProvided(expanded ExpandedQuery) bool {
	return expanded.Original != "" || expanded.Text != "" || expanded.Active || len(expanded.Added) != 0
}

func cloneExpandedQuery(expanded ExpandedQuery) ExpandedQuery {
	expanded.Added = append([]string(nil), expanded.Added...)
	expanded.Matched = append([]string(nil), expanded.Matched...)
	return expanded
}

// observed is the derivation minted for a fact the rented breadth engine SURFACED but Compound
// does not own — distinct from `inferred` so provenance stays legible. It extends the
// mem.Derivations vocabulary (defined in mem.go, owned by a parallel lane), registered here — the
// engine ingress that produces the term — via init so the shared literal is not edited in two
// places at once.
const DerivationObserved = "observed"

func init() { Derivations = append(Derivations, DerivationObserved) }

// captureObservedFact routes an engine-surfaced ObservedFact — a fact Compound does NOT own —
// into the memory-candidate inbox as a proposed, `observed`-derivation candidate: engine
// provenance, promotion basis NONE, never a ledger byte. It is content-addressed on
// entity|predicate|object and SKIPS if a candidate with that id already exists, so re-surfacing
// the same fact never resets an in-flight adjudication verdict or spams the inbox. It writes ONLY
// through the candidate-inbox door (never RecordEvidence / a ledger append), which is what keeps
// EngineDoctor's ledger-digest invariant intact by construction.
func (s *Store) captureObservedFact(engineName string, of ObservedFact, now time.Time) error {
	id := idFor("cand_", of.Entity, of.Predicate, of.Object)
	var existing Candidate
	if err := candidates.ReadCandidate(s.home, candidates.Memory, id, &existing); err == nil {
		return nil // skip-if-exists: never reset an adjudication verdict
	} else if !os.IsNotExist(err) {
		return err
	}
	c := Candidate{
		ID:             id,
		Entity:         of.Entity,
		Predicate:      of.Predicate,
		Object:         of.Object,
		Derivation:     DerivationObserved,
		ProvenanceLane: engineName,
		SourceTraceRef: "engine:" + engineName + ":breadth",
		PromotionBasis: "", // no basis — an observed fact can never auto-promote
		State:          "proposed",
		CreatedAt:      rfc3339(now),
	}
	if strings.TrimSpace(of.Snippet) != "" {
		c.SnippetHash = shortHash(of.Snippet)
	}
	return candidates.WriteCandidate(s.home, candidates.Memory, id, &c)
}

// currentGen is ledger generation = max seq across claims and relation edges.
func (s *Store) currentGen() int64 {
	gen, err := s.maxLedgerSeq()
	if err != nil {
		return 0
	}
	return gen
}

func (s *Store) visibleWireEdges(cur []Claim, minConf float64) ([]WireEdge, error) {
	visibleEntities := make(map[string]bool, len(cur))
	for _, c := range cur {
		visibleEntities[c.Entity] = true
	}
	rels, err := s.loadRelations(minConf)
	if err != nil {
		return nil, err
	}
	out := make([]WireEdge, 0, len(rels))
	for _, e := range rels {
		if !visibleEntities[e.From] || !visibleEntities[e.To] {
			continue
		}
		out = append(out, WireEdge{From: e.From, Kind: e.Kind, To: e.To, Confidence: e.Confidence})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].To < out[j].To
	})
	return out, nil
}

// AllCurrent returns the current (non-superseded, non-retired) claims — the canon a distiller
// compares new extractions against (RetrievedPriorState). Public accessor over currentVisible.
func (s *Store) AllCurrent() ([]Claim, error) { return s.currentVisible() }

// LedgerClaims returns every immutable claim row in the append-only ledger,
// including superseded history. Authority proofs use it when the existence of
// an earned promotion matters independently of current recall visibility.
func (s *Store) LedgerClaims() ([]Claim, error) { return s.loadClaimsRaw() }

// AllCurrentOperationalDecisions returns the runtime decision-control subset
// with its own protected supersession fold. Only another stamped/legacy
// operator decision may hide one of these claims; an ordinary memory edge,
// review mark, or same-slot shadow cannot erase grant/freeze/revocation
// authority before its consumer validates it.
func (s *Store) AllCurrentOperationalDecisions() ([]Claim, error) {
	claims, err := s.loadClaimsRaw()
	if err != nil {
		return nil, err
	}
	protected := map[string]bool{}
	for _, claim := range claims {
		if protectedOperationalDecisionClaim(claim) {
			protected[claim.ID] = true
		}
	}
	superseded := map[string]bool{}
	err = readJSONL(s.edgesLog(), func(line []byte) error {
		var edge Edge
		if err := json.Unmarshal(line, &edge); err != nil {
			return err
		}
		if edge.Kind == EdgeKindSupersedes && protected[edge.From] && protected[edge.To] {
			superseded[edge.To] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, claim := range claims {
		if protected[claim.ID] && protected[claim.Supersedes] {
			superseded[claim.Supersedes] = true
		}
	}
	var out []Claim
	for _, claim := range claims {
		if !protected[claim.ID] || claim.Visibility == "retired" || superseded[claim.ID] {
			continue
		}
		claim.State = ClaimStatePromoted
		claim.ReviewRefs = nil
		claim.ConfidenceDelta = 0
		out = append(out, claim)
	}
	return out, nil
}

// currentVisible returns the current (non-superseded as of now), non-retired claims — the set
// an engine is allowed to surface (the relevance gate, computed by the ledger).
func (s *Store) currentVisible() ([]Claim, error) {
	claims, err := s.loadClaims()
	if err != nil {
		return nil, err
	}
	superseded, err := s.supersededAsOf(s.maxSeq(claims))
	if err != nil {
		return nil, err
	}
	var out []Claim
	for _, c := range claims {
		if c.Visibility == "retired" || c.State == ClaimStateNeedsReview || superseded[c.ID] {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}
