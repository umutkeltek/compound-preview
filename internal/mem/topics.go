package mem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const TopicImportSchemaVersion = "compound.memory_topics.v0.1"

type TopicImportReport struct {
	SchemaVersion string   `json:"schema_version"`
	ImportedAt    string   `json:"imported_at"`
	SourceRef     string   `json:"source_ref,omitempty"`
	SourceHash    string   `json:"source_hash,omitempty"`
	EvidenceRef   string   `json:"evidence_ref,omitempty"`
	TopicCount    int      `json:"topic_count"`
	EntityCreated int      `json:"entity_created"`
	EntityUpdated int      `json:"entity_updated"`
	EntityReused  int      `json:"entity_reused"`
	EdgesCreated  int      `json:"edges_created"`
	EdgesReused   int      `json:"edges_reused"`
	Topics        []string `json:"topics"`
}

type topicImportDocument struct {
	SchemaVersion string      `json:"schema_version"`
	Topics        []TopicSpec `json:"topics"`
}

type TopicSpec struct {
	Slug        string   `json:"slug"`
	Aliases     []string `json:"aliases,omitempty"`
	Identifiers []string `json:"identifiers,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	PartOf      []string `json:"part_of,omitempty"`
	NarrowsTo   []string `json:"narrows_to,omitempty"`
	Children    []string `json:"children,omitempty"`
	Visibility  string   `json:"visibility,omitempty"`
}

func (s *Store) ImportTopicsFile(path string, now time.Time) (TopicImportReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TopicImportReport{}, err
	}
	return s.importTopics(data, now, path)
}

func (s *Store) ImportTopics(data []byte, now time.Time) (TopicImportReport, error) {
	return s.importTopics(data, now, "inline:memory-topics")
}

func (s *Store) importTopics(data []byte, now time.Time, sourceRef string) (TopicImportReport, error) {
	doc, err := decodeTopicImport(data)
	if err != nil {
		return TopicImportReport{}, err
	}
	sourceHash := topicImportHash(data)
	evidenceRef := "topic_import:" + sourceHash
	report := TopicImportReport{
		SchemaVersion: TopicImportSchemaVersion,
		ImportedAt:    rfc3339(now),
		SourceRef:     strings.TrimSpace(sourceRef),
		SourceHash:    sourceHash,
		EvidenceRef:   evidenceRef,
		TopicCount:    len(doc.Topics),
	}
	for _, topic := range doc.Topics {
		topic.Slug = strings.TrimSpace(topic.Slug)
		if topic.Slug == "" {
			return report, fmt.Errorf("topic needs slug")
		}
		report.Topics = append(report.Topics, topic.Slug)
		status, err := s.ensureTopicEntity(topic, now)
		if err != nil {
			return report, err
		}
		switch status {
		case "created":
			report.EntityCreated++
		case "updated":
			report.EntityUpdated++
		default:
			report.EntityReused++
		}
		for _, parent := range normalizeStringRefs(append(topic.PartOf, topic.Parent)) {
			created, err := s.ensureRelation(topic.Slug, "part_of", parent, 1, []string{evidenceRef}, now)
			if err != nil {
				return report, err
			}
			if created {
				report.EdgesCreated++
			} else {
				report.EdgesReused++
			}
		}
		for _, child := range normalizeStringRefs(append(topic.NarrowsTo, topic.Children...)) {
			created, err := s.ensureRelation(topic.Slug, "narrows_to", child, 1, []string{evidenceRef}, now)
			if err != nil {
				return report, err
			}
			if created {
				report.EdgesCreated++
			} else {
				report.EdgesReused++
			}
		}
	}
	sort.Strings(report.Topics)
	return report, nil
}

func topicImportHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

func decodeTopicImport(data []byte) (topicImportDocument, error) {
	var doc topicImportDocument
	if err := json.Unmarshal(data, &doc); err == nil && len(doc.Topics) > 0 {
		if doc.SchemaVersion != "" && doc.SchemaVersion != TopicImportSchemaVersion {
			return topicImportDocument{}, fmt.Errorf("topic import schema_version %q, want %q", doc.SchemaVersion, TopicImportSchemaVersion)
		}
		return doc, nil
	}
	var topics []TopicSpec
	if err := json.Unmarshal(data, &topics); err != nil {
		return topicImportDocument{}, fmt.Errorf("topics JSON must be {topics:[...]} or [...]")
	}
	if len(topics) == 0 {
		return topicImportDocument{}, fmt.Errorf("topics JSON has no topics")
	}
	return topicImportDocument{Topics: topics}, nil
}

func (s *Store) ensureTopicEntity(topic TopicSpec, now time.Time) (string, error) {
	want := Entity{
		Slug:        topic.Slug,
		Kind:        "topic",
		Aliases:     normalizeStringRefs(topic.Aliases),
		Identifiers: normalizeStringRefs(topic.Identifiers),
		Parent:      strings.TrimSpace(topic.Parent),
		Visibility:  orDef(topic.Visibility, "operational"),
	}
	entities, err := s.Entities()
	if err != nil {
		return "", err
	}
	for _, existing := range entities {
		if existing.Slug != want.Slug {
			continue
		}
		if sameEntityPayload(existing, want) {
			return "reused", nil
		}
		_, err := s.RegisterEntity(want, now)
		return "updated", err
	}
	_, err = s.RegisterEntity(want, now)
	return "created", err
}

func sameEntityPayload(a, b Entity) bool {
	return a.Kind == b.Kind &&
		a.Parent == b.Parent &&
		orDef(a.Visibility, "operational") == orDef(b.Visibility, "operational") &&
		stringSliceEqual(normalizeStringRefs(a.Aliases), normalizeStringRefs(b.Aliases)) &&
		stringSliceEqual(normalizeStringRefs(a.Identifiers), normalizeStringRefs(b.Identifiers))
}

func (s *Store) ensureRelation(from, kind, to string, conf float64, evidenceRefs []string, now time.Time) (bool, error) {
	edges, err := s.loadEdges()
	if err != nil {
		return false, err
	}
	for _, edge := range edges {
		if edge.From == from && edge.Kind == kind && edge.To == to {
			return false, nil
		}
	}
	if _, err := s.relate(from, kind, to, conf, evidenceRefs, now); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) TopicSubtree(topic string) ([]string, error) {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return nil, nil
	}
	root := topic
	if r, err := s.ResolveEntity(topic); err != nil {
		return nil, err
	} else if r.Status == "resolved" {
		root = r.Slug
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
	seen := map[string]bool{root: true}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range children[cur] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
		}
	}
	out := make([]string, 0, len(seen))
	for slug := range seen {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeStringRefs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range in {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
