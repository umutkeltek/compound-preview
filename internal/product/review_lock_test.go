package product

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewOperationLockConcurrentCreator(t *testing.T) {
	h := t.TempDir()
	original := openReviewOperationFile
	t.Cleanup(func() { openReviewOperationFile = original })
	calls := 0
	openReviewOperationFile = func(root *os.Root, path string, flags int, mode os.FileMode) (*os.File, error) {
		calls++
		if calls == 1 {
			if flags&(os.O_CREATE|os.O_EXCL) != os.O_CREATE|os.O_EXCL {
				t.Error("first open must create exclusively")
			}
			file, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if err != nil {
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
		} else if flags&(os.O_CREATE|os.O_EXCL) != 0 {
			t.Error("existing lock must be opened without creation flags")
		}
		return original(root, path, flags, mode)
	}
	release, err := lockReviewOperation(h, "review_a")
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("concurrent creator requires one exclusive-create and one existing-open attempt; got %d calls", calls)
	}
}

func TestReviewOperationLockDoesNotRetryOpenFailure(t *testing.T) {
	for _, injected := range []error{os.ErrNotExist, os.ErrPermission} {
		t.Run(injected.Error(), func(t *testing.T) {
			original := openReviewOperationFile
			t.Cleanup(func() { openReviewOperationFile = original })
			calls := 0
			openReviewOperationFile = func(*os.Root, string, int, os.FileMode) (*os.File, error) {
				calls++
				return nil, injected
			}
			if release, err := lockReviewOperation(t.TempDir(), "review_a"); err == nil {
				_ = release()
				t.Fatal("unavailable lock acquired")
			} else if !errors.Is(err, injected) || strings.Contains(err.Error(), "busy") {
				t.Fatalf("lock error was hidden as contention: %v", err)
			}
			if calls != 1 {
				t.Fatalf("open failure retried %d times", calls)
			}
		})
	}
}

func TestReviewOperationLockCreateAndReuse(t *testing.T) {
	h := t.TempDir()
	for i := 0; i < 2; i++ {
		release, err := lockReviewOperation(h, "review_a")
		if err != nil {
			t.Fatal(err)
		}
		if second, err := lockReviewOperation(h, "review_a"); err == nil {
			_ = second()
			t.Error("two holders acquired the same review operation")
		} else if !strings.Contains(err.Error(), "busy") {
			t.Errorf("ordinary contention: %v", err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewOperationLockRejectsUnsafeTargets(t *testing.T) {
	for _, fixture := range []string{"directory_symlink", "leaf_symlink", "leaf_directory"} {
		t.Run(fixture, func(t *testing.T) {
			h := t.TempDir()
			lock := filepath.Join(h, "lock")
			if fixture == "directory_symlink" {
				if err := os.Mkdir(filepath.Join(h, "other"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("other", lock); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(lock, 0700); err != nil {
					t.Fatal(err)
				}
				leaf := filepath.Join(lock, stableID("product-review-", "review_a")+".lock")
				if fixture == "leaf_symlink" {
					if err := os.WriteFile(filepath.Join(lock, "other"), nil, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("other", leaf); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(leaf, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if release, err := lockReviewOperation(h, "review_a"); err == nil {
				_ = release()
				t.Fatal("unsafe lock target acquired")
			}
		})
	}
}

func TestReviewOperationLockRejectsReplacementDuringOpen(t *testing.T) {
	for _, fixture := range []string{"leaf", "directory"} {
		t.Run(fixture, func(t *testing.T) {
			h := t.TempDir()
			original := openReviewOperationFile
			t.Cleanup(func() { openReviewOperationFile = original })
			openReviewOperationFile = func(root *os.Root, path string, flags int, mode os.FileMode) (*os.File, error) {
				file, err := original(root, path, flags, mode)
				if err != nil {
					return file, err
				}
				target := filepath.Join(h, path)
				if fixture == "directory" {
					target = filepath.Join(h, "lock")
				}
				if err := os.Rename(target, target+".previous"); err != nil {
					t.Fatal(err)
				}
				if fixture == "directory" {
					if err := os.Mkdir(target, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(h, path), nil, 0600); err != nil {
					t.Fatal(err)
				}
				return file, nil
			}
			if release, err := lockReviewOperation(h, "review_a"); err == nil {
				_ = release()
				t.Fatal("replaced lock target acquired")
			}
		})
	}
}
