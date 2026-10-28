package routeevidence

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- shared fixture helpers (synthetic values only; see AGENTS.md privacy law) ----

func marshalEvidence(t *testing.T, ev Evidence) string {
	t.Helper()
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writeLedgerLinesDirect overwrites the ledger with exactly these lines,
// each terminated with "\n", bypassing recordEvidenceLocked entirely -- it
// simulates hand-authored or out-of-band ledger content for the falsifiers
// below, not the package's own CAS writer.
func writeLedgerLinesDirect(t *testing.T, home string, lines []string) {
	t.Helper()
	path := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func indexFixtureRow(t *testing.T, requestID string, at time.Time) Evidence {
	t.Helper()
	ev, err := Build(BuildInput{
		CreatedAt:     at,
		RequestID:     requestID,
		WorkID:        "work_" + requestID,
		ClientProfile: "gateway.ingress",
		Protocol:      "openai-chat-completions",
		Provider:      "provider-x",
		Model:         "model-y",
		Status:        "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func seedRouteEvidenceLedgerDirect(t *testing.T, home string, n int, requestIDPrefix string) {
	t.Helper()
	dir := filepath.Join(home, "ledger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "execution-route-evidence.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < n; i++ {
		ev := indexFixtureRow(t, fmt.Sprintf("%s-%05d", requestIDPrefix, i), t0)
		if _, err := f.WriteString(marshalEvidence(t, ev) + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- F1: the load-bearing anti-quadratic depth falsifier ----
//
// Seed 2,000 rows directly (no package writer involved), then RecordJoined
// 200 NEW evidences through the real writer path. On the pre-change
// full-scan implementation, each of the 200 calls re-parses the entire
// (growing) ledger: linesParsed would be roughly 200 x 2000+ = 400,000+.
// The incremental index should do exactly one full rebuild (the unavoidable
// cold read of the pre-seeded 2,000 rows) and then one 1-line delta parse
// per subsequent call.
func TestUniqueEvidenceForRequestIDAmortizesAcrossAppends(t *testing.T) {
	home := t.TempDir()
	const seeded = 2000
	const appended = 200
	// Defensive: t.TempDir() already guarantees a fresh path per test, but
	// exercise the reset seam so it's proven live, not merely defined.
	resetRequestIDIndexForTest(home)
	seedRouteEvidenceLedgerDirect(t, home, seeded, "req-seed")

	for i := 0; i < appended; i++ {
		ev := indexFixtureRow(t, fmt.Sprintf("req-new-%05d", i), t0)
		if _, err := RecordJoined(home, ev); err != nil {
			t.Fatalf("record new evidence %d: %v", i, err)
		}
	}

	fullRebuilds, linesParsed := requestIDIndexCountersForTest(home)
	if fullRebuilds > 1 {
		t.Fatalf("fullRebuilds = %d, want <= 1 (amortized incremental parse, not a rescan per call)", fullRebuilds)
	}
	const slack = 250
	if want := int64(seeded + appended + slack); linesParsed > want {
		t.Fatalf("linesParsed = %d, want <= %d (O(new bytes) amortized, not O(full-ledger) per call)", linesParsed, want)
	}

	rows := strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl")))), "\n")
	if len(rows) != seeded+appended {
		t.Fatalf("ledger rows = %d, want %d", len(rows), seeded+appended)
	}
}

// ---- F2: idempotent re-record survives the index (fresh re-read, not a stale cache) ----
func TestRecordJoinedIdempotencyThroughIndexRereadsFreshEvidence(t *testing.T) {
	home := t.TempDir()
	ev := fixtureEvidence()
	first, err := RecordJoined(home, ev)
	if err != nil {
		t.Fatalf("record evidence: %v", err)
	}
	second, err := RecordJoined(home, ev)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if second.Evidence.ID != first.Evidence.ID || second.Transition.ID != first.Transition.ID {
		t.Fatalf("idempotent replay changed identity: first=%+v second=%+v", first, second)
	}
	rows := strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl")))), "\n")
	if len(rows) != 1 {
		t.Fatalf("idempotent replay persisted %d rows, want 1", len(rows))
	}

	altered := ev
	altered.Status = "failed"
	if _, err := RecordJoined(home, altered); err == nil || !strings.Contains(err.Error(), "already recorded as") {
		t.Fatalf("same request_id different payload err = %v, want already recorded as", err)
	}
	rows = strings.Split(strings.TrimSpace(string(readFile(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl")))), "\n")
	if len(rows) != 1 {
		t.Fatalf("rejected conflicting payload persisted %d rows, want 1", len(rows))
	}
}

// ---- F3: ambiguity across a shared request_id is preserved, not deduped ----
func TestUniqueEvidenceForRequestIDPreservesAmbiguity(t *testing.T) {
	home := t.TempDir()
	dup1 := indexFixtureRow(t, "req-dup", t0)
	dup2 := indexFixtureRow(t, "req-dup", t0.Add(time.Second))
	writeLedgerLinesDirect(t, home, []string{marshalEvidence(t, dup1), marshalEvidence(t, dup2)})

	_, ok, err := uniqueEvidenceForRequestID(home, "req-dup")
	if ok || err == nil || !strings.Contains(err.Error(), "ambiguous across 2 rows") {
		t.Fatalf("ok=%v err=%v, want ambiguous across 2 rows", ok, err)
	}
}

// ---- F4: cross-process append visibility (freshness keyed on size+modTime, never inode) ----
func TestLookupRequestIDSeesCrossProcessCASRenameAppend(t *testing.T) {
	home := t.TempDir()
	first := indexFixtureRow(t, "req-a", t0)
	writeLedgerLinesDirect(t, home, []string{marshalEvidence(t, first)})

	if _, ok, err := LookupRequestID(home, "req-a"); err != nil || !ok {
		t.Fatalf("warm lookup: ok=%v err=%v", ok, err)
	}

	ledgerPath := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	second := indexFixtureRow(t, "req-b", t0.Add(time.Minute))
	after := append(append([]byte(nil), before...), []byte(marshalEvidence(t, second)+"\n")...)

	beforeInfo, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate another process's CAS publish: full rewrite to a NEW file,
	// then rename over the canonical path -- this changes the inode while
	// preserving the content prefix, exactly like recordEvidenceLocked.
	swapPath := ledgerPath + ".swap"
	if err := os.WriteFile(swapPath, after, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swapPath, ledgerPath); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("test setup did not actually change the ledger's inode -- rename-over-path didn't take effect")
	}

	if _, ok, err := LookupRequestID(home, "req-b"); err != nil || !ok {
		t.Fatalf("cross-process appended request_id not found: ok=%v err=%v", ok, err)
	}
	if _, ok, err := LookupRequestID(home, "req-a"); err != nil || !ok {
		t.Fatalf("pre-existing request_id lost after cross-process append: ok=%v err=%v", ok, err)
	}
}

// ---- F5: shrink and same-size rewrite both force a full rebuild ----
func TestRequestIDIndexRebuildsOnShrink(t *testing.T) {
	home := t.TempDir()
	r1 := indexFixtureRow(t, "req-1", t0)
	r2 := indexFixtureRow(t, "req-2", t0.Add(time.Second))
	r3 := indexFixtureRow(t, "req-3", t0.Add(2*time.Second))
	writeLedgerLinesDirect(t, home, []string{marshalEvidence(t, r1), marshalEvidence(t, r2), marshalEvidence(t, r3)})

	for _, id := range []string{"req-1", "req-2", "req-3"} {
		if _, ok, err := LookupRequestID(home, id); err != nil || !ok {
			t.Fatalf("warm lookup %s: ok=%v err=%v", id, ok, err)
		}
	}

	writeLedgerLinesDirect(t, home, []string{marshalEvidence(t, r1)})

	if _, ok, err := LookupRequestID(home, "req-2"); err != nil || ok {
		t.Fatalf("req-2 survived a shrink: ok=%v err=%v", ok, err)
	}
	if _, ok, err := LookupRequestID(home, "req-3"); err != nil || ok {
		t.Fatalf("req-3 survived a shrink: ok=%v err=%v", ok, err)
	}
	if _, ok, err := LookupRequestID(home, "req-1"); err != nil || !ok {
		t.Fatalf("req-1 lost after shrink: ok=%v err=%v", ok, err)
	}
}

func TestRequestIDIndexRebuildsOnSameSizeRewrite(t *testing.T) {
	home := t.TempDir()
	oldRow := indexFixtureRow(t, "req-old-1", t0)
	newRow := indexFixtureRow(t, "req-new-1", t0)
	oldLine := marshalEvidence(t, oldRow)
	newLine := marshalEvidence(t, newRow)
	if len(oldLine) != len(newLine) {
		t.Fatalf("test fixture rows must be byte-identical in length to exercise the same-size path: old=%d new=%d", len(oldLine), len(newLine))
	}
	writeLedgerLinesDirect(t, home, []string{oldLine})

	if _, ok, err := LookupRequestID(home, "req-old-1"); err != nil || !ok {
		t.Fatalf("warm lookup: ok=%v err=%v", ok, err)
	}

	ledgerPath := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	beforeInfo, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	// Force an observably different modTime even on filesystems with coarse
	// mtime resolution, so this exercises the "same size, different modTime"
	// rewrite-paranoia branch specifically (not a false pass from a stale
	// same-second timestamp).
	rewriteAt := beforeInfo.ModTime().Add(time.Second)
	writeLedgerLinesDirect(t, home, []string{newLine})
	if err := os.Chtimes(ledgerPath, rewriteAt, rewriteAt); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := LookupRequestID(home, "req-old-1"); err != nil || ok {
		t.Fatalf("req-old-1 survived a same-size rewrite: ok=%v err=%v", ok, err)
	}
	if _, ok, err := LookupRequestID(home, "req-new-1"); err != nil || !ok {
		t.Fatalf("req-new-1 not found after same-size rewrite: ok=%v err=%v", ok, err)
	}
}

// ---- F6: parse-error parity (identical error shape, retry after fix succeeds, memo not poisoned) ----
func TestRequestIDIndexParseErrorParityAndRetryAfterFix(t *testing.T) {
	home := t.TempDir()
	lines := []string{
		marshalEvidence(t, indexFixtureRow(t, "req-1", t0)),
		marshalEvidence(t, indexFixtureRow(t, "req-2", t0.Add(time.Second))),
		`{not-valid-json`,
		marshalEvidence(t, indexFixtureRow(t, "req-4", t0.Add(3*time.Second))),
		marshalEvidence(t, indexFixtureRow(t, "req-5", t0.Add(4*time.Second))),
	}
	writeLedgerLinesDirect(t, home, lines)

	_, _, err := LookupRequestID(home, "req-5")
	if err == nil || !strings.Contains(err.Error(), "parse execution route evidence line 3") {
		t.Fatalf("err = %v, want parse error at line 3", err)
	}

	// Retry without any fix must reproduce the identical failure -- the memo
	// must not have advanced its cursor past the bad line.
	_, _, err = LookupRequestID(home, "req-5")
	if err == nil || !strings.Contains(err.Error(), "parse execution route evidence line 3") {
		t.Fatalf("retry err = %v, want identical parse error at line 3", err)
	}

	fixed := append([]string{}, lines...)
	fixed[2] = marshalEvidence(t, indexFixtureRow(t, "req-3", t0.Add(2*time.Second)))
	writeLedgerLinesDirect(t, home, fixed)

	for _, id := range []string{"req-1", "req-2", "req-3", "req-4", "req-5"} {
		if _, ok, err := LookupRequestID(home, id); err != nil || !ok {
			t.Fatalf("post-fix lookup %s: ok=%v err=%v", id, ok, err)
		}
	}
}

// ---- F7: trailing unterminated line cursor discipline ----
func TestRequestIDIndexTrailingUnterminatedLineCursorDiscipline(t *testing.T) {
	home := t.TempDir()
	row1 := indexFixtureRow(t, "req-term-1", t0)
	row2 := indexFixtureRow(t, "req-trail-2", t0.Add(time.Second))

	ledgerPath := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// row2 deliberately lacks a trailing newline -- the ledger's writer may
	// legitimately not have terminated it yet.
	body := marshalEvidence(t, row1) + "\n" + marshalEvidence(t, row2)
	if err := os.WriteFile(ledgerPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := LookupRequestID(home, "req-trail-2"); err != nil || !ok {
		t.Fatalf("unterminated trailing line not found: ok=%v err=%v", ok, err)
	}

	row3 := indexFixtureRow(t, "req-trail-3", t0.Add(2*time.Second))
	f, err := os.OpenFile(ledgerPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Out-of-band append: terminate row2 and add row3, properly terminated.
	if _, err := f.WriteString("\n" + marshalEvidence(t, row3) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"req-term-1", "req-trail-2", "req-trail-3"} {
		if _, ok, err := LookupRequestID(home, id); err != nil || !ok {
			t.Fatalf("lookup %s after out-of-band append: ok=%v err=%v", id, ok, err)
		}
	}
}
