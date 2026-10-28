package gitrepository

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCaptureSnapshotUsesSevenFreshGitCommands(t *testing.T) {
	repo := initSnapshotRepo(t)
	var calls [][]string
	ctx := ContextWithCommandFactory(context.Background(), func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.CommandContext(ctx, name, args...)
	})
	snapshot, err := CaptureCleanSnapshotContext(ctx, repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 7 {
		t.Fatalf("fresh snapshot started %d Git commands, want 7: %v", len(calls), calls)
	}
	want := []string{"git", "-C", repo, "rev-parse", "--show-object-format", "--revs-only", "--end-of-options", "HEAD^{commit}", "HEAD^{commit}", "HEAD^{tree}"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("object read did not bind exact three revisions: %v", calls[0])
	}
	calls = nil
	if err := RevalidateSnapshotContext(ctx, repo, snapshot); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 7 {
		t.Fatalf("fresh revalidation started %d Git commands, want 7", len(calls))
	}
	calls = nil
	if _, err := ReadIdentityContext(ctx, repo, snapshot.BaseSHA); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatalf("standalone identity no longer reads fresh format/lineage/origin: %v", calls)
	}
}

func TestCaptureSnapshotRejectsInvalidBaseExpressions(t *testing.T) {
	repo := initSnapshotRepo(t)
	blob := snapshotGitOutput(t, repo, "rev-parse", "HEAD:seed.txt")
	snapshotRunGit(t, repo, "tag", "-a", "blob-tag", "-m", "blob tag", blob)
	for _, ref := range []string{"missing", "HEAD^{tree}", "blob-tag", "--all", "--help", "^HEAD", "HEAD..HEAD", "HEAD...HEAD", "HEAD\nHEAD"} {
		t.Run(ref, func(t *testing.T) {
			if got, err := CaptureCleanSnapshot(repo, ref); err == nil {
				t.Fatalf("invalid commit expression %q produced %+v", ref, got)
			}
		})
	}
}

func TestCaptureSnapshotRejectsMalformedObjectRead(t *testing.T) {
	repo := initSnapshotRepo(t)
	oid := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	for name, output := range map[string]string{
		"missing":        "sha1\n" + oid + "\n" + oid + "\n",
		"extra":          "sha1\n" + oid + "\n" + oid + "\n" + oid + "\n" + oid + "\n",
		"extra blank":    "sha1\n" + oid + "\n" + oid + "\n" + oid + "\n\n",
		"unknown format": "sha512\n" + oid + "\n" + oid + "\n" + oid + "\n",
		"wrong length":   "sha256\n" + oid + "\n" + oid + "\n" + oid + "\n",
		"invalid oid":    "sha1\n" + oid + "\n" + oid + "\n" + strings.Repeat("z", 40) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := ContextWithCommandFactory(context.Background(), func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if len(args) > 2 && args[2] == "rev-parse" {
					return exec.CommandContext(ctx, "printf", "%s", output)
				}
				return exec.CommandContext(ctx, name, args...)
			})
			if got, err := CaptureCleanSnapshotContext(ctx, repo, "HEAD"); err == nil {
				t.Fatalf("malformed object read produced %+v", got)
			}
		})
	}
}

func TestCaptureSnapshotAcceptsSingleCommitExpressions(t *testing.T) {
	repo := initSnapshotRepo(t)
	snapshotRunGit(t, repo, "tag", "lightweight")
	snapshotRunGit(t, repo, "tag", "-a", "annotated", "-m", "commit tag")
	for _, ref := range []string{"main", "refs/heads/main", "HEAD~0", "HEAD^0", "HEAD^{commit}", "lightweight", "annotated"} {
		t.Run(ref, func(t *testing.T) {
			if _, err := CaptureCleanSnapshot(repo, ref); err != nil {
				t.Fatalf("valid commit expression refused: %v", err)
			}
		})
	}
}

func TestCaptureSnapshotRefusesUnbornHead(t *testing.T) {
	repo := t.TempDir()
	snapshotRunGit(t, repo, "init", "-b", "main")
	if _, err := CaptureCleanSnapshot(repo, "HEAD"); err == nil {
		t.Fatal("unborn source certified")
	}
}

func TestCaptureSnapshotPreservesParentCancellation(t *testing.T) {
	repo := initSnapshotRepo(t)
	parent, cancel := context.WithCancel(context.Background())
	ctx := ContextWithCommandFactory(parent, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cancel()
		return exec.CommandContext(ctx, name, args...)
	})
	if _, err := CaptureCleanSnapshotContext(ctx, repo, "HEAD"); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation was not preserved: %v", err)
	}
}

func TestCaptureSnapshotIgnoresAmbientGitSelectors(t *testing.T) {
	repo := initSnapshotRepo(t)
	other := t.TempDir()
	for key, value := range map[string]string{
		"GIT_DIR":              filepath.Join(other, ".git"),
		"GIT_WORK_TREE":        other,
		"GIT_INDEX_FILE":       filepath.Join(other, "index"),
		"GIT_OBJECT_DIRECTORY": filepath.Join(other, "objects"),
		"GIT_CONFIG_COUNT":     "1",
		"GIT_CONFIG_KEY_0":     "remote.origin.url",
		"GIT_CONFIG_VALUE_0":   "https://example.invalid/ambient.git",
	} {
		t.Setenv(key, value)
	}
	got, err := CaptureCleanSnapshot(repo, "HEAD")
	if err != nil || !got.Clean {
		t.Fatalf("ambient Git selectors affected explicit source read: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(other, "index")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambient index was touched: %v", err)
	}
}
