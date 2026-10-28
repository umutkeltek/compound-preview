package home

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

type livenessIndexTestEntry string

func (e livenessIndexTestEntry) Name() string               { return string(e) }
func (e livenessIndexTestEntry) IsDir() bool                { return false }
func (e livenessIndexTestEntry) Type() fs.FileMode          { return 0 }
func (e livenessIndexTestEntry) Info() (fs.FileInfo, error) { return nil, nil }

type countingRunAttemptFiles struct {
	entries  []fs.DirEntry
	bodies   map[string][]byte
	dirErr   error
	readErrs map[string]error
	dirReads int
	reads    int
}

func (f *countingRunAttemptFiles) ReadDir(string) ([]fs.DirEntry, error) {
	f.dirReads++
	return f.entries, f.dirErr
}

func (f *countingRunAttemptFiles) ReadFile(path string) ([]byte, error) {
	f.reads++
	name := filepath.Base(path)
	if err := f.readErrs[name]; err != nil {
		return nil, err
	}
	return f.bodies[name], nil
}

func TestOperationalHygieneReadsRealisticRunAttemptCorpusOnce(t *testing.T) {
	now := time.Now().UTC()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "global", now); err != nil {
		t.Fatal(err)
	}
	files := &countingRunAttemptFiles{bodies: map[string][]byte{}}
	for i := 0; i < 13000; i++ {
		name := "ratt_scale_" + formatLivenessIndex(i) + ".json"
		files.entries = append(files.entries, livenessIndexTestEntry(name))
		body, err := json.Marshal(map[string]any{
			"work_id": "work_other_" + formatLivenessIndex(i%200),
			"status":  "succeeded", "created_at": now.Add(-time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		files.bodies[name] = body
	}
	for i := 0; i < 200; i++ {
		workID := "work_scale_" + formatLivenessIndex(i)
		writeHomeFile(t, homePath, "work/leases/"+workID+".json", `{"work_id":"`+workID+`","by":"codex","claimed_at":"1"}`)
		writeHomeFile(t, homePath, "work/work/"+workID+".json", `{"id":"`+workID+`","state":"completed"}`)
	}

	report, err := operationalHygieneWithRunAttempts(homePath, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleLeases) != 200 {
		t.Fatalf("stale leases = %d, want 200", len(report.StaleLeases))
	}
	if files.dirReads != 1 || files.reads != 13000 {
		t.Fatalf("RunAttempt scans = %d dirs/%d files, want 1/13000", files.dirReads, files.reads)
	}
}

func TestOperationalHygieneSkipsRunAttemptScanWithoutEligibleLease(t *testing.T) {
	now := time.Now().UTC()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "global", now); err != nil {
		t.Fatal(err)
	}
	files := &countingRunAttemptFiles{}
	if _, err := operationalHygieneWithRunAttempts(homePath, files); err != nil {
		t.Fatal(err)
	}
	if files.dirReads != 0 || files.reads != 0 {
		t.Fatalf("empty lease pass read RunAttempts: %d dirs/%d files", files.dirReads, files.reads)
	}
}

func TestStaleLeaseCandidatesBulkScanIsLazyPastCursor(t *testing.T) {
	now := time.Now().UTC()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "global", now); err != nil {
		t.Fatal(err)
	}
	const workID = "work_cursor_skipped"
	writeHomeFile(t, homePath, "work/leases/"+workID+".json", `{"work_id":"work_cursor_skipped","by":"codex","claimed_at":"1"}`)
	files := &countingRunAttemptFiles{}
	scan := operationalArchiveScan{afterByKind: map[string]string{
		"lease": "work/leases/" + workID + ".json",
	}}
	candidates, err := staleLeaseCandidatesWithRunAttempts(homePath, scan, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("cursor-skipped candidates = %v, want none", candidates)
	}
	if files.dirReads != 0 || files.reads != 0 {
		t.Fatalf("cursor-skipped lease read RunAttempts: %d dirs/%d files", files.dirReads, files.reads)
	}
}

func TestRunAttemptLivenessIndexUnreadableDirectoryIsEmpty(t *testing.T) {
	files := &countingRunAttemptFiles{dirErr: errors.New("synthetic unreadable directory")}
	index := loadRunAttemptLivenessIndex("ignored", files)
	if index.live("work_any", time.Now()) {
		t.Fatal("unreadable RunAttempt directory reported live work")
	}
	if files.dirReads != 1 || files.reads != 0 {
		t.Fatalf("unreadable directory reads = %d dirs/%d files, want 1/0", files.dirReads, files.reads)
	}
}

func TestRunAttemptLivenessIndexPreservesPredicateSemantics(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)
	tests := []struct {
		name, body, workID string
		want               bool
	}{
		{"fresh heartbeat", `{"work_id":"work_a","status":"running","created_at":"2026-09-11T18:00:00Z","last_heartbeat_at":"2026-09-11T19:59:00Z"}`, "work_a", true},
		{"created fallback", `{"work_id":"work_b","status":"pending","created_at":"2026-09-11T19:59:00Z"}`, "work_b", true},
		{"strict boundary", `{"work_id":"work_c","status":"running","created_at":"2026-09-11T19:30:00Z"}`, "work_c", false},

		{"stale heartbeat overrides fresh creation", `{"work_id":"work_h","status":"running","created_at":"2026-09-11T19:59:00Z","last_heartbeat_at":"2026-09-11T19:29:00Z"}`, "work_h", false},

		{"null heartbeat falls back", `{"work_id":"work_h","status":"running","created_at":"2026-09-11T19:59:00Z","last_heartbeat_at":null}`, "work_h", true},

		{"zero heartbeat overrides creation", `{"work_id":"work_h","status":"running","created_at":"2026-09-11T19:59:00Z","last_heartbeat_at":"0001-01-01T00:00:00Z"}`, "work_h", false},

		{"missing activity is stale", `{"work_id":"work_h","status":"running"}`, "work_h", false},

		{"offset heartbeat", `{"work_id":"work_h","status":"running","last_heartbeat_at":"2026-09-11T22:59:00+03:00"}`, "work_h", true},

		{"blank status is nonterminal", `{"work_id":"work_h","created_at":"2026-09-11T19:59:00Z"}`, "work_h", true},
		{"unknown status remains nonterminal", `{"work_id":"work_d","status":"future_state","created_at":"2026-09-11T19:59:00Z"}`, "work_d", true},
		{"succeeded ignored", `{"work_id":"work_e","status":"succeeded","created_at":"2026-09-11T19:59:00Z"}`, "work_e", false},
		{"failed ignored", `{"work_id":"work_e","status":"failed","created_at":"2026-09-11T19:59:00Z"}`, "work_e", false},
		{"abandoned ignored", `{"work_id":"work_e","status":"abandoned","created_at":"2026-09-11T19:59:00Z"}`, "work_e", false},
		{"superseded ignored", `{"work_id":"work_e","status":"superseded","created_at":"2026-09-11T19:59:00Z"}`, "work_e", false},
		{"foreign work", `{"work_id":"work_other","status":"running","created_at":"2026-09-11T19:59:00Z"}`, "work_f", false},
		{"blank work", `{"work_id":"","status":"running","created_at":"2026-09-11T19:59:00Z"}`, "", false},
		{"malformed skipped", `{`, "work_g", false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := "ratt_semantics_" + formatLivenessIndex(i) + ".json"
			files := &countingRunAttemptFiles{
				entries: []fs.DirEntry{livenessIndexTestEntry(name)},
				bodies:  map[string][]byte{name: []byte(tt.body)},
			}
			if got := loadRunAttemptLivenessIndex("ignored", files).live(tt.workID, now); got != tt.want {
				t.Fatalf("live(%q) = %t, want %t", tt.workID, got, tt.want)
			}
			// Differential control against the production fresh archive probe.
			homePath := t.TempDir()
			writeHomeFile(t, homePath, "run_attempts/"+name, tt.body)
			if got := liveRunAttemptForWork(homePath, tt.workID, now); got != tt.want {
				t.Fatalf("fresh probe(%q) = %t, want %t", tt.workID, got, tt.want)
			}
		})
	}
}

func TestRunAttemptLivenessIndexSkipsUnreadableAndLetsAnyLiveAttemptWin(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)
	files := &countingRunAttemptFiles{
		entries: []fs.DirEntry{
			livenessIndexTestEntry("unreadable.json"),
			livenessIndexTestEntry("terminal.json"),
			livenessIndexTestEntry("live.json"),
		},
		bodies: map[string][]byte{
			"terminal.json": []byte(`{"work_id":"work_shared","status":"succeeded","created_at":"2026-09-11T19:59:30Z"}`),
			"live.json":     []byte(`{"work_id":"work_shared","status":"running","created_at":"2026-09-11T19:59:00Z"}`),
		},
		readErrs: map[string]error{"unreadable.json": errors.New("synthetic unreadable")},
	}
	index := loadRunAttemptLivenessIndex("ignored", files)
	if !index.live("work_shared", now) {
		t.Fatal("fresh non-terminal sibling did not win over unreadable and terminal attempts")
	}
	if files.reads != 3 {
		t.Fatalf("reads = %d, want 3", files.reads)
	}
}

func TestLiveRunAttemptForWorkBlankIDIsNotLive(t *testing.T) {
	if liveRunAttemptForWork(t.TempDir(), "  ", time.Now()) {
		t.Fatal("blank work id reported live")
	}
}

func TestRunAttemptLivenessIndexNewestActivityWinsInEitherOrder(t *testing.T) {
	now := time.Date(2026, 9, 11, 20, 0, 0, 0, time.UTC)
	for _, reverse := range []bool{false, true} {
		files := &countingRunAttemptFiles{
			entries: []fs.DirEntry{livenessIndexTestEntry("stale.json"), livenessIndexTestEntry("fresh.json")},
			bodies: map[string][]byte{
				"stale.json": []byte(`{"work_id":"work_shared","status":"running","created_at":"2026-09-11T18:59:00Z"}`),
				"fresh.json": []byte(`{"work_id":"work_shared","status":"pending","created_at":"2026-09-11T19:59:00Z"}`),
			},
		}
		if reverse {
			files.entries[0], files.entries[1] = files.entries[1], files.entries[0]
		}
		if !loadRunAttemptLivenessIndex("ignored", files).live("work_shared", now) {
			t.Fatalf("newest activity lost with reverse=%t", reverse)
		}
	}
}

func formatLivenessIndex(i int) string {
	s := []byte("000000")
	for p := len(s) - 1; p >= 0 && i > 0; p-- {
		s[p] = byte('0' + i%10)
		i /= 10
	}
	return string(s)
}
