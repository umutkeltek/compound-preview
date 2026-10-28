package work

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/localmodel"
)

func TestExecutorsProjectionTreatsHookStreamStatErrorAsAbsent(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatal(err)
	}
	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(homePath, "events")
	if err := os.RemoveAll(eventsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bench := WorkbenchReport{}
	if _, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{Workbench: &bench}); err != nil {
		t.Fatalf("legacy hook-stream existence probe should ignore stat error: %v", err)
	}
}

func TestExecutorsProjectionProbeLocalLMStudio(t *testing.T) {
	t.Parallel()
	loaded := 4096
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v0/models" {
			t.Fatalf("path = %q, want /api/v0/models", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"id":                    "fixture-model",
				"state":                 "loaded",
				"max_context_length":    8192,
				"loaded_context_length": loaded,
			}},
		})
	}))
	defer server.Close()

	homePath := t.TempDir()
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	projection, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{
		ProbeLocal: true,
		LMStudioClient: localmodel.LMStudioClient{
			BaseURL:    server.URL + "/v1",
			HTTPClient: server.Client(),
		},
	})
	if err != nil {
		t.Fatalf("executors projection: %v", err)
	}
	row := requireExecutorProjectionRow(t, projection, "local_lm_studio")
	if row.Status != "reachable" || row.StatusSource != "live_probe" || row.Tone != "green" {
		t.Fatalf("local LM Studio row = %+v, want reachable live probe", row)
	}
	if !strings.Contains(row.ProjectionNote, "1 model") {
		t.Fatalf("projection note = %q, want model count", row.ProjectionNote)
	}
}

func TestExecutorsProjectionDefaultDoesNotProbeLocalLMStudio(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	if err := home.Init(homePath, "test", now); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	projection, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{
		LMStudioClient: localmodel.LMStudioClient{BaseURL: "http://127.0.0.1:1/v1"},
	})
	if err != nil {
		t.Fatalf("executors projection: %v", err)
	}
	row := requireExecutorProjectionRow(t, projection, "local_lm_studio")
	if row.Status != "not_checked" || row.StatusSource != "profile_guide" {
		t.Fatalf("local LM Studio row = %+v, want static profile without probing", row)
	}
}

// A terminal-work lease's WorkID must survive onto the row as
// StaleLeaseWorkIDs (additive field) so the lease-hygiene attention group can
// name exactly which stale leases it will sweep; a live-work lease must never
// leak into that list.
func TestExecutorsProjectionReusesCallerWorkbench(t *testing.T) {
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)

	// Parity: a caller-built RecentLimit-8 report must yield the same
	// projection as the internal build it replaces.
	bench, err := store.Workbench(now, WorkbenchOptions{RecentLimit: 8})
	if err != nil {
		t.Fatal(err)
	}
	internal, err := store.ExecutorsProjection(now)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{Workbench: &bench})
	if err != nil {
		t.Fatal(err)
	}
	internalJSON, err := json.Marshal(internal)
	if err != nil {
		t.Fatal(err)
	}
	reusedJSON, err := json.Marshal(reused)
	if err != nil {
		t.Fatal(err)
	}
	if string(internalJSON) != string(reusedJSON) {
		t.Fatalf("projection from reused workbench diverged from internal build:\n%s\nvs\n%s", reusedJSON, internalJSON)
	}

	// Depth: the passed report must actually be USED, not silently rebuilt.
	// A sentinel integrity note in the caller's report must surface in the
	// projection; if the engine reverts to always rebuilding, this fails.
	sentinel := "sentinel-integrity-from-caller-workbench"
	bench.Integrity = append(bench.Integrity, sentinel)
	proj, err := store.ExecutorsProjection(now, ExecutorsProjectionOptions{Workbench: &bench})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, note := range proj.Integrity {
		if note == sentinel {
			found = true
		}
	}
	if !found {
		t.Fatalf("caller workbench was ignored: sentinel integrity note missing from projection integrity %v", proj.Integrity)
	}
}

func TestExecutorsProjectionRecordsStaleLeaseWorkIDs(t *testing.T) {
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	store := newExecutorTestStore(t, now)

	terminalWork, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "terminal work with residual lease"}, now)
	if err != nil {
		t.Fatal(err)
	}
	closeWorkbenchTestWork(t, store, terminalWork.ID, now, "terminal work closed")
	// Since I1 close releases its own lease; the projection's stale-lease row is
	// the backstop for pre-I1 / crash-window residue, written here directly.
	writeResidualLeaseForTerminalWork(t, store, terminalWork.ID, "codex", now)

	liveWork, err := store.Create(CreateInput{Owner: "codex", SuccessPredicate: "live claimed work"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(liveWork.ID, "codex", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	proj, err := store.ExecutorsProjection(now)
	if err != nil {
		t.Fatal(err)
	}
	codex := requireExecutorProjectionRow(t, proj, "codex")
	if len(codex.StaleLeaseWorkIDs) != 1 || codex.StaleLeaseWorkIDs[0] != terminalWork.ID {
		t.Fatalf("stale_lease_work_ids = %v, want exactly [%q]", codex.StaleLeaseWorkIDs, terminalWork.ID)
	}
	for _, id := range codex.StaleLeaseWorkIDs {
		if id == liveWork.ID {
			t.Fatalf("live-work lease id leaked into stale_lease_work_ids: %v", codex.StaleLeaseWorkIDs)
		}
	}
}

func requireExecutorProjectionRow(t *testing.T, projection ExecutorsProjection, id string) ExecutorRow {
	t.Helper()
	for _, row := range projection.Executors {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("missing executor row %q in %+v", id, projection.Executors)
	return ExecutorRow{}
}
