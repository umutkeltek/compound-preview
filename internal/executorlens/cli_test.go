package executorlens

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/behaviorallens"
	"compound/internal/envelope"
)

func runExecCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runCLI(args, &out, &errb)
	return out.String(), errb.String(), code
}

// writeExecReceipt writes a synthetic executor receipt to home/events/<id>.json.
func writeExecReceipt(t *testing.T, home, id, laneID, name, workID, outcome, summary string) {
	t.Helper()
	rec := &envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            id,
			CreatedAt:     "2026-07-14T10:00:00Z",
			Producer:      envelope.Producer{Kind: "executor", Name: name, LaneID: laneID},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    outcome,
		MoveType:   "outward_delta",
		StateDelta: envelope.StateDelta{Changed: true, Summary: summary},
	}
	dir := filepath.Join(home, "events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write receipt: %v", err)
	}
}

// The CLI run verb over a home with an enabled agreement lens: the JSON report carries composed
// signals, records them, and its outward public projection carries NO operator_private row.
func TestExecutorLensCLIRunPublicProjectionOnly(t *testing.T) {
	home := t.TempDir()
	// enable the operator_private agreement lens for this home
	cfg := behaviorallens.Config{
		SchemaVersion:        behaviorallens.ConfigSchemaVersion,
		EnabledPrivateLenses: []string{behaviorallens.AgreementAsymmetryLensID},
	}
	cfgDir := filepath.Join(home, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	cb, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(cfgDir, "behavioral-lenses.json"), cb, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	writeExecReceipt(t, home, "rec_cx", "codex-native", "codex", "work_a", "completed", "Landed cleanly.")
	writeExecReceipt(t, home, "rec_cl", "fable-opus-loc", "claude", "work_a", "failed", "It might be broken; not sure.")

	out, errStr, code := runExecCLI(t, "run", "--home", home, "--record", "--json")
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, errStr)
	}
	var report RunReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode run report: %v (%s)", err, out)
	}
	if !report.AgreementEnabled {
		t.Fatalf("agreement lens should be enabled in this home")
	}
	if report.Composed == 0 || report.Recorded == 0 {
		t.Fatalf("expected composed+recorded signals, got composed=%d recorded=%d", report.Composed, report.Recorded)
	}
	for _, s := range report.PublicSignals {
		if s.Visibility == behaviorallens.VisibilityOperatorPrivate {
			t.Fatalf("LEAK: CLI emitted an operator_private signal %q", s.ID)
		}
	}
	// The home-local ledger must still contain the operator_private agreement signal.
	all, err := behaviorallens.LoadSignals(home)
	if err != nil {
		t.Fatalf("LoadSignals: %v", err)
	}
	var private bool
	for _, s := range all {
		if s.Visibility == behaviorallens.VisibilityOperatorPrivate {
			private = true
		}
	}
	if !private {
		t.Fatalf("expected an operator_private signal recorded home-local; ledger=%d rows", len(all))
	}
}

// subjects verb lists the executor lanes and observation counts without writing anything.
func TestExecutorLensCLISubjects(t *testing.T) {
	home := t.TempDir()
	writeExecReceipt(t, home, "rec_cx", "codex-native", "codex", "work_a", "completed", "I think this might work.")

	out, errStr, code := runExecCLI(t, "subjects", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("subjects exit=%d stderr=%s", code, errStr)
	}
	var col Collection
	if err := json.Unmarshal([]byte(out), &col); err != nil {
		t.Fatalf("decode collection: %v (%s)", err, out)
	}
	if len(col.Subjects) != 1 || col.Subjects[0] != "codex-native" {
		t.Fatalf("expected codex-native subject, got %v", col.Subjects)
	}
	if len(col.Hedging) != 1 {
		t.Fatalf("expected 1 hedging observation, got %d", len(col.Hedging))
	}
	// subjects must not record anything
	all, _ := behaviorallens.LoadSignals(home)
	if len(all) != 0 {
		t.Fatalf("subjects must not record signals, ledger has %d rows", len(all))
	}
}

func TestExecutorLensCLIUsage(t *testing.T) {
	_, _, code := runExecCLI(t)
	if code != 2 {
		t.Fatalf("no args should print usage and exit 2, got %d", code)
	}
	_, errStr, code := runExecCLI(t, "bogus")
	if code != 2 || !strings.Contains(errStr, "usage") {
		t.Fatalf("unknown subcommand should print usage/exit 2, got code=%d err=%s", code, errStr)
	}
}
