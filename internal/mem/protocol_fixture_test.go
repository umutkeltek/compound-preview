package mem

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProtocolIndexContextGoldenFixture(t *testing.T) {
	req := Request{
		SchemaVersion: ProtocolVersion,
		RequestID:     "req_fixture_index_context",
		Op:            OpIndex,
		CorpusGen:     42,
		Claims: []ClaimText{{
			ClaimID:    "claim_app_runtime",
			Text:       "app runtime go",
			Entity:     "app",
			ClaimType:  "architecture",
			Seq:        7,
			CreatedAt:  "2026-07-02T00:00:00Z",
			Confidence: 0.91,
			Derivation: "deterministic",
			EvidenceN:  2,
		}},
		Edges: []WireEdge{{
			From:       "app",
			Kind:       "depends_on",
			To:         "auth",
			Confidence: 0.9,
		}},
	}
	assertJSONFixture(t, filepath.Join("..", "..", "fixtures", "engine", "index-context-request.json"), req)
}

func TestProtocolDegradedResponseGoldenFixture(t *testing.T) {
	resp := Response{
		OK:            true,
		SchemaVersion: ProtocolVersion,
		RequestID:     "req_fixture_query_degraded",
		Hits:          []Hit{{ClaimID: "claim_app_runtime", Score: 0.75}},
		DegradedLanes: []string{"vector"},
		CorpusGen:     42,
	}
	assertJSONFixture(t, filepath.Join("..", "..", "fixtures", "engine", "query-degraded-response.json"), resp)
}

func assertJSONFixture(t *testing.T, path string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("fixture %s mismatch\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}
