package contract

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/mem"
)

type unavailableTraceEngine struct{}

func (unavailableTraceEngine) Capabilities() mem.EngineCapabilities {
	return mem.EngineCapabilities{Name: "protocol", Breadth: true}
}

func (unavailableTraceEngine) Index([]mem.Claim, int64) error {
	return mem.ErrEngineUnavailable
}

func (unavailableTraceEngine) Query(string, string, []string, int) ([]mem.Hit, error) {
	return nil, mem.ErrEngineUnavailable
}

type invalidNameTraceEngine struct{}

func (invalidNameTraceEngine) Capabilities() mem.EngineCapabilities {
	return mem.EngineCapabilities{Name: "invalid/engine", Breadth: true, Embedder: "test:embedder:3:f32"}
}

func (invalidNameTraceEngine) Index([]mem.Claim, int64) error { return nil }

func (invalidNameTraceEngine) Query(string, string, []string, int) ([]mem.Hit, error) {
	return nil, nil
}

type scriptedTraceEngine struct {
	capabilities mem.EngineCapabilities
	indexErr     error
	queryErr     error
	hits         []mem.Hit
	degraded     bool
}

func (e scriptedTraceEngine) Capabilities() mem.EngineCapabilities { return e.capabilities }
func (e scriptedTraceEngine) Index([]mem.Claim, int64) error       { return e.indexErr }
func (e scriptedTraceEngine) Query(string, string, []string, int) ([]mem.Hit, error) {
	return append([]mem.Hit(nil), e.hits...), e.queryErr
}
func (e scriptedTraceEngine) QueryWithDiagnostics(string, string, []string, int) ([]mem.Hit, []string, error) {
	var lanes []string
	if e.degraded {
		lanes = []string{"vector"}
	}
	return append([]mem.Hit(nil), e.hits...), lanes, e.queryErr
}

func seedTraceLedgerMemory(t *testing.T, homePath string) string {
	t.Helper()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := store.Capture([]mem.Candidate{{
		Entity: "retrieval-posture", Predicate: "architecture",
		Object:     "ledger ranking keeps task retrieval memory available during provider failure",
		Confidence: 0.99, Derivation: "deterministic", ClaimType: "architecture",
		PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "trace_retrieval_posture",
		SnippetHash: "sha256:retrieval-posture", Visibility: "operational",
	}}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := store.Adjudicate(captured[0].ID, candidates.Promote, "test", fixedNow)
	if err != nil || promoted.Claim == nil {
		t.Fatalf("promote fallback memory: result=%+v err=%v", promoted, err)
	}
	return "claim://" + promoted.Claim.ID
}

func packWithTraceEngine(t *testing.T, homePath string, engine mem.Engine) (envelope.ContextPack, EngineDependencyTrace) {
	t.Helper()
	original := breadthEngineFactory
	breadthEngineFactory = func(string) (mem.Engine, func()) { return engine, func() {} }
	t.Cleanup(func() { breadthEngineFactory = original })
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "opaque-test-command")
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "gbrain")
	pack, trace, err := PackWithCapabilitiesTraced(Input{
		Harness: "codex", Cwd: "/workspace/synthetic",
		Task: "task retrieval memory provider posture", RepoKind: "control-plane", Mode: "execute",
	}, DefaultPolicy(), homePath, fixedNow)
	if err != nil {
		t.Fatalf("orient must preserve fail-open ledger fallback: %v", err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("materialized ContextPack invalid: %v\npack=%+v", err, pack)
	}
	return pack, trace
}

func TestPackWithCapabilitiesTracedRecordsBrokenConfiguredEngine(t *testing.T) {
	homePath := t.TempDir()
	wantMemory := seedTraceLedgerMemory(t, homePath)

	pack, trace := packWithTraceEngine(t, homePath, unavailableTraceEngine{})
	if !trace.Attempted || trace.RequestedEngine != "gbrain" {
		t.Fatalf("trace attempt/request = %+v, want attempted gbrain", trace)
	}
	if trace.ObservedEngine != "" {
		t.Fatalf("failure before real capabilities must not claim observed engine: %+v", trace)
	}
	if trace.Outcome != EngineOutcomeDegraded || trace.ErrorClass != EngineErrorUnavailable {
		t.Fatalf("trace outcome/error = %+v, want degraded/unavailable", trace)
	}
	if trace.Embedder != "" || trace.CorpusGeneration != 0 {
		t.Fatalf("failed engine must not invent capability evidence: %+v", trace)
	}
	if pack.Retrieval == nil || pack.Retrieval.Level != envelope.RetrievalLevelDegraded || pack.Retrieval.ErrorClass != envelope.RetrievalErrorUnavailable {
		t.Fatalf("pack retrieval = %+v, want degraded/unavailable", pack.Retrieval)
	}
	if pack.Retrieval.Fallback != envelope.RetrievalFallbackLedgerRanker || pack.Retrieval.Authority != envelope.RetrievalAuthorityCompoundLedger {
		t.Fatalf("degraded pack lost ledger fallback/authority: %+v", pack.Retrieval)
	}
	if !contains(pack.ActiveMemory, wantMemory) {
		t.Fatalf("degraded pack lost task-relevant ledger memory %q: %v", wantMemory, pack.ActiveMemory)
	}
}

func TestPackWithCapabilitiesTracedProjectsBoundedFailureClasses(t *testing.T) {
	tests := []struct {
		name       string
		engine     scriptedTraceEngine
		wantClass  string
		unownedRef string
	}{
		{name: "embedder mismatch", engine: scriptedTraceEngine{capabilities: mem.EngineCapabilities{Name: "semantic-index", Breadth: true}, queryErr: mem.ErrEmbedderMismatch}, wantClass: EngineErrorEmbedderMismatch},
		{name: "protocol mismatch", engine: scriptedTraceEngine{capabilities: mem.EngineCapabilities{Name: "semantic-index", Breadth: true}, queryErr: mem.ErrProtocolVersion}, wantClass: EngineErrorProtocolVersion},
		{name: "engine ahead", engine: scriptedTraceEngine{capabilities: mem.EngineCapabilities{Name: "semantic-index", Breadth: true, CorpusGen: 1 << 30}}, wantClass: EngineErrorEngineAhead},
		{name: "unowned hit", engine: scriptedTraceEngine{capabilities: mem.EngineCapabilities{Name: "semantic-index", Breadth: true}, hits: []mem.Hit{{ClaimID: "claim_unowned_provider_hit", Score: 1}}}, wantClass: EngineErrorUnownedClaim, unownedRef: "claim://claim_unowned_provider_hit"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			homePath := t.TempDir()
			wantMemory := seedTraceLedgerMemory(t, homePath)
			pack, trace := packWithTraceEngine(t, homePath, tc.engine)
			if trace.Outcome != EngineOutcomeRefused || trace.ErrorClass != tc.wantClass {
				t.Fatalf("trace = %+v, want refused/%s", trace, tc.wantClass)
			}
			if pack.Retrieval == nil || pack.Retrieval.Level != envelope.RetrievalLevelRefused || pack.Retrieval.ErrorClass != tc.wantClass {
				t.Fatalf("pack retrieval = %+v, want refused/%s", pack.Retrieval, tc.wantClass)
			}
			if pack.Retrieval.Fallback != envelope.RetrievalFallbackLedgerRanker || pack.Retrieval.Authority != envelope.RetrievalAuthorityCompoundLedger {
				t.Fatalf("refused posture lost ledger fallback/authority: %+v", pack.Retrieval)
			}
			if !contains(pack.ActiveMemory, wantMemory) {
				t.Fatalf("refused pack lost ledger-ranked memory %q: %v", wantMemory, pack.ActiveMemory)
			}
			if tc.unownedRef != "" && (contains(pack.ActiveMemory, tc.unownedRef) || hasIncluded(pack, tc.unownedRef)) {
				t.Fatalf("unowned engine hit entered active pack: active=%v included=%v", pack.ActiveMemory, pack.Included)
			}
		})
	}
}

func TestPackWithCapabilitiesTracedProjectsUsedAndNotAttempted(t *testing.T) {
	t.Run("successful degraded breadth query", func(t *testing.T) {
		homePath := t.TempDir()
		wantMemory := seedTraceLedgerMemory(t, homePath)
		claimID := strings.TrimPrefix(wantMemory, "claim://")
		pack, trace := packWithTraceEngine(t, homePath, scriptedTraceEngine{
			capabilities: mem.EngineCapabilities{Name: "semantic-index", Breadth: true},
			hits:         []mem.Hit{{ClaimID: claimID, Score: 1}},
			degraded:     true,
		})
		if trace.Outcome != EngineOutcomeDegraded || trace.ErrorClass != EngineErrorRetrieval {
			t.Fatalf("trace = %+v, want degraded/retrieval_error", trace)
		}
		if pack.Retrieval == nil || pack.Retrieval.Level != envelope.RetrievalLevelDegraded || pack.Retrieval.ErrorClass != envelope.RetrievalErrorRetrieval {
			t.Fatalf("pack retrieval = %+v, want degraded/retrieval_error", pack.Retrieval)
		}
	})
	t.Run("zero-hit degraded breadth query", func(t *testing.T) {
		homePath := t.TempDir()
		seedTraceLedgerMemory(t, homePath)
		pack, trace := packWithTraceEngine(t, homePath, scriptedTraceEngine{
			capabilities: mem.EngineCapabilities{Name: "semantic-index", Breadth: true},
			degraded:     true,
		})
		if trace.Outcome != EngineOutcomeDegraded || trace.ErrorClass != EngineErrorRetrieval {
			t.Fatalf("zero-hit trace = %+v, want degraded/retrieval_error", trace)
		}
		if pack.Retrieval == nil || pack.Retrieval.Level != envelope.RetrievalLevelDegraded {
			t.Fatalf("zero-hit pack retrieval = %+v, want degraded", pack.Retrieval)
		}
	})
	t.Run("successful breadth dependency", func(t *testing.T) {
		homePath := t.TempDir()
		wantMemory := seedTraceLedgerMemory(t, homePath)
		claimID := strings.TrimPrefix(wantMemory, "claim://")
		pack, trace := packWithTraceEngine(t, homePath, scriptedTraceEngine{
			capabilities: mem.EngineCapabilities{Name: "semantic-index", Breadth: true},
			hits:         []mem.Hit{{ClaimID: claimID, Score: 1}},
		})
		if trace.Outcome != EngineOutcomeUsed || trace.ErrorClass != "" {
			t.Fatalf("trace = %+v, want used without error", trace)
		}
		if pack.Retrieval == nil || pack.Retrieval.Level != envelope.RetrievalLevelUsed || pack.Retrieval.ErrorClass != "" || pack.Retrieval.Fallback != "" {
			t.Fatalf("used pack retrieval = %+v", pack.Retrieval)
		}
		if pack.Retrieval.RequestedEngine != "gbrain" || pack.Retrieval.ObservedEngine != "semantic-index" {
			t.Fatalf("used engine identities = %+v", pack.Retrieval)
		}
	})

	t.Run("no configured breadth capability", func(t *testing.T) {
		homePath := t.TempDir()
		wantMemory := seedTraceLedgerMemory(t, homePath)
		t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")
		t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "")
		pack, trace, err := PackWithCapabilitiesTraced(Input{
			Harness: "codex", Cwd: "/workspace/synthetic",
			Task: "task retrieval memory offline posture", RepoKind: "control-plane", Mode: "execute",
		}, DefaultPolicy(), homePath, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		if trace.Attempted || trace.Outcome != EngineOutcomeNotAttempted {
			t.Fatalf("trace = %+v, want not_attempted", trace)
		}
		if pack.Retrieval == nil || pack.Retrieval.Level != envelope.RetrievalLevelNotAttempted || pack.Retrieval.Fallback != envelope.RetrievalFallbackLedgerRanker {
			t.Fatalf("offline retrieval = %+v, want not_attempted ledger fallback", pack.Retrieval)
		}
		if pack.Retrieval.RequestedEngine != "" || pack.Retrieval.ObservedEngine != "" || pack.Retrieval.ErrorClass != "" {
			t.Fatalf("offline posture fabricated engine evidence: %+v", pack.Retrieval)
		}
		if !contains(pack.ActiveMemory, wantMemory) {
			t.Fatalf("offline pack lost ledger-ranked memory %q: %v", wantMemory, pack.ActiveMemory)
		}
		if err := pack.Validate(); err != nil {
			t.Fatalf("offline ContextPack invalid: %v", err)
		}
	})
}

func TestPackRetrievalPostureNeverSerializesRawProviderFailure(t *testing.T) {
	homePath := t.TempDir()
	seedTraceLedgerMemory(t, homePath)
	rawFailure := "provider endpoint=https://private.invalid query=QUERY_MARKER prompt=PROMPT_MARKER body=BODY_MARKER private_path=PATH_MARKER credential=CREDENTIAL_MARKER request_id=req-private response=RESPONSE_MARKER retry after task text"
	pack, _ := packWithTraceEngine(t, homePath, scriptedTraceEngine{
		capabilities: mem.EngineCapabilities{Name: "semantic-index", Breadth: true},
		queryErr:     fmt.Errorf("%s: %w", rawFailure, mem.ErrEngineUnavailable),
	})
	body, err := json.Marshal(pack.Retrieval)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		rawFailure, "https://private.invalid", "QUERY_MARKER", "PROMPT_MARKER", "BODY_MARKER",
		"PATH_MARKER", "CREDENTIAL_MARKER", "req-private", "RESPONSE_MARKER", "task text", "retry after",
	} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("retrieval posture leaked provider detail %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(string(body), `"error_class":"unavailable"`) {
		t.Fatalf("posture lost bounded failure class: %s", body)
	}
}

func TestPackWithCapabilitiesTracedRefusesUnreportableEngineIdentity(t *testing.T) {
	homePath := t.TempDir()
	pack, trace := packWithTraceEngine(t, homePath, invalidNameTraceEngine{})
	if !trace.Attempted || trace.ObservedEngine != "" || trace.Outcome != EngineOutcomeRefused || trace.ErrorClass != EngineErrorProtocolVersion {
		t.Fatalf("invalid-name trace = %+v, want bounded protocol refusal", trace)
	}
	if pack.Retrieval == nil || pack.Retrieval.Level != envelope.RetrievalLevelRefused || pack.Retrieval.ObservedEngine != "" || pack.Retrieval.ErrorClass != envelope.RetrievalErrorProtocolVersion {
		t.Fatalf("invalid-name posture = %+v, want bounded protocol refusal", pack.Retrieval)
	}
}
