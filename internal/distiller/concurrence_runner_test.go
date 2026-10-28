package distiller

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"compound/internal/mem"
)

// laneServerReturningContent stands up a fake OpenAI-compatible lane whose
// assistant content is the given raw string (already a JSON string literal).
func laneServerReturningContent(t *testing.T, content string) *httptest.Server {
	t.Helper()
	body := `{"model":"fake","choices":[{"message":{"role":"assistant","content":` + strconv.Quote(content) + `}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runnerAt(srv *httptest.Server) *ConcurrenceRunner {
	return &ConcurrenceRunner{
		HTTPClient: srv.Client(),
		LookupLane: func(string) (ConcurrenceLane, bool) {
			return ConcurrenceLane{BaseURL: srv.URL, Model: "fake/model"}, true
		},
	}
}

func testProbe() mem.ConcurrenceProbe {
	return mem.ConcurrenceProbe{
		SchemaVersion: mem.ConcurrenceProbeSchemaVersion,
		ID:            "p", ClaimID: "claim_x", Content: "is it safe?",
		Lanes: []string{"alfa"}, Threshold: 1,
	}
}

// TestConcurrenceRunnerAgreeHappyPath: a clean agree is reported as agree.
func TestConcurrenceRunnerAgreeHappyPath(t *testing.T) {
	srv := laneServerReturningContent(t, `{"verdict":"agree","summary":"ok"}`)
	ans, err := runnerAt(srv).RunConcurrence(testProbe(), "alfa")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ans.Verdict != mem.ConcurrenceVerdictAgree || ans.Summary != "ok" {
		t.Fatalf("expected agree/ok, got %+v", ans)
	}
}

// TestConcurrenceRunnerUnrecognizedVerdictNeverAgrees: a verdict outside the
// closed set is mapped to abstain — NEVER agree — so a chatty model cannot
// inflate the agreement count.
func TestConcurrenceRunnerUnrecognizedVerdictNeverAgrees(t *testing.T) {
	srv := laneServerReturningContent(t, `{"verdict":"probably","summary":"shrug"}`)
	ans, err := runnerAt(srv).RunConcurrence(testProbe(), "alfa")
	if err != nil {
		t.Fatalf("unrecognized verdict is not an error: %v", err)
	}
	if ans.Verdict != mem.ConcurrenceVerdictAbstain {
		t.Fatalf("unrecognized verdict must abstain, got %q", ans.Verdict)
	}
}

// TestConcurrenceRunnerMalformedVerdictAbstains: a lane that returns non-JSON
// abstains with an error (the caller records it as abstain), never agreement.
func TestConcurrenceRunnerMalformedVerdictAbstains(t *testing.T) {
	srv := laneServerReturningContent(t, `this is not json at all`)
	ans, err := runnerAt(srv).RunConcurrence(testProbe(), "alfa")
	if err == nil {
		t.Fatal("expected error on malformed verdict")
	}
	if ans.Verdict != mem.ConcurrenceVerdictAbstain {
		t.Fatalf("malformed verdict must abstain, got %q", ans.Verdict)
	}
}

// TestConcurrenceRunnerUnconfiguredLaneAbstains: a lane with no endpoint abstains
// with a config-pointing error; it can never silently count as agreement.
func TestConcurrenceRunnerUnconfiguredLaneAbstains(t *testing.T) {
	r := &ConcurrenceRunner{LookupLane: func(string) (ConcurrenceLane, bool) { return ConcurrenceLane{}, false }}
	ans, err := r.RunConcurrence(testProbe(), "ghost")
	if err == nil {
		t.Fatal("expected error on unconfigured lane")
	}
	if ans.Verdict != mem.ConcurrenceVerdictAbstain {
		t.Fatalf("unconfigured lane must abstain, got %q", ans.Verdict)
	}
}
