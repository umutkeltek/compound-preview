package contract

import (
	"reflect"
	"testing"

	"compound/internal/mem"
)

type contractObservedEchoEngine struct{}

func (contractObservedEchoEngine) Capabilities() mem.EngineCapabilities {
	return mem.EngineCapabilities{Name: "contract-observed-echo", Breadth: true}
}
func (contractObservedEchoEngine) Index([]mem.Claim, int64) error { return nil }
func (contractObservedEchoEngine) Query(text, _ string, _ []string, _ int) ([]mem.Hit, error) {
	return []mem.Hit{{
		Kind: mem.KindObservedFact,
		Observed: &mem.ObservedFact{
			Entity: "engine-query-echo", Predicate: "query_echo", Object: text,
		},
	}}, nil
}

func TestReadOnlyRetrievalDoesNotCaptureEngineObservedFact(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "fixture-observed-echo")
	priorFactory := breadthEngineFactory
	breadthEngineFactory = func(string) (mem.Engine, func()) {
		return contractObservedEchoEngine{}, func() {}
	}
	defer func() { breadthEngineFactory = priorFactory }()

	in := Input{
		Harness: "codex", Cwd: "/repo/product", Task: "CONTRACT_QUERY_PRIVACY_SENTINEL",
		RepoKind: "product", RepoKindVia: "flag", Mode: "execute",
	}
	field := reflect.ValueOf(&in).Elem().FieldByName("ReadOnlyRetrieval")
	if !field.IsValid() || !field.CanSet() {
		t.Fatal("contract Input does not expose ReadOnlyRetrieval")
	}
	field.SetBool(true)
	if _, err := PackWithCapabilities(in, DefaultPolicy(), homePath, fixedNow); err != nil {
		t.Fatal(err)
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := store.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("read-only contract retrieval captured engine echo: %+v", candidates)
	}
}
