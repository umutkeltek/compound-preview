package work

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Locating a delegated run's transcript used to be a SEARCH across guessed
// harness homes, and the search lost. Six delegated sweeps were reported as
// having produced nothing and were closed as cancelled; their transcripts
// existed the whole time, under the lane's own profile home. The harvester had
// looked in the default harness home and in a path the lane doctor reported but
// nothing ever wrote to.
//
// Every fact needed to find them was already recorded. The spawn stamps the
// workspace onto the RunAttempt (see Store.Delegate -> SetWorkspace), and the
// harness derives its per-project transcript directory from that same workspace
// path. The only missing fact was WHICH profile home holds the projects tree,
// and the lane owner already publishes it: `sb profile plan <profile> --json`
// returns a `harness_settings` artifact whose parent directory is the root.
//
// So this is a lookup, not a search — and when the root is unknown it says so
// instead of guessing. A wrong guess here is worse than no answer: it reports
// "no transcript" for a run that produced one, which is exactly the failure
// that cost a full audit wave.

// DelegateSessionStoreResolverFunc answers "where does this lane's harness keep
// its session store" for a delegated binding. It returns the profile home root
// (the directory CONTAINING `projects/`), not the projects directory itself.
type DelegateSessionStoreResolverFunc func(harness, lane string) (root string, err error)

// DelegateSessionStoreResolver is installed by the CLI, which owns the seam to
// the lane profile owner. Injection keeps internal/work free of a process
// dependency and lets tests pin the derivation without a live lane. When nil,
// transcript resolution reports an unknown root rather than inventing one.
var DelegateSessionStoreResolver DelegateSessionStoreResolverFunc

// ErrSessionStoreRootUnknown is returned when no resolver can name the lane's
// profile home. Callers must surface it; they must not fall back to scanning
// candidate homes, because a miss then reads as "the run produced nothing".
var ErrSessionStoreRootUnknown = errors.New("session store root unknown for lane")

// transcriptSlug is the harness's own per-project directory name: the absolute
// workspace path with every separator AND every '.' replaced by '-'. A workspace
// that already contains '-' keeps it, which is why a nested path can yield a
// doubled dash -- that is the harness's encoding, not a bug to normalize away.
// The dot rule matters because every live workspace lives under a dot-directory
// (e.g. ~/.compound/...), and the harness encodes ".compound" as "-compound",
// producing a doubled dash after the parent directory. Reproducing it exactly
// is the whole point; a "tidier" slug finds nothing.
func transcriptSlug(workspace string) string {
	s := filepath.Clean(workspace)
	s = strings.ReplaceAll(s, string(filepath.Separator), "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

// TranscriptDirFor resolves the directory holding a delegated run's transcripts
// from the profile home root and the workspace stamped on its RunAttempt.
func TranscriptDirFor(sessionStoreRoot, workspace string) (string, error) {
	root := strings.TrimSpace(sessionStoreRoot)
	ws := strings.TrimSpace(workspace)
	if root == "" {
		return "", ErrSessionStoreRootUnknown
	}
	if ws == "" {
		return "", errors.New("transcript dir: run attempt recorded no workspace")
	}
	if !filepath.IsAbs(ws) {
		// The slug encodes an absolute path. A relative workspace would resolve
		// against whatever cwd happens to be current at harvest time, which is
		// not the cwd the run used -- so it would silently point somewhere else.
		return "", fmt.Errorf("transcript dir: workspace %q is not absolute", ws)
	}
	return filepath.Join(root, "projects", transcriptSlug(ws)), nil
}

// TranscriptsFor lists the transcript files for a delegated run, newest last.
// An existing directory with no transcripts and a missing directory are
// different answers: the first means the harness ran and wrote nothing, the
// second means we are looking in the wrong place. Both return an empty slice,
// so callers get `exists` to tell them apart.
func TranscriptsFor(sessionStoreRoot, workspace string) (dir string, files []string, exists bool, err error) {
	dir, err = TranscriptDirFor(sessionStoreRoot, workspace)
	if err != nil {
		return "", nil, false, err
	}
	info, statErr := os.Stat(dir)
	if statErr != nil || !info.IsDir() {
		return dir, nil, false, nil
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return dir, nil, true, fmt.Errorf("transcript dir: read %s: %w", dir, readErr)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)
	return dir, files, true, nil
}
