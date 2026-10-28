package finding

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Ordinary-save-product packet sentinels — used in the red-first falsifiers so the
// no-leak assertion cannot pass because a generic fixture accidentally lacks the body.
const (
	OrdinarySaveBodySentinel     = "BODY_SENTINEL_7f4d7b\nsecond line"
	OrdinarySaveEvidenceSentinel = "EVIDENCE_SENTINEL_c91e2a"
)

func TestRecordShowRoundTrip(t *testing.T) {
	home := t.TempDir()
	rec, err := Record(home, Finding{Topic: "deploy", Body: "the deploy verb is `bin/ship`", WorkID: "work_abc"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.ID, "find_") {
		t.Fatalf("id = %q, want find_ prefix", rec.ID)
	}
	if rec.CID != CID(rec.Body) || !strings.HasPrefix(rec.CID, "sha256:") {
		t.Fatalf("cid = %q", rec.CID)
	}
	if rec.SourceClass != "evidence" {
		t.Fatalf("source_class = %q, want evidence (findings are never promoted truth)", rec.SourceClass)
	}
	got, err := Show(home, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != rec.Body || got.WorkID != "work_abc" || got.Topic != "deploy" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// TestRecordWritesOnlyUnderObservedFindings is the never-ledger invariant: recording a finding must
// leave the ledger byte-identical, so every file it writes lands under observed/findings.
func TestRecordWritesOnlyUnderObservedFindings(t *testing.T) {
	home := t.TempDir()
	if _, err := Record(home, Finding{Topic: "ci-retry", Body: "flaky tests retry via CI_RETRIES=2"}, fixedNow); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(home, p)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(filepath.ToSlash(rel), "observed/findings/") {
			t.Fatalf("Record wrote outside observed/findings (never-ledger invariant broken): %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecordIdempotentOnContent(t *testing.T) {
	home := t.TempDir()
	a, err := Record(home, Finding{Topic: "t", Body: "same body"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Record(home, Finding{Topic: "t", Body: "same body"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("same (topic,body) must yield same id: %q vs %q", a.ID, b.ID)
	}
	all, err := List(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("idempotent record must not duplicate: got %d files", len(all))
	}
}

func TestListFilterAndSupersession(t *testing.T) {
	home := t.TempDir()
	v1, err := Record(home, Finding{Topic: "runbook", Body: "v1 procedure"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Record(home, Finding{Topic: "runbook", Body: "v2 procedure", Supersedes: v1.ID}, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Record(home, Finding{Topic: "unrelated", Body: "noise"}, fixedNow); err != nil {
		t.Fatal(err)
	}

	runbook, err := List(home, "runbook")
	if err != nil {
		t.Fatal(err)
	}
	if len(runbook) != 2 {
		t.Fatalf("topic filter should return 2 runbook findings, got %d", len(runbook))
	}
	if runbook[0].ID != v2.ID || runbook[0].Supersedes != v1.ID {
		t.Fatalf("newest-first + supersession wrong: %+v", runbook[0])
	}
}

func TestShowRejectsPathEscape(t *testing.T) {
	home := t.TempDir()
	if _, err := Show(home, "../../etc/passwd"); err == nil {
		t.Fatal("Show must reject a path-shaped id")
	}
}

// TestRecordWithStatusFirstWriteReturnsRecorded — falsifier 1: first write returns recorded.
func TestRecordWithStatusRejectsOversizeBodyBeforeCreatingFindingHome(t *testing.T) {
	const maxBodyBytes = MaxBodyBytes

	exactHome := t.TempDir()
	if _, err := RecordWithStatus(exactHome, Finding{
		Topic: "exact-bound",
		Body:  strings.Repeat("x", maxBodyBytes),
	}, fixedNow); err != nil {
		t.Fatalf("exact-bound body rejected: %v", err)
	}

	oversizeHome := filepath.Join(t.TempDir(), "not-created")
	if _, err := RecordWithStatus(oversizeHome, Finding{
		Topic: "one-byte-over",
		Body:  strings.Repeat("x", maxBodyBytes+1),
	}, fixedNow); err == nil {
		t.Fatal("RecordWithStatus accepted a body one byte over the 1 MiB invariant")
	} else if !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("oversize error = %q, want declared 1 MiB limit", err)
	}
	if _, err := os.Stat(oversizeHome); !os.IsNotExist(err) {
		t.Fatalf("oversize rejection created finding home: stat err=%v", err)
	}
}

func TestRecordWithStatusFirstWriteReturnsRecorded(t *testing.T) {
	home := t.TempDir()
	res, err := RecordWithStatus(home, Finding{
		Topic:        "ordinary-save-proof",
		Body:         OrdinarySaveBodySentinel,
		WorkID:       "work_first",
		ProducedBy:   "agent:first",
		EvidenceRefs: []string{OrdinarySaveEvidenceSentinel},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRecorded {
		t.Fatalf("first write status = %q, want %q", res.Status, StatusRecorded)
	}
	if !strings.HasPrefix(res.Finding.ID, "find_") {
		t.Fatalf("id = %q, want find_ prefix", res.Finding.ID)
	}
	if res.Finding.CID != CID(OrdinarySaveBodySentinel) {
		t.Fatalf("cid = %q, want %q", res.Finding.CID, CID(OrdinarySaveBodySentinel))
	}
	if res.Finding.SourceClass != "evidence" {
		t.Fatalf("source_class = %q, want evidence", res.Finding.SourceClass)
	}
}

func TestRecordWithStatusPostLinkCleanupFailureIsReportedAfterDirectorySync(t *testing.T) {
	home := t.TempDir()
	originalLink := linkFindingFile
	originalRemove := removeStagedFindingFile
	originalSync := syncFindingDirectoryFunc
	t.Cleanup(func() {
		linkFindingFile = originalLink
		removeStagedFindingFile = originalRemove
		syncFindingDirectoryFunc = originalSync
	})

	events := make([]string, 0, 3)
	linkFindingFile = func(tempPath, targetPath string) error {
		events = append(events, "link")
		return os.Link(tempPath, targetPath)
	}
	cleanupErr := errors.New("injected post-link cleanup failure")
	removeStagedFindingFile = func(string) error {
		events = append(events, "remove")
		return cleanupErr
	}
	syncFindingDirectoryFunc = func(path string) error {
		events = append(events, "sync")
		return syncDir(path)
	}

	first, err := RecordWithStatus(home, Finding{
		Topic:        "ordinary-save-cleanup",
		Body:         OrdinarySaveBodySentinel,
		WorkID:       "work_cleanup_first",
		ProducedBy:   "agent:first",
		EvidenceRefs: []string{OrdinarySaveEvidenceSentinel},
	}, fixedNow)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("post-link cleanup error = %v, want injected cleanup failure", err)
	}
	if first.Status != StatusRecorded {
		t.Fatalf("successful hard link status = %q, want %q despite cleanup error", first.Status, StatusRecorded)
	}
	if got, want := strings.Join(events, ","), "link,remove,sync"; got != want {
		t.Fatalf("publication syscall order = %q, want %q", got, want)
	}

	target := filepath.Join(home, Subdir, first.Finding.ID+".json")
	targetBytes, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read authoritative target after cleanup error: %v", readErr)
	}
	wantBytes, marshalErr := json.MarshalIndent(&first.Finding, "", "  ")
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	wantBytes = append(wantBytes, '\n')
	if string(targetBytes) != string(wantBytes) {
		t.Fatalf("authoritative target differs from recorded finding:\ngot:\n%s\nwant:\n%s", targetBytes, wantBytes)
	}
	shown, showErr := Show(home, first.Finding.ID)
	if showErr != nil {
		t.Fatalf("show authoritative target after cleanup error: %v", showErr)
	}
	if shown.ID != first.Finding.ID || shown.CID != first.Finding.CID || shown.Body != OrdinarySaveBodySentinel {
		t.Fatalf("authoritative target is not the linked finding: %+v", shown)
	}

	retry, retryErr := RecordWithStatus(home, Finding{
		Topic:      "ordinary-save-cleanup",
		Body:       OrdinarySaveBodySentinel,
		WorkID:     "work_cleanup_retry",
		ProducedBy: "agent:retry",
	}, fixedNow.Add(time.Minute))
	if retryErr != nil {
		t.Fatalf("retry after linked cleanup error: %v", retryErr)
	}
	if retry.Status != StatusDuplicate {
		t.Fatalf("retry status = %q, want %q", retry.Status, StatusDuplicate)
	}
	afterRetry, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(afterRetry) != string(targetBytes) {
		t.Fatalf("retry changed authoritative bytes:\nbefore:\n%s\nafter:\n%s", targetBytes, afterRetry)
	}
}

// TestRecordWithStatusExactRetryReturnsDuplicate — falsifier 1: an exact retry returns duplicate.
func TestRecordWithStatusExactRetryReturnsDuplicate(t *testing.T) {
	home := t.TempDir()
	first, err := RecordWithStatus(home, Finding{
		Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel,
		WorkID: "work_first", ProducedBy: "agent:first",
		EvidenceRefs: []string{OrdinarySaveEvidenceSentinel},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := RecordWithStatus(home, Finding{
		Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel,
		WorkID: "work_second", ProducedBy: "agent:second",
		EvidenceRefs: []string{"trace:second"},
	}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != StatusDuplicate {
		t.Fatalf("retry status = %q, want %q", second.Status, StatusDuplicate)
	}
	if second.Finding.ID != first.Finding.ID {
		t.Fatalf("retry must surface first-finding id %q, got %q", first.Finding.ID, second.Finding.ID)
	}
	if second.Finding.CID != first.Finding.CID {
		t.Fatalf("retry must surface first-finding cid %q, got %q", first.Finding.CID, second.Finding.CID)
	}
}

// TestRecordWithStatusDuplicatePreservesFirstProvenance — falsifier 2: an exact retry with
// different producer, work ID, evidence, timestamp, and supersedes returns the original
// record byte-for-byte and does not overwrite first provenance or creation time.
func TestRecordWithStatusDuplicatePreservesFirstProvenance(t *testing.T) {
	home := t.TempDir()
	first, err := RecordWithStatus(home, Finding{
		Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel,
		WorkID: "work_first", ProducedBy: "agent:first",
		EvidenceRefs: []string{OrdinarySaveEvidenceSentinel},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := os.ReadFile(filepath.Join(home, Subdir, first.Finding.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RecordWithStatus(home, Finding{
		Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel,
		WorkID: "work_second", ProducedBy: "agent:second",
		Supersedes:   "find_aaaaaaaaaaaa",
		EvidenceRefs: []string{"trace:second", "trace:third"},
	}, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := os.ReadFile(filepath.Join(home, Subdir, first.Finding.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Fatalf("retry must leave the on-disk record byte-identical:\nfirst:\n%s\nsecond:\n%s", firstBytes, secondBytes)
	}
	// The durable record must carry the FIRST provenance, not the retry's.
	var found Finding
	if err := json.Unmarshal(secondBytes, &found); err != nil {
		t.Fatal(err)
	}
	if found.ProducedBy != "agent:first" {
		t.Fatalf("retry clobbered produced_by: %q", found.ProducedBy)
	}
	if found.WorkID != "work_first" {
		t.Fatalf("retry clobbered work_id: %q", found.WorkID)
	}
	if found.CreatedAt != first.Finding.CreatedAt {
		t.Fatalf("retry clobbered created_at: %q vs %q", found.CreatedAt, first.Finding.CreatedAt)
	}
	if len(found.EvidenceRefs) != 1 || found.EvidenceRefs[0] != OrdinarySaveEvidenceSentinel {
		t.Fatalf("retry clobbered evidence_refs: %v", found.EvidenceRefs)
	}
}

// TestRecordWithStatusDifferentTopicSameBody — falsifier 3: different topic creates a second record.
func TestRecordWithStatusDifferentTopicSameBody(t *testing.T) {
	home := t.TempDir()
	a, err := RecordWithStatus(home, Finding{Topic: "topic-a", Body: OrdinarySaveBodySentinel}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RecordWithStatus(home, Finding{Topic: "topic-b", Body: OrdinarySaveBodySentinel}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if a.Finding.ID == b.Finding.ID {
		t.Fatalf("different topic must yield distinct id: %q", a.Finding.ID)
	}
	if a.Finding.CID != b.Finding.CID {
		t.Fatalf("same body must yield same cid: %q vs %q", a.Finding.CID, b.Finding.CID)
	}
}

// TestRecordWithStatusSameTopicDifferentBody — falsifier 4: same topic with different body creates another record.
func TestRecordWithStatusSameTopicDifferentBody(t *testing.T) {
	home := t.TempDir()
	a, err := RecordWithStatus(home, Finding{Topic: "ordinary-save-proof", Body: "first body"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RecordWithStatus(home, Finding{Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if a.Finding.ID == b.Finding.ID {
		t.Fatalf("different body must yield distinct id: %q", a.Finding.ID)
	}
}

// TestRecordWithStatusConcurrentPublishesOneRecord — falsifier 5: concurrent saves converge
// on exactly one complete valid file with one recorded and every other result duplicate.
func TestRecordWithStatusConcurrentPublishesOneRecord(t *testing.T) {
	home := t.TempDir()
	const concurrency = 8
	var wg sync.WaitGroup
	results := make([]RecordResult, concurrency)
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = RecordWithStatus(home, Finding{
				Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel,
				WorkID: "work_concurrent", ProducedBy: "agent:concurrent",
			}, fixedNow)
		}(i)
	}
	wg.Wait()
	recorded := 0
	duplicate := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d returned error: %v", i, err)
		}
		switch results[i].Status {
		case StatusRecorded:
			recorded++
		case StatusDuplicate:
			duplicate++
		default:
			t.Fatalf("worker %d status = %q", i, results[i].Status)
		}
	}
	if recorded != 1 || duplicate != concurrency-1 {
		t.Fatalf("expected 1 recorded + %d duplicate, got %d/%d", concurrency-1, recorded, duplicate)
	}
	entries, err := os.ReadDir(filepath.Join(home, Subdir))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("expected exactly one finding file, got %d", files)
	}
}

// TestRecordWithStatusMalformedExistingFailsClosed — falsifier 6: a malformed/truncated
// existing deterministic target fails closed and remains byte-identical.
func TestRecordWithStatusMalformedExistingFailsClosed(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, Subdir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Build a finding so we know its id, then write malformed bytes at that path.
	res, err := RecordWithStatus(home, Finding{Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	// Reshape so the dir only contains the malformed file (the prior call deleted the staged one).
	corrupt := []byte("{\"id\":\"" + res.Finding.ID + "\",\"not\":\"a finding\"")
	if err := os.WriteFile(filepath.Join(home, Subdir, res.Finding.ID+".json"), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = RecordWithStatus(home, Finding{Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel}, fixedNow)
	if err == nil {
		t.Fatal("malformed existing target must fail closed")
	}
	// Byte-identical preservation.
	now, err := os.ReadFile(filepath.Join(home, Subdir, res.Finding.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(now) != string(corrupt) {
		t.Fatalf("malformed target was modified on validation failure")
	}
}

// TestRecordWithStatusExistingTargetInvariantsFailClosed — falsifier 7: a target whose ID,
// topic/body digest, CID, source_class, created_at, or topic/body disagrees with the owned
// invariants fails closed and remains byte-identical. The probe writes each corruption at the
// deterministic target so the validator must refuse — never overwrite, never repair.
func TestRecordWithStatusExistingTargetInvariantsFailClosed(t *testing.T) {
	type mutation struct {
		name string
		// apply mutates a fresh record in place before the corrupt bytes land at the
		// deterministic target.
		apply func(f *Finding)
	}
	mutations := []mutation{
		{
			name: "id-disagrees-with-deterministic-digest",
			apply: func(f *Finding) {
				f.ID = "find_000000000000"
			},
		},
		{
			name: "id-prefers-non-find-prefix",
			apply: func(f *Finding) {
				f.ID = "find_doesnotmatchdigest12"
			},
		},
		{
			name: "cid-mismatch",
			apply: func(f *Finding) {
				f.CID = CID("totally different body content for an unrelated cid value")
			},
		},
		{
			name: "source-class-not-evidence",
			apply: func(f *Finding) {
				f.SourceClass = "promoted-claim"
			},
		},
		{
			name: "missing-created-at",
			apply: func(f *Finding) {
				f.CreatedAt = ""
			},
		},
		{
			name: "invalid-created-at",
			apply: func(f *Finding) {
				f.CreatedAt = "yesterday at five-ish"
			},
		},
		{
			name: "topic-does-not-match-deterministic-input",
			apply: func(f *Finding) {
				f.Topic = "different-topic"
			},
		},
		{
			name: "body-does-not-match-deterministic-input",
			apply: func(f *Finding) {
				f.Body = "totally different body"
			},
		},
	}

	for _, m := range mutations {
		m := m
		t.Run(m.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, Subdir), 0o755); err != nil {
				t.Fatal(err)
			}
			// Build a record with the canonical (topic, body) to learn the deterministic id.
			first, err := RecordWithStatus(home, Finding{Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel}, fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			// Now corrupt the on-disk record with the chosen mutation.
			corrupt := Finding{
				ID:           first.Finding.ID,
				CID:          first.Finding.CID,
				Topic:        first.Finding.Topic,
				Body:         first.Finding.Body,
				SourceClass:  first.Finding.SourceClass,
				CreatedAt:    first.Finding.CreatedAt,
				ProducedBy:   first.Finding.ProducedBy,
				WorkID:       first.Finding.WorkID,
				EvidenceRefs: first.Finding.EvidenceRefs,
				Supersedes:   first.Finding.Supersedes,
			}
			m.apply(&corrupt)
			data, _ := json.MarshalIndent(&corrupt, "", "  ")
			if err := os.WriteFile(filepath.Join(home, Subdir, first.Finding.ID+".json"), append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			corruptBytes, err := os.ReadFile(filepath.Join(home, Subdir, first.Finding.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}

			// The honest retry MUST fail closed (any of the invariants is now violated).
			if _, err := RecordWithStatus(home, Finding{
				Topic:        "ordinary-save-proof",
				Body:         OrdinarySaveBodySentinel,
				WorkID:       "work_first",
				ProducedBy:   "agent:first",
				EvidenceRefs: []string{OrdinarySaveEvidenceSentinel},
			}, fixedNow); err == nil {
				t.Fatalf("corruption %q: must fail closed", m.name)
			}

			// The target MUST remain byte-identical to the corrupt bytes we wrote.
			now, err := os.ReadFile(filepath.Join(home, Subdir, first.Finding.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(now) != string(corruptBytes) {
				t.Fatalf("corruption %q: target was modified on validation failure\nwant=%s\ngot=%s", m.name, corruptBytes, now)
			}
		})
	}
}

// TestRecordWithStatusNoWriteOutsideObservedFindings — falsifier 8: recording writes no file
// outside observed/findings; the ledger tree/digest remains byte-identical.
func TestRecordWithStatusNoWriteOutsideObservedFindings(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "ledger"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "ledger", "digest"), []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := walkSnapshot(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordWithStatus(home, Finding{Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel}, fixedNow); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordWithStatus(home, Finding{Topic: "ordinary-save-proof", Body: OrdinarySaveBodySentinel}, fixedNow); err != nil {
		t.Fatal(err)
	}
	after, err := walkSnapshot(home)
	if err != nil {
		t.Fatal(err)
	}
	if !equalTree(before, after) {
		t.Fatalf("tree outside observed/findings must remain byte-identical\nbefore=%v\nafter=%v", before, after)
	}
}

func walkSnapshot(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasPrefix(filepath.ToSlash(p), filepath.ToSlash(filepath.Join(root, Subdir))+"/") {
			rel, _ := filepath.Rel(root, p)
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = string(b)
		}
		return nil
	})
	return out, err
}

func equalTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRecordWithStatusDefaultsPrivacyPrivate(t *testing.T) {
	home := t.TempDir()
	res, err := RecordWithStatus(home, Finding{
		Topic: "ordinary-save-private-default",
		Body:  OrdinarySaveBodySentinel,
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, Subdir, res.Finding.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if got := stored["privacy"]; got != "private" {
		t.Fatalf("stored privacy = %v, want private\n%s", got, raw)
	}
}

func TestRecordWithStatusDefaultsVisibilityOperational(t *testing.T) {
	home := t.TempDir()
	res, err := RecordWithStatus(home, Finding{
		Topic: "ordinary-save-operational-default",
		Body:  OrdinarySaveBodySentinel,
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, Subdir, res.Finding.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if got := stored["visibility"]; got != "operational" {
		t.Fatalf("stored visibility = %v, want operational\n%s", got, raw)
	}
}

func TestRecordWithStatusPersistsPrivateFileMode(t *testing.T) {
	home := t.TempDir()
	res, err := RecordWithStatus(home, Finding{
		Topic: "ordinary-save-private-mode",
		Body:  OrdinarySaveBodySentinel,
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, Subdir, res.Finding.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("stored finding mode = %04o, want 0600", got)
	}
}

func TestRecordWithStatusTightensFindingDirectoryMode(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, Subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, "find_legacy.json")
	legacyBytes := []byte("{\"legacy\":true}\n")
	if err := os.WriteFile(legacyPath, legacyBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RecordWithStatus(home, Finding{
		Topic: "ordinary-save-private-directory",
		Body:  OrdinarySaveBodySentinel,
	}, fixedNow); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("existing finding directory mode = %04o, want tightened 0700", got)
	}
	keptLegacy, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(keptLegacy) != string(legacyBytes) {
		t.Fatalf("tightening directory permissions changed a pre-existing record")
	}
}

func TestRecordWithStatusPreExistingDuplicateSyncsDirectoryBeforeSuccess(t *testing.T) {
	home := t.TempDir()
	first, err := RecordWithStatus(home, Finding{
		Topic:      "ordinary-save-pre-existing-sync",
		Body:       OrdinarySaveBodySentinel,
		WorkID:     "work_first",
		ProducedBy: "agent:first",
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	originalSync := syncFindingDirectoryFunc
	t.Cleanup(func() {
		syncFindingDirectoryFunc = originalSync
	})
	syncCalls := 0
	syncFindingDirectoryFunc = func(path string) error {
		syncCalls++
		return syncDir(path)
	}

	duplicate, err := RecordWithStatus(home, Finding{
		Topic:      first.Finding.Topic,
		Body:       first.Finding.Body,
		WorkID:     "work_retry",
		ProducedBy: "agent:retry",
	}, fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatalf("pre-existing duplicate: %v", err)
	}
	if duplicate.Status != StatusDuplicate || duplicate.Finding.ID != first.Finding.ID {
		t.Fatalf("duplicate result = %+v, want kept %s", duplicate, first.Finding.ID)
	}
	if syncCalls != 1 {
		t.Fatalf("pre-existing duplicate directory sync calls = %d, want 1 before success", syncCalls)
	}
}

func TestRecordWithStatusPreExistingDuplicateSyncFailureReturnsKeptResult(t *testing.T) {
	home := t.TempDir()
	first, err := RecordWithStatus(home, Finding{
		Topic:        "ordinary-save-pre-existing-sync-failure",
		Body:         OrdinarySaveBodySentinel,
		WorkID:       "work_first",
		ProducedBy:   "agent:first",
		EvidenceRefs: []string{OrdinarySaveEvidenceSentinel},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, Subdir, first.Finding.ID+".json")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	originalSync := syncFindingDirectoryFunc
	t.Cleanup(func() {
		syncFindingDirectoryFunc = originalSync
	})
	syncErr := errors.New("injected pre-existing duplicate sync failure")
	syncFindingDirectoryFunc = func(string) error {
		return syncErr
	}

	duplicate, err := RecordWithStatus(home, Finding{
		Topic:      first.Finding.Topic,
		Body:       first.Finding.Body,
		WorkID:     "work_retry",
		ProducedBy: "agent:retry",
	}, fixedNow.Add(time.Minute))
	if !errors.Is(err, syncErr) {
		t.Fatalf("pre-existing duplicate sync error = %v, want injected failure", err)
	}
	if duplicate.Status != StatusDuplicate {
		t.Fatalf("duplicate status on sync failure = %q, want %q", duplicate.Status, StatusDuplicate)
	}
	if duplicate.Finding.ID != first.Finding.ID ||
		duplicate.Finding.WorkID != first.Finding.WorkID ||
		duplicate.Finding.ProducedBy != first.Finding.ProducedBy {
		t.Fatalf("sync failure did not retain kept finding: got %+v want %+v", duplicate.Finding, first.Finding)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("duplicate sync failure changed first-writer bytes:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestRecordWithStatusConcurrentLoserSyncFailureReturnsKeptDuplicate(t *testing.T) {
	home := t.TempDir()
	originalLink := linkFindingFile
	originalSync := syncFindingDirectoryFunc
	t.Cleanup(func() {
		linkFindingFile = originalLink
		syncFindingDirectoryFunc = originalSync
	})

	linkPublished := make(chan struct{})
	releaseWinner := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseWinner)
		})
	}
	t.Cleanup(release)

	linkFindingFile = func(tempPath, targetPath string) error {
		if err := os.Link(tempPath, targetPath); err != nil {
			return err
		}
		close(linkPublished)
		<-releaseWinner
		return nil
	}
	syncErr := errors.New("injected concurrent duplicate sync failure")
	syncFindingDirectoryFunc = func(string) error {
		return syncErr
	}

	type recordCall struct {
		result RecordResult
		err    error
	}
	winnerDone := make(chan recordCall, 1)
	go func() {
		result, err := RecordWithStatus(home, Finding{
			Topic:      "ordinary-save-concurrent-duplicate-sync",
			Body:       OrdinarySaveBodySentinel,
			WorkID:     "work_winner",
			ProducedBy: "agent:winner",
		}, fixedNow)
		winnerDone <- recordCall{result: result, err: err}
	}()

	select {
	case <-linkPublished:
	case <-time.After(5 * time.Second):
		t.Fatal("winner did not publish target before timeout")
	}

	loser, err := RecordWithStatus(home, Finding{
		Topic:      "ordinary-save-concurrent-duplicate-sync",
		Body:       OrdinarySaveBodySentinel,
		WorkID:     "work_loser",
		ProducedBy: "agent:loser",
	}, fixedNow.Add(time.Minute))
	if !errors.Is(err, syncErr) {
		t.Fatalf("concurrent loser sync error = %v, want injected failure", err)
	}
	if loser.Status != StatusDuplicate {
		t.Fatalf("concurrent loser status = %q, want %q", loser.Status, StatusDuplicate)
	}
	if loser.Finding.WorkID != "work_winner" || loser.Finding.ProducedBy != "agent:winner" {
		t.Fatalf("concurrent loser did not retain winner finding: %+v", loser.Finding)
	}
	target := filepath.Join(home, Subdir, loser.Finding.ID+".json")
	targetBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := json.MarshalIndent(&loser.Finding, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	wantBytes = append(wantBytes, '\n')
	if string(targetBytes) != string(wantBytes) {
		t.Fatalf("concurrent loser observed changed winner bytes:\ngot:\n%s\nwant:\n%s", targetBytes, wantBytes)
	}

	release()
	select {
	case winner := <-winnerDone:
		if !errors.Is(winner.err, syncErr) || winner.result.Status != StatusRecorded {
			t.Fatalf("winner completion = result:%+v err:%v, want recorded plus injected sync failure", winner.result, winner.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("winner did not complete after release")
	}
}

func TestRecordWithStatusEEXISTDuplicateSyncsDirectoryBeforeSuccess(t *testing.T) {
	home := t.TempDir()
	originalLink := linkFindingFile
	originalSync := syncFindingDirectoryFunc
	t.Cleanup(func() {
		linkFindingFile = originalLink
		syncFindingDirectoryFunc = originalSync
	})

	linkFindingFile = func(tempPath, targetPath string) error {
		if err := os.Link(tempPath, targetPath); err != nil {
			return err
		}
		return os.ErrExist
	}
	syncCalls := 0
	syncFindingDirectoryFunc = func(path string) error {
		syncCalls++
		return syncDir(path)
	}

	duplicate, err := RecordWithStatus(home, Finding{
		Topic:      "ordinary-save-eexist-sync",
		Body:       OrdinarySaveBodySentinel,
		WorkID:     "work_winner",
		ProducedBy: "agent:winner",
	}, fixedNow)
	if err != nil {
		t.Fatalf("EEXIST duplicate: %v", err)
	}
	if duplicate.Status != StatusDuplicate {
		t.Fatalf("EEXIST status = %q, want %q", duplicate.Status, StatusDuplicate)
	}
	if syncCalls != 1 {
		t.Fatalf("EEXIST duplicate directory sync calls = %d, want 1 before success", syncCalls)
	}
	dirInfo, err := os.Stat(filepath.Join(home, Subdir))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("EEXIST finding directory mode = %04o, want 0700", got)
	}
	targetInfo, err := os.Stat(filepath.Join(home, Subdir, duplicate.Finding.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := targetInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("EEXIST finding mode = %04o, want 0600", got)
	}
}

func TestRecordWithStatusEEXISTDuplicateSyncFailureReturnsKeptResult(t *testing.T) {
	home := t.TempDir()
	originalLink := linkFindingFile
	originalSync := syncFindingDirectoryFunc
	t.Cleanup(func() {
		linkFindingFile = originalLink
		syncFindingDirectoryFunc = originalSync
	})

	linkFindingFile = func(tempPath, targetPath string) error {
		if err := os.Link(tempPath, targetPath); err != nil {
			return err
		}
		return os.ErrExist
	}
	syncErr := errors.New("injected EEXIST duplicate sync failure")
	syncFindingDirectoryFunc = func(string) error {
		return syncErr
	}

	duplicate, err := RecordWithStatus(home, Finding{
		Topic:        "ordinary-save-eexist-sync-failure",
		Body:         OrdinarySaveBodySentinel,
		WorkID:       "work_winner",
		ProducedBy:   "agent:winner",
		EvidenceRefs: []string{OrdinarySaveEvidenceSentinel},
	}, fixedNow)
	if !errors.Is(err, syncErr) {
		t.Fatalf("EEXIST duplicate sync error = %v, want injected failure", err)
	}
	if duplicate.Status != StatusDuplicate || duplicate.Finding.WorkID != "work_winner" {
		t.Fatalf("EEXIST sync failure did not retain duplicate result: %+v", duplicate)
	}
	target := filepath.Join(home, Subdir, duplicate.Finding.ID+".json")
	targetBytes, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := json.MarshalIndent(&duplicate.Finding, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	wantBytes = append(wantBytes, '\n')
	if string(targetBytes) != string(wantBytes) {
		t.Fatalf("EEXIST sync failure changed winner bytes:\ngot:\n%s\nwant:\n%s", targetBytes, wantBytes)
	}
}

func TestRecordWithStatusEEXISTValidatesBeforeDirectorySync(t *testing.T) {
	home := t.TempDir()
	originalLink := linkFindingFile
	originalSync := syncFindingDirectoryFunc
	t.Cleanup(func() {
		linkFindingFile = originalLink
		syncFindingDirectoryFunc = originalSync
	})

	corrupt := []byte("{\"truncated\":")
	linkFindingFile = func(tempPath, targetPath string) error {
		if err := os.Link(tempPath, targetPath); err != nil {
			return err
		}
		if err := os.WriteFile(targetPath, corrupt, 0o600); err != nil {
			return err
		}
		return os.ErrExist
	}
	syncCalls := 0
	syncFindingDirectoryFunc = func(string) error {
		syncCalls++
		return nil
	}

	if _, err := RecordWithStatus(home, Finding{
		Topic: "ordinary-save-eexist-validate-first",
		Body:  OrdinarySaveBodySentinel,
	}, fixedNow); err == nil {
		t.Fatal("corrupt EEXIST target must fail validation")
	}
	if syncCalls != 0 {
		t.Fatalf("EEXIST corrupt target synced %d time(s), want validation failure before sync", syncCalls)
	}
	target := filepath.Join(home, Subdir, "find_"+digest12("ordinary-save-eexist-validate-first|"+OrdinarySaveBodySentinel)+".json")
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(corrupt) {
		t.Fatalf("EEXIST validation failure changed corrupt target bytes: %q", after)
	}
}
