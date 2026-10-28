package mem

import (
	"os"
	"testing"
)

// Evidence is stored, retrievable, and content-addressed: recording the same content twice is
// idempotent (one item, stable id), and the item round-trips by id.
func TestEvidenceStoredAndIdempotent(t *testing.T) {
	s := newStore(t)
	e1, err := s.RecordEvidence(EvidenceItem{SourceKind: "codex", SourceID: "sess1", QuoteText: "we chose Go for the backend", Actor: "user"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if e1.ID == "" || e1.ContentHash == "" {
		t.Fatalf("recorded evidence must have an id + content hash; got %+v", e1)
	}
	// same content again → same id, no duplicate.
	e2, _ := s.RecordEvidence(EvidenceItem{SourceKind: "codex", SourceID: "sess1", QuoteText: "we chose Go for the backend", Actor: "user"}, t0)
	if e2.ID != e1.ID {
		t.Errorf("identical evidence must be content-addressed to the same id; %q != %q", e2.ID, e1.ID)
	}
	all, _ := s.Evidence()
	if len(all) != 1 {
		t.Errorf("recording identical evidence twice must store ONE item; got %d", len(all))
	}
	// round-trip by id.
	got, ok, _ := s.GetEvidence(e1.ID)
	if !ok || got.QuoteText != "we chose Go for the backend" {
		t.Errorf("evidence must round-trip by id; got ok=%v %+v", ok, got)
	}
	// different content → different id.
	e3, _ := s.RecordEvidence(EvidenceItem{SourceKind: "codex", SourceID: "sess1", QuoteText: "actually Rust now"}, t0)
	if e3.ID == e1.ID {
		t.Error("different content must get a different id")
	}
}

// Arbitrary source_kind values are accepted; empty source/evidence is rejected.
func TestEvidenceValidation(t *testing.T) {
	s := newStore(t)
	if e, err := s.RecordEvidence(EvidenceItem{SourceKind: "podcast", QuoteText: "x"}, t0); err != nil || e.SourceKind != "podcast" {
		t.Fatalf("arbitrary source_kind should be accepted: evidence=%+v err=%v", e, err)
	}
	if _, err := s.RecordEvidence(EvidenceItem{QuoteText: "x"}, t0); err == nil {
		t.Error("empty source_kind must be rejected")
	}
	if _, err := s.RecordEvidence(EvidenceItem{SourceKind: "Bad Kind!", QuoteText: "x"}, t0); err == nil {
		t.Error("non-slug source_kind must be rejected")
	}
	if _, err := s.RecordEvidence(EvidenceItem{SourceKind: "note"}, t0); err == nil {
		t.Error("evidence with neither quote_text nor source_id must be rejected")
	}
}

// Legacy pre-ratchet evidence rows (UUID-suffixed ids from the old gbrain
// transcript import, extended-hex note ids, canonical-looking ids hashed by the
// old formula) cannot match the canonical content-address formula; the fold must
// pass them with structural checks instead of failing the whole ledger.
// Post-ratchet canonical rows still fail loud on any drift.
func TestEvidenceFoldToleratesLegacyRowsButNotCanonicalTampering(t *testing.T) {
	s := newStore(t)

	appendRaw := func(line string) {
		t.Helper()
		f, err := os.OpenFile(s.evidenceLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}

	// Legacy transcript row: UUID-suffixed id, session_id, non-canonical hash,
	// observed before the ratchet date.
	appendRaw(`{"id":"ev_3ea05141-2d1c-4f57-bb96-b89aee61699a_000","source_kind":"transcript","source_id":"3ea05141-2d1c-4f57-bb96-b89aee61699a","session_id":"3ea05141-2d1c-4f57-bb96-b89aee61699a","quote_text":"legacy transcript quote","content_hash":"628b6ec3ad523e7d","actor":"user","observed_at":"2026-06-24T12:44:04Z"}`)
	// Legacy canonical-looking row: ev_<12hex> id but old-formula hash, pre-ratchet.
	appendRaw(`{"id":"ev_47e4c659f035","source_kind":"note","source_id":"n1","quote_text":"legacy note quote","content_hash":"deadbeefdeadbeef","actor":"user","observed_at":"2026-06-30T12:00:00Z"}`)

	all, err := s.Evidence()
	if err != nil {
		t.Fatalf("fold must tolerate legacy evidence rows: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("fold must load both legacy rows; got %d", len(all))
	}

	// A POST-ratchet canonical row with a wrong hash is still a forgery: fail loud.
	canonical, err := s.RecordEvidence(EvidenceItem{SourceKind: "codex", SourceID: "s1", QuoteText: "tamper me", Actor: "user"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	tamperObserved := "2026-08-05T12:00:00Z"
	appendRaw(`{"id":"` + canonical.ID + `","source_kind":"codex","source_id":"DIFFERENT","quote_text":"tampered","content_hash":"00000000deadbeef","actor":"user","observed_at":"` + tamperObserved + `"}`)
	if _, err := s.Evidence(); err == nil {
		t.Error("post-ratchet canonical-format evidence with mismatched content address must fail the fold")
	}
}
