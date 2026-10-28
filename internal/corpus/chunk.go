package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"compound/internal/home"
	"compound/internal/sourceobs"
)

type Chunk struct {
	ID        string `json:"id"`
	ItemID    string `json:"item_id"`
	Ordinal   int    `json:"ordinal"`
	Section   string `json:"section,omitempty"`
	Prefix    string `json:"prefix"`
	Text      string `json:"text"`
	EmbedText string `json:"embed_text"`
	ChunkHash string `json:"chunk_hash"`
}

func ChunkItem(item sourceobs.SourceManifestItem, body string, cfg ChunkConfig) ([]Chunk, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	units := splitStructureUnits(body)
	base := packUnits(units, cfg.Size)
	if len(base) == 0 && strings.TrimSpace(body) != "" {
		base = []chunkDraft{{text: trimToRuneBoundary(body), section: ""}}
	}
	var chunks []Chunk
	for i, draft := range base {
		text := draft.text
		if i > 0 && cfg.Overlap > 0 {
			overlap := tailRunes(base[i-1].text, cfg.Overlap)
			if overlap != "" {
				text = overlap + "\n\n" + text
				if runeLen(text) > cfg.Size {
					text = tailRunes(text, cfg.Size)
				}
			}
		}
		prefix := buildPrefix(item, draft.section)
		embedText := prefix + " " + text
		sum := sha256.Sum256([]byte(embedText))
		chunks = append(chunks, Chunk{
			ID:        fmt.Sprintf("%s#%d", item.ID, i),
			ItemID:    item.ID,
			Ordinal:   i,
			Section:   draft.section,
			Prefix:    prefix,
			Text:      text,
			EmbedText: embedText,
			ChunkHash: "sha256:" + hex.EncodeToString(sum[:]),
		})
	}
	return chunks, nil
}

type structureUnit struct {
	text    string
	section string
}

type chunkDraft struct {
	text    string
	section string
}

func splitStructureUnits(body string) []structureUnit {
	parts := strings.Split(body, "\n\n")
	var out []structureUnit
	headingPath := ""
	turn := 0
	for _, part := range parts {
		text := strings.TrimSpace(part)
		if text == "" {
			continue
		}
		section := headingPath
		if heading := markdownHeading(text); heading != "" {
			headingPath = heading
			section = headingPath
		}
		if isTurn(text) {
			section = fmt.Sprintf("turn:%d", turn)
			turn++
		}
		out = append(out, structureUnit{text: text, section: section})
	}
	return out
}

func markdownHeading(text string) string {
	first := strings.TrimSpace(strings.Split(text, "\n")[0])
	if !strings.HasPrefix(first, "#") {
		return ""
	}
	level := 0
	for _, r := range first {
		if r != '#' {
			break
		}
		level++
	}
	if level == 0 || level > 6 || len(first) <= level || first[level] != ' ' {
		return ""
	}
	return truncateRunes(strings.TrimSpace(first[level:]), 80)
}

func isTurn(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.HasPrefix(lower, "user:") ||
		strings.HasPrefix(lower, "assistant:") ||
		strings.HasPrefix(lower, "system:") ||
		strings.HasPrefix(lower, "turn ")
}

func packUnits(units []structureUnit, size int) []chunkDraft {
	var out []chunkDraft
	var current chunkDraft
	flush := func() {
		if strings.TrimSpace(current.text) == "" {
			return
		}
		out = append(out, current)
		current = chunkDraft{}
	}
	for _, unit := range units {
		pieces := hardSplit(unit.text, size)
		for _, piece := range pieces {
			if strings.TrimSpace(piece) == "" {
				continue
			}
			if current.text == "" {
				current = chunkDraft{text: piece, section: unit.section}
				continue
			}
			joined := current.text + "\n\n" + piece
			if runeLen(joined) <= size {
				current.text = joined
				if current.section == "" {
					current.section = unit.section
				}
				continue
			}
			flush()
			current = chunkDraft{text: piece, section: unit.section}
		}
	}
	flush()
	return out
}

func hardSplit(text string, size int) []string {
	if runeLen(text) <= size {
		return []string{text}
	}
	runes := []rune(text)
	var out []string
	for start := 0; start < len(runes); start += size {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, strings.TrimSpace(string(runes[start:end])))
	}
	return out
}

func buildPrefix(item sourceobs.SourceManifestItem, section string) string {
	sourceKind := truncateRunes(strings.TrimSpace(item.SourceKind), 40)
	title := strings.TrimSpace(item.PageSlug)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(item.SourceRef), filepath.Ext(item.SourceRef))
	}
	title = truncateRunes(title, 120)
	section = truncateRunes(strings.TrimSpace(section), 80)
	entities := prefixEntities(item.CompoundEntityRefs)
	fields := []string{sourceKind, title, section, entities}
	prefix := prefixFromFields(fields)
	if runeLen(prefix) <= 240 {
		return prefix
	}
	for _, idx := range []int{3, 2, 1} {
		if fields[idx] == "" {
			continue
		}
		fields[idx] = "…"
		prefix = prefixFromFields(fields)
		if runeLen(prefix) <= 240 {
			return prefix
		}
		fields[idx] = ""
		prefix = prefixFromFields(fields)
		if runeLen(prefix) <= 240 {
			return prefix
		}
	}
	return truncateRunes(prefix, 240)
}

func prefixEntities(values []string) string {
	seen := map[string]bool{}
	var entities []string
	for _, value := range values {
		value = truncateRunes(strings.TrimSpace(value), 40)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		entities = append(entities, value)
	}
	sort.Strings(entities)
	if len(entities) > 5 {
		entities = entities[:5]
	}
	return strings.Join(entities, ", ")
}

func prefixFromFields(fields []string) string {
	var parts []string
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			parts = append(parts, field)
		}
	}
	if len(parts) == 0 {
		return "[]"
	}
	return "[" + strings.Join(parts, " · ") + "]"
}

func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || runeLen(value) <= limit {
		return value
	}
	runes := []rune(value)
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func tailRunes(value string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= n {
		return value
	}
	return string(runes[len(runes)-n:])
}

func runeLen(value string) int {
	return utf8.RuneCountInString(value)
}

func trimToRuneBoundary(value string) string {
	if utf8.ValidString(value) {
		return value
	}
	return string([]rune(value))
}

// ChunkTextStatus reports why (or whether) ResolveChunkText returned chunk text.
type ChunkTextStatus string

const (
	// ChunkTextOK means the chunk text was resolved and (per includeText) returned.
	ChunkTextOK ChunkTextStatus = "ok"
	// ChunkTextStaleSource means the live source file no longer matches the file_hash
	// recorded at index time. Returning text derived from a re-chunk in this state
	// would silently diverge from what was actually embedded, so no text is returned.
	ChunkTextStaleSource ChunkTextStatus = "stale_source"
	// ChunkTextSuppressedPrivate means the manifest item is privacy=private and the
	// caller did not opt in (includeText=false). Text flows into an agent's context and
	// onward to a model provider; a path does not — that is why suppression is the default.
	ChunkTextSuppressedPrivate ChunkTextStatus = "suppressed_private"
	// ChunkTextSourceUnavailable means the source file could not be read. Items are
	// body_policy=source_in_place: the body lives OUTSIDE COMPOUND_HOME and can be moved
	// or deleted at any time without the index knowing. That is an expected state of the
	// world, not a query failure — one unreadable body must degrade its own hit, never
	// abort the whole search.
	ChunkTextSourceUnavailable ChunkTextStatus = "source_unavailable"
	// ChunkTextStaleIndex means the index was written under a different chunk schema
	// than this binary produces. The file_hash gate cannot see this drift: the source
	// can be byte-identical while the chunker that split it has changed, so ordinal N
	// today need not be the ordinal N that was embedded. Re-derived text would then be
	// text that was never indexed — a wrong answer that looks right.
	ChunkTextStaleIndex ChunkTextStatus = "stale_index"
)

// ResolvedChunkText is the outcome of resolving a chunk_id back to its source text.
type ResolvedChunkText struct {
	Status ChunkTextStatus
	Text   string
}

// ResolveChunkText re-derives a chunk's text from its source. The corpus index never
// persists chunk text (vectors.json is chunk_id -> floats only), so recovering it means
// re-reading the file named in the manifest and re-chunking it with the manifest's
// deterministic chunk config — the same config that produced the indexed chunk.
//
// chunkID is "<item_id>#<ordinal>" as produced by ChunkItem/Query. Resolution:
//  1. Split chunkID into item_id + ordinal.
//  2. Find the manifest item by item_id.
//  3. Schema gate: if the manifest's prefix_schema_version is not this binary's, the
//     chunker has changed under the index — return ChunkTextStaleIndex with no text.
//  4. Current-item privacy gate: when a lane is supplied, apply the same
//     predicate used by index and query before any body access. Governed private
//     text is suppressed before a read unless includeText is explicit; malformed
//     labels fail closed. Omitted lane keeps the legacy source_in_place posture.
//  5. Body source dispatch (the new seam):
//     - body_policy=source_in_place: read the file at item.source_ref. The body lives
//     OUTSIDE COMPOUND_HOME and can be moved/deleted; that is expected and degrades
//     this hit to ChunkTextSourceUnavailable.
//     - body_policy=owned_snapshot: read the artifact bytes via home.ArtifactStore.
//     The artifact is the source — fall-back to SourceRef would hide custody
//     corruption, so missing/corrupt/wrong-size/wrong-hash artifacts each fail
//     closed (no text, never silent).
//  6. Drift gate: sha256 the resolved bytes and compare to item.file_hash. On mismatch,
//     return ChunkTextStaleSource with no text.
//  7. Re-chunk with the manifest's chunk config; bounds-check ordinal against the result.
//
// homePath is the COMPOUND_HOME that owns the artifact store. It is REQUIRED for
// body_policy=owned_snapshot (the resolver fails closed if it is empty); for
// body_policy=source_in_place it is ignored, so legacy call sites that build a Manifest
// directly (and have no home) keep working unchanged.
func ResolveChunkText(homePath string, manifest Manifest, chunkID string, includeText bool, lanes ...LaneConfig) (ResolvedChunkText, error) {
	itemID, ordinal, err := splitChunkID(chunkID)
	if err != nil {
		return ResolvedChunkText{}, err
	}
	item, ok := findManifestItem(manifest, itemID)
	if !ok {
		return ResolvedChunkText{}, fmt.Errorf("corpus chunk text: item %q not found in manifest", itemID)
	}
	// Checked before any source read: chunk-schema drift is a property of the manifest
	// alone and invalidates every re-derivation below, so reporting a per-file state
	// (unreadable, changed) first would send the operator after the wrong repair.
	if strings.TrimSpace(manifest.PrefixSchemaVersion) != PrefixSchemaVersion {
		return ResolvedChunkText{Status: ChunkTextStaleIndex}, nil
	}
	if len(lanes) > 1 {
		return ResolvedChunkText{}, fmt.Errorf("corpus chunk text: at most one lane config may be supplied")
	}
	governed := strings.EqualFold(strings.TrimSpace(item.BodyPolicy), "owned_snapshot")
	if len(lanes) == 1 {
		lane, err := ValidateLaneConfig(lanes[0])
		if err != nil {
			return ResolvedChunkText{}, err
		}
		if err := validateCurrentItemAccess(lane, item); err != nil {
			return ResolvedChunkText{}, fmt.Errorf("corpus chunk text: %w", err)
		}
		governed = itemAccessIsGoverned(lane, item)
	}
	if governed {
		switch strings.ToLower(strings.TrimSpace(item.Privacy)) {
		case "private":
			if !includeText {
				return ResolvedChunkText{Status: ChunkTextSuppressedPrivate}, nil
			}
		case "public":
			// Public text may proceed to body integrity checks below.
		default:
			return ResolvedChunkText{}, fmt.Errorf("corpus chunk text: item %q has malformed privacy field (must be public or private, got %q)", itemID, strings.TrimSpace(item.Privacy))
		}
	}
	raw, status, err := readItemBodyBytes(homePath, item)
	if err != nil {
		return ResolvedChunkText{}, err
	}
	if status != ChunkTextOK {
		// Degrade this hit, do not fail the query — see ChunkTextSourceUnavailable /
		// ChunkTextStaleSource. The FileHash half of the three-way invariant was
		// already enforced inside readItemBodyBytes (shared with the index path).
		return ResolvedChunkText{Status: status}, nil
	}
	chunks, err := ChunkItem(item, string(raw), manifest.Chunk)
	if err != nil {
		return ResolvedChunkText{}, fmt.Errorf("corpus chunk text: rechunk item %q: %w", itemID, err)
	}
	if ordinal < 0 || ordinal >= len(chunks) {
		return ResolvedChunkText{}, fmt.Errorf("corpus chunk text: ordinal %d out of range for item %q (chunks=%d)", ordinal, itemID, len(chunks))
	}
	// Preserve the legacy source_in_place ordering and degradation contract:
	// its body availability/integrity state is observed before suppression.
	if strings.EqualFold(strings.TrimSpace(item.Privacy), "private") && !includeText {
		return ResolvedChunkText{Status: ChunkTextSuppressedPrivate}, nil
	}
	return ResolvedChunkText{Status: ChunkTextOK, Text: chunks[ordinal].Text}, nil
}

// readItemBodyBytes is the production body-resolution seam. It returns the
// raw bytes (one source of truth for both indexing and query) plus a per-hit
// refusal status when the body cannot be served verbatim. It never falls
// back across body_policy: an owned_snapshot item never reads from SourceRef,
// and a source_in_place item never reads from the artifact store.
//
// All owned-body consumers MUST enforce the same three-way invariant before
// any byte reaches the embedder (index path) or the body resolver (query
// path): BodyRef digest == sha256(bytes), sha256(bytes) == FileHash, and
// BodyBytes <= 0 || len(bytes) == BodyBytes. The previous index path only
// checked the first two, which let a BodyRef-valid artifact whose FileHash
// disagreed with the actual bytes still produce chunk hashes that no
// manifest would later recognize. FileHash is enforced here for both
// body_policies so the index path and the query path share one gate.
//
// The status value returned here is what the caller (index or query) maps
// onto its own failure surface — ChunkTextSourceUnavailable / ChunkTextStaleSource
// for the corpus query path; an error for the index path, where missing body
// bytes cannot be embedded.
// readSourceInPlaceBody is the narrow source-byte seam used by the resolver.
// Keeping it distinct from artifact reads makes the privacy boundary directly
// falsifiable without weakening artifact-store integrity checks.
var readSourceInPlaceBody = os.ReadFile

func readItemBodyBytes(homePath string, item sourceobs.SourceManifestItem) ([]byte, ChunkTextStatus, error) {
	switch strings.TrimSpace(item.BodyPolicy) {
	case "source_in_place":
		if strings.TrimSpace(item.SourceRef) == "" {
			return nil, ChunkTextSourceUnavailable, nil
		}
		raw, err := readSourceInPlaceBody(item.SourceRef)
		if err != nil {
			return nil, ChunkTextSourceUnavailable, nil
		}
		if status := matchRecordedBodyHash(item, raw); status != ChunkTextOK {
			return nil, status, nil
		}
		return raw, ChunkTextOK, nil
	case "owned_snapshot":
		if strings.TrimSpace(item.BodyRef) == "" {
			return nil, ChunkTextSourceUnavailable, fmt.Errorf("corpus owned body: item %s missing body_ref", item.ID)
		}
		if strings.TrimSpace(homePath) == "" {
			return nil, ChunkTextSourceUnavailable, fmt.Errorf("corpus owned body: item %s requires COMPOUND_HOME", item.ID)
		}
		store, err := home.NewArtifactStore(homePath)
		if err != nil {
			return nil, ChunkTextSourceUnavailable, fmt.Errorf("corpus owned body: item %s artifact store: %w", item.ID, err)
		}
		raw, err := store.Get(item.BodyRef)
		if err != nil {
			// Map the artifact-store failure mode to the corpus failure surface:
			//   - file gone at the artifact path → source_unavailable
			//   - bytes at the path don't match the ref's claimed hash → stale_source
			// Both close; neither widens to SourceRef. The "manifest is corrupt"
			// gate is the sourceobs doctor's job; the resolver just refuses the hit.
			if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
				return nil, ChunkTextSourceUnavailable, nil
			}
			return nil, ChunkTextStaleSource, nil
		}
		// BodyBytes, when present in the manifest, must match the artifact's
		// exact byte length — the same invariant sourceobs's doctor enforces.
		if item.BodyBytes > 0 && int64(len(raw)) != item.BodyBytes {
			return nil, ChunkTextStaleSource, nil
		}
		if status := matchRecordedBodyHash(item, raw); status != ChunkTextOK {
			return nil, status, nil
		}
		return raw, ChunkTextOK, nil
	default:
		return nil, ChunkTextSourceUnavailable, fmt.Errorf("corpus body: item %s unknown body_policy %q", item.ID, item.BodyPolicy)
	}
}

// matchRecordedBodyHash enforces the FileHash half of the three-way
// invariant shared by every owned-body consumer: sha256(bytes) must equal
// the recorded FileHash. BodyRef digest is already verified upstream
// (artifact store for owned_snapshot; nothing in source_in_place) and
// BodyBytes is checked in the calling branch. This is the single gate the
// index and query paths share — centralizing it here prevents a future
// caller from forgetting one half and silently embedding drift.
//
// A blank/empty FileHash ALWAYS fails closed (stale_source). The corpus
// layer never trusts a hashless manifest half — a missing hash either
// signals a malformed manifest on the sourceobs side or a previously
// trusted body policy that has since tightened. Either way the resolver
// must refuse, never widen by accepting "no claim".
func matchRecordedBodyHash(item sourceobs.SourceManifestItem, raw []byte) ChunkTextStatus {
	expected := strings.TrimSpace(item.FileHash)
	if expected == "" {
		return ChunkTextStaleSource
	}
	sum := sha256.Sum256(raw)
	live := "sha256:" + hex.EncodeToString(sum[:])
	if live != expected {
		return ChunkTextStaleSource
	}
	return ChunkTextOK
}

func splitChunkID(chunkID string) (string, int, error) {
	idx := strings.LastIndex(chunkID, "#")
	if idx <= 0 || idx == len(chunkID)-1 {
		return "", 0, fmt.Errorf("corpus chunk text: malformed chunk id %q", chunkID)
	}
	ordinal, err := strconv.Atoi(chunkID[idx+1:])
	if err != nil || ordinal < 0 {
		return "", 0, fmt.Errorf("corpus chunk text: malformed chunk ordinal in %q", chunkID)
	}
	return chunkID[:idx], ordinal, nil
}

func findManifestItem(manifest Manifest, itemID string) (sourceobs.SourceManifestItem, bool) {
	for _, item := range manifest.Items {
		if item.ID == itemID {
			return item, true
		}
	}
	return sourceobs.SourceManifestItem{}, false
}
