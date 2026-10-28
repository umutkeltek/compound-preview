package work

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The regression this pins is a real one: nine delegated sweeps ran on
// 2026-07-26, all nine stamped their workspace, and all nine were reported as
// having no transcript because the harvester searched the wrong homes. Given the
// lane's profile home, the workspace alone locates every one of them.
func TestTranscriptDirReproducesTheHarnessSlugExactly(t *testing.T) {
	t.Parallel()
	root := "/profiles/_providers/example-lane"
	// Shaped like a real workspace from that wave: a scratch root holding a
	// directory whose own name is an encoded absolute path, so it already starts
	// with '-'. The harness encodes '/' as '-' without collapsing anything, so
	// that segment yields a DOUBLED dash. Normalizing it finds nothing, which is
	// the failure mode this test exists to prevent.
	ws := "/scratch/run-501/-encoded-abs-path/sess/scratchpad/wt-collide-work"
	want := filepath.Join(
		root, "projects",
		"-scratch-run-501--encoded-abs-path-sess-scratchpad-wt-collide-work",
	)
	got, err := TranscriptDirFor(root, ws)
	if err != nil {
		t.Fatalf("TranscriptDirFor: %v", err)
	}
	if got != want {
		t.Fatalf("slug mismatch\n got: %s\nwant: %s", got, want)
	}
}

// The harness's per-project directory name turns EVERY path separator AND every
// '.' into '-'. A workspace under ~/.compound/... therefore yields a doubled
// dash after the home directory: ".compound" becomes "-compound", and the '/'
// that preceded it is also '-'. The liveness signal for compound's own
// delegate lanes had been dead for the whole campaign because the slug builder
// preserved the literal dot, producing a directory that did not exist on disk
// while the real one sat next to it. This test pins the rule the harness
// actually applies, so reproducing it becomes a contract instead of a guess.
func TestTranscriptSlugTurnsDotsIntoDashesToo(t *testing.T) {
	t.Parallel()
	root := "/profiles/_providers/example-lane"
	ws := "/home/example/.compound/delegate-workspaces/foo"
	want := filepath.Join(
		root, "projects",
		"-home-example--compound-delegate-workspaces-foo",
	)
	got, err := TranscriptDirFor(root, ws)
	if err != nil {
		t.Fatalf("TranscriptDirFor: %v", err)
	}
	if got != want {
		t.Fatalf("slug mismatch\n got: %s\nwant: %s", got, want)
	}
}

// An unknown root must be an error, never a guess. If this ever degrades into
// scanning candidate homes, a miss reads as "the run produced nothing" and a
// completed run gets closed as cancelled -- which is precisely what happened.
func TestTranscriptDirRefusesToGuessAnUnknownRoot(t *testing.T) {
	t.Parallel()
	if _, err := TranscriptDirFor("", "/abs/workspace"); !errors.Is(err, ErrSessionStoreRootUnknown) {
		t.Fatalf("empty root must return ErrSessionStoreRootUnknown, got %v", err)
	}
	if _, err := TranscriptDirFor("/root", ""); err == nil {
		t.Fatal("a run attempt with no workspace must be an error, not an empty path")
	}
	// A relative workspace would resolve against the harvest-time cwd, not the
	// cwd the run used, so it would point somewhere plausible and wrong.
	if _, err := TranscriptDirFor("/root", "relative/path"); err == nil {
		t.Fatal("a relative workspace must be rejected, not silently resolved")
	}
}

// "directory absent" and "directory present but empty" are different findings.
// Conflating them is what turns a lookup bug into a false claim about the run.
func TestTranscriptsDistinguishesMissingDirFromEmptyDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ws := "/work/space"
	dir, files, exists, err := TranscriptsFor(root, ws)
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if exists || len(files) != 0 {
		t.Fatalf("absent dir: want exists=false files=0, got exists=%v files=%d", exists, len(files))
	}

	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	_, files, exists, err = TranscriptsFor(root, ws)
	if err != nil {
		t.Fatalf("empty dir should not error: %v", err)
	}
	if !exists || len(files) != 0 {
		t.Fatalf("empty dir: want exists=true files=0, got exists=%v files=%d", exists, len(files))
	}

	// Only .jsonl transcripts count; sidecar files the harness drops alongside
	// them are not transcripts and must not inflate the count.
	for name, isTranscript := range map[string]bool{
		"a.jsonl": true, "b.jsonl": true, "notes.md": false, "cache.bin": false,
	} {
		if writeErr := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); writeErr != nil {
			t.Fatalf("write %s: %v", name, writeErr)
		}
		_ = isTranscript
	}
	_, files, exists, err = TranscriptsFor(root, ws)
	if err != nil || !exists {
		t.Fatalf("populated dir: exists=%v err=%v", exists, err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 .jsonl transcripts, got %d: %v", len(files), files)
	}
}
