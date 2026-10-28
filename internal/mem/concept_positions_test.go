package mem

import (
	"bytes"
	"os"
	"reflect"
	"testing"
	"time"
)

const (
	topicRoot = "topic:technology-sector"
	topicMid  = "topic:application-platforms"
	topicLeaf = "topic:workflow-automation"
)

func conceptTopicsJSON() []byte {
	return []byte(`{
  "schema_version": "compound.memory_topics.v0.1",
  "topics": [
    {"slug": "topic:technology-sector", "narrows_to": ["topic:application-platforms"]},
    {"slug": "topic:application-platforms", "part_of": ["topic:technology-sector"], "narrows_to": ["topic:workflow-automation"]},
    {"slug": "topic:workflow-automation", "part_of": ["topic:application-platforms"]}
  ]
}`)
}

func TestImportTopicsIdempotentAndResolvesSubtree(t *testing.T) {
	s := newStore(t)
	first, err := s.ImportTopics(conceptTopicsJSON(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if first.EntityCreated != 3 || first.EdgesCreated != 4 {
		t.Fatalf("first import created entities=%d edges=%d, want 3/4", first.EntityCreated, first.EdgesCreated)
	}
	if first.SourceRef != "inline:memory-topics" || first.SourceHash == "" || first.EvidenceRef == "" {
		t.Fatalf("topic import should record source provenance: %+v", first)
	}
	second, err := s.ImportTopics(conceptTopicsJSON(), t0.Add(1))
	if err != nil {
		t.Fatal(err)
	}
	if second.EntityCreated != 0 || second.EntityUpdated != 0 || second.EntityReused != 3 {
		t.Fatalf("second import entity counts = created %d updated %d reused %d, want 0/0/3", second.EntityCreated, second.EntityUpdated, second.EntityReused)
	}
	if second.EdgesCreated != 0 || second.EdgesReused != 4 {
		t.Fatalf("second import edge counts = created %d reused %d, want 0/4", second.EdgesCreated, second.EdgesReused)
	}
	entities, err := s.Entities()
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 3 {
		t.Fatalf("idempotent import should leave 3 entity rows, got %d", len(entities))
	}
	edges, err := s.loadEdges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 4 {
		t.Fatalf("idempotent import should leave 4 hierarchy edge rows, got %d", len(edges))
	}
	for _, edge := range edges {
		if !stringSliceContains(edge.EvidenceRefs, first.EvidenceRef) {
			t.Fatalf("topic relation edge should carry import evidence ref %q: %+v", first.EvidenceRef, edge)
		}
	}
	subtree, err := s.TopicSubtree(topicMid)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{topicMid, topicLeaf}
	if !reflect.DeepEqual(subtree, want) {
		t.Fatalf("mid topic subtree = %v, want %v", subtree, want)
	}
}

func TestPositionFoldTopicScopeAsOfContestedAndClaimLedgerInvariant(t *testing.T) {
	s := newStore(t)
	if _, err := s.ImportTopics(conceptTopicsJSON(), t0); err != nil {
		t.Fatal(err)
	}
	entity := "project:synthetic-concept-lab"
	root := promoteTopicClaim(t, s, entity, "market_shape", "broad", topicRoot, "2026-01-01T00:00:00Z", t0)
	midOld := promoteTopicClaim(t, s, entity, "delivery_mode", "manual", topicMid, "2026-01-02T00:00:00Z", t0.Add(1))
	midNew := promoteTopicClaim(t, s, entity, "delivery_mode", "automated", topicMid, "2026-02-01T00:00:00Z", t0.Add(2))
	leaf := promoteTopicClaim(t, s, entity, "adoption_curve", "early", topicLeaf, "2026-03-01T00:00:00Z", t0.Add(3))
	c := cand(entity, "adoption_curve", "late", "2026-04-01T00:00:00Z")
	c.TopicRefs = []string{topicLeaf}
	if got, err := s.Capture([]Candidate{c}, t0.Add(4)); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Fatalf("contradicting candidate should be proposed, got %d candidate(s)", len(got))
	}
	sweep, err := s.SynthesisSweep([]string{"contradict"}, t0.Add(5))
	if err != nil {
		t.Fatal(err)
	}
	if sweep.ReviewDebtCount != 1 || len(sweep.ReopenedClaims) != 1 || sweep.ReopenedClaims[0].ClaimID != leaf.Claim.ID {
		t.Fatalf("contradiction should reopen the leaf claim only; report=%+v", sweep)
	}

	claimsBefore, err := os.ReadFile(s.claimsLog())
	if err != nil {
		t.Fatal(err)
	}
	foldTime := t0.Add(6)
	fold, err := s.FoldPositions(PositionFoldOptions{Entity: entity}, foldTime)
	if err != nil {
		t.Fatal(err)
	}
	claimsAfter, err := os.ReadFile(s.claimsLog())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(claimsBefore, claimsAfter) {
		t.Fatal("fold must not write the claims ledger")
	}
	if fold.Count != 3 {
		t.Fatalf("fold produced %d positions, want 3 (%+v)", fold.Count, fold.Positions)
	}
	byTopic := positionsByTopic(fold.Positions)
	if byTopic["market_shape"].ActiveClaimID != root.Claim.ID {
		t.Fatalf("root position = %+v, want active root claim %s", byTopic["market_shape"], root.Claim.ID)
	}
	mid := byTopic["delivery_mode"]
	if mid.ActiveObject != "automated" || mid.ActiveClaimID != midNew.Claim.ID {
		t.Fatalf("mid position active = %s/%s, want automated/%s", mid.ActiveObject, mid.ActiveClaimID, midNew.Claim.ID)
	}
	if !stringSliceContains(mid.SupersededHistoryRefs, midOld.Claim.ID) {
		t.Fatalf("mid position should preserve superseded ref %s; got %+v", midOld.Claim.ID, mid)
	}
	if len(mid.ClaimChain) != 2 || mid.ClaimChain[0] != midOld.Claim.ID || mid.ClaimChain[1] != midNew.Claim.ID {
		t.Fatalf("mid claim chain = %v, want old->new", mid.ClaimChain)
	}
	leafPosition := byTopic["adoption_curve"]
	if leafPosition.Status != "contested" || leafPosition.ActiveClaimID != leaf.Claim.ID || len(leafPosition.ReviewRefs) == 0 {
		t.Fatalf("leaf position should be contested with review refs; got %+v", leafPosition)
	}

	if _, err := s.FoldPositions(PositionFoldOptions{}, foldTime); err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(s.positionsLog())
	if err != nil {
		t.Fatal(err)
	}
	refold, err := s.FoldPositions(PositionFoldOptions{Entity: entity}, foldTime)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(s.positionsLog())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) || !reflect.DeepEqual(fold.Positions, refold.Positions) {
		t.Fatal("scoped re-fold must be idempotent and must not overwrite the global positions projection")
	}

	scoped, err := s.Position(entity, PositionFoldOptions{Topic: topicMid})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Positions) != 2 {
		t.Fatalf("--topic mid should return exactly mid+leaf subtree positions, got %d: %+v", len(scoped.Positions), scoped.Positions)
	}
	if hasPositionTopic(scoped.Positions, "market_shape") || !hasPositionTopic(scoped.Positions, "delivery_mode") || !hasPositionTopic(scoped.Positions, "adoption_curve") {
		t.Fatalf("topic-scoped positions wrong: %+v", scoped.Positions)
	}
	past, err := s.Position(entity, PositionFoldOptions{AsOfSeq: midOld.Claim.Seq, Topic: topicMid})
	if err != nil {
		t.Fatal(err)
	}
	if len(past.Positions) != 1 || past.Positions[0].Position.ActiveObject != "manual" || len(past.Positions[0].Position.SupersededHistoryRefs) != 0 {
		t.Fatalf("--as-of before supersession should return old stance only; got %+v", past.Positions)
	}
	recall, err := s.Recall(entity, RecallOpts{Topic: topicMid, IncludeNeedsReview: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(recall.Claims) != 2 {
		t.Fatalf("topic-scoped recall should return current mid+leaf claims with needs_review included, got %d: %+v", len(recall.Claims), recall.Claims)
	}

	near := cand(entity, "source_note", "near-topic", "2026-05-01T00:00:00Z")
	near.TopicRefs = []string{topicLeaf}
	far := cand(entity, "source_note", "far-topic", "2026-05-01T00:00:00Z")
	far.TopicRefs = []string{topicRoot}
	captured, err := s.Capture([]Candidate{near, far}, t0.Add(7))
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range captured {
		if _, err := s.AnnotateCandidate(candidate.ID, CandidateAnnotation{
			Kind:             "panel_verdict",
			Decision:         "annotate",
			OriginalDecision: "needs_human",
			RunID:            "prun_topic_scope",
			Rationale:        "kept as evidence candidate only",
			Confidence:       0.7,
			EvidenceRefs:     []string{"panel_run:prun_topic_scope"},
		}, t0.Add(8)); err != nil {
			t.Fatal(err)
		}
	}
	withEvidence, err := s.Recall(entity, RecallOpts{Topic: topicMid, IncludeNeedsReview: true, IncludeEvidence: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withEvidence.EvidenceCandidates) != 1 || withEvidence.EvidenceCandidates[0].Object != "near-topic" {
		t.Fatalf("topic-scoped recall evidence candidates should respect subtree filter, got %+v", withEvidence.EvidenceCandidates)
	}
}

func TestLegacyClaimRowsWithoutTopicRefsStillWork(t *testing.T) {
	s := newStore(t)
	legacy := Claim{
		ID:           "claim_legacy_topicless",
		Seq:          1,
		Entity:       "project:legacy-fixture",
		Predicate:    "status",
		Object:       "active",
		ValidFrom:    "2026-01-01T00:00:00Z",
		Confidence:   0.8,
		Derivation:   "deterministic",
		EvidenceRefs: []string{"trace_legacy"},
		Visibility:   "operational",
		CreatedAt:    rfc3339(t0),
	}
	if err := appendJSONL(s.claimsLog(), &legacy); err != nil {
		t.Fatal(err)
	}
	recall, err := s.Recall("project:legacy-fixture", RecallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recall.Claims) != 1 || len(recall.Claims[0].TopicRefs) != 0 {
		t.Fatalf("legacy topicless claim should recall normally with empty topic refs; got %+v", recall.Claims)
	}
	positions, err := s.Positions()
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 || positions[0].ActiveObject != "active" {
		t.Fatalf("legacy topicless claim should fold normally; got %+v", positions)
	}
}

func TestPositionFoldTopicScopeIncludesLegacyCanonicalTopicAxis(t *testing.T) {
	s := newStore(t)
	_, err := s.ImportTopics([]byte(`{
		"schema_version": "compound.memory_topics.v0.1",
		"topics": [
			{"slug": "standing-law", "narrows_to": ["candidate-governance"]},
			{"slug": "candidate-governance", "part_of": ["standing-law"]}
		]
	}`), t0)
	if err != nil {
		t.Fatal(err)
	}
	legacy := Claim{
		ID:         "claim_legacy_canonical_topic",
		Seq:        10,
		Entity:     "candidate-inbox",
		Predicate:  "authority_status",
		Object:     "candidate inbox is evidence, not truth",
		ValidFrom:  "2026-01-01T00:00:00Z",
		Confidence: 0.9,
		Derivation: "deterministic",
		ClaimType:  "constraint",
		Qualifiers: map[string]string{
			"domain": "candidate-governance",
		},
		EvidenceRefs: []string{"trace_legacy_canonical_topic"},
		Visibility:   "operational",
		State:        ClaimStatePromoted,
		CreatedAt:    rfc3339(t0),
	}
	if err := appendJSONL(s.claimsLog(), &legacy); err != nil {
		t.Fatal(err)
	}

	result, err := s.Position(legacy.Entity, PositionFoldOptions{Topic: "standing-law"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Positions) != 1 {
		t.Fatalf("legacy canonical-topic claim under standing-law subtree produced %d positions, want 1: %+v", len(result.Positions), result.Positions)
	}
	if result.Positions[0].Position.ActiveClaimID != legacy.ID {
		t.Fatalf("active claim = %q, want %q", result.Positions[0].Position.ActiveClaimID, legacy.ID)
	}
}

func promoteTopicClaim(t *testing.T, s *Store, entity, predicate, object, topicRef, observed string, now time.Time) AdjudicateResult {
	t.Helper()
	c := cand(entity, predicate, object, observed)
	c.TopicRefs = []string{topicRef}
	got, err := s.Capture([]Candidate{c}, now)
	if err != nil {
		t.Fatalf("capture %s.%s: %v", entity, predicate, err)
	}
	if len(got) != 1 {
		t.Fatalf("capture %s.%s produced %d candidates, want 1", entity, predicate, len(got))
	}
	res, err := promoteForTest(t, s, got[0].ID, "", now)
	if err != nil {
		t.Fatalf("promote %s.%s: %v", entity, predicate, err)
	}
	if res.Claim == nil {
		t.Fatalf("promote %s.%s did not return a claim", entity, predicate)
	}
	return res
}

func positionsByTopic(positions []Position) map[string]Position {
	out := map[string]Position{}
	for _, position := range positions {
		out[position.Topic] = position
	}
	return out
}

func hasPositionTopic(details []PositionDetail, topic string) bool {
	for _, detail := range details {
		if detail.Position.Topic == topic {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
