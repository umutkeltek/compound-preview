package corpus

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"compound/internal/mem"
	"compound/internal/sourceobs"
)

type QueryReport struct {
	SchemaVersion    string     `json:"schema_version"`
	Lane             string     `json:"lane"`
	Query            string     `json:"query"`
	EmbedderIdentity string     `json:"embedder_identity"`
	Hits             []QueryHit `json:"hits"`
	Cards            []string   `json:"cards"`
}

type QueryHit struct {
	ItemID     string  `json:"item_id"`
	ChunkID    string  `json:"chunk_id"`
	SourceKind string  `json:"source_kind,omitempty"`
	Title      string  `json:"title,omitempty"`
	Score      float64 `json:"score"`
	Text       string  `json:"text,omitempty"`
	TextStatus string  `json:"text_status"`
	// Neighbors are the chunks adjacent to this hit inside the same item,
	// populated only when QueryOptions.NeighborRadius > 0. They hang off the
	// hit (not in QueryReport.Hits) because they were never ranked against
	// the query — splicing them into the ranked list would let a downstream
	// consumer sort context against results. The intent is the chunk before
	// and after, not new answers.
	Neighbors []QueryNeighbor `json:"neighbors,omitempty"`
}

// QueryNeighbor is an adjacent chunk attached to a hit as context. There is
// deliberately NO Score field: a neighbor was not ranked, and any placeholder
// score invites sorting context against results.
type QueryNeighbor struct {
	ChunkID    string `json:"chunk_id"`
	Ordinal    int    `json:"ordinal"`
	Text       string `json:"text,omitempty"`
	TextStatus string `json:"text_status"`
}

// MaxNeighborRadius bounds context expansion. At radius r a k-hit query asks
// for up to k*(2r+1) chunks; an unbounded radius turns one query into a
// whole-corpus read.
const MaxNeighborRadius = 5

type QueryOptions struct {
	IncludeText bool
	// NeighborRadius expands each hit with up to this many chunks on each
	// side, within the same item. 0 (the zero value) disables expansion, so
	// every caller predating this option is byte-identical. A negative radius
	// is REJECTED, not clamped — clamping would silently turn a caller's bug
	// into "expansion is off", which looks exactly like success.
	NeighborRadius int
}

// validate returns an error when opts carries an out-of-range NeighborRadius.
// Extracted so the validation contract is unit-testable without spinning up
// the full query machinery.
func (opts QueryOptions) validate() error {
	if opts.NeighborRadius < 0 {
		return fmt.Errorf("corpus query: neighbor radius %d is negative", opts.NeighborRadius)
	}
	if opts.NeighborRadius > MaxNeighborRadius {
		return fmt.Errorf("corpus query: neighbor radius %d exceeds max %d", opts.NeighborRadius, MaxNeighborRadius)
	}
	return nil
}

// runCorpusEngineQuery is the narrow engine-call seam used to prove privacy
// preflight happens before both ProtocolEngine.Query and its embedder work.
var runCorpusEngineQuery = func(engine *mem.ProtocolEngine, text string, k int) ([]mem.Hit, error) {
	return engine.Query(text, mem.GateAll, nil, k)
}

func Query(ctx context.Context, homePath string, lane LaneConfig, embedder Embedder, text string, k int) (QueryReport, error) {
	return QueryWithOptions(ctx, homePath, lane, embedder, text, k, QueryOptions{})
}

func QueryWithOptions(ctx context.Context, homePath string, lane LaneConfig, embedder Embedder, text string, k int, opts QueryOptions) (QueryReport, error) {
	lane, err := ValidateLaneConfig(lane)
	if err != nil {
		return QueryReport{}, err
	}
	if err := validateEmbedder(lane, embedder); err != nil {
		return QueryReport{}, err
	}
	if err := opts.validate(); err != nil {
		return QueryReport{}, err
	}
	manifest, err := validateQueryItemAccess(homePath, lane)
	if err != nil {
		return QueryReport{}, err
	}
	transport := NewIndexTransport(homePath, lane, embedder)
	engine := mem.NewProtocolEngine(transport, lane.Embedder.Identity())
	defer engine.Close()
	hits, err := runCorpusEngineQuery(engine, text, k)
	if err != nil {
		return QueryReport{}, err
	}
	// Metadata (and therefore QueryHit.Title) is derived from the manifest
	// returned by validateQueryItemAccess, NOT from a fresh read of the
	// indexed manifest on disk. The indexed privacy snapshot is stale by
	// definition once the current SourceManifest is allowed to reclassify
	// an item; loadChunkMeta(dir) would re-render chunkTitle against the
	// stale indexed privacy and could publish a private basename on a hit
	// whose body the same query correctly suppresses. The validated
	// manifest already carries current privacy propagated across every
	// governed item, so deriving meta here keeps title and body text in
	// lock-step on the same privacy decision.
	meta := metaFromManifest(manifest)
	report := QueryReport{
		SchemaVersion:    ManifestSchemaVersion,
		Lane:             lane.Lane,
		Query:            text,
		EmbedderIdentity: lane.Embedder.Identity(),
		Cards:            []string{},
	}
	for _, hit := range hits {
		info := meta[hit.ClaimID]
		resolved, err := ResolveChunkText(homePath, manifest, hit.ClaimID, opts.IncludeText, lane)
		if err != nil {
			return QueryReport{}, fmt.Errorf("resolve chunk text for %s: %w", hit.ClaimID, err)
		}
		qh := QueryHit{
			ItemID:     info.itemID,
			ChunkID:    hit.ClaimID,
			SourceKind: info.sourceKind,
			Title:      info.title,
			Score:      hit.Score,
			Text:       resolved.Text,
			TextStatus: string(resolved.Status),
		}
		if opts.NeighborRadius > 0 {
			neighbors, err := expandNeighbors(homePath, manifest, lane, opts.IncludeText, hit.ClaimID, opts.NeighborRadius)
			if err != nil {
				return QueryReport{}, fmt.Errorf("resolve neighbors for %s: %w", hit.ClaimID, err)
			}
			qh.Neighbors = neighbors
		}
		report.Hits = append(report.Hits, qh)
	}
	return report, nil
}

// expandNeighbors returns the chunks adjacent to anchorChunkID within its
// item, dropping any neighbor whose chunk resolves to a non-OK status.
// "Dropped, not degraded" is the rule — a withheld or stale neighbor is part
// of what the gate hides, so a private-bodies-fetch flag carrying a
// suppressed neighbor would leak the existence of an item the caller is
// supposed not to know about. Cross-item bleed is closed by construction:
// candidate ordinals live inside anchorItem's slice and the chunk id is
// rebuilt from the same item id, so expansion cannot address another item.
func expandNeighbors(homePath string, manifest Manifest, lane LaneConfig, includeText bool, anchorChunkID string, radius int) ([]QueryNeighbor, error) {
	itemID, ordinals, err := neighborOrdinalsFor(manifest, anchorChunkID, radius)
	if err != nil || len(ordinals) == 0 {
		return nil, err
	}
	out := make([]QueryNeighbor, 0, len(ordinals))
	for _, ordinal := range ordinals {
		candidate := buildChunkID(itemID, ordinal)
		resolved, err := ResolveChunkText(homePath, manifest, candidate, includeText, lane)
		if err != nil {
			return nil, err
		}
		if resolved.Status != ChunkTextOK {
			continue
		}
		out = append(out, QueryNeighbor{
			ChunkID:    candidate,
			Ordinal:    ordinal,
			Text:       resolved.Text,
			TextStatus: string(resolved.Status),
		})
	}
	return out, nil
}

// neighborOrdinalsFor returns the ordinals adjacent to anchorChunkID within
// its item, after clamping the radius against the item's chunk count. The
// anchor's own ordinal is excluded. Item boundaries are the only clamp
// sources, so an expansion cannot address another item: the chunk id is
// rebuilt from the same item id as the anchor.
//
// Extracted from expandNeighbors so the bounding/clamping logic is
// unit-testable without spinning up ResolveChunkText's body chunker.
func neighborOrdinalsFor(manifest Manifest, anchorChunkID string, radius int) (itemID string, ordinals []int, err error) {
	itemID, anchorOrdinal, err := splitChunkID(anchorChunkID)
	if err != nil {
		return "", nil, err
	}
	item, ok := findManifestItem(manifest, itemID)
	if !ok {
		return itemID, nil, nil
	}
	chunkCount := len(item.Chunks)
	if chunkCount == 0 {
		return itemID, nil, nil
	}
	if radius < 0 {
		radius = 0
	}
	low := anchorOrdinal - radius
	if low < 0 {
		low = 0
	}
	high := anchorOrdinal + radius + 1
	if high > chunkCount {
		high = chunkCount
	}
	for ordinal := low; ordinal < high; ordinal++ {
		if ordinal == anchorOrdinal {
			continue
		}
		ordinals = append(ordinals, ordinal)
	}
	return itemID, ordinals, nil
}

// buildChunkID assembles a chunk id from item id and ordinal. The shape is
// the inverse of splitChunkID in chunk.go — same "<item_id>#<ordinal>" form
// that ResolveChunkText accepts.
func buildChunkID(itemID string, ordinal int) string {
	return itemID + "#" + strconv.Itoa(ordinal)
}

// validateQueryItemAccess refreshes item privacy from the current source
// manifest before constructing/calling the query engine. This prevents an
// index built while the source_in_place gate was off (or before a privacy
// change) from becoming a bypass after the gate is enabled. Only index
// metadata and source-manifest metadata are read here; source body bytes and
// the query embedder remain behind the decision.
//
// The returned manifest carries the current privacy classification on every
// item so that the downstream body-resolution seam (ResolveChunkText) sees
// the authority's current stance, not the indexed snapshot. Body policy stays
// sourced from the indexed item — a current body-policy edit cannot disguise a
// legacy indexed source_in_place body and bypass its newly enabled gate.
//
// Projection-integrity guard: before the indexed privacy label is overwritten
// by the current authority, every governed indexed item must already carry a
// well-formed privacy value (public/private). A corrupted or replaced
// projection cannot be widened back into a valid read posture just because
// the current authority is benign — the fail-closed check runs on the input
// the resolver would otherwise observe.
//
// Legacy default-off compatibility: the current SourceManifest is loaded only
// when at least one indexed item is governed (owned_snapshot, or source_in_place
// with the explicit gate enabled). A lane whose every indexed item is
// source_in_place with gate=false never consults the canonical manifest, so a
// published quickstart running a public lane does not gain a new dependency on
// a current SourceManifest being present at query time.
func validateQueryItemAccess(homePath string, lane LaneConfig) (Manifest, error) {
	dir := IndexDir(homePath, lane)
	indexed, err := readManifest(dir)
	if err != nil {
		return Manifest{}, err
	}
	// Indexed-projection well-formedness check runs first: it is independent of
	// whether the current SourceManifest is reachable, and it prevents a
	// malformed projection from being silently corrected by a benign current
	// authority before the body read posture is decided.
	for _, indexedItem := range indexed.Items {
		if !itemAccessIsGoverned(lane, indexedItem) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(indexedItem.Privacy)) {
		case "public", "private":
			// well-formed
		default:
			return Manifest{}, fmt.Errorf("%s item %q has malformed privacy field (must be public or private, got %q)", indexedItem.BodyPolicy, indexedItem.ID, strings.TrimSpace(indexedItem.Privacy))
		}
	}
	needsCurrent := false
	for _, indexedItem := range indexed.Items {
		if itemAccessIsGoverned(lane, indexedItem) {
			needsCurrent = true
			break
		}
	}
	if !needsCurrent {
		return indexed, nil
	}
	current, err := sourceobs.LoadSourceManifest(homePath, lane.SourceFamily)
	if err != nil {
		return Manifest{}, fmt.Errorf("corpus query: load current source manifest: %w", err)
	}
	currentByID := make(map[string]sourceobs.SourceManifestItem, len(current.Items))
	for _, item := range current.Items {
		currentByID[item.ID] = item
	}
	for i, indexedItem := range indexed.Items {
		governed := itemAccessIsGoverned(lane, indexedItem)
		currentItem, ok := currentByID[indexedItem.ID]
		if !ok {
			if governed {
				return Manifest{}, fmt.Errorf("corpus query: governed item %q is absent from current source manifest; privacy cannot be validated", indexedItem.ID)
			}
			continue
		}
		// A governed sibling can require loading current authority for the
		// whole lane. That must not silently opt a legacy, gate-off
		// source_in_place item into current-privacy semantics: preserving
		// its indexed label is the documented default-off contract.
		if !governed {
			continue
		}
		accessItem := currentItem
		// The indexed body policy identifies the bytes this query could resolve;
		// only mutable privacy metadata comes from the current authority item.
		// A current body-policy edit therefore cannot disguise a legacy indexed
		// source_in_place body and bypass its newly enabled gate.
		accessItem.BodyPolicy = indexedItem.BodyPolicy
		if err := validateCurrentItemAccess(lane, accessItem); err != nil {
			return Manifest{}, fmt.Errorf("corpus query: %w", err)
		}
		// Propagate the current privacy classification into the manifest that
		// downstream body resolution will read. Indexed privacy can be stale;
		// the body's read posture must reflect the authority's current
		// stance, so an index-public item reclassified to private suppresses
		// body text (and never reads source bytes) on the very next query.
		indexed.Items[i].Privacy = currentItem.Privacy
	}
	return indexed, nil
}

type IndexTransport struct {
	homePath string
	lane     LaneConfig
	embedder Embedder
	loaded   *loadedIndex
}

type loadedIndex struct {
	identity IdentityFile
	manifest Manifest
	vectors  VectorFile
}

func NewIndexTransport(homePath string, lane LaneConfig, embedder Embedder) *IndexTransport {
	return &IndexTransport{homePath: homePath, lane: lane, embedder: embedder}
}

func (t *IndexTransport) RoundTrip(req mem.Request) (mem.Response, error) {
	resp := mem.Response{SchemaVersion: mem.ProtocolVersion, RequestID: req.RequestID}
	switch req.Op {
	case mem.OpCapabilities:
		loaded, err := t.load()
		if err != nil {
			resp.OK = false
			resp.Error = &mem.WireError{Code: mem.CodeEngineUnavailable, Message: err.Error()}
			return resp, nil
		}
		resp.OK = true
		resp.Name = "corpus-index"
		resp.Breadth = true
		resp.Ready = true
		resp.Embedder = loaded.identity.EmbedderIdentity
		return resp, nil
	case mem.OpQuery:
		loaded, err := t.load()
		if err != nil {
			resp.OK = false
			resp.Error = &mem.WireError{Code: mem.CodeEngineUnavailable, Message: err.Error()}
			return resp, nil
		}
		hits, err := t.query(loaded, req)
		if err != nil {
			resp.OK = false
			resp.Error = &mem.WireError{Code: mem.CodeBadRequest, Message: err.Error()}
			return resp, nil
		}
		resp.OK = true
		resp.Hits = hits
		return resp, nil
	default:
		resp.OK = false
		resp.Error = &mem.WireError{Code: mem.CodeBadRequest, Message: "unsupported corpus op " + req.Op}
		return resp, nil
	}
}

func (t *IndexTransport) Close() error { return nil }

func (t *IndexTransport) load() (*loadedIndex, error) {
	if t.loaded != nil {
		return t.loaded, nil
	}
	dir := IndexDir(t.homePath, t.lane)
	identity, err := readIdentity(dir)
	if err != nil {
		return nil, err
	}
	manifest, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	vectors, err := readVectors(dir)
	if err != nil {
		return nil, err
	}
	t.loaded = &loadedIndex{identity: identity, manifest: manifest, vectors: vectors}
	return t.loaded, nil
}

func (t *IndexTransport) query(index *loadedIndex, req mem.Request) ([]mem.Hit, error) {
	queryVectors, err := t.embedder.Embed(context.Background(), []string{req.Text})
	if err != nil {
		return nil, err
	}
	if len(queryVectors) != 1 {
		return nil, fmt.Errorf("query embedder returned %d vectors", len(queryVectors))
	}
	query, err := normalizeVector(queryVectors[0])
	if err != nil {
		return nil, err
	}
	allow := map[string]bool{}
	for _, id := range req.Allowlist {
		allow[id] = true
	}
	current := manifestChunkHashes(index.manifest)
	var hits []mem.Hit
	for id, vec := range index.vectors.Vectors {
		if _, ok := current[id]; !ok {
			continue
		}
		if req.Gate != mem.GateAll && !allow[id] {
			continue
		}
		normalized, err := normalizeVector(vec)
		if err != nil {
			continue
		}
		if len(normalized) != len(query) {
			continue
		}
		hits = append(hits, mem.Hit{ClaimID: id, Score: cosine(query, normalized)})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ClaimID < hits[j].ClaimID
		}
		return hits[i].Score > hits[j].Score
	})
	if req.K > 0 && len(hits) > req.K {
		hits = hits[:req.K]
	}
	return hits, nil
}

func cosine(a, b []float32) float64 {
	sum := 0.0
	for i := range a {
		sum += float64(a[i] * b[i])
	}
	return sum
}

type chunkInfo struct {
	itemID     string
	sourceKind string
	title      string
	privacy    string
}

const (
	// privateChunkTitle stands in for a private item that has no page_slug. Even the
	// basename of a private file can name a client, product, or person, so the label
	// is opaque; the hit still carries item_id for identification.
	privateChunkTitle = "(private source)"
	// untitledChunkTitle covers a source_ref with no usable base ("", ".", "/").
	untitledChunkTitle = "(untitled source)"
)

// chunkTitle picks the title a hit carries out through `memory search --json`, the
// console, and ContextPack materialization. Chunk text is privacy-gated in
// ResolveChunkText; the title travels the same surfaces and must be gated too — it
// previously fell back to the raw source_ref, publishing a full local path even while
// the text beside it was correctly suppressed. The decision reads the manifest's
// privacy field rather than inspecting the shape of the path string.
func chunkTitle(pageSlug, sourceRef, privacy string) string {
	if title := strings.TrimSpace(pageSlug); title != "" {
		return title
	}
	if strings.EqualFold(strings.TrimSpace(privacy), "private") {
		return privateChunkTitle
	}
	base := filepath.Base(strings.TrimSpace(sourceRef))
	if base == "." || base == string(filepath.Separator) || strings.TrimSpace(base) == "" {
		return untitledChunkTitle
	}
	return base
}

func loadChunkMeta(dir string) (map[string]chunkInfo, error) {
	manifest, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	return metaFromManifest(manifest), nil
}

// metaFromManifest projects a Manifest into per-chunk metadata, sharing the
// same title rule as loadChunkMeta. QueryWithOptions uses this directly off
// the validateQueryItemAccess result so chunkTitle sees the current privacy
// classification rather than the stale indexed privacy snapshot. The
// loadChunkMeta(dir) path remains for any callers that still need it from a
// raw disk manifest.
func metaFromManifest(manifest Manifest) map[string]chunkInfo {
	out := map[string]chunkInfo{}
	for _, item := range manifest.Items {
		info := chunkInfo{
			itemID:     item.ID,
			sourceKind: item.SourceKind,
			privacy:    strings.ToLower(strings.TrimSpace(item.Privacy)),
		}
		info.title = chunkTitle(item.PageSlug, item.SourceRef, info.privacy)
		for _, chunk := range item.Chunks {
			out[fmt.Sprintf("%s#%d", item.ID, chunk.Ordinal)] = info
		}
	}
	return out
}
