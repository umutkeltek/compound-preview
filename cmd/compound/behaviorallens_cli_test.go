package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/behaviorallens"
)

func writeObs(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write obs: %v", err)
	}
	return path
}

const sycophantObs = `{"subject":"lane:cli","payload":{"cases":[
  {"artifact_id":"a1","supportive_endorsement":0.9,"critical_endorsement":0.1},
  {"artifact_id":"a2","supportive_endorsement":0.85,"critical_endorsement":0.2}]}}`

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runBehavioralLens(args, &out, &errb)
	return out.String(), errb.String(), code
}

func TestBehavioralLensCLIRunRecordAndSignals(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	obs := writeObs(t, t.TempDir(), "obs.json", sycophantObs)

	out, errStr, code := runCLI(t, "run", "--lens", "sycophancy", "--observation", obs, "--home", home, "--record")
	if code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, errStr)
	}
	if !strings.Contains(out, "lens=sycophancy") || !strings.Contains(out, "recorded=") {
		t.Fatalf("run output missing recorded signal: %s", out)
	}

	// signals --json reads back exactly one recorded public_safe row.
	sjson, errStr, code := runCLI(t, "signals", "--home", home, "--json")
	if code != 0 {
		t.Fatalf("signals exit=%d stderr=%s", code, errStr)
	}
	var rows []behaviorallens.InferredSignal
	if err := json.Unmarshal([]byte(sjson), &rows); err != nil {
		t.Fatalf("decode signals json: %v (%s)", err, sjson)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 recorded signal, got %d", len(rows))
	}
	if rows[0].LensID != "sycophancy" || rows[0].Visibility != behaviorallens.VisibilityPublicSafe {
		t.Fatalf("unexpected signal: %+v", rows[0])
	}
	if rows[0].Value < 0.6 {
		t.Fatalf("sycophantic observation should score high, got %.3f", rows[0].Value)
	}
}

func TestBehavioralLensCLIComposeAndPublicProjection(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	obs := writeObs(t, t.TempDir(), "obs.json", sycophantObs)

	_, errStr, code := runCLI(t, "compose", "--observation", obs, "--home", home, "--record")
	if code != 0 {
		t.Fatalf("compose exit=%d stderr=%s", code, errStr)
	}

	// the shipped registry is public_safe-only, so the public projection equals the full ledger here.
	pjson, _, code := runCLI(t, "signals", "--home", home, "--public", "--json")
	if code != 0 {
		t.Fatalf("signals --public exit=%d", code)
	}
	var pub []behaviorallens.InferredSignal
	if err := json.Unmarshal([]byte(pjson), &pub); err != nil {
		t.Fatalf("decode public signals: %v", err)
	}
	if len(pub) != 1 || pub[0].Visibility != behaviorallens.VisibilityPublicSafe {
		t.Fatalf("public projection should carry the one public_safe signal, got %+v", pub)
	}
}

func TestBehavioralLensCLIRejectsUnknownLens(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	obs := writeObs(t, t.TempDir(), "obs.json", sycophantObs)
	_, errStr, code := runCLI(t, "run", "--lens", "does-not-exist", "--observation", obs, "--home", home)
	if code == 0 {
		t.Fatal("unknown lens must be a nonzero exit")
	}
	if !strings.Contains(errStr, "no registered lens") {
		t.Fatalf("expected clear error, got %s", errStr)
	}
}

func TestBehavioralLensCLIUsage(t *testing.T) {
	t.Parallel()
	_, _, code := runCLI(t)
	if code != 2 {
		t.Fatalf("no args should print usage and exit 2, got %d", code)
	}
}
