package eventstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMultiProcessConcurrentPutProducesValidFrames(t *testing.T) {
	homePath := t.TempDir()
	const children = 50

	type child struct {
		id     string
		cmd    *exec.Cmd
		output bytes.Buffer
	}
	procs := make([]child, 0, children)
	for i := 0; i < children; i++ {
		prefix, schema := "rec_", "compound.receipt.v0.1"
		if i%2 == 1 {
			prefix, schema = "tr_", "compound.transition.v0.1"
		}
		id := fmt.Sprintf("%sprocess_%02d", prefix, i)
		body, err := json.Marshal(map[string]any{
			"schema_version": schema,
			"id":             id,
			"created_at":     "2026-01-01T00:00:00Z",
			"ordinal":        i,
		})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(),
			childEnv+"=1",
			childHomeEnv+"="+homePath,
			childIDEnv+"="+id,
			childJSONEnv+"="+string(body),
		)
		procs = append(procs, child{id: id, cmd: cmd})
		procs[len(procs)-1].cmd.Stdout = &procs[len(procs)-1].output
		procs[len(procs)-1].cmd.Stderr = &procs[len(procs)-1].output
		if err := procs[len(procs)-1].cmd.Start(); err != nil {
			t.Fatalf("start child %s: %v", id, err)
		}
	}
	for i := range procs {
		if err := procs[i].cmd.Wait(); err != nil {
			t.Errorf("child %s: %v\n%s", procs[i].id, err, procs[i].output.String())
		}
	}
	if t.Failed() {
		return
	}

	store, err := Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Scan(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(entries); got != children {
		t.Fatalf("scan count = %d, want %d", got, children)
	}
	frames := strictSegmentFrames(t, filepath.Join(homePath, eventsDirName, segmentsDirName))
	if got := len(frames); got != children {
		t.Fatalf("frame count = %d, want %d", got, children)
	}
	families := map[string]bool{}
	for _, frame := range frames {
		families[frame.Family] = true
	}
	if !families["receipt"] || !families["transition"] {
		t.Fatalf("families = %v, want receipt+transition", families)
	}
}

func TestRotationAtSoftCapKeepsEveryRecordRetrievable(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{
		clock:           func() time.Time { return now },
		segmentMaxBytes: 4096,
	})
	const records = 10
	for i := 0; i < records; i++ {
		id := fmt.Sprintf("rec_rotate_%02d", i)
		putRecord(t, store, id, "compound.receipt.v0.1", now, strings.Repeat(string(rune('a'+i)), 1400))
	}

	familyDir := filepath.Join(store.engine.segRoot, "receipt")
	if _, err := os.Stat(filepath.Join(familyDir, "2026-07-15.1.jsonl")); err != nil {
		t.Fatalf("rotation file missing: %v", err)
	}
	paths := segmentPathsForTest(t, familyDir)
	var largestFrame int64
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.SplitAfter(data, []byte("\n")) {
			if len(line) > 0 && int64(len(line)) > largestFrame {
				largestFrame = int64(len(line))
			}
		}
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 4096+largestFrame {
			t.Fatalf("%s size=%d exceeds cap by more than one frame (%d)", path, info.Size(), largestFrame)
		}
		if _, err := parseSegment(path, false); err != nil {
			t.Fatalf("validate %s: %v", path, err)
		}
	}
	for i := 0; i < records; i++ {
		id := fmt.Sprintf("rec_rotate_%02d", i)
		if _, err := store.Read(id); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
	}
}

func TestIDConflictIdempotencyLookbackAndLegacyLayer(t *testing.T) {
	now := time.Date(2026, 7, 15, 23, 59, 59, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	record := eventRecord("rec_idempotent", "compound.receipt.v0.1", now, "same")
	first, err := store.PutJSON("rec_idempotent", record, "  ")
	if err != nil || !first.Created {
		t.Fatalf("first put = %+v, %v", first, err)
	}
	before := readFamilyBytes(t, store.engine.segRoot, "receipt")
	second, err := store.PutJSON("rec_idempotent", record, "")
	if err != nil || second.Created {
		t.Fatalf("idempotent put = %+v, %v", second, err)
	}
	if got := readFamilyBytes(t, store.engine.segRoot, "receipt"); !bytes.Equal(got, before) {
		t.Fatal("idempotent retry appended another frame")
	}
	conflict := eventRecord("rec_idempotent", "compound.receipt.v0.1", now, "different")
	if _, err := store.PutJSON("rec_idempotent", conflict, "  "); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("same-day conflict error = %v, want ErrIDConflict", err)
	}
	if got := readFamilyBytes(t, store.engine.segRoot, "receipt"); !bytes.Equal(got, before) {
		t.Fatal("conflict mutated segment")
	}

	now = now.Add(2 * time.Second)
	if _, err := store.PutJSON("rec_idempotent", record, "  "); err != nil {
		t.Fatalf("cross-midnight idempotent retry: %v", err)
	}
	if _, err := store.PutJSON("rec_idempotent", conflict, "  "); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("cross-midnight conflict error = %v, want ErrIDConflict", err)
	}

	legacyID := "rec_legacy_layer"
	legacy := eventRecord(legacyID, "compound.receipt.v0.1", now, "legacy")
	legacyBytes, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(store.rootPath(), legacyID+".json")
	if err := os.WriteFile(legacyPath, append(legacyBytes, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := store.PutJSON(legacyID, legacy, "")
	if err != nil || result.Created {
		t.Fatalf("cross-layer identical = %+v, %v", result, err)
	}
	legacyConflict := eventRecord(legacyID, "compound.receipt.v0.1", now, "changed")
	if _, err := store.PutJSON(legacyID, legacyConflict, ""); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("cross-layer conflict error = %v, want ErrIDConflict", err)
	}
}

func TestFilesystemGuardDegradesLoudlyOrFailsClosed(t *testing.T) {
	t.Run("default non-local degrades once", func(t *testing.T) {
		var warnings []string
		store := openConfiguredStore(t, engineConfig{
			probe: func(string) (string, bool, error) { return "nfs", false, nil },
			logf:  func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) },
		})
		now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
		putRecord(t, store, "rec_nfs_a", "compound.receipt.v0.1", now, "a")
		putRecord(t, store, "rec_nfs_b", "compound.receipt.v0.1", now, "b")
		if len(warnings) != 1 || !strings.Contains(strings.ToLower(warnings[0]), "nfs") {
			t.Fatalf("warnings = %q, want one loud nfs warning", warnings)
		}
		if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_nfs_a.json")); err != nil {
			t.Fatalf("legacy fallback missing: %v", err)
		}
		if _, err := os.Stat(store.engine.segRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-local fallback created segments, stat err=%v", err)
		}
	})

	t.Run("require local fails", func(t *testing.T) {
		t.Setenv(requireLocalEnv, "1")
		store := openConfiguredStore(t, engineConfig{
			probe: func(string) (string, bool, error) { return "nfs", false, nil },
			logf:  func(string, ...any) {},
		})
		record := eventRecord("rec_require_local", "compound.receipt.v0.1", time.Now(), "blocked")
		if _, err := store.PutJSON("rec_require_local", record, "  "); !errors.Is(err, ErrNonLocalFS) {
			t.Fatalf("error = %v, want ErrNonLocalFS", err)
		}
		if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_require_local.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("require-local wrote legacy file, stat err=%v", err)
		}
	})

	t.Run("local uses segments", func(t *testing.T) {
		store := openConfiguredStore(t, engineConfig{
			probe: func(string) (string, bool, error) { return "apfs", true, nil },
		})
		putRecord(t, store, "rec_local", "compound.receipt.v0.1", time.Now(), "segment")
		if _, err := os.Stat(store.engine.segRoot); err != nil {
			t.Fatalf("segment root missing: %v", err)
		}
		if _, err := os.Stat(filepath.Join(store.rootPath(), "rec_local.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("local write minted loose file, stat err=%v", err)
		}
	})

	t.Run("real probe names filesystem", func(t *testing.T) {
		fsType, _, err := checkLocalFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(fsType) == "" {
			t.Fatal("real statfs probe returned an empty filesystem type")
		}
	})
}

func TestIngestionDayUsesWriteClockNotCreatedAt(t *testing.T) {
	now := time.Date(2026, 7, 15, 23, 59, 59, 0, time.UTC)
	store := openConfiguredStore(t, engineConfig{clock: func() time.Time { return now }})
	createdAt := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	putRecord(t, store, "rec_before_midnight", "compound.receipt.v0.1", createdAt, "before")
	now = time.Date(2026, 7, 16, 0, 0, 1, 0, time.UTC)
	putRecord(t, store, "rec_after_midnight", "compound.receipt.v0.1", createdAt, "after")

	familyDir := filepath.Join(store.engine.segRoot, "receipt")
	for _, day := range []string{"2026-07-15.jsonl", "2026-07-16.jsonl"} {
		if _, err := os.Stat(filepath.Join(familyDir, day)); err != nil {
			t.Fatalf("ingestion-day segment %s missing: %v", day, err)
		}
	}
	if _, err := os.Stat(filepath.Join(familyDir, "1999-01-01.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created_at incorrectly selected segment, stat err=%v", err)
	}
}

func openConfiguredStore(t *testing.T, config engineConfig) *Store {
	t.Helper()
	store, err := openWithConfig(t.TempDir(), config)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func eventRecord(id, schema string, createdAt time.Time, value string) map[string]any {
	return map[string]any{
		"schema_version": schema,
		"id":             id,
		"created_at":     createdAt.UTC().Format(time.RFC3339Nano),
		"value":          value,
	}
}

func putRecord(t *testing.T, store *Store, id, schema string, createdAt time.Time, value string) PutResult {
	t.Helper()
	result, err := store.PutJSON(id, eventRecord(id, schema, createdAt, value), "  ")
	if err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
	return result
}

func strictSegmentFrames(t *testing.T, root string) []Frame {
	t.Helper()
	var frames []Frame
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		parsed, err := parseSegment(path, false)
		if err != nil {
			return err
		}
		for _, item := range parsed {
			frames = append(frames, item.Frame)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func segmentPathsForTest(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths
}

func readFamilyBytes(t *testing.T, root, family string) []byte {
	t.Helper()
	var out []byte
	for _, path := range segmentPathsForTest(t, filepath.Join(root, family)) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, data...)
	}
	return out
}
