package executorlens

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/behaviorallens"
	"compound/internal/envelope"
)

// fixedNow keeps signal ids/timestamps deterministic and never compares wall-clock against a fixture
// date (AGENTS.md: no time bombs).
var fixedNow = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

// writeReceipt writes a faithful receipt envelope to home/events/<id>.json. id must be rec_-prefixed
// so the collector's prefix filter reads it. All fixtures are synthetic — generic lane ids, no
// operator-personal data (public-repo privacy law).
func writeReceipt(t *testing.T, home, id, laneID, producerName, workID, outcome, summary, createdAt string) {
	t.Helper()
	rec := &envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            id,
			CreatedAt:     createdAt,
			Producer:      envelope.Producer{Kind: "executor", Name: producerName, LaneID: laneID},
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
	writeEnvelopeFile(t, home, id, rec)
}

func writeEnvelopeFile(t *testing.T, home, id string, e envelope.Envelope) {
	t.Helper()
	dir := filepath.Join(home, "events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir events: %v", err)
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatalf("write envelope file: %v", err)
	}
}

// enableAgreementLens writes the per-home config that opts the operator_private agreement-asymmetry
// lens in. Absent this file a home is fail-closed (stranger clone) by construction.
func enableAgreementLens(t *testing.T, home string) {
	t.Helper()
	cfg := behaviorallens.Config{
		SchemaVersion:        behaviorallens.ConfigSchemaVersion,
		EnabledPrivateLenses: []string{behaviorallens.AgreementAsymmetryLensID},
	}
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "behavioral-lenses.json"), b, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// FALSIFIER 1: synthetic executor receipts -> collector -> Observations -> Compose yields a hedging
// public_safe signal for an executor subject, and a hedgy output scores the hedging NEGATIVE pole.
func TestFalsifier1_HedgyExecutorOutputScoresHedgingNegative(t *testing.T) {
	home := t.TempDir()
	// A hedgy Codex executor output: uncertainty markers dominate.
	writeReceipt(t, home, "rec_hedgy01", "codex-native", "codex", "work_alpha", "completed",
		"I think this might possibly work, but I'm not sure; it could perhaps be maybe broken.",
		"2026-07-14T10:00:00Z")

	report, err := Run(home, true, fixedNow)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var hedging *behaviorallens.InferredSignal
	for i := range report.PublicSignals {
		s := report.PublicSignals[i]
		if s.LensID == behaviorallens.HedgingLensID && s.Subject == "codex-native" {
			hedging = &report.PublicSignals[i]
		}
	}
	if hedging == nil {
		t.Fatalf("expected a hedging public_safe signal for subject codex-native; got %+v", report.PublicSignals)
	}
	if hedging.Visibility != behaviorallens.VisibilityPublicSafe {
		t.Fatalf("hedging signal must be public_safe, got %q", hedging.Visibility)
	}
	if hedging.Value >= 0 {
		t.Fatalf("hedgy output must score the hedging NEGATIVE pole (value<0); got %.3f", hedging.Value)
	}
	if len(report.Subjects) != 1 || report.Subjects[0] != "codex-native" {
		t.Fatalf("expected exactly the codex-native executor subject; got %v", report.Subjects)
	}
}

// FALSIFIER 2 — THE BOUNDARY. With the home Config enabling agreement-asymmetry, real-shaped
// synthetic receipts produce an operator_private signal that IS recorded under COMPOUND_HOME but is
// DROPPED from the outward public projection; a stranger/no-config home produces NO operator_private
// signal at all (Compose skips it). The scratch-mutation teeth (force the private lens's value into
// the public projection) is proven out of band and documented in the handoff.
func TestFalsifier2_OperatorPrivateBoundary(t *testing.T) {
	// Two executor lanes judge the SAME works with divergent verdicts: codex always reports the
	// outcome the operator wants (completed), claude reports failed/blocked. That divergence gives the
	// agreement-asymmetry lens teeth (operator-agreement rate above peer-agreement rate).
	setup := func(t *testing.T, withConfig bool) (string, RunReport) {
		home := t.TempDir()
		if withConfig {
			enableAgreementLens(t, home)
		}
		writeReceipt(t, home, "rec_cx_a", "codex-native", "codex", "work_a", "completed", "Landed it cleanly.", "2026-07-14T10:00:00Z")
		writeReceipt(t, home, "rec_cx_b", "codex-native", "codex", "work_b", "completed", "All green, shipped.", "2026-07-14T10:05:00Z")
		writeReceipt(t, home, "rec_cl_a", "fable-opus-loc", "claude", "work_a", "failed", "It might be broken; I'm not sure.", "2026-07-14T10:10:00Z")
		writeReceipt(t, home, "rec_cl_b", "fable-opus-loc", "claude", "work_b", "blocked", "Blocked; possibly a missing gate.", "2026-07-14T10:15:00Z")
		report, err := Run(home, true, fixedNow)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return home, report
	}

	// --- config-enabled home: private signal recorded, dropped from public projection ---
	t.Run("config_enabled", func(t *testing.T) {
		home, report := setup(t, true)
		if !report.AgreementEnabled {
			t.Fatalf("agreement-asymmetry should be enabled in this home config")
		}
		// The outward projection must carry NO operator_private signal.
		for _, s := range report.PublicSignals {
			if s.Visibility == behaviorallens.VisibilityOperatorPrivate {
				t.Fatalf("LEAK: operator_private signal %q escaped into the public projection", s.ID)
			}
		}
		// But the full home-local ledger MUST contain the operator_private agreement-asymmetry signal
		// (it was produced and recorded — the boundary is what LEAVES, not what is written home-local).
		all, err := behaviorallens.LoadSignals(home)
		if err != nil {
			t.Fatalf("LoadSignals: %v", err)
		}
		var privateFound bool
		for _, s := range all {
			if s.LensID == behaviorallens.AgreementAsymmetryLensID && s.Visibility == behaviorallens.VisibilityOperatorPrivate {
				privateFound = true
			}
		}
		if !privateFound {
			t.Fatalf("expected an operator_private agreement-asymmetry signal recorded under COMPOUND_HOME; ledger=%d rows", len(all))
		}
		// PublicSignals (the read-and-project front door) must also drop it.
		pub, err := behaviorallens.PublicSignals(home)
		if err != nil {
			t.Fatalf("PublicSignals: %v", err)
		}
		for _, s := range pub {
			if s.Visibility == behaviorallens.VisibilityOperatorPrivate {
				t.Fatalf("LEAK: PublicSignals returned an operator_private row %q", s.ID)
			}
		}
	})

	// --- stranger/no-config home: NO operator_private signal produced at all ---
	t.Run("stranger_no_config", func(t *testing.T) {
		home, report := setup(t, false)
		if report.AgreementEnabled {
			t.Fatalf("a stranger clone must NOT have the agreement lens enabled")
		}
		all, err := behaviorallens.LoadSignals(home)
		if err != nil {
			t.Fatalf("LoadSignals: %v", err)
		}
		for _, s := range all {
			if s.Visibility == behaviorallens.VisibilityOperatorPrivate {
				t.Fatalf("fail-closed violated: a stranger clone recorded an operator_private signal %q", s.ID)
			}
		}
		// It still produces public_safe hedging signals — the public half always runs.
		if len(report.PublicSignals) == 0 {
			t.Fatalf("stranger clone should still produce public_safe hedging signals")
		}
		// And the agreement lens must have been SKIPPED (visible in the skip notes), not silently absent.
		var skippedPrivate bool
		for _, sk := range report.Skipped {
			if sk.LensID == behaviorallens.AgreementAsymmetryLensID {
				skippedPrivate = true
			}
		}
		if !skippedPrivate {
			t.Fatalf("expected the agreement-asymmetry lens to be recorded as skipped for a stranger clone; skipped=%+v", report.Skipped)
		}
	})
}

// FALSIFIER 3: no provider/network import anywhere in the collector's non-test source. A provider
// call added to any tested path would trip this guard.
func TestFalsifier3_NoNetworkImports(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	forbidden := []string{"net", "net/http", "net/url", "crypto/tls"}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if path == bad {
					t.Fatalf("%s imports forbidden network package %q — the collector must make NO provider/network call", name, path)
				}
			}
		}
	}
}

// classifyExecutor: executor lanes are recognized; control-plane producers are not.
func TestClassifyExecutor(t *testing.T) {
	cases := []struct {
		name       string
		candidates []string
		wantSubj   string
		wantFam    string
		wantOK     bool
	}{
		{"codex lane", []string{"codex-native", "codex"}, "codex-native", "codex", true},
		{"claude fable lane", []string{"fable-opus-loc", "claude"}, "fable-opus-loc", "claude-code", true},
		{"actor fallback when lane generic", []string{"worktree-agent-xyz", "codex"}, "codex", "codex", true},
		{"control plane producer skipped", []string{"", "os-event"}, "", "", false},
		{"compound producer skipped", []string{"compound"}, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subj, fam, ok := classifyExecutor(tc.candidates...)
			if ok != tc.wantOK || subj != tc.wantSubj || fam != tc.wantFam {
				t.Fatalf("classifyExecutor(%v) = (%q,%q,%v); want (%q,%q,%v)",
					tc.candidates, subj, fam, ok, tc.wantSubj, tc.wantFam, tc.wantOK)
			}
		})
	}
}

// A non-executor receipt (control-plane producer) yields no observations.
func TestNonExecutorReceiptIgnored(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, "rec_sys01", "", "os-event", "work_x", "completed",
		"definitely done, absolutely certain.", "2026-07-14T10:00:00Z")
	col, err := CollectObservations(home)
	if err != nil {
		t.Fatalf("CollectObservations: %v", err)
	}
	if len(col.Subjects) != 0 || len(col.Hedging) != 0 || len(col.Agreement) != 0 {
		t.Fatalf("control-plane receipt must yield no executor observations; got %+v", col)
	}
}

func TestExecutorObservationsUseLogicalEventReferences(t *testing.T) {
	home := t.TempDir()
	writeReceipt(t, home, "rec_logical01", "codex-native", "codex", "work_x", "completed",
		"Landed the bounded change.", "2026-07-14T10:00:00Z")
	col, err := CollectObservations(home)
	if err != nil {
		t.Fatalf("CollectObservations: %v", err)
	}
	if len(col.Hedging) != 1 {
		t.Fatalf("hedging observations = %d, want 1", len(col.Hedging))
	}
	refs := col.Hedging[0].EvidenceRefs
	if len(refs) != 1 || refs[0] != "compound://events/rec_logical01" {
		t.Fatalf("evidence refs = %v, want canonical logical event ref", refs)
	}
}

// A missing events directory is a clean empty collection (fresh home).
func TestMissingEventsDirIsClean(t *testing.T) {
	home := t.TempDir()
	col, err := CollectObservations(home)
	if err != nil {
		t.Fatalf("CollectObservations on empty home: %v", err)
	}
	if len(col.Subjects) != 0 || len(col.Hedging) != 0 {
		t.Fatalf("empty home should yield an empty collection; got %+v", col)
	}
}
