package mem

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const PositionFoldSchemaVersion = "compound.memory_position_fold.v0.2"

type PositionFoldOptions struct {
	Entity  string
	Topic   string
	AsOfSeq int64
}

type PositionFoldReport struct {
	SchemaVersion string     `json:"schema_version"`
	FoldedAt      string     `json:"folded_at"`
	AsOfSeq       int64      `json:"as_of_seq"`
	Entity        string     `json:"entity,omitempty"`
	Topic         string     `json:"topic,omitempty"`
	TopicSubtree  []string   `json:"topic_subtree,omitempty"`
	Count         int        `json:"count"`
	Positions     []Position `json:"positions"`
}

type PositionDetail struct {
	Position Position       `json:"position"`
	Claims   []Claim        `json:"claims"`
	Evidence []EvidenceItem `json:"evidence,omitempty"`
	Sources  []string       `json:"sources,omitempty"`
}

type PositionQueryResult struct {
	SchemaVersion string           `json:"schema_version"`
	Entity        string           `json:"entity"`
	AsOfSeq       int64            `json:"as_of_seq"`
	Topic         string           `json:"topic,omitempty"`
	TopicSubtree  []string         `json:"topic_subtree,omitempty"`
	Positions     []PositionDetail `json:"positions"`
	Coverage      string           `json:"coverage"`
}

func (s *Store) positionsLog() string { return s.home + "/ledger/positions.jsonl" }

func (s *Store) FoldPositions(opts PositionFoldOptions, now time.Time) (PositionFoldReport, error) {
	positions, asOf, subtree, err := s.derivePositions(opts, now)
	if err != nil {
		return PositionFoldReport{}, err
	}
	if shouldWriteGlobalPositionsProjection(opts) {
		if err := writePositionsLog(s.positionsLog(), positions); err != nil {
			return PositionFoldReport{}, err
		}
	}
	return PositionFoldReport{
		SchemaVersion: PositionFoldSchemaVersion,
		FoldedAt:      rfc3339(now),
		AsOfSeq:       asOf,
		Entity:        strings.TrimSpace(opts.Entity),
		Topic:         strings.TrimSpace(opts.Topic),
		TopicSubtree:  subtree,
		Count:         len(positions),
		Positions:     positions,
	}, nil
}

func shouldWriteGlobalPositionsProjection(opts PositionFoldOptions) bool {
	return strings.TrimSpace(opts.Entity) == "" && strings.TrimSpace(opts.Topic) == "" && opts.AsOfSeq == 0
}

func (s *Store) LoadFoldedPositions() ([]Position, error) {
	var out []Position
	stale := false
	err := readJSONL(s.positionsLog(), func(line []byte) error {
		var p Position
		if err := json.Unmarshal(line, &p); err != nil {
			return err
		}
		if p.SchemaVersion != PositionFoldSchemaVersion {
			stale = true
			return nil
		}
		out = append(out, p)
		return nil
	})
	if stale {
		// A stale derived checkpoint must never hide current claims. Returning an
		// empty projection makes ContextPack fall back to the live claim path until
		// a global `compound memory fold` rewrites the checkpoint.
		return nil, err
	}
	return out, err
}

func (s *Store) Position(entity string, opts PositionFoldOptions) (PositionQueryResult, error) {
	opts.Entity = strings.TrimSpace(entity)
	positions, asOf, subtree, err := s.derivePositions(opts, time.Time{})
	if err != nil {
		return PositionQueryResult{Entity: opts.Entity}, err
	}
	claims, err := s.loadClaimsAsOf(asOf)
	if err != nil {
		return PositionQueryResult{Entity: opts.Entity}, err
	}
	claimByID := map[string]Claim{}
	for _, claim := range claims {
		claimByID[claim.ID] = claim
	}
	evidence, err := s.Evidence()
	if err != nil {
		return PositionQueryResult{Entity: opts.Entity}, err
	}
	evidenceByID := map[string]EvidenceItem{}
	for _, item := range evidence {
		evidenceByID[item.ID] = item
	}
	resultEntity := opts.Entity
	if len(positions) > 0 {
		resultEntity = positions[0].Entity
	}
	result := PositionQueryResult{
		SchemaVersion: PositionFoldSchemaVersion,
		Entity:        resultEntity,
		AsOfSeq:       asOf,
		Topic:         strings.TrimSpace(opts.Topic),
		TopicSubtree:  subtree,
	}
	for _, p := range positions {
		detail := PositionDetail{Position: p}
		for _, claimID := range p.ClaimChain {
			claim, ok := claimByID[claimID]
			if !ok {
				continue
			}
			detail.Claims = append(detail.Claims, claim)
			for _, ref := range claim.EvidenceRefs {
				if item, ok := evidenceByID[ref]; ok {
					detail.Evidence = append(detail.Evidence, item)
					if item.SourceID != "" {
						detail.Sources = append(detail.Sources, item.SourceID)
					}
				} else if ref != "" {
					detail.Sources = append(detail.Sources, ref)
				}
			}
		}
		detail.Sources = normalizeStringRefs(detail.Sources)
		result.Positions = append(result.Positions, detail)
	}
	switch {
	case len(result.Positions) > 0:
		result.Coverage = fmt.Sprintf("%d position(s) for %q as of seq %d", len(result.Positions), resultEntity, asOf)
	case strings.TrimSpace(opts.Topic) != "":
		result.Coverage = fmt.Sprintf("no positions for %q under topic %q as of seq %d", resultEntity, opts.Topic, asOf)
	default:
		result.Coverage = fmt.Sprintf("no positions for %q as of seq %d", resultEntity, asOf)
	}
	return result, nil
}

func (s *Store) Positions() ([]Position, error) {
	positions, _, _, err := s.derivePositions(PositionFoldOptions{}, time.Time{})
	return positions, err
}

func (s *Store) derivePositions(opts PositionFoldOptions, now time.Time) ([]Position, int64, []string, error) {
	claims, err := s.loadClaimsAsOf(opts.AsOfSeq)
	if err != nil {
		return nil, 0, nil, err
	}
	asOf := opts.AsOfSeq
	if asOf == 0 {
		asOf, err = s.maxLedgerSeq()
		if err != nil {
			return nil, 0, nil, err
		}
	}
	superseded, err := s.supersededAsOf(asOf)
	if err != nil {
		return nil, 0, nil, err
	}
	topicSet, subtree, err := s.topicScopeSet(opts.Topic)
	if err != nil {
		return nil, 0, nil, err
	}
	foldedAt := ""
	if !now.IsZero() {
		foldedAt = rfc3339(now)
	}
	entity := strings.TrimSpace(opts.Entity)
	if entity != "" {
		entity, err = s.canonicalRecallEntity(entity, claims, false)
		if err != nil {
			return nil, 0, nil, err
		}
	}
	out := foldPositionRows(claims, superseded, asOf, foldedAt, entity, topicSet)
	return out, asOf, subtree, nil
}

// foldPositionRows is the single pure position fold shared by historical/global
// projection generation and invocation-scoped CurrentView. Callers own input
// stabilization and optional scope resolution; this function owns slot grouping,
// ordering, supersession, and Position construction.
func foldPositionRows(claims []Claim, superseded map[string]bool, asOf int64, foldedAt, entity string, topicSet map[string]bool) []Position {
	type key struct{ slot, entity, topic, id string }
	groups := map[string][]Claim{}
	keys := map[string]key{}
	var order []string
	for _, c := range claims {
		if c.Seq > asOf {
			continue
		}
		if entity != "" && c.Entity != entity {
			continue
		}
		// The canonical topic axis predates TopicRefs; both are valid subtree
		// memberships so legacy promoted claims remain scopeable.
		if len(topicSet) > 0 && !topicSet[c.Topic()] && !claimHasTopicRef(c, topicSet) {
			continue
		}
		slot := ClaimSlotKey(c.Entity, c.Predicate, c.ClaimType, c.Qualifiers, c.ID)
		if _, ok := groups[slot]; !ok {
			order = append(order, slot)
			keys[slot] = key{slot: slot, entity: c.Entity, topic: c.Topic(), id: positionIDForClaim(c)}
		}
		groups[slot] = append(groups[slot], c)
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := keys[order[i]], keys[order[j]]
		if a.entity != b.entity {
			return a.entity < b.entity
		}
		if a.topic != b.topic {
			return a.topic < b.topic
		}
		return a.slot < b.slot
	})
	out := make([]Position, 0, len(order))
	for _, slot := range order {
		k := keys[slot]
		cs := groups[slot]
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Seq == cs[j].Seq {
				return cs[i].ID < cs[j].ID
			}
			return cs[i].Seq < cs[j].Seq
		})
		position, ok := buildPosition(k.id, k.entity, k.topic, cs, superseded, asOf, foldedAt)
		if ok {
			out = append(out, position)
		}
	}
	return out
}

func buildPosition(id, entity, topic string, claims []Claim, superseded map[string]bool, asOf int64, foldedAt string) (Position, bool) {
	chain := make([]string, 0, len(claims))
	supporting := []string{}
	history := []string{}
	reviewRefs := []string{}
	evidenceRefs := []string{}
	topicRefs := []string{}
	var active Claim
	found := false
	contested := false
	for _, claim := range claims {
		chain = append(chain, claim.ID)
		evidenceRefs = append(evidenceRefs, claim.EvidenceRefs...)
		topicRefs = append(topicRefs, claim.TopicRefs...)
		if claim.State == ClaimStateNeedsReview {
			contested = true
			reviewRefs = append(reviewRefs, claim.ReviewRefs...)
		}
		if superseded[claim.ID] {
			history = append(history, claim.ID)
			continue
		}
		active, found = claim, true
		supporting = []string{claim.ID}
	}
	if !found {
		return Position{}, false
	}
	status := "active"
	if active.Visibility == "retired" {
		status = "retired"
	}
	if contested {
		status = "contested"
	}
	return Position{
		SchemaVersion:         PositionFoldSchemaVersion,
		ID:                    id,
		Entity:                entity,
		Topic:                 topic,
		Predicate:             active.Predicate,
		ClaimType:             active.ClaimType,
		ActiveClaimID:         active.ID,
		ActiveObject:          active.Object,
		Status:                status,
		SupportingClaimRefs:   normalizeStringRefs(supporting),
		SupersededHistoryRefs: normalizeStringRefs(history),
		ClaimChain:            chain,
		ReviewRefs:            normalizeStringRefs(reviewRefs),
		EvidenceRefs:          normalizeStringRefs(evidenceRefs),
		TopicRefs:             normalizeStringRefs(topicRefs),
		FoldedAt:              foldedAt,
		AsOfSeq:               asOf,
	}, true
}

func positionIDForClaim(claim Claim) string {
	slot := ClaimSlotKey(claim.Entity, claim.Predicate, claim.ClaimType, claim.Qualifiers, claim.ID)
	return strings.TrimSpace(claim.Entity) + "::" + strings.TrimSpace(claim.Topic()) + "::slot-" + shortHash(slot)
}

func (s *Store) topicScopeSet(topic string) (map[string]bool, []string, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return nil, nil, nil
	}
	subtree, err := s.TopicSubtree(topic)
	if err != nil {
		return nil, nil, err
	}
	set := map[string]bool{}
	for _, slug := range subtree {
		set[slug] = true
	}
	return set, subtree, nil
}

func claimHasTopicRef(claim Claim, topicSet map[string]bool) bool {
	for _, ref := range claim.TopicRefs {
		if topicSet[ref] {
			return true
		}
	}
	return false
}

// ClaimHasTopicRef is the exported form of the recall/position topic gate: it reports
// whether a claim carries any topic ref inside topicSet (the slug set of a topic subtree,
// e.g. built from TopicSubtree). Ambient orientation reuses this one proven predicate to
// narrow recall by topic instead of reimplementing the intersection.
func ClaimHasTopicRef(claim Claim, topicSet map[string]bool) bool {
	return claimHasTopicRef(claim, topicSet)
}

func writePositionsLog(path string, positions []Position) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, position := range positions {
		if err := enc.Encode(position); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
