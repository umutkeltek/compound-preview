package mem

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// EntityKinds — the routing-backbone vocabulary (what an entity can be).
var EntityKinds = []string{"project", "product", "repo", "server", "person", "concept", "topic", "doctrine", "tool"}

// Entity is a canonical identity that survives path / host / name moves — the routing backbone the
// audit found missing in mem (where Claim.Entity was a bare string with no catalog/aliasing). All
// of an entity's aliases and identifiers resolve to ONE Slug, so a display name, a moved folder, and
// a repo URL all map to the same entity. This is the spine-native convergence of the Gen-1
// memory.Entity + scope's alias/identifier resolution (the session-content SCORING in scope.Resolve
// converges later on top of this registry).
type Entity struct {
	Slug        string   `json:"slug"` // canonical id
	Kind        string   `json:"kind"`
	Aliases     []string `json:"aliases,omitempty"`
	Identifiers []string `json:"identifiers,omitempty"` // stable ids: repo url, bundle id, domain, path prefix
	Parent      string   `json:"parent,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
	CreatedAt   string   `json:"created_at"`
}

// EntityResolution is the outcome of resolving a token to a canonical entity.
type EntityResolution struct {
	Slug   string `json:"slug,omitempty"`
	Status string `json:"status"`        // resolved | multi | unresolved
	Via    string `json:"via,omitempty"` // slug | identifier | alias | path
}

func (s *Store) entitiesLog() string { return s.home + "/ledger/entities.jsonl" }

// RegisterEntity appends an entity to the registry (append-only; last write wins per slug on load).
// The registry participates in the memory owner's lock so CurrentView can capture
// claims, edges, and identity resolution from one coherent generation.
//
// An append whose payload already matches the slug's current row is skipped. The
// row carries the entity's whole identifier set, so a re-append of unchanged
// state costs the size of that set while adding nothing any reader can observe;
// repeated registration of a stable entity is the common case (every git
// snapshot import re-registers its repo). CreatedAt is excluded from the
// comparison because it is restamped on every call and no caller reads it.
func (s *Store) RegisterEntity(e Entity, now time.Time) (Entity, error) {
	if strings.TrimSpace(e.Slug) == "" {
		return Entity{}, fmt.Errorf("entity needs a slug")
	}
	if !oneOf(e.Kind, EntityKinds) {
		return Entity{}, fmt.Errorf("entity kind must be one of %v, got %q", EntityKinds, e.Kind)
	}
	if e.Visibility == "" {
		e.Visibility = "operational"
	}
	e.CreatedAt = rfc3339(now)
	stored := e
	err := s.withLock(func() error {
		current, found, err := lastEntityFor(s.entitiesLog(), e.Slug)
		if err != nil {
			return err
		}
		if found && sameEntityPayload(current, e) {
			stored = current
			return nil
		}
		return appendJSONL(s.entitiesLog(), &e)
	})
	return stored, err
}

// lastEntityFor streams the registry for the current row of one slug. It reads
// the file rather than Entities() so the write decision is made against what is
// actually on disk, and streams rather than loading so the cost does not scale
// with the registry's total size.
func lastEntityFor(path, slug string) (Entity, bool, error) {
	var out Entity
	found := false
	err := eachLine(path, func(line []byte) error {
		var e Entity
		if json.Unmarshal(line, &e) != nil || e.Slug != slug {
			return nil
		}
		out, found = e, true
		return nil
	})
	return out, found, err
}

// Entities loads the registry (append-only, last-wins by slug, deterministic order).
func (s *Store) Entities() ([]Entity, error) {
	if s.entityLoader != nil {
		return s.entityLoader()
	}
	return s.loadEntities()
}

func (s *Store) loadEntities() ([]Entity, error) {
	return loadEntitiesFrom(s.entitiesLog())
}

// loadEntitiesFrom is the registry loader bound to an explicit path so a fold
// can prove old and new encodings agree by running the production reader over
// both. Store.loadEntities is the only production caller.
func loadEntitiesFrom(path string) ([]Entity, error) {
	byID := map[string]Entity{}
	var order []string
	err := readJSONL(path, func(line []byte) error {
		var e Entity
		if json.Unmarshal(line, &e) != nil || e.Slug == "" {
			return nil // skip a torn line rather than bricking the registry
		}
		if _, seen := byID[e.Slug]; !seen {
			order = append(order, e.Slug)
		}
		byID[e.Slug] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]Entity, 0, len(order))
	for _, slug := range order {
		out = append(out, byID[slug])
	}
	return out, nil
}

// TopicEntities streams only the routing fields probe selection consumes. The
// entity ledger can contain very large identifier payloads, so this path stops
// decoding as soon as a row is known not to be a topic and drains the rest of
// that JSONL record without materializing it.
func (s *Store) TopicEntities() ([]Entity, error) {
	return topicEntitiesFrom(s.entitiesLog())
}

// topicEntitiesFrom is the streaming topic reader bound to an explicit path, so
// a fold can prove this reader also agrees across encodings.
func topicEntitiesFrom(path string) ([]Entity, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return []Entity{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("entity registry is not a regular file: %s", path)
	}

	reader := bufio.NewReaderSize(f, 1024*1024)
	byID := map[string]Entity{}
	seen := map[string]bool{}
	var order []string
	for {
		record := &entityJSONLRecord{source: reader}
		entity, topic := decodeTopicEntity(record)
		if err := record.drain(); err != nil {
			return nil, err
		}
		if record.bytesRead == 0 && record.sourceEOF {
			break
		}
		// A torn or otherwise incomplete row must not brick the registry or
		// become a partially decoded topic.
		if record.lastNonSpace != '}' {
			continue
		}
		if entity.Slug != "" && !seen[entity.Slug] {
			seen[entity.Slug] = true
			order = append(order, entity.Slug)
		}
		if !topic {
			delete(byID, entity.Slug)
			continue
		}
		byID[entity.Slug] = entity
	}

	out := make([]Entity, 0, len(byID))
	for _, slug := range order {
		if entity, ok := byID[slug]; ok {
			out = append(out, entity)
		}
	}
	return out, nil
}

// entityJSONLRecord exposes one JSONL record as an io.Reader without copying
// the complete line. This keeps a large identifiers array bounded by the
// bufio chunk size while still letting encoding/json parse the small prefix.
type entityJSONLRecord struct {
	source       *bufio.Reader
	pending      []byte
	terminal     bool
	done         bool
	sourceEOF    bool
	readErr      error
	bytesRead    int
	lastNonSpace byte
}

func (r *entityJSONLRecord) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.done {
		if r.readErr != nil {
			err := r.readErr
			r.readErr = nil
			return 0, err
		}
		return 0, io.EOF
	}
	if len(r.pending) == 0 {
		fragment, err := r.source.ReadSlice('\n')
		r.pending = fragment
		switch {
		case err == nil:
			r.terminal = true
		case errors.Is(err, bufio.ErrBufferFull):
			r.terminal = false
		case errors.Is(err, io.EOF):
			r.terminal = true
			r.sourceEOF = true
		default:
			r.terminal = true
			r.readErr = err
		}
		if len(r.pending) == 0 && r.terminal {
			r.done = true
			return r.Read(p)
		}
	}
	n := copy(p, r.pending)
	r.observe(r.pending[:n])
	r.pending = r.pending[n:]
	if len(r.pending) == 0 && r.terminal {
		r.done = true
	}
	return n, nil
}

func (r *entityJSONLRecord) drain() error {
	if len(r.pending) > 0 {
		r.observe(r.pending)
		r.pending = nil
	}
	if r.terminal || r.done {
		r.done = true
		return r.readErr
	}
	for {
		fragment, err := r.source.ReadSlice('\n')
		r.observe(fragment)
		switch {
		case err == nil:
			r.done = true
			return nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			r.done = true
			r.sourceEOF = true
			return nil
		default:
			r.done = true
			return err
		}
	}
}

func (r *entityJSONLRecord) observe(data []byte) {
	r.bytesRead += len(data)
	for i := len(data) - 1; i >= 0; i-- {
		b := data[i]
		switch b {
		case ' ', '\t', '\r', '\n':
		default:
			r.lastNonSpace = b
			return
		}
	}
}

func decodeTopicEntity(reader io.Reader) (Entity, bool) {
	decoder := json.NewDecoder(reader)
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return Entity{}, false
	}
	var entity Entity
	aliasesSeen := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Entity{}, false
		}
		key, ok := token.(string)
		if !ok {
			return Entity{}, false
		}
		switch key {
		case "slug":
			if err := decoder.Decode(&entity.Slug); err != nil {
				return Entity{}, false
			}
		case "kind":
			if err := decoder.Decode(&entity.Kind); err != nil {
				return Entity{}, false
			}
			if !strings.EqualFold(strings.TrimSpace(entity.Kind), "topic") {
				return entity, false
			}
		case "aliases":
			if err := decoder.Decode(&entity.Aliases); err != nil {
				return Entity{}, false
			}
			aliasesSeen = true
		case "identifiers":
			// Entity's canonical JSON order places routing fields before the
			// potentially huge identifiers payload.
			return entity, entity.Slug != "" && strings.EqualFold(strings.TrimSpace(entity.Kind), "topic")
		default:
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return Entity{}, false
			}
		}
		if aliasesSeen && entity.Slug != "" && strings.EqualFold(strings.TrimSpace(entity.Kind), "topic") {
			return entity, true
		}
	}
	if _, err := decoder.Token(); err != nil {
		return Entity{}, false
	}
	return entity, entity.Slug != "" && strings.EqualFold(strings.TrimSpace(entity.Kind), "topic")
}

// ResolveEntity maps a token (a slug, an identifier, an alias, or a path under a path-identifier)
// to a canonical entity slug — the path-independent lookup. Precedence slug > identifier > alias >
// path. If exactly one entity matches → resolved; if several distinct slugs match → multi; if none
// → unresolved (the kernel returns unresolved/multi rather than guessing).
func (s *Store) ResolveEntity(token string) (EntityResolution, error) {
	ents, err := s.Entities()
	if err != nil {
		return EntityResolution{}, err
	}
	return resolveEntity(ents, token), nil
}

type entityResolver struct {
	entities []Entity
	cache    map[string]EntityResolution
}

func newEntityResolver(entities []Entity) *entityResolver {
	return &entityResolver{entities: entities, cache: map[string]EntityResolution{}}
}

func (r *entityResolver) Resolve(token string) EntityResolution {
	if resolved, ok := r.cache[token]; ok {
		return resolved
	}
	resolved := resolveEntity(r.entities, token)
	r.cache[token] = resolved
	return resolved
}

func resolveEntity(ents []Entity, token string) EntityResolution {
	tok := strings.TrimSpace(token)
	low := strings.ToLower(tok)
	// rank: slug(3) > identifier(2) > alias(1) > path(0); keep the best tier each entity matches.
	best := map[string]int{} // slug -> tier
	via := map[string]string{}
	consider := func(slug string, tier int, how string) {
		if t, ok := best[slug]; !ok || tier > t {
			best[slug] = tier
			via[slug] = how
		}
	}
	for _, e := range ents {
		switch {
		case e.Slug == tok:
			consider(e.Slug, 3, "slug")
		}
		for _, id := range e.Identifiers {
			if id == tok {
				consider(e.Slug, 2, "identifier")
			} else if isPathPrefix(id, tok) {
				consider(e.Slug, 0, "path")
			}
		}
		for _, a := range e.Aliases {
			if strings.ToLower(a) == low {
				consider(e.Slug, 1, "alias")
			}
		}
	}
	if len(best) == 0 {
		return EntityResolution{Status: "unresolved"}
	}
	highest := -1
	for _, tier := range best {
		if tier > highest {
			highest = tier
		}
	}
	slugs := make([]string, 0, len(best))
	for slug, tier := range best {
		if tier == highest {
			slugs = append(slugs, slug)
		}
	}
	sort.Strings(slugs)
	if len(slugs) > 1 {
		// More than one distinct entity matched at the same strongest tier:
		// preserve uncertainty instead of letting lower-precedence matches
		// force ambiguity over an explicit id or identifier.
		return EntityResolution{Status: "multi", Slug: strings.Join(slugs, ",")}
	}
	return EntityResolution{Slug: slugs[0], Status: "resolved", Via: via[slugs[0]]}
}

// isPathPrefix reports whether tok is the path identifier id itself or a path nested under it —
// so a moved-but-registered path resolves to its entity. Only fires for path-shaped identifiers.
func isPathPrefix(id, tok string) bool {
	if !strings.Contains(id, "/") || id == "" {
		return false
	}
	return tok == id || strings.HasPrefix(tok, strings.TrimRight(id, "/")+"/")
}
