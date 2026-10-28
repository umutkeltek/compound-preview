package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/eventstore"
)

// Only the disposable synthetic journey calls this. Preserve failure-time
// lifecycle and door evidence in go-test.log before testing removes its home;
// never dump the process environment or an operator home into CI artifacts.
func logSelectedContinueFailure(t *testing.T, homePath string) {
	t.Helper()
	if t.Failed() {
		t.Log(selectedContinueFailureEvidence(homePath))
	}
}

func selectedContinueFailureEvidence(homePath string) string {
	var out strings.Builder
	for _, dir := range []string{"work/work", "work/leases", "run_attempts"} {
		err := filepath.WalkDir(filepath.Join(homePath, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() || !(strings.HasSuffix(path, ".json") || strings.HasSuffix(path, ".log")) {
				return nil
			}
			body, readErr := os.ReadFile(path)
			rel, _ := filepath.Rel(homePath, path)
			fmt.Fprintf(&out, "\n--- synthetic %s (read error: %v) ---\n%s\n", rel, readErr, body)
			return nil
		})
		if err != nil {
			fmt.Fprintf(&out, "\nsynthetic %s scan error: %v\n", dir, err)
		}
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		fmt.Fprintf(&out, "\nsynthetic events open error: %v\n", err)
		return out.String()
	}
	body, err := events.ReadWorkLog()
	fmt.Fprintf(&out, "\n--- synthetic work events (read error: %v) ---\n%s\n", err, body)
	return out.String()
}

func TestSelectedContinueFailureEvidenceIncludesDetachedLog(t *testing.T) {
	homePath := t.TempDir()
	for path, body := range map[string]string{
		"work/work/work_fixture.json":                 `{"id":"work_fixture","state":"running"}`,
		"work/leases/work_fixture.json":               `{"by":"fixture-lease-holder"}`,
		"run_attempts/ratt_fixture.json":              `{"id":"ratt_fixture"}`,
		"run_attempts/detached/work_fixture.json":     `{"launch_id":"fixture-launch"}`,
		"run_attempts/detached/work_fixture.door.log": "fixture finalization marker",
		"config/unrelated.json":                       "excluded configuration sentinel",
	} {
		full := filepath.Join(homePath, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := events.AppendWorkLog([]byte("{\"work_id\":\"fixture-work-event\"}\n"), nil); err != nil {
		t.Fatal(err)
	}
	got := selectedContinueFailureEvidence(homePath)
	for _, want := range []string{"work_fixture", "fixture-lease-holder", "ratt_fixture", "fixture-launch", "fixture finalization marker", "fixture-work-event"} {
		if !strings.Contains(got, want) {
			t.Errorf("failure evidence omitted %q: %s", want, got)
		}
	}
	if strings.Contains(got, "excluded configuration sentinel") {
		t.Error("failure diagnostics leaked unrelated configuration")
	}
}
