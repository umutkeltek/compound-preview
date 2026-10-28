package contract

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"compound/internal/breadth"
	"compound/internal/mem"
)

func syntheticContractPostgresDSN() string {
	return strings.Join([]string{"postgres", "://", "synthetic", "@", "db.example.invalid", "/", "compound"}, "")
}

const (
	postgresExperimentEnv = "COMPOUND_EXPERIMENTAL_POSTGRES"
	postgresProfileEnv    = "COMPOUND_POSTGRES_PROFILE"
	postgresDSNEnv        = "COMPOUND_DSN"
	postgresVectorDimsEnv = "COMPOUND_VECTOR_DIMS"
)

func setPostgresSelectionEnv(t *testing.T, store string, signalMask int) {
	t.Helper()
	t.Setenv("COMPOUND_STORE", store)
	t.Setenv(postgresExperimentEnv, "")
	t.Setenv(postgresProfileEnv, "")
	t.Setenv(postgresDSNEnv, "")
	t.Setenv(postgresVectorDimsEnv, "")
	if signalMask&1 != 0 {
		t.Setenv(postgresExperimentEnv, "1")
	}
	if signalMask&2 != 0 {
		t.Setenv(postgresProfileEnv, "local-lab")
	}
	if signalMask&4 != 0 {
		t.Setenv(postgresDSNEnv, syntheticContractPostgresDSN())
	}
	if signalMask&8 != 0 {
		t.Setenv(postgresVectorDimsEnv, "1536")
	}
}

func TestSelectedExternalStoreUsesStablePgvectorRequestedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		store      string
		signalMask int
	}{
		{name: "valid postgres gate", store: "postgres", signalMask: 1 | 2 | 4},
		{name: "unknown external backend", store: "postgre"},
		{name: "partial opt in", signalMask: 1},
		{name: "partial profile on file", store: "file", signalMask: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setPostgresSelectionEnv(t, tc.store, tc.signalMask)
			if got := requestedEngineIdentity(); got != "pgvector" {
				t.Fatalf("requestedEngineIdentity() = %q, want pgvector for selected external store", got)
			}
		})
	}
}

func TestExternalStoreSelectionNeverFallsBackToFileRecall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset", value: "", want: false},
		{name: "file", value: "file", want: false},
		{name: "postgres", value: "postgres", want: true},
		{name: "unknown", value: "postgre", want: true},
		{name: "padded_file", value: " file ", want: true},
		{name: "padded_postgres", value: " postgres ", want: true},
		{name: "case_varied_file", value: "FILE", want: true},
		{name: "case_varied_postgres", value: "POSTGRES", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COMPOUND_STORE", tc.value)
			if got := externalStoreSelected(); got != tc.want {
				t.Fatalf("externalStoreSelected() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBreadthEmbedderConfigurationDoesNotSelectExternalStore(t *testing.T) {
	setPostgresSelectionEnv(t, "", 0)
	t.Setenv("COMPOUND_EMBED_URL", "https://embed.example.invalid")
	t.Setenv("COMPOUND_EMBED_MODEL", "synthetic-embedder")

	if externalStoreSelected() {
		t.Fatal("breadth embedder configuration was misclassified as external Postgres storage")
	}
}

func TestPartialPostgresConfigurationCountsAsExternalSelection(t *testing.T) {
	for _, store := range []string{"", "file"} {
		storeName := store
		if storeName == "" {
			storeName = "unset"
		}
		for signalMask := 1; signalMask < 16; signalMask++ {
			t.Run(fmt.Sprintf("%s/signals_%04b", storeName, signalMask), func(t *testing.T) {
				setPostgresSelectionEnv(t, store, signalMask)
				if !externalStoreSelected() {
					t.Fatal("partial Postgres configuration was classified as the file backend")
				}
			})
		}
	}
}

func TestUnknownExternalStoreSelectionCannotFallBackToBreadthEngine(t *testing.T) {
	t.Setenv("COMPOUND_STORE", "postgre")

	priorPgFactory := pgvectorEngineFactory
	priorBreadthFactory := breadthEngineFactory
	pgCalls := 0
	breadthCalls := 0
	pgvectorEngineFactory = func() (CloseableEngine, error) {
		pgCalls++
		return nil, errors.New("synthetic selected backend unavailable")
	}
	breadthEngineFactory = func(string) (mem.Engine, func()) {
		breadthCalls++
		return unavailableTraceEngine{}, func() {}
	}
	t.Cleanup(func() {
		pgvectorEngineFactory = priorPgFactory
		breadthEngineFactory = priorBreadthFactory
	})

	homePath := t.TempDir()
	seedTraceLedgerMemory(t, homePath)
	_, trace, err := PackWithCapabilitiesTraced(Input{
		Harness:  "codex",
		Cwd:      "/workspace/synthetic",
		Task:     "task retrieval memory provider posture",
		RepoKind: "control-plane",
		Mode:     "execute",
	}, DefaultPolicy(), homePath, fixedNow)
	if err != nil {
		t.Fatalf("orient with unavailable selected backend: %v", err)
	}
	if pgCalls == 0 {
		t.Fatal("selected external backend factory was not attempted")
	}
	if breadthCalls != 0 {
		t.Fatalf("selected external backend fell through to breadth engine %d time(s)", breadthCalls)
	}
	if !trace.Attempted || trace.Outcome != EngineOutcomeDegraded || trace.ErrorClass != EngineErrorUnavailable {
		t.Fatalf("trace = %+v, want attempted degraded/unavailable selected backend", trace)
	}
}

func TestReadOnlyExternalStoreSelectionReportsPgvectorInsteadOfBreadth(t *testing.T) {
	setPostgresSelectionEnv(t, "postgre", 0)

	priorPgFactory := pgvectorEngineFactory
	priorResolver := breadthEmbedderResolver
	pgCalls := 0
	pgvectorEngineFactory = func() (CloseableEngine, error) {
		pgCalls++
		return nil, errors.New("synthetic selected backend unavailable")
	}
	breadthEmbedderResolver = func(string) (breadth.EmbedderAdapter, bool) {
		return &capturedDependencyEmbedder{}, true
	}
	t.Cleanup(func() {
		pgvectorEngineFactory = priorPgFactory
		breadthEmbedderResolver = priorResolver
	})

	homePath := t.TempDir()
	seedTraceLedgerMemory(t, homePath)
	_, trace, err := PackWithCapabilitiesTraced(Input{
		Harness:           "codex",
		Cwd:               "/workspace/synthetic",
		Task:              "task retrieval memory provider posture",
		RepoKind:          "control-plane",
		Mode:              "execute",
		ReadOnlyRetrieval: true,
	}, DefaultPolicy(), homePath, fixedNow)
	if err != nil {
		t.Fatalf("read-only orient with unavailable selected backend: %v", err)
	}
	if pgCalls == 0 {
		t.Fatal("selected external backend factory was not attempted")
	}
	if trace.RequestedEngine != "pgvector" {
		t.Fatalf("requested engine = %q, want pgvector rather than the bypassed breadth lane", trace.RequestedEngine)
	}
	if !trace.Attempted || trace.Outcome != EngineOutcomeDegraded || trace.ErrorClass != EngineErrorUnavailable {
		t.Fatalf("trace = %+v, want attempted degraded/unavailable selected backend", trace)
	}
}

func TestNotAttemptedRetrievalStillNamesSelectedExternalStore(t *testing.T) {
	posture := retrievalPostureFromEngineTrace(EngineDependencyTrace{
		Outcome:         EngineOutcomeNotAttempted,
		RequestedEngine: "pgvector",
	})
	if posture.RequestedEngine != "pgvector" {
		t.Fatalf("not-attempted posture requested engine = %q, want selected pgvector identity", posture.RequestedEngine)
	}
	if posture.Fallback != "ledger_ranker" {
		t.Fatalf("not-attempted posture fallback = %q, want ledger_ranker", posture.Fallback)
	}
}

func TestPartialPostgresConfigurationCannotFallBackToBreadthEngine(t *testing.T) {
	for _, store := range []string{"", "file"} {
		storeName := store
		if storeName == "" {
			storeName = "unset"
		}
		for signalMask := 1; signalMask < 16; signalMask++ {
			t.Run(fmt.Sprintf("%s/signals_%04b", storeName, signalMask), func(t *testing.T) {
				setPostgresSelectionEnv(t, store, signalMask)

				priorPgFactory := pgvectorEngineFactory
				priorBreadthFactory := breadthEngineFactory
				pgCalls := 0
				breadthCalls := 0
				pgvectorEngineFactory = func() (CloseableEngine, error) {
					pgCalls++
					return nil, errors.New("synthetic partial backend unavailable")
				}
				breadthEngineFactory = func(string) (mem.Engine, func()) {
					breadthCalls++
					return unavailableTraceEngine{}, func() {}
				}
				t.Cleanup(func() {
					pgvectorEngineFactory = priorPgFactory
					breadthEngineFactory = priorBreadthFactory
				})

				homePath := t.TempDir()
				seedTraceLedgerMemory(t, homePath)
				_, trace, err := PackWithCapabilitiesTraced(Input{
					Harness:  "codex",
					Cwd:      "/workspace/synthetic",
					Task:     "task retrieval memory provider posture",
					RepoKind: "control-plane",
					Mode:     "execute",
				}, DefaultPolicy(), homePath, fixedNow)
				if err != nil {
					t.Fatalf("orient with partial backend selection: %v", err)
				}
				if pgCalls == 0 {
					t.Fatal("partial external backend factory was not attempted")
				}
				if breadthCalls != 0 {
					t.Fatalf("partial external backend fell through to breadth engine %d time(s)", breadthCalls)
				}
				if !trace.Attempted || trace.Outcome != EngineOutcomeDegraded || trace.ErrorClass != EngineErrorUnavailable {
					t.Fatalf("trace = %+v, want attempted degraded/unavailable partial backend", trace)
				}
			})
		}
	}
}

func TestCleanFileConfigurationRetainsBreadthEngine(t *testing.T) {
	for _, store := range []string{"", "file"} {
		storeName := store
		if storeName == "" {
			storeName = "unset"
		}
		t.Run(storeName, func(t *testing.T) {
			setPostgresSelectionEnv(t, store, 0)
			t.Setenv("COMPOUND_MEM_ENGINE_BIN", "/synthetic/breadth-engine")

			priorPgFactory := pgvectorEngineFactory
			priorBreadthFactory := breadthEngineFactory
			pgCalls := 0
			breadthCalls := 0
			pgvectorEngineFactory = func() (CloseableEngine, error) {
				pgCalls++
				return nil, errors.New("unexpected external backend call")
			}
			breadthEngineFactory = func(string) (mem.Engine, func()) {
				breadthCalls++
				return unavailableTraceEngine{}, func() {}
			}
			t.Cleanup(func() {
				pgvectorEngineFactory = priorPgFactory
				breadthEngineFactory = priorBreadthFactory
			})

			homePath := t.TempDir()
			seedTraceLedgerMemory(t, homePath)
			_, _, err := PackWithCapabilitiesTraced(Input{
				Harness:  "codex",
				Cwd:      "/workspace/synthetic",
				Task:     "task retrieval memory provider posture",
				RepoKind: "control-plane",
				Mode:     "execute",
			}, DefaultPolicy(), homePath, fixedNow)
			if err != nil {
				t.Fatalf("orient with clean file selection: %v", err)
			}
			if pgCalls != 0 {
				t.Fatalf("clean file configuration attempted external backend %d time(s)", pgCalls)
			}
			if breadthCalls == 0 {
				t.Fatal("clean file configuration did not retain breadth engine")
			}
		})
	}
}
