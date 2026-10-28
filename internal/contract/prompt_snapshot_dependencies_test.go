package contract

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/breadth"
	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/mem"
)

func TestTopiclessPromptMaterializationDoesNotOpenEntityLedger(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	claimID := seedTempClaim(t, store, "compound", "architecture", "topicless prompt architecture")
	// A directory is a decisive unreadable-as-a-ledger sentinel. Topicless prompt
	// materialization owes no entity-registry read, so this unrelated path cannot
	// affect the served claims+edges view.
	if err := os.Mkdir(filepath.Join(home, "ledger", "entities.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	pack, err := PackWithCapabilities(promptDependencyInput("topicless prompt architecture"), DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatalf("topicless prompt consulted entity ledger: %v", err)
	}
	if !contains(pack.ActiveMemory, "claim://"+claimID) {
		t.Fatalf("topicless prompt lost relevant memory: %v", pack.ActiveMemory)
	}
}

func TestPromptMemoryRevisionTracksAppliedTemperatureOnly(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	relevantID := seedTempClaim(t, store, "compound", "shipping", "shipping architecture")
	irrelevantID := seedTempClaim(t, store, "unrelated", "noise", "gardening notes")
	in := promptDependencyInput("compound shipping")
	baseline := promptMemoryRevision(t, home, in)

	writeTempProjection(t, home, []mem.TemperatureEntry{{ClaimID: relevantID, Temperature: 0.2}})
	warm := promptMemoryRevision(t, home, in)
	if warm == baseline {
		t.Fatalf("applied relevant temperature did not change memory revision %s", baseline)
	}

	if _, err := mem.WriteTemperature(home, mem.TemperatureProjection{
		SchemaVersion: mem.TemperatureSchemaVersion,
		ProjectionRef: mem.TemperatureProjectionRef,
		ComputedAt:    fixedNow.Add(time.Hour).Format(time.RFC3339),
		Entries: []mem.TemperatureEntry{
			{ClaimID: relevantID, Temperature: 0.2},
			{ClaimID: irrelevantID, Temperature: -0.25},
			{ClaimID: "claim_absent_unrelated", Temperature: -0.25},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := promptMemoryRevision(t, home, in); got != warm {
		t.Fatalf("unapplied temperature body churned revision: got=%s want=%s", got, warm)
	}
}

type capturedDependencyEmbedder struct {
	task string
}

func (e *capturedDependencyEmbedder) Identity() string { return "stub:captured-deps:2:f32" }
func (e *capturedDependencyEmbedder) Dim() int         { return 2 }
func (e *capturedDependencyEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		switch {
		case text == "extract_facts" || strings.Contains(text, "extract facts pipeline"):
			out = append(out, []float32{1, 0})
		default:
			out = append(out, []float32{0, 1})
		}
	}
	return out, nil
}

type failingDependencyEmbedder struct{}

func (*failingDependencyEmbedder) Identity() string { return "stub:failing-deps:2:f32" }
func (*failingDependencyEmbedder) Dim() int         { return 2 }
func (*failingDependencyEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("forced semantic dependency failure")
}

func TestPromptUsesOneCapturedDependencySetForOutputAndRevision(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	claimID := seedDependencyClaim(t, store, "memory-system", "architecture", "extract facts pipeline", "topic:shipping")
	if _, err := store.RegisterEntity(mem.Entity{Slug: "topic:shipping", Kind: "topic", Aliases: []string{"delivery"}}, fixedNow); err != nil {
		t.Fatal(err)
	}
	writeTempProjection(t, home, []mem.TemperatureEntry{{ClaimID: claimID, Temperature: 0.2}})
	if err := mem.SaveRecallAliases(home, mem.RecallAliasRegistry{
		SchemaVersion: mem.RecallAliasSchemaVersion,
		Entries:       []mem.RecallAliasEntry{{From: "memory not updating", To: []string{"extract_facts"}, Kind: "phrase"}},
	}); err != nil {
		t.Fatal(err)
	}

	in := promptDependencyInput("memory not updating")
	in.Topic = "delivery"
	in.ReadOnlyRetrieval = true
	embedder := &capturedDependencyEmbedder{task: in.Task}
	priorResolver := breadthEmbedderResolver
	breadthEmbedderResolver = func(string) (breadth.EmbedderAdapter, bool) { return embedder, true }
	t.Cleanup(func() { breadthEmbedderResolver = priorResolver })

	baseline := promptPack(t, home, in)
	baselineRevision := sourceRev(baseline.SourceRevisions, memoryRelevanceSourceRef)
	if !contains(baseline.ActiveMemory, "claim://"+claimID) {
		t.Fatalf("baseline did not surface topic-scoped claim: %v", baseline.ActiveMemory)
	}

	priorCaptured := runtimeMemoryDependenciesCaptured
	runtimeMemoryDependenciesCaptured = func() {
		runtimeMemoryDependenciesCaptured = nil
		writeTempProjection(t, home, []mem.TemperatureEntry{{ClaimID: claimID, Temperature: -0.25}})
		if err := mem.SaveRecallAliases(home, mem.RecallAliasRegistry{
			SchemaVersion: mem.RecallAliasSchemaVersion,
			Entries:       []mem.RecallAliasEntry{{From: "memory not updating", To: []string{"other_term"}, Kind: "phrase"}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RegisterEntity(mem.Entity{Slug: "topic:collision", Kind: "topic", Aliases: []string{"delivery"}}, fixedNow.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { runtimeMemoryDependenciesCaptured = priorCaptured })

	duringMutation := promptPack(t, home, in)
	if !reflect.DeepEqual(duringMutation.ActiveMemory, baseline.ActiveMemory) ||
		!reflect.DeepEqual(duringMutation.ActivePositions, baseline.ActivePositions) ||
		sourceRev(duringMutation.SourceRevisions, memoryRelevanceSourceRef) != baselineRevision {
		t.Fatalf("one invocation mixed dependency generations:\nbaseline_memory=%v\nduring_memory=%v\nbaseline_positions=%v\nduring_positions=%v\nbaseline_rev=%s\nduring_rev=%s",
			baseline.ActiveMemory, duringMutation.ActiveMemory, baseline.ActivePositions, duringMutation.ActivePositions,
			baselineRevision, sourceRev(duringMutation.SourceRevisions, memoryRelevanceSourceRef))
	}

	afterMutation := promptPack(t, home, in)
	if sourceRev(afterMutation.SourceRevisions, memoryRelevanceSourceRef) == baselineRevision {
		t.Fatalf("fresh invocation ignored changed dependencies: revision remained %s", baselineRevision)
	}
}

func seedDependencyClaim(t *testing.T, store *mem.Store, subject, predicate, value, topicRef string) string {
	t.Helper()
	candidates, err := store.Capture([]mem.Candidate{{
		Entity: subject, Predicate: predicate, Object: value, TopicRefs: []string{topicRef},
		Confidence: 0.95, Derivation: "deterministic", ClaimType: "architecture",
		PromotionBasis: candidates.BasisCuratedPage, SourceTraceRef: "conversation://prompt-dependency-test",
		SnippetHash: "sha256:prompt-dependency", Visibility: "contextual",
	}}, fixedNow)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("capture dependency claim: count=%d err=%v", len(candidates), err)
	}
	result, err := store.Adjudicate(candidates[0].ID, "promote", "test", fixedNow)
	if err != nil || result.Claim == nil {
		t.Fatalf("promote dependency claim: %v", err)
	}
	return result.Claim.ID
}

func TestPromptMemoryRevisionTracksCapturedTopicResolutionOnly(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := store.Capture([]mem.Candidate{{
		Entity: "project:delivery", Predicate: "architecture", Object: "shipping architecture",
		Confidence: 0.9, Derivation: "deterministic", ClaimType: "architecture",
		TopicRefs: []string{"topic:shipping"}, PromotionBasis: candidates.BasisRuntimeProof,
		SourceTraceRef: "trace_topic_revision", SnippetHash: "sha256:topic-revision", Visibility: "contextual",
	}}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := store.Adjudicate(captured[0].ID, "promote", "test", fixedNow)
	if err != nil || promoted.Claim == nil {
		t.Fatalf("promote topic claim: %+v err=%v", promoted, err)
	}
	if _, err := store.RegisterEntity(mem.Entity{Slug: "topic:shipping", Kind: "topic", Aliases: []string{"delivery"}}, fixedNow); err != nil {
		t.Fatal(err)
	}
	in := promptDependencyInput("shipping")
	in.Topic = "delivery"
	baselinePack := promptPack(t, home, in)
	baseline := sourceRev(baselinePack.SourceRevisions, memoryRelevanceSourceRef)
	if !contains(baselinePack.ActiveMemory, "claim://"+promoted.Claim.ID) {
		t.Fatalf("topic fixture did not surface relevant claim: %v", baselinePack.ActiveMemory)
	}

	if _, err := store.RegisterEntity(mem.Entity{Slug: "topic:unrelated", Kind: "topic", Aliases: []string{"elsewhere"}}, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := promptMemoryRevision(t, home, in); got != baseline {
		t.Fatalf("unrelated entity row churned captured topic revision: got=%s want=%s", got, baseline)
	}

	if _, err := store.RegisterEntity(mem.Entity{Slug: "topic:collision", Kind: "topic", Aliases: []string{"delivery"}}, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	changedPack := promptPack(t, home, in)
	changed := sourceRev(changedPack.SourceRevisions, memoryRelevanceSourceRef)
	if changed == baseline {
		t.Fatalf("relevant topic alias resolution changed served scope without revision change %s", baseline)
	}
	if contains(changedPack.ActiveMemory, "claim://"+promoted.Claim.ID) {
		t.Fatalf("topic resolution collision did not change served scope: %v", changedPack.ActiveMemory)
	}
}

func TestPromptMemoryRevisionTracksAppliedRecallExpansionOnly(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	_ = seedTempClaim(t, store, "memory-system", "architecture", "deployment automation")
	task := "memory not updating"
	embedder := &runtimeSemanticEmbedder{task: task}
	priorResolver := breadthEmbedderResolver
	breadthEmbedderResolver = func(string) (breadth.EmbedderAdapter, bool) { return embedder, true }
	t.Cleanup(func() { breadthEmbedderResolver = priorResolver })
	in := promptDependencyInput(task)
	in.ReadOnlyRetrieval = true
	baseline := promptMemoryRevision(t, home, in)

	if err := mem.SaveRecallAliases(home, mem.RecallAliasRegistry{
		SchemaVersion: mem.RecallAliasSchemaVersion,
		Entries:       []mem.RecallAliasEntry{{From: "unmatched phrase", To: []string{"unused"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := promptMemoryRevision(t, home, in); got != baseline {
		t.Fatalf("unmatched recall alias churned revision: got=%s want=%s", got, baseline)
	}

	if err := mem.SaveRecallAliases(home, mem.RecallAliasRegistry{
		SchemaVersion: mem.RecallAliasSchemaVersion,
		Entries:       []mem.RecallAliasEntry{{From: "memory not updating", To: []string{"extract_facts"}, Kind: "phrase"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := promptMemoryRevision(t, home, in); got == baseline {
		t.Fatalf("applied recall expansion did not change revision %s", baseline)
	}
}

func TestPromptMemoryRevisionOmitsMatchedRecallAliasWithoutEmbedder(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	const task = "memory not updating"
	_ = seedTempClaim(t, store, "memory-system", "status", task)
	priorResolver := breadthEmbedderResolver
	breadthEmbedderResolver = func(string) (breadth.EmbedderAdapter, bool) { return nil, false }
	t.Cleanup(func() { breadthEmbedderResolver = priorResolver })
	in := promptDependencyInput(task)
	in.ReadOnlyRetrieval = true
	assertUnusedRecallAliasDoesNotChangeRevision(t, home, in)
}

func TestPromptMemoryRevisionOmitsMatchedRecallAliasAfterSemanticFailure(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	const task = "memory not updating"
	_ = seedTempClaim(t, store, "memory-system", "status", task)
	priorResolver := breadthEmbedderResolver
	breadthEmbedderResolver = func(string) (breadth.EmbedderAdapter, bool) {
		return &failingDependencyEmbedder{}, true
	}
	t.Cleanup(func() { breadthEmbedderResolver = priorResolver })
	in := promptDependencyInput(task)
	in.ReadOnlyRetrieval = true
	assertUnusedRecallAliasDoesNotChangeRevision(t, home, in)
}

func assertUnusedRecallAliasDoesNotChangeRevision(t *testing.T, home string, in Input) {
	t.Helper()
	if err := mem.SaveRecallAliases(home, mem.RecallAliasRegistry{
		SchemaVersion: mem.RecallAliasSchemaVersion,
		Entries:       []mem.RecallAliasEntry{{From: in.Task, To: []string{"first_unused_expansion"}, Kind: "phrase"}},
	}); err != nil {
		t.Fatal(err)
	}
	before := promptMemoryRevision(t, home, in)
	if err := mem.SaveRecallAliases(home, mem.RecallAliasRegistry{
		SchemaVersion: mem.RecallAliasSchemaVersion,
		Entries:       []mem.RecallAliasEntry{{From: in.Task, To: []string{"second_unused_expansion"}, Kind: "phrase"}},
	}); err != nil {
		t.Fatal(err)
	}
	if after := promptMemoryRevision(t, home, in); after != before {
		t.Fatalf("unused recall alias changed revision: before=%s after=%s", before, after)
	}
}

func TestPromptPositionsIgnorePersistedProjectionAndTrackOwnedMeaning(t *testing.T) {
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	firstID := seedTempClaim(t, store, "project:compound", "architecture", "current Compound architecture")
	fold, err := store.FoldPositions(mem.PositionFoldOptions{}, fixedNow)
	if err != nil || len(fold.Positions) != 1 {
		t.Fatalf("fold positions: count=%d err=%v", len(fold.Positions), err)
	}
	in := promptDependencyInput("current Compound architecture")
	baseline := promptPack(t, home, in)
	if len(baseline.ActivePositions) != 1 || baseline.ActivePositions[0].Stance != "current Compound architecture" {
		t.Fatalf("baseline prompt position = %+v", baseline.ActivePositions)
	}
	baselineRevision := sourceRev(baseline.SourceRevisions, memoryRelevanceSourceRef)
	positionsPath := filepath.Join(home, "ledger", "positions.jsonl")

	if err := os.WriteFile(positionsPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	emptyProjection := promptPack(t, home, in)
	if !reflect.DeepEqual(emptyProjection.ActivePositions, baseline.ActivePositions) || sourceRev(emptyProjection.SourceRevisions, memoryRelevanceSourceRef) != baselineRevision {
		t.Fatalf("empty persisted projection changed prompt meaning/revision:\nbaseline=%+v\nafter=%+v", baseline.ActivePositions, emptyProjection.ActivePositions)
	}

	stale := fold.Positions[0]
	stale.ActiveObject = "FORGED STALE PROJECTION"
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(positionsPath, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	staleProjection := promptPack(t, home, in)
	if !reflect.DeepEqual(staleProjection.ActivePositions, baseline.ActivePositions) || sourceRev(staleProjection.SourceRevisions, memoryRelevanceSourceRef) != baselineRevision {
		t.Fatalf("forged persisted projection changed prompt meaning/revision:\nbaseline=%+v\nafter=%+v", baseline.ActivePositions, staleProjection.ActivePositions)
	}

	captured, err := store.Capture([]mem.Candidate{{
		Entity: "project:compound", Predicate: "architecture", Object: "successor Compound architecture",
		Confidence: 0.95, Derivation: "deterministic", ClaimType: "architecture",
		PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "trace_position_successor",
		SnippetHash: "sha256:position-successor", Visibility: "contextual",
	}}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Adjudicate(captured[0].ID, "promote", "test", fixedNow.Add(time.Minute))
	if err != nil || second.Claim == nil || second.Claim.ID == firstID {
		t.Fatalf("promote position successor: %+v err=%v", second, err)
	}
	changed := promptPack(t, home, promptDependencyInput("successor Compound architecture"))
	if sourceRev(changed.SourceRevisions, memoryRelevanceSourceRef) == baselineRevision || len(changed.ActivePositions) != 1 || changed.ActivePositions[0].Stance != "successor Compound architecture" {
		t.Fatalf("owned position meaning did not change revision/output: rev=%s positions=%+v", sourceRev(changed.SourceRevisions, memoryRelevanceSourceRef), changed.ActivePositions)
	}
}

func promptDependencyInput(task string) Input {
	return Input{Harness: "codex", Cwd: "/repo/compound", Task: task, RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute"}
}

func promptPack(t *testing.T, home string, in Input) envelope.ContextPack {
	t.Helper()
	pack, err := PackWithCapabilities(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func promptMemoryRevision(t *testing.T, home string, in Input) string {
	t.Helper()
	revision := sourceRev(promptPack(t, home, in).SourceRevisions, memoryRelevanceSourceRef)
	if revision == "" || revision == "unknown" {
		t.Fatalf("memory revision missing for task %q", in.Task)
	}
	return revision
}
