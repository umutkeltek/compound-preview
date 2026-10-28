package mem

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// CurrentView is one immutable, invocation-scoped fold of the owned memory
// ledger. It stabilizes claims and edges under the memory owner's shared lock,
// then releases the lock before any rented engine work begins.
type CurrentView struct {
	owner      *Store
	claims     []Claim
	byID       map[string]Claim
	edges      []Edge
	positions  []Position
	topicToken string
	topic      EntityResolution
	hasTopic   bool
	generation int64
	revision   string
}

// CurrentView builds the current memory view from exactly one claims read and
// one edges read at one shared-lock generation. The ordinary prompt path never
// opens the append-only entity registry.
func (s *Store) CurrentView() (*CurrentView, error) {
	return s.currentViewForTopic("")
}

// CurrentViewForTopic captures one token's entity resolution with the claims
// and edges generation protected by the same shared memory lock. It stores only
// the immutable token/result pair, never entity rows or a mutable resolver.
func (s *Store) CurrentViewForTopic(token string) (*CurrentView, error) {
	return s.currentViewForTopic(strings.TrimSpace(token))
}

func (s *Store) currentViewForTopic(topicToken string) (*CurrentView, error) {
	lockPath := filepath.Join(s.home, "lock", "mem.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer lockFile.Close()
	if s.beforeLockObserver != nil {
		s.beforeLockObserver()
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_SH); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	claimsRaw, err := readCurrentViewInput(s.claimsLog())
	if err != nil {
		return nil, err
	}
	if s.currentViewAfterClaimsRead != nil {
		s.currentViewAfterClaimsRead()
	}
	edgesRaw, err := readCurrentViewInput(s.edgesLog())
	if err != nil {
		return nil, err
	}
	claims, err := decodeCurrentViewRows[Claim](claimsRaw, "claims")
	if err != nil {
		return nil, err
	}
	edges, err := decodeCurrentViewRows[Edge](edgesRaw, "edges")
	if err != nil {
		return nil, err
	}
	var topic EntityResolution
	hasTopic := topicToken != ""
	if hasTopic {
		entities, err := s.Entities()
		if err != nil {
			return nil, err
		}
		topic = resolveEntity(entities, topicToken)
	}

	generation := currentViewGeneration(claims, edges)
	decorated, superseded := foldCurrentViewState(claims, edges, generation)
	current := visibleCurrentViewClaims(decorated, superseded)
	positions := foldPositionRows(decorated, superseded, generation, "", "", nil)
	byID := make(map[string]Claim, len(current))
	for _, claim := range current {
		byID[claim.ID] = claim
	}
	h := sha256.New()
	_, _ = h.Write([]byte("compound.memory.current-view.v1\x00claims.jsonl\x00"))
	_, _ = h.Write(claimsRaw)
	_, _ = h.Write([]byte("\x00edges.jsonl\x00"))
	_, _ = h.Write(edgesRaw)

	return &CurrentView{
		owner:      s,
		claims:     current,
		byID:       byID,
		edges:      edges,
		positions:  positions,
		topicToken: topicToken,
		topic:      topic,
		hasTopic:   hasTopic,
		generation: generation,
		revision:   "sha256:" + hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// TopicResolution returns only the single token/result pair captured by
// CurrentViewForTopic. Wrong or uncaptured tokens cannot ask the immutable view
// to perform a new resolution.
func (v *CurrentView) TopicResolution(token string) (EntityResolution, bool) {
	if v == nil || !v.hasTopic || strings.TrimSpace(token) != v.topicToken {
		return EntityResolution{Status: "unresolved"}, false
	}
	return v.topic, true
}

// Positions returns the owner fold derived from the exact claims/edges
// generation captured by this view. The persisted positions.jsonl projection is
// deliberately not consulted: it is reproducible history, not current authority.
func (v *CurrentView) Positions() []Position {
	if v == nil {
		return nil
	}
	out := make([]Position, len(v.positions))
	for i, position := range v.positions {
		out[i] = cloneCurrentViewPosition(position)
	}
	return out
}

// Claims returns a deep copy so callers cannot mutate the invocation snapshot.
func (v *CurrentView) Claims() []Claim {
	if v == nil {
		return nil
	}
	out := make([]Claim, len(v.claims))
	for i, claim := range v.claims {
		out[i] = cloneCurrentViewClaim(claim)
	}
	return out
}

// Claim resolves a current claim by owned ID and returns a defensive copy.
func (v *CurrentView) Claim(id string) (Claim, bool) {
	if v == nil {
		return Claim{}, false
	}
	claim, ok := v.byID[id]
	if !ok {
		return Claim{}, false
	}
	return cloneCurrentViewClaim(claim), true
}

// Generation is the maximum sequence consumed across claims and edges.
func (v *CurrentView) Generation() int64 {
	if v == nil {
		return 0
	}
	return v.generation
}

// Revision fingerprints only the claim and edge bytes consumed by this view;
// unrelated ledger bodies never enter prompt-time memory freshness.
func (v *CurrentView) Revision() string {
	if v == nil {
		return ""
	}
	return v.revision
}

// TopicSubtree derives topic narrowing from the same immutable edge generation.
// Entity/alias resolution remains the Store owner's separate registry concern.
func (v *CurrentView) TopicSubtree(root string) []string {
	if v == nil || root == "" {
		return nil
	}
	children := map[string][]string{}
	for _, edge := range v.edges {
		switch edge.Kind {
		case "part_of":
			children[edge.To] = append(children[edge.To], edge.From)
		case "narrows_to":
			children[edge.From] = append(children[edge.From], edge.To)
		}
	}
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range children[current] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
		}
	}
	out := make([]string, 0, len(seen))
	for topic := range seen {
		out = append(out, topic)
	}
	sort.Strings(out)
	return out
}

func readCurrentViewInput(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return raw, err
}

func decodeCurrentViewRows[T any](raw []byte, input string) ([]T, error) {
	lines := bytes.Split(raw, []byte{'\n'})
	out := make([]T, 0, len(lines))
	for index, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row T
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("decode current-view %s row %d: %w", input, index+1, err)
		}
		out = append(out, row)
	}
	return out, nil
}

func currentViewGeneration(claims []Claim, edges []Edge) int64 {
	var generation int64
	for _, claim := range claims {
		if claim.Seq > generation {
			generation = claim.Seq
		}
	}
	for _, edge := range edges {
		if edge.Seq > generation {
			generation = edge.Seq
		}
	}
	return generation
}

func foldCurrentViewState(claims []Claim, edges []Edge, generation int64) ([]Claim, map[string]bool) {
	type reviewSourceState struct {
		markSeq         int64
		clearSeq        int64
		confidenceDelta float64
	}
	reviewByClaim := map[string]map[string]*reviewSourceState{}
	superseded := map[string]bool{}
	for _, edge := range edges {
		if edge.Seq > generation {
			continue
		}
		if edge.Kind == EdgeKindSupersedes {
			superseded[edge.To] = true
		}
		if edge.Kind != EdgeKindNeedsReview && edge.Kind != EdgeKindReviewCleared {
			continue
		}
		sources := reviewByClaim[edge.To]
		if sources == nil {
			sources = map[string]*reviewSourceState{}
			reviewByClaim[edge.To] = sources
		}
		state := sources[edge.From]
		if state == nil {
			state = &reviewSourceState{}
			sources[edge.From] = state
		}
		switch edge.Kind {
		case EdgeKindNeedsReview:
			if edge.Seq >= state.markSeq {
				state.markSeq = edge.Seq
				state.confidenceDelta = edge.ConfidenceDelta
			}
		case EdgeKindReviewCleared:
			if edge.Seq >= state.clearSeq {
				state.clearSeq = edge.Seq
			}
		}
	}
	for _, claim := range claims {
		if claim.Supersedes != "" && claim.Seq <= generation {
			superseded[claim.Supersedes] = true
		}
	}

	decorated := make([]Claim, 0, len(claims))
	for _, claim := range claims {
		claim.State = ClaimStatePromoted
		claim.ReviewRefs = nil
		claim.ConfidenceDelta = 0
		if sources := reviewByClaim[claim.ID]; sources != nil {
			var refs []string
			var latestSeq int64
			for source, state := range sources {
				if state.markSeq == 0 || state.clearSeq > state.markSeq {
					continue
				}
				if source != "" {
					refs = append(refs, source)
				}
				if state.markSeq >= latestSeq {
					latestSeq = state.markSeq
					claim.ConfidenceDelta = state.confidenceDelta
				}
			}
			if latestSeq > 0 {
				sort.Strings(refs)
				claim.State = ClaimStateNeedsReview
				claim.ReviewRefs = refs
			}
		}
		decorated = append(decorated, claim)
	}
	return decorated, superseded
}

func visibleCurrentViewClaims(claims []Claim, superseded map[string]bool) []Claim {
	out := make([]Claim, 0, len(claims))
	for _, claim := range claims {
		if claim.Visibility == "retired" || claim.State == ClaimStateNeedsReview || superseded[claim.ID] {
			continue
		}
		out = append(out, claim)
	}
	return out
}

func cloneCurrentViewClaim(claim Claim) Claim {
	if claim.Qualifiers != nil {
		qualifiers := claim.Qualifiers
		claim.Qualifiers = make(map[string]string, len(qualifiers))
		for key, value := range qualifiers {
			claim.Qualifiers[key] = value
		}
	}
	claim.TopicRefs = append([]string(nil), claim.TopicRefs...)
	claim.EvidenceRefs = append([]string(nil), claim.EvidenceRefs...)
	claim.ReviewRefs = append([]string(nil), claim.ReviewRefs...)
	return claim
}

func cloneCurrentViewPosition(position Position) Position {
	position.SupportingClaimRefs = append([]string(nil), position.SupportingClaimRefs...)
	position.SupersededHistoryRefs = append([]string(nil), position.SupersededHistoryRefs...)
	position.ClaimChain = append([]string(nil), position.ClaimChain...)
	position.ReviewRefs = append([]string(nil), position.ReviewRefs...)
	position.EvidenceRefs = append([]string(nil), position.EvidenceRefs...)
	position.TopicRefs = append([]string(nil), position.TopicRefs...)
	return position
}

func (v *CurrentView) wireEdges(minConfidence float64) []WireEdge {
	visibleEntities := make(map[string]bool, len(v.claims))
	for _, claim := range v.claims {
		visibleEntities[claim.Entity] = true
	}
	out := make([]WireEdge, 0)
	for _, edge := range v.edges {
		if edge.Kind == EdgeKindSupersedes || edge.Kind == EdgeKindReinforces || edge.Kind == EdgeKindNeedsReview || edge.Confidence < minConfidence {
			continue
		}
		if !visibleEntities[edge.From] || !visibleEntities[edge.To] {
			continue
		}
		out = append(out, WireEdge{From: edge.From, Kind: edge.Kind, To: edge.To, Confidence: edge.Confidence})
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
	return out
}
