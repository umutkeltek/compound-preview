package hook

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/contract"
	"compound/internal/mem"
	"compound/internal/work"
)

const (
	promptScaleRawClaimCount     = 10_351
	promptScaleCurrentClaimCount = 632
	promptScaleEdgeCount         = 66_371
	promptScaleEntityHistoryRows = 100_000
	promptScaleNodeCount         = 670
	promptScaleRuns              = 20
	promptScaleTarget            = "SCALE_TARGET_MEANING"
)

func TestLiveUserPromptScaleStaysInsideNativeBudget(t *testing.T) {
	// Wall-clock assertion is meaningful only when the machine is not contending with another
	// certifying battery. Read the 1-minute load average at the start of the test and skip
	// the hard assertion when the load exceeds half the CPU count; the always-on CPU-time
	// sibling TestLiveUserPromptScaleStaysInsideCPUTimeBudget still asserts the same
	// property under contention. Observed on 2026-09-04 in R2B #3: a sibling lane's director
	// suite held the box at load ~12 and pushed this test's max to 1.98s (vs the 1.5s
	// ceiling) with no regression in the hook — the assertion must skip, not red.
	loadSkipThreshold := runtime.NumCPU() / 2
	if loadSkipThreshold < 1 {
		loadSkipThreshold = 1
	}
	if load, ok := promptScaleLoadAverageOneMinute(); ok && load > float64(loadSkipThreshold) {
		t.Skipf("load %.1f exceeds %d (half of NumCPU=%d); wall-clock budget not meaningful under contention", load, loadSkipThreshold, runtime.NumCPU())
	}

	homePath := t.TempDir()
	seedPromptScaleMemory(t, homePath)
	seedPromptScaleEntityHistory(t, homePath)
	seedPromptScaleCapabilities(t, homePath)
	assertPromptScaleShape(t, homePath)

	embedder := newPromptScaleEmbedServer(t)
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "openai")
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "prompt-scale-test")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", embedder.server.URL)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")

	// Warm the real production materialization path outside the native breaker.
	if _, err := contract.MaterializeRuntime(contract.Input{
		Harness: "codex", Cwd: "/repo/compound", Task: "warm prompt runtime",
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute", ReadOnlyRetrieval: true,
	}, contract.DefaultPolicy(), homePath, fixedNow); err != nil {
		t.Fatal(err)
	}
	cacheTimes := promptScaleCacheTimes(t, homePath)
	embedder.resetTaskCounts()

	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := workStore.List()
	if err != nil {
		t.Fatal(err)
	}
	deps := baseDeps()
	deps.HomePath = homePath
	deps.Store = workStore
	if got := deps.Registry.InjectionBudgetMS("codex"); got != 1500 {
		t.Fatalf("codex injection budget = %dms, want pinned 1500ms", got)
	}
	// claude_code's budget is deliberately larger and is NOT the gate this test enforces:
	// it stopped binding UserPromptSubmit, so its budget now covers the once-per-session
	// SessionStart build. The latency ceilings below stay pinned to codex's per-turn
	// 1500ms, which is the stricter of the two and the one a prompt actually runs under.
	if got := deps.Registry.InjectionBudgetMS("claude_code"); got != 5000 {
		t.Fatalf("claude session injection budget = %dms, want pinned 5000ms", got)
	}

	latencies := make([]time.Duration, 0, promptScaleRuns)
	for i := 0; i < promptScaleRuns; i++ {
		harness := "codex"
		if i%2 == 1 {
			harness = "claude_code"
		}
		raw := fmt.Sprintf("PROMPT_SCALE_QUERY_%02d accelerate worldwide shipping", i)
		started := time.Now()
		res, err := Run(Fixture{
			Hook: "user-prompt-submit", Harness: harness, Live: true,
			Cwd: "/repo/compound", RepoKind: "control-plane", Task: raw, Prompt: raw,
		}, deps, fixedNow.Add(time.Duration(i)*time.Second))
		elapsed := time.Since(started)
		latencies = append(latencies, elapsed)
		if err != nil {
			t.Fatalf("prompt %d (%s): %v", i, harness, err)
		}
		if res.BreakerTripped || res.Event.Status == "fail_open" {
			t.Fatalf("prompt %d (%s) tripped breaker: %+v", i, harness, res)
		}
		if res.Event.IDs.WorkID != "" || len(res.Produced) != 3 {
			t.Fatalf("prompt %d created work or wrong spine: work=%q produced=%v", i, res.Event.IDs.WorkID, kinds(res.Produced))
		}
		if res.Injection == nil {
			t.Fatalf("prompt %d missing persisted injection", i)
		}
		persisted, err := os.ReadFile(res.Injection.Path)
		if err != nil {
			t.Fatal(err)
		}
		if res.ContextOutput != string(persisted) {
			t.Fatalf("prompt %d returned bytes differ from persisted injection", i)
		}
		for _, want := range []string{promptScaleTarget, "capability://skill:scale-target"} {
			if !strings.Contains(res.ContextOutput, want) {
				t.Fatalf("prompt %d missing semantic-only meaning %q", i, want)
			}
		}
		assertPromptSentinelAbsentFromTree(t, homePath, raw)
	}

	after, err := workStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("live prompts changed work count %d -> %d", len(before), len(after))
	}
	for i := 0; i < promptScaleRuns; i++ {
		raw := fmt.Sprintf("PROMPT_SCALE_QUERY_%02d accelerate worldwide shipping", i)
		if got := embedder.taskCount(raw); got != 1 {
			t.Fatalf("prompt %d exact semantic task embeds = %d, want 1", i, got)
		}
	}
	for path, beforeTime := range cacheTimes {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(beforeTime) {
			t.Fatalf("warm prompt cache churned %s: before=%s after=%s", path, beforeTime, info.ModTime())
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[int(math.Ceil(float64(len(latencies))*0.95))-1]
	maxLatency := latencies[len(latencies)-1]
	if p95 > 1200*time.Millisecond {
		t.Fatalf("prompt p95 = %s, want <= 1.2s; latencies=%v", p95, latencies)
	}
	if maxLatency >= 1500*time.Millisecond {
		t.Fatalf("prompt max = %s, want < 1.5s; latencies=%v", maxLatency, latencies)
	}
	t.Logf("prompt scale latency p95=%s max=%s distribution=%v", p95, maxLatency, latencies)
}

func seedPromptScaleEntityHistory(t *testing.T, homePath string) {
	t.Helper()
	path := filepath.Join(homePath, "ledger", "entities.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	encoder := json.NewEncoder(writer)
	for i := 0; i < promptScaleEntityHistoryRows; i++ {
		entity := mem.Entity{
			Slug:      fmt.Sprintf("topic:unrelated-%03d", i%128),
			Kind:      "topic",
			Aliases:   []string{fmt.Sprintf("archived unrelated alias %06d", i)},
			CreatedAt: fixedNow.Add(time.Duration(i) * time.Microsecond).Format(time.RFC3339Nano),
		}
		if err := encoder.Encode(entity); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedPromptScaleMemory(t *testing.T, homePath string) {
	t.Helper()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	claimsPath := filepath.Join(homePath, "ledger", "claims.jsonl")
	edgesPath := filepath.Join(homePath, "ledger", "edges.jsonl")
	if err := os.MkdirAll(filepath.Dir(claimsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	claimsFile, err := os.Create(claimsPath)
	if err != nil {
		t.Fatal(err)
	}
	claimsWriter := bufio.NewWriterSize(claimsFile, 1<<20)
	claimsEncoder := json.NewEncoder(claimsWriter)
	supersessionEdges := make([]mem.Edge, 0, promptScaleRawClaimCount-promptScaleCurrentClaimCount)
	seq := int64(0)
	for entityIndex := 0; entityIndex < promptScaleCurrentClaimCount; entityIndex++ {
		chainLength := promptScaleRawClaimCount / promptScaleCurrentClaimCount
		if entityIndex < promptScaleRawClaimCount%promptScaleCurrentClaimCount {
			chainLength++
		}
		previousID := ""
		for version := 0; version < chainLength; version++ {
			seq++
			id := fmt.Sprintf("claim_scale_%03d_%02d", entityIndex, version)
			entity := fmt.Sprintf("scale-filler-%03d", entityIndex)
			predicate := "status"
			object := fmt.Sprintf("historical mechanical corpus evidence parcel %03d version %02d", entityIndex, version)
			claimType := "status"
			confidence := 0.9
			if entityIndex == 0 && version == chainLength-1 {
				entity = "release-transit"
				predicate = "architecture"
				object = promptScaleTarget + " deployment automation releases verified artifacts"
				claimType = "architecture"
				confidence = 0.99
			}
			claim := mem.Claim{
				ID: id, Seq: seq, Entity: entity, Predicate: predicate, Object: object,
				ValidFrom: fixedNow.Format(time.RFC3339Nano), Confidence: confidence,
				Derivation: "deterministic", ClaimType: claimType,
				PromotionBasis: "runtime-proof", EvidenceRefs: []string{"trace_scale_history"},
				Supersedes: previousID, Visibility: "operational", State: mem.ClaimStatePromoted,
				CreatedAt: fixedNow.Add(time.Duration(seq) * time.Microsecond).Format(time.RFC3339Nano),
			}
			if err := claimsEncoder.Encode(claim); err != nil {
				t.Fatal(err)
			}
			if previousID != "" {
				supersessionEdges = append(supersessionEdges, mem.Edge{
					Kind: mem.EdgeKindSupersedes, From: id, To: previousID, Confidence: 1,
					CreatedAt: fixedNow.Add(time.Duration(seq) * time.Microsecond).Format(time.RFC3339Nano),
				})
			}
			previousID = id
		}
	}
	if err := claimsWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := claimsFile.Close(); err != nil {
		t.Fatal(err)
	}
	if seq != promptScaleRawClaimCount {
		t.Fatalf("constructed raw claims = %d, want %d", seq, promptScaleRawClaimCount)
	}

	edgesFile, err := os.Create(edgesPath)
	if err != nil {
		t.Fatal(err)
	}
	edgesWriter := bufio.NewWriterSize(edgesFile, 1<<20)
	edgesEncoder := json.NewEncoder(edgesWriter)
	edgeSeq := seq
	for i := range supersessionEdges {
		edgeSeq++
		supersessionEdges[i].Seq = edgeSeq
		if err := edgesEncoder.Encode(supersessionEdges[i]); err != nil {
			t.Fatal(err)
		}
	}
	for edgeIndex := len(supersessionEdges); edgeIndex < promptScaleEdgeCount; edgeIndex++ {
		edgeSeq++
		edge := mem.Edge{
			Seq: edgeSeq, Kind: "relates",
			From:       fmt.Sprintf("scale-filler-%03d", edgeIndex%promptScaleCurrentClaimCount),
			To:         fmt.Sprintf("scale-filler-%03d", (edgeIndex*17+1)%promptScaleCurrentClaimCount),
			Confidence: 0.75,
			CreatedAt:  fixedNow.Add(time.Duration(edgeSeq) * time.Microsecond).Format(time.RFC3339Nano),
		}
		if err := edgesEncoder.Encode(edge); err != nil {
			t.Fatal(err)
		}
	}
	if err := edgesWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := edgesFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "ledger", "seq"), []byte(fmt.Sprintf("%d\n", edgeSeq)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FoldPositions(mem.PositionFoldOptions{}, fixedNow); err != nil {
		t.Fatal(err)
	}
}

func assertPromptScaleShape(t *testing.T, homePath string) {
	t.Helper()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	rawClaims, err := store.LedgerClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(rawClaims) != promptScaleRawClaimCount {
		t.Fatalf("raw claim rows = %d, want %d", len(rawClaims), promptScaleRawClaimCount)
	}
	currentClaims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if len(currentClaims) != promptScaleCurrentClaimCount {
		t.Fatalf("current visible claims = %d, want %d", len(currentClaims), promptScaleCurrentClaimCount)
	}
	if got := countPromptScaleJSONLLines(t, filepath.Join(homePath, "ledger", "edges.jsonl")); got != promptScaleEdgeCount {
		t.Fatalf("edge rows = %d, want %d", got, promptScaleEdgeCount)
	}
	if got := countPromptScaleJSONLLines(t, filepath.Join(homePath, "ledger", "entities.jsonl")); got != promptScaleEntityHistoryRows {
		t.Fatalf("entity history rows = %d, want %d", got, promptScaleEntityHistoryRows)
	}
	capStore, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := capStore.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.NodeCount != promptScaleNodeCount {
		t.Fatalf("capability nodes = %d, want %d", stats.NodeCount, promptScaleNodeCount)
	}
}

func countPromptScaleJSONLLines(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return count
}

func seedPromptScaleCapabilities(t *testing.T, homePath string) {
	t.Helper()
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(capgraph.Node{
		ID: "skill:scale-target", Kind: "skill", Name: promptScaleTarget + " Launch Transit",
		Triggers: []string{"deployment orchestration"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < promptScaleNodeCount; i++ {
		if err := store.Add(capgraph.Node{
			ID: fmt.Sprintf("skill:scale-filler-%03d", i), Kind: "skill",
			Name: fmt.Sprintf("Mechanical Filler %03d", i), Triggers: []string{fmt.Sprintf("filler capability %03d", i)}, Weight: 1,
		}); err != nil {
			t.Fatalf("add capability %d: %v", i, err)
		}
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.NodeCount != promptScaleNodeCount {
		t.Fatalf("seeded nodes = %d, want %d", stats.NodeCount, promptScaleNodeCount)
	}
}

type promptScaleEmbedServer struct {
	server *httptest.Server

	mu         sync.Mutex
	taskCounts map[string]int
	target     []float32
	filler     []float32
}

func newPromptScaleEmbedServer(t *testing.T) *promptScaleEmbedServer {
	t.Helper()
	server := &promptScaleEmbedServer{
		taskCounts: map[string]int{},
		target:     promptScaleVector(false),
		filler:     promptScaleVector(true),
	}
	server.server = httptest.NewServer(http.HandlerFunc(server.handle))
	t.Cleanup(server.server.Close)
	return server
}

func (s *promptScaleEmbedServer) handle(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Input []string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := make([]map[string]any, 0, len(request.Input))
	for i, text := range request.Input {
		isTask := strings.HasPrefix(text, "PROMPT_SCALE_QUERY_") || text == "warm prompt runtime"
		isTarget := isTask || strings.Contains(text, promptScaleTarget)
		if isTask {
			s.mu.Lock()
			s.taskCounts[text]++
			s.mu.Unlock()
		}
		vector := s.filler
		if isTarget {
			vector = s.target
		}
		data = append(data, map[string]any{"index": i, "embedding": vector})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (s *promptScaleEmbedServer) resetTaskCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taskCounts = map[string]int{}
}

func (s *promptScaleEmbedServer) taskCount(task string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taskCounts[task]
}

func promptScaleVector(alternate bool) []float32 {
	vector := make([]float32, 768)
	for i := range vector {
		value := float32((i%29)+1) / 29
		if alternate && i%2 == 1 {
			value = -value
		}
		vector[i] = value
	}
	return vector
}

func promptScaleCacheTimes(t *testing.T, homePath string) map[string]time.Time {
	t.Helper()
	wantNames := map[string]bool{"manifest.json": true, "vectors.bin": true, "vectors.json": true}
	times := map[string]time.Time{}
	for _, family := range []string{"breadth", "capgraph"} {
		root := filepath.Join(homePath, "indexes", family)
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !wantNames[entry.Name()] {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			times[path] = info.ModTime()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	positionsPath := filepath.Join(homePath, "ledger", "positions.jsonl")
	positionsInfo, err := os.Stat(positionsPath)
	if err != nil {
		t.Fatal(err)
	}
	times[positionsPath] = positionsInfo.ModTime()
	if len(times) != 5 {
		t.Fatalf("semantic cache/projection files = %d, want 5: %v", len(times), times)
	}
	return times
}
