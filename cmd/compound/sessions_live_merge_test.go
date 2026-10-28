package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// fakeMultiplexerSource is a deterministic MultiplexerSource for unit
// tests that exercise the merge logic without touching a real tmux
// server. Each List() call returns the configured observations
// verbatim; each ResumeArgv returns a fixed attach argv.
type fakeMultiplexerSource struct {
	observations []work.MultiplexerObservation
	listErr      error
	resumeArgv   []string
}

func (f *fakeMultiplexerSource) List() ([]work.MultiplexerObservation, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]work.MultiplexerObservation, len(f.observations))
	copy(out, f.observations)
	return out, nil
}

func (f *fakeMultiplexerSource) ResumeArgv(name string) []string {
	return append([]string{}, f.resumeArgv...)
}

// fakeRunner is a minimal SessionRunner for the merge unit test. The
// merge logic only ever calls Has on it (via the default runner path)
// — anything that would probe tmux is short-circuited by the empty
// runattempt list (no attempts → no Has calls).
type fakeRunner struct {
	has map[string]bool
}

func (f *fakeRunner) Available() error              { return nil }
func (f *fakeRunner) Has(name string) (bool, error) { return f.has[name], nil }
func (f *fakeRunner) SessionStatus(name string) (work.SessionStatus, error) {
	return work.SessionStatus{}, nil
}
func (f *fakeRunner) Create(name, dir, command string, args []string) error { return nil }
func (f *fakeRunner) SendKeys(name, text string, enter bool) error          { return nil }
func (f *fakeRunner) Kill(name string) error                                { return nil }
func (f *fakeRunner) AttachArgv(name string) []string                       { return []string{"tmux", "attach", "-t", name} }
func (f *fakeRunner) Multiplexer() work.MultiplexerSource                   { return nil }

// TestMergeMultiplexerIntoEmptyCompound proves the second observation
// source contributes rows even when compound has no recorded attempts.
// Without the multiplexer source, the report would be empty; the
// presence of multiplexer-only rows is the new surface.
func TestMergeMultiplexerIntoEmptyCompound(t *testing.T) {
	t.Parallel()
	homeDir := initEmptyCompoundHome(t)
	now := time.Now()
	mux := &fakeMultiplexerSource{
		observations: []work.MultiplexerObservation{
			{
				Name:         "operator-claude-1",
				CWD:          "/tmp/work/foo",
				LastActivity: now.Add(-2 * time.Minute),
			},
			{
				Name:         "operator-codex-2",
				CWD:          "/tmp/work/bar",
				LastActivity: now.Add(-30 * time.Second),
			},
		},
		resumeArgv: []string{"tmux", "attach-session", "-t", "N"},
	}
	var stdout, stderr strings.Builder
	code := runSessionsLiveWith([]string{"--json", "--home", homeDir}, &stdout, &stderr, now, &fakeRunner{}, mux)
	if code != 0 {
		t.Fatalf("sessions live: exit %d, stderr=%s", code, stderr.String())
	}
	var report work.SessionsLiveReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Multiplexer != 2 {
		t.Fatalf("multiplexer count = %d, want 2; rows=%+v", report.Multiplexer, report.Rows)
	}
	seen := map[string]bool{}
	for _, row := range report.Rows {
		if row.Provenance != "multiplexer" {
			t.Fatalf("row %+v has provenance=%q, want multiplexer", row, row.Provenance)
		}
		seen[row.Session] = true
		if !strings.HasPrefix(row.ResumeCmd, "tmux attach-session -t") {
			t.Fatalf("row %s resume cmd = %q, want tmux attach-session -t <name>", row.Session, row.ResumeCmd)
		}
	}
	if !seen["operator-claude-1"] || !seen["operator-codex-2"] {
		t.Fatalf("missing multiplexer session in rows: %+v", seen)
	}
}

// TestMergeMultiplexerFailureProducesLoudRow proves the multiplexer
// source's failure mode surfaces a visible row instead of silently
// truncating the report. The dangerous failure is 'nothing reported,
// must be fine' — this test guards against that exact regression.
func TestMergeMultiplexerFailureProducesLoudRow(t *testing.T) {
	t.Parallel()
	homeDir := initEmptyCompoundHome(t)
	now := time.Now()
	mux := &fakeMultiplexerSource{
		listErr: errMultiplexerSynth,
	}
	var stdout, stderr strings.Builder
	code := runSessionsLiveWith([]string{"--json", "--home", homeDir}, &stdout, &stderr, now, &fakeRunner{}, mux)
	if code != 0 {
		t.Fatalf("sessions live: exit %d, stderr=%s", code, stderr.String())
	}
	var report work.SessionsLiveReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Multiplexer != 1 {
		t.Fatalf("multiplexer count = %d, want 1 (the loud failure row)", report.Multiplexer)
	}
	found := false
	for _, row := range report.Rows {
		if row.State == "multiplexer_unknown" {
			found = true
			if !strings.Contains(row.Detail, "synth-failure") {
				t.Fatalf("failure row detail = %q, want contains 'synth-failure'", row.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("expected a multiplexer_unknown row, got %+v", report.Rows)
	}
}

var errMultiplexerSynth = &multiplexerSynthErr{}

type multiplexerSynthErr struct{}

func (e *multiplexerSynthErr) Error() string { return "synth-failure: tmux list-sessions crashed" }

// TestNoMultiplexerSourceLeavesReportUnchanged proves that passing a
// nil multiplexer source still produces the historical compound-only
// report (no rows from a hidden default). The default is only applied
// when the test seam has a *work.TmuxRunner; a fake runner cannot get
// a default multiplexer source, and the report MUST reflect that.
func TestNoMultiplexerSourceLeavesReportUnchanged(t *testing.T) {
	t.Parallel()
	homeDir := initEmptyCompoundHome(t)
	now := time.Now()
	var stdout, stderr strings.Builder
	code := runSessionsLiveWith([]string{"--json", "--home", homeDir}, &stdout, &stderr, now, &fakeRunner{}, nil)
	if code != 0 {
		t.Fatalf("sessions live: exit %d, stderr=%s", code, stderr.String())
	}
	var report work.SessionsLiveReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Multiplexer != 0 {
		t.Fatalf("multiplexer count = %d, want 0 (no multiplexer source provided)", report.Multiplexer)
	}
	for _, row := range report.Rows {
		if row.Provenance == "multiplexer" {
			t.Fatalf("unexpected multiplexer row %+v without a source", row)
		}
	}
}

// TestJsonSchemaBackwardCompatForExistingFields guards the JSON contract
// for fields that existed before this change. Existing consumers parse
// the report by field name; renaming or removing a field without a
// graceful migration breaks them. This test enumerates the documented
// keys and asserts they keep emitting. The new keys ('multiplexer' is
// always present; 'provenance' is per-row) are checked by seeding at
// least one compound row.
func TestJsonSchemaBackwardCompatForExistingFields(t *testing.T) {
	t.Parallel()
	homeDir := initEmptyCompoundHome(t)
	now := time.Now()
	raStore := runattempt.Open(homeDir)
	reservation, err := raStore.Mint("work_schema", "codex:test", "codex", now)
	if err != nil {
		t.Fatalf("runattempt.Mint: %v", err)
	}
	if _, err := raStore.SetOrigin(reservation.ID, runattempt.OriginClaimReservation); err != nil {
		t.Fatalf("runattempt.SetOrigin: %v", err)
	}
	var stdout, stderr strings.Builder
	code := runSessionsLiveWith([]string{"--json", "--home", homeDir}, &stdout, &stderr, now, &fakeRunner{}, nil)
	if code != 0 {
		t.Fatalf("sessions live: exit %d, stderr=%s", code, stderr.String())
	}
	required := []string{
		"schema_version", "generated_at", "live", "ghost", "unbound",
		"reservation", "unknown", "rows", "multiplexer",
	}
	for _, key := range required {
		if !strings.Contains(stdout.String(), `"`+key+`"`) {
			t.Fatalf("expected key %q in JSON output, got:\n%s", key, stdout.String())
		}
	}
	// New per-row key — emitted only when at least one row exists.
	if !strings.Contains(stdout.String(), `"provenance"`) {
		t.Fatalf("expected new per-row key 'provenance' in JSON output, got:\n%s", stdout.String())
	}
}

// TestRunattemptOriginReservationDoesNotInflateMultiplexerCount
// protects against a subtle bug where a `reservation` row from the
// compound side accidentally got counted as multiplexer. The
// multiplexer counter must reflect the second observation source
// only, never compound-recorded rows.
func TestRunattemptOriginReservationDoesNotInflateMultiplexerCount(t *testing.T) {
	t.Parallel()
	homeDir := initEmptyCompoundHome(t)
	now := time.Now()
	raStore := runattempt.Open(homeDir)
	reservation, err := raStore.Mint("work_test", "codex:test", "codex", now)
	if err != nil {
		t.Fatalf("runattempt.Mint: %v", err)
	}
	if _, err := raStore.SetOrigin(reservation.ID, runattempt.OriginClaimReservation); err != nil {
		t.Fatalf("runattempt.SetOrigin: %v", err)
	}
	var stdout, stderr strings.Builder
	code := runSessionsLiveWith([]string{"--json", "--home", homeDir}, &stdout, &stderr, now, &fakeRunner{}, nil)
	if code != 0 {
		t.Fatalf("sessions live: exit %d, stderr=%s", code, stderr.String())
	}
	var report work.SessionsLiveReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report.Multiplexer != 0 {
		t.Fatalf("multiplexer count = %d, want 0 (no multiplexer rows; only a reservation)", report.Multiplexer)
	}
	if report.Reservation != 1 {
		t.Fatalf("reservation count = %d, want 1", report.Reservation)
	}
	for _, row := range report.Rows {
		if row.Provenance != "compound" {
			t.Fatalf("reservation row has unexpected provenance: %+v", row)
		}
	}
	_ = home.Resolve // referenced for the import to survive go vet
}
