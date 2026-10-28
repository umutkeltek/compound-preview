package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/sourceobs"
)

var canonObsNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

func writeCanonObsFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func baselineCanonObsFamily(t *testing.T, home, family string, pairs []sourceobs.CanonPathNode, now time.Time) {
	t.Helper()
	manifest, err := sourceobs.BuildCanonManifest(family, pairs, now)
	if err != nil {
		t.Fatalf("build canon manifest: %v", err)
	}
	if _, err := sourceobs.RecordSourceManifest(home, manifest); err != nil {
		t.Fatalf("record canon manifest: %v", err)
	}
}

func canonObsManifestHash(t *testing.T, home, family, sourceRef string) string {
	t.Helper()
	m, err := sourceobs.LoadSourceManifest(home, family)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	for _, it := range m.Items {
		if it.SourceRef == sourceRef {
			return it.FileHash
		}
	}
	t.Fatalf("source ref %q not in manifest", sourceRef)
	return ""
}

func countFilesWithPrefix(t *testing.T, root, prefix string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), prefix) {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return count
}

// F1 (transaction) + F6 (idempotent re-park): a mint that fails AFTER drift is observed must NOT
// consume the baseline — the manifest still holds the OLD hash so the drift stays re-detectable, and
// no transition event is minted. Re-observing the same drift must not spray duplicate candidates.
// RED today: ObserveCanonDrift rebaselines the manifest before the mint is attempted.
func TestCanonObserveFailedMintDoesNotConsumeBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	srcDir := t.TempDir()
	fA := filepath.Join(srcDir, "settings.json")
	writeCanonObsFile(t, fA, `{"v":"A"}`)
	baselineCanonObsFamily(t, home, "harness-canon", []sourceobs.CanonPathNode{{Path: fA, NodeRef: "harness:example"}}, canonObsNow)
	baselineHash := canonObsManifestHash(t, home, "harness-canon", fA)

	// Drift A->B.
	writeCanonObsFile(t, fA, `{"v":"B"}`)

	// Force the mint to fail deterministically: make <home>/events a regular file so
	// recordCanonTransition's MkdirAll(<home>/events) returns ENOTDIR (chmod-independent).
	eventsPath := filepath.Join(home, "events")
	if err := os.RemoveAll(eventsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runSourceCanonObserve([]string{
		"--family", "harness-canon",
		"--cause-class", "accept",
		"--caused-by", "work_canon_test",
		"--next-consumer", "capgraph",
	}, &stdout, &stderr, canonObsNow.Add(time.Hour))
	if code == 0 {
		t.Fatalf("forced mint failure must exit non-zero; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}

	// Transaction safety: the baseline was NOT committed — manifest still holds the pre-drift hash.
	if got := canonObsManifestHash(t, home, "harness-canon", fA); got != baselineHash {
		t.Fatalf("failed mint consumed the drift: manifest hash advanced from %s to %s", baselineHash, got)
	}
	candidatesAfterFail := countFilesWithPrefix(t, home, "src_cand_")

	// Restore events so a clean rerun can proceed, then re-observe in observation-only mode: the drift
	// must still be detected (proving it was never consumed) and re-parking must not duplicate candidates.
	if err := os.Remove(eventsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(eventsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// No transition event survived the failed mint.
	if n := len(testEventEntries(t, home, "tr_canon_")); n != 0 {
		t.Fatalf("failed mint left %d transition event(s)", n)
	}
	var rstdout, rstderr bytes.Buffer
	code = runSourceCanonObserve([]string{"--family", "harness-canon"}, &rstdout, &rstderr, canonObsNow.Add(2*time.Hour))
	if code != 0 {
		t.Fatalf("observation-only rerun failed: code=%d stderr=%s", code, rstderr.String())
	}
	if !strings.Contains(rstdout.String(), "- changed: 1") {
		t.Fatalf("rerun should still detect the drift; got:\n%s", rstdout.String())
	}
	if got := countFilesWithPrefix(t, home, "src_cand_"); got != candidatesAfterFail {
		t.Fatalf("re-park duplicated candidates: %d -> %d", candidatesAfterFail, got)
	}
}

// F3 (flag coupling): any single cause flag without its partners is a usage error (exit 2), raised
// before any home mutation. RED today: coupling is unvalidated, so these fall through to the runtime.
func TestCanonObserveIncompleteCauseFlagsExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"cause-class alone", []string{"--family", "harness-canon", "--cause-class", "accept"}},
		{"caused-by alone", []string{"--family", "harness-canon", "--caused-by", "work_x"}},
		{"next-consumer alone", []string{"--family", "harness-canon", "--next-consumer", "capgraph"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("COMPOUND_HOME", home)
			var stdout, stderr bytes.Buffer
			code := runSourceCanonObserve(tc.args, &stdout, &stderr, canonObsNow)
			if code != 2 {
				t.Fatalf("incomplete cause flags must exit 2, got %d; stderr=%s", code, stderr.String())
			}
			// Exit happens before the home is resolved or touched: nothing is written under it.
			if entries, _ := os.ReadDir(home); len(entries) > 0 {
				t.Fatalf("coupling violation mutated the home before validation: %v", entries)
			}
		})
	}
}

// F4 (honest output): a driftful observation-only run (no cause flags) prints the not-minted line in
// non-JSON and carries transition_minted:false + not_minted_reason in JSON. RED today: neither exists.
func TestCanonObserveObservationOnlyDriftIsHonest(t *testing.T) {
	const notMinted = "- transition: not minted (observation-only: no --cause-class/--caused-by)"

	seedDrift := func(t *testing.T) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("COMPOUND_HOME", home)
		srcDir := t.TempDir()
		fA := filepath.Join(srcDir, "settings.json")
		writeCanonObsFile(t, fA, `{"v":"A"}`)
		baselineCanonObsFamily(t, home, "harness-canon", []sourceobs.CanonPathNode{{Path: fA, NodeRef: "harness:example"}}, canonObsNow)
		writeCanonObsFile(t, fA, `{"v":"B"}`)
		return home
	}

	t.Run("non-json", func(t *testing.T) {
		seedDrift(t)
		var stdout, stderr bytes.Buffer
		code := runSourceCanonObserve([]string{"--family", "harness-canon"}, &stdout, &stderr, canonObsNow.Add(time.Hour))
		if code != 0 {
			t.Fatalf("observation-only run failed: code=%d stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), notMinted) {
			t.Fatalf("driftful observation-only run must print the honest not-minted line; got:\n%s", stdout.String())
		}
	})

	t.Run("json", func(t *testing.T) {
		seedDrift(t)
		var stdout, stderr bytes.Buffer
		code := runSourceCanonObserve([]string{"--family", "harness-canon", "--json"}, &stdout, &stderr, canonObsNow.Add(time.Hour))
		if code != 0 {
			t.Fatalf("observation-only json run failed: code=%d stderr=%s", code, stderr.String())
		}
		var rep struct {
			TransitionMinted bool   `json:"transition_minted"`
			NotMintedReason  string `json:"not_minted_reason"`
			Drift            struct {
				ChangedRefs []string `json:"changed_refs"`
			} `json:"drift"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
			t.Fatalf("parse json: %v\n%s", err, stdout.String())
		}
		if len(rep.Drift.ChangedRefs) != 1 {
			t.Fatalf("expected one changed ref, got %v", rep.Drift.ChangedRefs)
		}
		if rep.TransitionMinted {
			t.Fatalf("observation-only run must report transition_minted:false")
		}
		if rep.NotMintedReason != "observation-only: no --cause-class/--caused-by" {
			t.Fatalf("not_minted_reason = %q, want the observation-only reason", rep.NotMintedReason)
		}
	})
}

// F5 (happy-path regression): a full-cause-flag driftful run mints exactly one transition and commits
// the baseline; a rerun with no further change mints nothing. Green throughout — a guard, not a fix.
func TestCanonObserveHappyPathMintsCommitsThenRerunClean(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	srcDir := t.TempDir()
	fA := filepath.Join(srcDir, "settings.json")
	writeCanonObsFile(t, fA, `{"v":"A"}`)
	baselineCanonObsFamily(t, home, "harness-canon", []sourceobs.CanonPathNode{{Path: fA, NodeRef: "harness:example"}}, canonObsNow)
	baselineHash := canonObsManifestHash(t, home, "harness-canon", fA)
	writeCanonObsFile(t, fA, `{"v":"B"}`)

	flags := []string{"--family", "harness-canon", "--cause-class", "repair", "--caused-by", "work_canon_test", "--next-consumer", "capgraph"}
	var stdout, stderr bytes.Buffer
	code := runSourceCanonObserve(flags, &stdout, &stderr, canonObsNow.Add(time.Hour))
	if code != 0 {
		t.Fatalf("happy path failed: code=%d stderr=%s", code, stderr.String())
	}
	if n := len(testEventEntries(t, home, "tr_canon_")); n != 1 {
		t.Fatalf("happy path must mint exactly one transition, got %d", n)
	}
	if got := canonObsManifestHash(t, home, "harness-canon", fA); got == baselineHash {
		t.Fatalf("successful mint must commit the baseline; hash still %s", baselineHash)
	}

	var rstdout, rstderr bytes.Buffer
	code = runSourceCanonObserve(flags, &rstdout, &rstderr, canonObsNow.Add(2*time.Hour))
	if code != 0 {
		t.Fatalf("clean rerun failed: code=%d stderr=%s", code, rstderr.String())
	}
	if n := len(testEventEntries(t, home, "tr_canon_")); n != 1 {
		t.Fatalf("rerun with no change must mint nothing new, got %d transition event(s)", n)
	}
}

func TestCanonObserveInstructionSchemaDriftHoldsWithoutAdvancingBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPOUND_HOME", home)
	sourcePath := filepath.Join(t.TempDir(), "AGENTS.md")
	writeCanonObsFile(t, sourcePath, "# Contract\n\nRoutine guidance.\n")
	baselineCanonObsFamily(
		t,
		home,
		"instruction-canon",
		[]sourceobs.CanonPathNode{{Path: sourcePath, NodeRef: "harness:codex"}},
		canonObsNow,
	)
	baselineHash := canonObsManifestHash(t, home, "instruction-canon", sourcePath)
	writeCanonObsFile(t, sourcePath, "# Contract\n\n## New authority surface\n\nRoutine guidance.\n")

	var stdout, stderr bytes.Buffer
	code := runSourceCanonObserve(
		[]string{"--family", "instruction-canon", "--json"},
		&stdout,
		&stderr,
		canonObsNow.Add(time.Hour),
	)
	if code != 1 {
		t.Fatalf("instruction schema drift exit = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "instruction supply-chain admission hold") {
		t.Fatalf("missing admission hold diagnostic: %s", stderr.String())
	}
	if got := canonObsManifestHash(t, home, "instruction-canon", sourcePath); got != baselineHash {
		t.Fatalf("held instruction schema drift advanced baseline: got %s want %s", got, baselineHash)
	}
}
