package mem

import "strings"

const clusterSep = "\x1f"

// ClusterKey is the broad topic identity used by memory anti-echo logic.
func ClusterKey(entity, topic, claimType string) string {
	return strings.Join([]string{
		NormalizeClaimValue(entity),
		NormalizeClaimValue(topic),
		NormalizeClaimValue(claimType),
	}, clusterSep)
}

// ClaimSlotKey is the narrow identity for versioning one fact. Domain remains a
// broad topic axis, while predicate keeps independent facts in that domain from
// superseding or contradicting each other. An explicit decision_id is already
// the stronger identity, so predicate aliases for that named decision share a
// slot intentionally.
func ClaimSlotKey(entity, predicate, claimType string, qualifiers map[string]string, fallbackID string) string {
	slotPredicate := predicate
	if NormalizeClaimValue(claimType) == "decision" && strings.TrimSpace(qualifiers["decision_id"]) != "" {
		slotPredicate = ""
	}
	return strings.Join([]string{
		NormalizeClaimValue(decisionClusterEntity(entity, claimType, qualifiers, fallbackID)),
		NormalizeClaimValue(topicFromQualifiers(predicate, qualifiers)),
		NormalizeClaimValue(slotPredicate),
		NormalizeClaimValue(claimType),
	}, clusterSep)
}

// Topic is the candidate-side topic axis: domain qualifier first, predicate fallback.
func (c Candidate) Topic() string {
	return topicFromQualifiers(c.Predicate, c.Qualifiers)
}

// ClusterKey resolves the entity through the registry when there is one unambiguous slug.
func (s *Store) ClusterKey(entity, topic, claimType string) (string, error) {
	entities, err := s.Entities()
	if err != nil {
		return "", err
	}
	return clusterKeyWithResolver(newEntityResolver(entities), entity, topic, claimType), nil
}

func (s *Store) candidateClusterKey(c Candidate) (string, error) {
	return s.claimSlotKey(c.Entity, c.Predicate, c.ClaimType, c.Qualifiers, "")
}

func (s *Store) claimClusterKey(c Claim) (string, error) {
	return s.claimSlotKey(c.Entity, c.Predicate, c.ClaimType, c.Qualifiers, c.ID)
}

func (s *Store) candidateEchoKey(c Candidate) (string, error) {
	return s.ClusterKey(decisionClusterEntity(c.Entity, c.ClaimType, c.Qualifiers, ""), c.Topic(), c.ClaimType)
}

func (s *Store) claimEchoKey(c Claim) (string, error) {
	return s.ClusterKey(decisionClusterEntity(c.Entity, c.ClaimType, c.Qualifiers, c.ID), c.Topic(), c.ClaimType)
}

func (s *Store) claimSlotKey(entity, predicate, claimType string, qualifiers map[string]string, fallbackID string) (string, error) {
	entities, err := s.Entities()
	if err != nil {
		return "", err
	}
	return claimSlotKeyWithResolver(newEntityResolver(entities), entity, predicate, claimType, qualifiers, fallbackID), nil
}

func clusterKeyWithResolver(resolver *entityResolver, entity, topic, claimType string) string {
	resolved := entity
	if r := resolver.Resolve(entity); r.Status == "resolved" {
		resolved = r.Slug
	}
	return ClusterKey(resolved, topic, claimType)
}

func candidateClusterKeyWithResolver(resolver *entityResolver, c Candidate) string {
	return claimSlotKeyWithResolver(resolver, c.Entity, c.Predicate, c.ClaimType, c.Qualifiers, "")
}

func claimClusterKeyWithResolver(resolver *entityResolver, c Claim) string {
	return claimSlotKeyWithResolver(resolver, c.Entity, c.Predicate, c.ClaimType, c.Qualifiers, c.ID)
}

func candidateEchoKeyWithResolver(resolver *entityResolver, c Candidate) string {
	entity := decisionClusterEntity(c.Entity, c.ClaimType, c.Qualifiers, "")
	return clusterKeyWithResolver(resolver, entity, c.Topic(), c.ClaimType)
}

func claimEchoKeyWithResolver(resolver *entityResolver, c Claim) string {
	entity := decisionClusterEntity(c.Entity, c.ClaimType, c.Qualifiers, c.ID)
	return clusterKeyWithResolver(resolver, entity, c.Topic(), c.ClaimType)
}

func claimSlotKeyWithResolver(resolver *entityResolver, entity, predicate, claimType string, qualifiers map[string]string, fallbackID string) string {
	resolved := entity
	if r := resolver.Resolve(entity); r.Status == "resolved" {
		resolved = r.Slug
	}
	return ClaimSlotKey(resolved, predicate, claimType, qualifiers, fallbackID)
}

func sameClusterKey(a, b string) bool {
	return a != "" && a == b
}

// NormalizeClaimValue is used for identity comparison, not display.
func NormalizeClaimValue(v string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(v))), " ")
}

func SameClaimQualifiers(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, av := range a {
		if NormalizeClaimValue(b[key]) != NormalizeClaimValue(av) {
			return false
		}
	}
	return true
}

func SameClaimObject(a, b string) bool {
	return NormalizeClaimValue(a) == NormalizeClaimValue(b)
}

func topicFromQualifiers(predicate string, qualifiers map[string]string) string {
	if d := qualifiers["domain"]; d != "" {
		return d
	}
	if d := qualifiers["domains"]; d != "" {
		return d
	}
	return predicate
}

func decisionClusterEntity(entity, claimType string, qualifiers map[string]string, fallbackID string) string {
	if NormalizeClaimValue(claimType) != "decision" {
		return entity
	}
	if strings.HasPrefix(NormalizeClaimValue(entity), "decision:") {
		return entity
	}
	if id := qualifiers["decision_id"]; id != "" {
		return decisionEntityKey(id)
	}
	if isMeaningfulClaimID(fallbackID) {
		return decisionEntityKey(fallbackID)
	}
	return entity
}

func decisionEntityKey(id string) string {
	if strings.HasPrefix(NormalizeClaimValue(id), "decision:") {
		return id
	}
	return "decision:" + id
}
