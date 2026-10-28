package gitrepository

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCommittedContentPathsSelectsExactACMInventory pins the canonical content
// selection: rename detection stays off, deletions never enter the inventory,
// re-added files do, and every path arrives as raw bytes.
func TestCommittedContentPathsSelectsExactACMInventory(t *testing.T) {
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "deleted.txt", "deleted\n")
	writeSnapshotFile(t, repo, "readded.txt", "first\n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "seed extras")
	base := snapshotGitOutput(t, repo, "rev-parse", "HEAD")

	snapshotRunGit(t, repo, "mv", "seed.txt", "renamed.txt")
	snapshotRunGit(t, repo, "rm", "-q", "deleted.txt")
	snapshotRunGit(t, repo, "rm", "-q", "readded.txt")
	writeSnapshotFile(t, repo, "readded.txt", "second\n")
	writeSnapshotFile(t, repo, " leading.txt", "leading\n")
	writeSnapshotFile(t, repo, "trailing.txt ", "trailing\n")
	writeSnapshotFile(t, repo, "weird\nname.txt", "newline\n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "reshaped content")
	head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")

	got, err := CommittedContentPaths(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{" leading.txt", "readded.txt", "renamed.txt", "trailing.txt ", "weird\nname.txt"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("committed content inventory = %q, want %q", got, want)
	}
}

// TestCommittedContentIgnoresWorkingTreeAndMovedRefs proves the inventory and
// the scanned bodies come from the object database: a mutated caller file and
// a moved ref cannot change either one.
func TestCommittedContentIgnoresWorkingTreeAndMovedRefs(t *testing.T) {
	repo := initSnapshotRepo(t)
	base := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	snapshotRunGit(t, repo, "switch", "-c", "candidate")
	writeSnapshotFile(t, repo, "candidate.txt", "committed\n")
	snapshotRunGit(t, repo, "add", "candidate.txt")
	snapshotRunGit(t, repo, "commit", "-m", "candidate")
	head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")

	paths, err := CommittedContentPaths(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"candidate.txt"}) {
		t.Fatalf("committed content inventory = %q, want [candidate.txt]", paths)
	}
	body, err := CommittedFileBody(context.Background(), repo, head, "candidate.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "committed\n" {
		t.Fatalf("committed body = %q, want the committed bytes", body)
	}

	// Mutate the checkout and move the ref the caller passed around.
	writeSnapshotFile(t, repo, "candidate.txt", "mutated\n")
	if err := os.Remove(filepath.Join(repo, "seed.txt")); err != nil {
		t.Fatal(err)
	}
	snapshotRunGit(t, repo, "branch", "-f", "main", head)

	after, err := CommittedContentPaths(context.Background(), repo, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after, paths) {
		t.Fatalf("mutated checkout changed the inventory: %q, want %q", after, paths)
	}
	afterBody, err := CommittedFileBody(context.Background(), repo, head, "candidate.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBody) != string(body) {
		t.Fatalf("mutated checkout changed the committed body: %q, want %q", afterBody, body)
	}
}

// TestCommittedDiffCheckCatchesWhitespaceInvisibleToWiderRange pins the
// independent member range check: a whitespace error the wider proof range
// cannot see still refuses the narrower member range.
func TestCommittedDiffCheckCatchesWhitespaceInvisibleToWiderRange(t *testing.T) {
	repo := initSnapshotRepo(t)
	writeSnapshotFile(t, repo, "ws.txt", "ws \nold\n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "wide base")
	wide := snapshotGitOutput(t, repo, "rev-parse", "HEAD")

	writeSnapshotFile(t, repo, "ws.txt", "mid\n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "member removes the whitespace line")
	member := snapshotGitOutput(t, repo, "rev-parse", "HEAD")

	writeSnapshotFile(t, repo, "ws.txt", "ws \nnew\n")
	snapshotRunGit(t, repo, "add", "-A")
	snapshotRunGit(t, repo, "commit", "-m", "head restores the whitespace line")
	head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")

	if err := CommittedDiffCheck(context.Background(), repo, wide, head); err != nil {
		t.Fatalf("wide range diff-check = %v, want clean", err)
	}
	err := CommittedDiffCheck(context.Background(), repo, member, head)
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("member range diff-check = %v, want whitespace refusal", err)
	}
}

// TestCommittedContentFailsClosedOnUnusableObjects refuses symbolic refs,
// abbreviated ids and objects the repository does not hold.
func TestCommittedContentFailsClosedOnUnusableObjects(t *testing.T) {
	repo := initSnapshotRepo(t)
	head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	missing := strings.Repeat("0", 40)
	for _, tc := range []struct {
		name, base, head string
	}{
		{"symbolic", "main", head},
		{"abbreviated", head[:7], head},
		{"missing", missing, head},
		{"missing-head", head, missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CommittedContentPaths(context.Background(), repo, tc.base, tc.head); err == nil {
				t.Fatal("unusable object accepted as committed content")
			}
			if err := CommittedDiffCheck(context.Background(), repo, tc.base, tc.head); err == nil {
				t.Fatal("unusable object accepted for diff-check")
			}
		})
	}
}

// TestCommittedReadsUseCallerFactoryAndSanitizedEnvironment binds all shared
// reads, including ancestry, to the caller's cancellation and execution owner.
func TestCommittedReadsUseCallerFactoryAndSanitizedEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		older bool
	}{{"native-git", false}, {"older-git", true}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initSnapshotRepo(t)
			base := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
			writeSnapshotFile(t, repo, "candidate.txt", "candidate\n")
			snapshotRunGit(t, repo, "add", "candidate.txt")
			snapshotRunGit(t, repo, "commit", "-m", "candidate")
			head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
			t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "absent"))
			calls := 0
			compatibility := false
			ctx := ContextWithCommandFactory(t.Context(), func(ctx context.Context, name string, args ...string) *exec.Cmd {
				calls++
				for _, arg := range args {
					if arg == "read-tree" {
						compatibility = true
					}
				}
				if tc.older {
					return olderGitAttributeCommand(ctx, name, args...)
				}
				return exec.CommandContext(ctx, name, args...)
			})
			if ok, err := CommittedAncestor(ctx, repo, base, head); err != nil || !ok {
				t.Fatalf("ancestry: %v %v", ok, err)
			}
			if _, err := CommittedContentPaths(ctx, repo, base, head); err != nil {
				t.Fatal(err)
			}
			if _, err := CommittedFileBody(ctx, repo, head, "candidate.txt"); err != nil {
				t.Fatal(err)
			}
			if err := CommittedDiffCheck(ctx, repo, base, head); err != nil {
				t.Fatal(err)
			}
			wantCalls := 10
			if compatibility {
				// Repository resolution, private read-tree, merge-base and
				// cached diff must all use the same caller factory.
				wantCalls += 4
			}
			if tc.older && !compatibility {
				t.Fatal("older Git did not execute the compatibility path")
			}
			if calls != wantCalls {
				t.Fatalf("factory calls = %d, want %d", calls, wantCalls)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := CommittedAncestor(canceled, repo, base, head); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled ancestry = %v", err)
			}
		})
	}
}

func TestCommittedDiffCheckUsesReviewedAttributes(t *testing.T) {
	repo := initSnapshotRepo(t)
	base := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	writeSnapshotFile(t, repo, ".gitattributes", "*.txt whitespace=blank-at-eol\n")
	writeSnapshotFile(t, repo, "candidate.txt", "bad \n")
	snapshotRunGit(t, repo, "add", ".gitattributes", "candidate.txt")
	snapshotRunGit(t, repo, "commit", "-m", "whitespace candidate")
	head := snapshotGitOutput(t, repo, "rev-parse", "HEAD")
	writeSnapshotFile(t, repo, ".gitattributes", "*.txt -whitespace\n")
	snapshotRunGit(t, repo, "diff", "--check", base+"..."+head)
	if err := CommittedDiffCheck(t.Context(), repo, base, head); err == nil {
		t.Fatal("mutable attributes hid reviewed whitespace error")
	}
}
