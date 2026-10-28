// candidate_list_topic.go — work_c13acdc090b5 (S3b): the stranger dealer's canonical topic
// snapshot and exclusion helpers. Candidate selection starts from Store.ListCandidates, while
// preparation re-reads candidate authority against one taxonomy snapshot before a card can be
// rendered or recorded.
package mem

import (
	"fmt"
	"strings"
)

// StrangerDealContext is an opaque, per-orient snapshot of the exclusion taxonomy. Building it
// once avoids reloading the topic registry for every proposed/parked candidate in the pool. The
// finalizer reconstructs it under the memory lock so a taxonomy change between preparation and
// commit cannot make an excluded candidate look current.
type StrangerDealContext struct {
	excludeTopic string
	excluded     map[string]bool
	resolver     *entityResolver
	topicSlugs   map[string]bool
}

// NewStrangerDealContext canonicalizes the operator's exclusion topic and its subtree. Ambiguous
// aliases fail closed: a weak-tie card is optional, so uncertainty must suppress it rather than
// guess which topic the operator meant.
func (s *Store) NewStrangerDealContext(excludeTopic string) (StrangerDealContext, error) {
	excludeTopic = strings.TrimSpace(excludeTopic)
	resolver, topicSlugs, err := s.strangerTopicResolver()
	if err != nil {
		return StrangerDealContext{}, err
	}
	context := StrangerDealContext{
		excludeTopic: excludeTopic,
		resolver:     resolver,
		topicSlugs:   topicSlugs,
	}
	if excludeTopic == "" {
		return context, nil
	}
	resolution := resolver.Resolve(excludeTopic)
	if resolution.Status == "multi" {
		return StrangerDealContext{}, fmt.Errorf("stranger exclusion topic %q is ambiguous", excludeTopic)
	}
	if resolution.Status == "resolved" && !topicSlugs[resolution.Slug] {
		return StrangerDealContext{}, fmt.Errorf("stranger exclusion %q resolves to non-topic %q", excludeTopic, resolution.Slug)
	}
	excluded, err := s.strangerTopicSubtree(excludeTopic, resolver)
	if err != nil {
		return StrangerDealContext{}, err
	}
	context.excluded = excluded
	return context, nil
}

// strangerTopicSubtree derives the excluded subtree from the same entity snapshot held by the
// context. TopicSubtree resolves the root by loading entities again; that is correct for general
// callers but would make one orient observe two different taxonomy generations.
func (s *Store) strangerTopicSubtree(topic string, resolver *entityResolver) (map[string]bool, error) {
	root := strings.TrimSpace(topic)
	if resolution := resolver.Resolve(root); resolution.Status == "resolved" {
		root = resolution.Slug
	}
	edges, err := s.loadRelations(0)
	if err != nil {
		return nil, err
	}
	children := map[string][]string{}
	for _, edge := range edges {
		switch edge.Kind {
		case "part_of":
			children[edge.To] = append(children[edge.To], edge.From)
		case "narrows_to":
			children[edge.From] = append(children[edge.From], edge.To)
		}
	}
	excluded := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range children[current] {
			if excluded[child] {
				continue
			}
			excluded[child] = true
			queue = append(queue, child)
		}
	}
	return excluded, nil
}

func (s *Store) strangerTopicResolver() (*entityResolver, map[string]bool, error) {
	entities, err := s.Entities()
	if err != nil {
		return nil, nil, err
	}
	topicSlugs := make(map[string]bool, len(entities))
	for _, entity := range entities {
		if entity.Kind == "topic" {
			topicSlugs[entity.Slug] = true
		}
	}
	return newEntityResolver(entities), topicSlugs, nil
}

func canonicalStrangerTopicMembership(candidate Candidate, resolver *entityResolver, topicSlugs map[string]bool) ([]string, error) {
	raw := normalizeStringRefs(append([]string{candidate.Topic()}, candidate.TopicRefs...))
	canonical := make([]string, 0, len(raw))
	for _, membership := range raw {
		resolution := resolver.Resolve(membership)
		switch {
		case resolution.Status == "multi":
			return nil, fmt.Errorf("candidate %s topic membership %q is ambiguous", candidate.ID, membership)
		case resolution.Status == "resolved" && topicSlugs[resolution.Slug]:
			canonical = append(canonical, resolution.Slug)
		default:
			canonical = append(canonical, membership)
		}
	}
	return normalizeStringRefs(canonical), nil
}
