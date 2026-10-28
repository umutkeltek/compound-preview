package breadth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"unicode"

	"compound/internal/mem"
)

const (
	Name            = "compound-breadth"
	DefaultEmbedder = "compound-breadth:lexical:0:0"
	k1              = 1.2
	b               = 0.75
)

type Engine struct {
	claims       []mem.ClaimText
	docs         []doc
	gen          int64
	ready        bool
	embedder     EmbedderAdapter
	compoundHome string
	cache        *vectorCache
}

type doc struct {
	claim    mem.ClaimText
	tokens   []string
	freq     map[string]int
	wrapped  string
	textHash [32]byte
	vector   []float32
}

type EngineConfig struct {
	Embedder     EmbedderAdapter
	CompoundHome string
}

type QueryResult struct {
	Hits          []mem.Hit
	DegradedLanes []string
	Warning       string
}

func NewEngine(cfg EngineConfig) (*Engine, error) {
	e := &Engine{embedder: cfg.Embedder, compoundHome: cfg.CompoundHome}
	if cfg.Embedder != nil && cfg.Embedder.Dim() > 0 {
		cache, err := newVectorCache(cfg.CompoundHome, cfg.Embedder.Identity())
		if err != nil {
			return nil, err
		}
		e.cache = cache
	}
	return e, nil
}

func (e *Engine) CorpusGen() int64 { return e.gen }
func (e *Engine) Ready() bool      { return e.ready }

func (e *Engine) Index(claims []mem.ClaimText, gen int64) int {
	indexed, _ := e.IndexWithError(claims, gen)
	return indexed
}

func (e *Engine) IndexWithError(claims []mem.ClaimText, gen int64) (int, error) {
	if e.ready && e.gen == gen {
		return 0, nil
	}
	e.claims = append([]mem.ClaimText(nil), claims...)
	e.docs = make([]doc, 0, len(claims))
	for _, c := range claims {
		tokens := Tokenize(c.Text)
		freq := make(map[string]int, len(tokens))
		for _, tok := range tokens {
			freq[tok]++
		}
		wrapped := wrapDocText(c)
		e.docs = append(e.docs, doc{claim: c, tokens: tokens, freq: freq, wrapped: wrapped, textHash: textHash(wrapped)})
	}
	if e.embedder != nil {
		if err := e.indexVectors(context.Background(), gen); err != nil {
			return 0, err
		}
	}
	e.gen = gen
	e.ready = true
	return len(claims), nil
}

func (e *Engine) Query(text, gate string, allowlist []string, k int) []mem.Hit {
	result, _ := e.QueryWithLanes(text, gate, allowlist, k)
	return result.Hits
}

func (e *Engine) QueryWithLanes(text, gate string, allowlist []string, k int) (QueryResult, error) {
	if k <= 0 || !e.ready {
		return QueryResult{}, nil
	}
	allowed := allowSet(gate, allowlist)
	allowedDocs := make([]doc, 0, len(e.docs))
	for _, d := range e.docs {
		if gate != mem.GateAll && !allowed[d.claim.ClaimID] {
			continue
		}
		allowedDocs = append(allowedDocs, d)
	}
	if len(allowedDocs) == 0 {
		return QueryResult{}, nil
	}

	bm25 := e.bm25(text, allowedDocs)
	reference := referenceContains(text, allowedDocs)
	if e.embedder != nil {
		queryVectors, err := e.embedder.Embed(context.Background(), []string{text})
		if err != nil || len(queryVectors) != 1 {
			warn := fmt.Sprintf("vector lane degraded: %v", err)
			if err == nil {
				warn = fmt.Sprintf("vector lane degraded: embedder returned %d query vectors", len(queryVectors))
			}
			return QueryResult{Hits: lexicalRanking(bm25, reference, k), DegradedLanes: []string{"vector"}, Warning: warn}, nil
		}
		return QueryResult{Hits: e.semanticRanking(queryVectors[0], allowedDocs, bm25, reference, k)}, nil
	}
	return QueryResult{Hits: lexicalRanking(bm25, reference, k)}, nil
}

func lexicalRanking(bm25, reference []mem.Hit, k int) []mem.Hit {
	scores := reciprocalRankFusion([]rankedLane{{weight: 1, hits: bm25}})
	// Reference hits are reserved before the final k cut so exact/substring
	// coverage competes on order, not membership. Expansion/BM25 hits fill only
	// the remaining slots.
	out := make([]mem.Hit, 0, len(scores)+len(reference))
	seen := make(map[string]bool, len(scores)+len(reference))
	for _, h := range reference {
		if seen[h.ClaimID] {
			continue
		}
		seen[h.ClaimID] = true
		out = append(out, h)
	}
	extras := make([]mem.Hit, 0, len(scores))
	for id, score := range scores {
		if seen[id] {
			continue
		}
		extras = append(extras, mem.Hit{ClaimID: id, Score: score})
	}
	sort.SliceStable(extras, func(i, j int) bool {
		if extras[i].Score == extras[j].Score {
			return extras[i].ClaimID < extras[j].ClaimID
		}
		return extras[i].Score > extras[j].Score
	})
	out = append(out, extras...)
	if len(out) > k {
		out = out[:k]
	}
	return out
}

func (e *Engine) semanticRanking(queryVector []float32, docs []doc, bm25, reference []mem.Hit, k int) []mem.Hit {
	lexScore := make(map[string]float64, len(bm25))
	lexRank := make(map[string]int, len(bm25))
	for i, h := range bm25 {
		lexScore[h.ClaimID] = h.Score
		lexRank[h.ClaimID] = i
	}
	type semanticHit struct {
		id       string
		cosine   float64
		lexScore float64
		lexRank  int
	}
	semantic := make([]semanticHit, 0, len(docs))
	for _, d := range docs {
		if len(d.vector) != len(queryVector) {
			continue
		}
		semanticScore := cosine(queryVector, d.vector)
		if semanticScore <= 0 {
			continue
		}
		rank, ok := lexRank[d.claim.ClaimID]
		if !ok {
			rank = int(^uint(0) >> 1)
		}
		semantic = append(semantic, semanticHit{
			id:       d.claim.ClaimID,
			cosine:   semanticScore,
			lexScore: lexScore[d.claim.ClaimID],
			lexRank:  rank,
		})
	}
	sort.SliceStable(semantic, func(i, j int) bool {
		if semantic[i].cosine != semantic[j].cosine {
			return semantic[i].cosine > semantic[j].cosine
		}
		if semantic[i].lexScore != semantic[j].lexScore {
			return semantic[i].lexScore > semantic[j].lexScore
		}
		if semantic[i].lexRank != semantic[j].lexRank {
			return semantic[i].lexRank < semantic[j].lexRank
		}
		return semantic[i].id < semantic[j].id
	})
	out := make([]mem.Hit, 0, len(semantic)+len(reference))
	seen := make(map[string]bool, len(semantic)+len(reference))
	// Semantic-primary: emit the cosine-sorted claims first (lexical already applied as
	// a tiebreak in the sort above). Reference/substring hits are NOT reserved ahead of
	// semantic — that blind reservation let common substrings ("system", "skill") flood
	// the top-k and bury the semantically-correct answer, stalling recall at the lexical
	// baseline (measured 2026-07-07: reserved 0.879 vs pure-semantic 1.000). Reference
	// hits only backfill the tail for claims that had no vector (partial/degraded index).
	for _, h := range semantic {
		seen[h.id] = true
		out = append(out, mem.Hit{ClaimID: h.id, Score: h.cosine})
	}
	for _, h := range reference {
		if seen[h.ClaimID] {
			continue
		}
		seen[h.ClaimID] = true
		out = append(out, h)
	}
	if len(out) > k {
		out = out[:k]
	}
	return out
}

func (e *Engine) bm25(text string, docs []doc) []mem.Hit {
	queryTerms := unique(salientTerms(text))
	if len(queryTerms) == 0 {
		return nil
	}
	avgLen := 0.0
	for _, d := range docs {
		avgLen += float64(len(d.tokens))
	}
	avgLen /= float64(len(docs))
	df := make(map[string]int, len(queryTerms))
	for _, term := range queryTerms {
		for _, d := range docs {
			if d.freq[term] > 0 {
				df[term]++
			}
		}
	}
	hits := make([]mem.Hit, 0, len(docs))
	n := float64(len(docs))
	for _, d := range docs {
		score := 0.0
		dl := float64(len(d.tokens))
		if dl == 0 {
			continue
		}
		for _, term := range queryTerms {
			tf := float64(d.freq[term])
			if tf == 0 {
				continue
			}
			idf := math.Log(1 + (n-float64(df[term])+0.5)/(float64(df[term])+0.5))
			denom := tf + k1*(1-b+b*dl/avgLen)
			score += idf * (tf * (k1 + 1) / denom)
		}
		if score > 0 {
			hits = append(hits, mem.Hit{ClaimID: d.claim.ClaimID, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

type rankedLane struct {
	weight float64
	hits   []mem.Hit
}

func reciprocalRankFusion(lanes []rankedLane) map[string]float64 {
	out := map[string]float64{}
	for _, lane := range lanes {
		if lane.weight <= 0 {
			continue
		}
		for rank, h := range lane.hits {
			out[h.ClaimID] += lane.weight / (60 + float64(rank+1))
		}
	}
	return out
}

func referenceContains(text string, docs []doc) []mem.Hit {
	terms := unique(salientTerms(text))
	if len(terms) == 0 {
		return nil
	}
	hits := make([]mem.Hit, 0, len(docs))
	for _, d := range docs {
		hay := strings.ToLower(d.claim.Text)
		score := 0.0
		for _, term := range terms {
			if strings.Contains(hay, term) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, mem.Hit{ClaimID: d.claim.ClaimID, Score: score})
		}
	}
	// Mirror mem.LexicalEngine/refMatch score-only ordering so engine-compare's
	// top-k reference set is reserved when the reference set itself exceeds k.
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].Score > hits[j].Score
	})
	return hits
}

func allowSet(gate string, allowlist []string) map[string]bool {
	if gate == mem.GateAll {
		return nil
	}
	allow := make(map[string]bool, len(allowlist))
	for _, id := range allowlist {
		allow[id] = true
	}
	return allow
}

func unique(tokens []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}

func Tokenize(text string) []string {
	var out []string
	for _, chunk := range alnumChunks(text) {
		out = append(out, splitIdentifier(chunk)...)
	}
	return out
}

// queryStopwords are English function/question words stripped from QUERY text
// before scoring. Claims are never filtered — only the query — so that
// natural-language phrasing ("what is the database for the sm project?") ranks
// the same salient terms ("sm database") and terse entity/predicate claims stop
// being diluted out of the top-k by filler words that match many claims.
var queryStopwords = map[string]bool{
	"a": true, "about": true, "again": true, "an": true, "and": true, "any": true,
	"are": true, "as": true, "at": true, "be": true, "been": true, "being": true,
	"but": true, "by": true, "can": true, "did": true, "do": true, "does": true,
	"for": true, "from": true, "had": true, "has": true, "have": true, "how": true,
	"i": true, "if": true, "in": true, "into": true, "is": true, "it": true,
	"its": true, "me": true, "my": true, "not": true, "of": true, "on": true,
	"or": true, "our": true, "so": true, "that": true, "the": true, "their": true,
	"then": true, "there": true, "these": true, "they": true, "this": true,
	"to": true, "was": true, "we": true, "were": true, "what": true, "when": true,
	"where": true, "which": true, "who": true, "why": true, "will": true,
	"with": true, "would": true, "you": true, "your": true,
}

// salientTerms tokenizes query text and drops function/question words so the
// scoring lanes weight only content-bearing terms. Falls back to the unfiltered
// tokens when filtering would leave nothing (e.g. an all-stopword query), so it
// never returns an empty term set.
func salientTerms(text string) []string {
	toks := Tokenize(text)
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		if queryStopwords[t] {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return toks
	}
	return out
}

func alnumChunks(text string) []string {
	var chunks []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		chunks = append(chunks, string(cur))
		cur = cur[:0]
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return chunks
}

func splitIdentifier(s string) []string {
	rs := []rune(s)
	var parts []string
	start := 0
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1], rs[i]
		var next rune
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		if unicode.IsUpper(cur) && (unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && next != 0 && unicode.IsLower(next))) {
			parts = append(parts, strings.ToLower(string(rs[start:i])))
			start = i
		}
	}
	parts = append(parts, strings.ToLower(string(rs[start:])))
	return parts
}

func (e *Engine) indexVectors(ctx context.Context, gen int64) error {
	existing := map[vectorKey]vectorRow{}
	var loadedManifest vectorManifest
	cacheLoaded := false
	// The cache key is home + embedder identity — NOT the dimension, which an
	// OpenAI-compatible embedder may only learn after its first call. Gating
	// construction on Dim()>0 silently disabled the persistent cache for every
	// model outside the static inference table (measured 2026-08-23: qwen3-
	// embedding re-embedded the full corpus on each sidecar spawn because its
	// dim was unknown at construction time). Load validates dims against the
	// manifest, so construct whenever a home exists.
	if e.cache == nil && strings.TrimSpace(e.compoundHome) != "" {
		cache, err := newVectorCache(e.compoundHome, e.embedder.Identity())
		if err != nil {
			return err
		}
		e.cache = cache
	}
	if e.cache != nil {
		manifest, loaded, err := e.cache.load()
		if err != nil {
			switch {
			case errors.Is(err, errVectorCacheMiss), errors.Is(err, errVectorCacheCorrupt), errors.Is(err, errVectorTemplateMismatch), errors.Is(err, errVectorDimMismatch):
				existing = map[vectorKey]vectorRow{}
			default:
				return err
			}
		} else {
			loadedManifest = manifest
			cacheLoaded = true
			existing = loaded
		}
	}
	rows := make([]vectorRow, len(e.docs))
	missingTexts := make([]string, 0)
	missingPositions := make([]int, 0)
	for i, d := range e.docs {
		key := vectorKey{claimID: d.claim.ClaimID, textHash: d.textHash}
		if row, ok := existing[key]; ok {
			rows[i] = row
			continue
		}
		missingTexts = append(missingTexts, d.wrapped)
		missingPositions = append(missingPositions, i)
	}
	if len(missingTexts) > 0 {
		vecs, err := e.embedder.Embed(ctx, missingTexts)
		if err != nil {
			return err
		}
		if len(vecs) != len(missingTexts) {
			return fmt.Errorf("embedder returned %d vectors for %d documents", len(vecs), len(missingTexts))
		}
		if e.cache == nil {
			cache, err := newVectorCache(e.compoundHome, e.embedder.Identity())
			if err != nil {
				return err
			}
			e.cache = cache
		}
		for i, vec := range vecs {
			pos := missingPositions[i]
			if len(vec) != e.embedder.Dim() {
				return fmt.Errorf("%w: embedded doc dim %d != adapter dim %d", errVectorDimMismatch, len(vec), e.embedder.Dim())
			}
			rows[pos] = vectorRow{ClaimID: e.docs[pos].claim.ClaimID, TextHash: e.docs[pos].textHash, Vector: vec}
		}
	}
	for i := range rows {
		e.docs[i].vector = rows[i].Vector
	}
	if e.cache == nil {
		return errors.New("vector cache unavailable")
	}
	if len(missingTexts) == 0 && cacheLoaded && loadedManifest.CorpusGen == gen &&
		loadedManifest.ClaimCount == len(rows) && loadedManifest.EmbeddedCount == len(rows) && len(existing) == len(rows) {
		return nil
	}
	if err := e.cache.write(rows, gen); err != nil {
		return err
	}
	return nil
}

func wrapDocText(c mem.ClaimText) string {
	// Claims are atomic and already carry entity/predicate/object/id in Text, so we
	// embed the raw index text. The old "entity: <E> [<T>]" prefix homogenized all
	// same-entity claims (every decision embedded with an identical prefix), which
	// collapsed semantic separation and stalled recall at the lexical baseline
	// (measured 2026-07-07: wrapper 0.879 vs raw 1.000 on the golden set). The
	// contextual-prefix technique belongs to the corpus chunk adapter (context-blind
	// markdown), not to self-describing claims.
	return c.Text
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	sum := 0.0
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

type ProtocolConfig struct {
	Embedder     EmbedderAdapter
	CompoundHome string
	Stderr       io.Writer
}

func (cfg ProtocolConfig) identity() string {
	if cfg.Embedder == nil {
		return DefaultEmbedder
	}
	return cfg.Embedder.Identity()
}

func RunProtocol(in io.Reader, out io.Writer, cfg ProtocolConfig) error {
	engine, err := NewEngine(EngineConfig{Embedder: cfg.Embedder, CompoundHome: cfg.CompoundHome})
	if err != nil {
		return err
	}
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	for {
		var req mem.Request
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		resp := mem.Response{RequestID: req.RequestID, SchemaVersion: mem.ProtocolVersion}
		if req.SchemaVersion != 0 && req.SchemaVersion != mem.ProtocolVersion {
			resp.OK, resp.Error = false, &mem.WireError{Code: mem.CodeBadRequest, Message: "unsupported schema_version"}
			if err := enc.Encode(&resp); err != nil {
				return err
			}
			continue
		}
		switch req.Op {
		case mem.OpCapabilities:
			resp.OK, resp.Name, resp.Breadth, resp.Embedder, resp.Ready, resp.CorpusGen = true, Name, true, cfg.identity(), engine.Ready(), engine.CorpusGen()
		case mem.OpIndex:
			indexed, err := engine.IndexWithError(req.Claims, req.CorpusGen)
			if err != nil {
				resp.OK = false
				resp.Error = breadthWireError(err)
			} else {
				resp.OK, resp.Indexed, resp.CorpusGen = true, indexed, engine.CorpusGen()
			}
		case mem.OpQuery:
			if !engine.Ready() {
				resp.OK = false
				resp.Error = &mem.WireError{Code: mem.CodeIndexMissing, Message: "index before query"}
			} else {
				result, err := engine.QueryWithLanes(req.Text, req.Gate, req.Allowlist, req.K)
				if err != nil {
					resp.OK = false
					resp.Error = breadthWireError(err)
				} else {
					resp.OK, resp.Hits, resp.DegradedLanes, resp.CorpusGen = true, result.Hits, result.DegradedLanes, engine.CorpusGen()
					if result.Warning != "" {
						warn := cfg.Stderr
						if warn == nil {
							warn = io.Discard
						}
						fmt.Fprintln(warn, "compound-breadth-engine:", result.Warning)
					}
				}
			}
		default:
			resp.OK = false
			resp.Error = &mem.WireError{Code: mem.CodeBadRequest, Message: "unknown op"}
		}
		if err := enc.Encode(&resp); err != nil {
			return err
		}
	}
}

func breadthWireError(err error) *mem.WireError {
	code := mem.CodeEngineUnavailable
	switch {
	case errors.Is(err, errVectorEmbedderMismatch):
		code = mem.CodeEmbedderMismatch
	case errors.Is(err, errVectorDimMismatch):
		code = mem.CodeDimMismatch
	case errors.Is(err, errVectorCacheMiss):
		code = mem.CodeIndexMissing
	}
	return &mem.WireError{Code: code, Message: err.Error()}
}
