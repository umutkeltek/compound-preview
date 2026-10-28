package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An argv element beginning with ~ never expands: exec does not run a shell, so a
// registry promising `bash ~/...` promises a command that can only fail with
// "No such file or directory" — the exact failure shape that left the scheduled
// capture backup exiting 127 on every cycle while the registry looked complete.
func TestReclaimArgvNeverContainsTilde(t *testing.T) {
	for _, class := range RetentionClasses() {
		for i, arg := range class.ReclaimCommandArgv {
			if strings.HasPrefix(arg, "~") {
				t.Fatalf("class %q argv[%d] %q starts with '~', which no shell will expand under exec", class.ID, i, arg)
			}
		}
		if strings.Contains(class.ReclaimCommand, "~/") {
			t.Fatalf("class %q display command %q contains a '~/' path that argv cannot honor", class.ID, class.ReclaimCommand)
		}
	}
}

// A registry entrypoint naming a script under this repository's scripts/ must name a
// script that actually exists and is tracked. A missing owner script is not a
// theoretical concern here: the archived-artifact class pointed at a script that was
// never committed, and the scheduled lane failed every cycle behind it.
func TestLocalScriptReclaimCommandsResolveToTrackedScripts(t *testing.T) {
	root := retentionTestRepoRoot(t)
	for _, class := range RetentionClasses() {
		for _, arg := range class.ReclaimCommandArgv {
			if !strings.HasPrefix(arg, "scripts/") {
				continue
			}
			path := filepath.Join(root, arg)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("class %q names reclaim script %s, which does not exist in this repo: %v", class.ID, arg, err)
			}
			if info.IsDir() {
				t.Fatalf("class %q names reclaim script %s, which is a directory", class.ID, arg)
			}
			if info.Mode()&0o111 == 0 {
				t.Fatalf("class %q names reclaim script %s, which is not executable", class.ID, arg)
			}
		}
	}
}

// The archived-artifact class is the one whose owner script went missing once; the
// mechanism assertion keeps the class from regressing to a promise with no owner.
func TestArchivedArtifactClassOwnsAReclaimMechanism(t *testing.T) {
	class, ok := RetentionClassByID("archived-artifact")
	if !ok {
		t.Fatal("archived-artifact class must stay declared")
	}
	if class.ReclaimCommand == "" || len(class.ReclaimCommandArgv) == 0 {
		t.Fatal("archived-artifact must own a concrete reclaim command and argv")
	}
	if class.Mode != ReclaimOperatorGated {
		t.Fatal("archived-artifact reclaim must stay operator-gated; unattended deletion of sealed captures is not reclaimable")
	}
}

func retentionTestRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root")
		}
		dir = parent
	}
}
