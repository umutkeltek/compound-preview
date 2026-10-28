package eventstore

import (
	"bytes"
	"errors"
	"testing"
)

func TestFrameCanonicalizesAndSelfValidates(t *testing.T) {
	record := []byte("{\n  \"schema_version\": \"compound.receipt.v0.1\",\n  \"id\": \"rec_frame\",\n  \"created_at\": \"2026-07-15T00:00:00Z\",\n  \"value\": {\"b\": 2, \"a\": 1}\n}\n")
	frame, canonicalRecord, err := buildFrame("rec_frame", record)
	if err != nil {
		t.Fatal(err)
	}
	if frame.FrameVersion != 1 || frame.Family != "receipt" || frame.ID != "rec_frame" {
		t.Fatalf("unexpected frame header: %+v", frame)
	}
	if bytes.Contains(canonicalRecord, []byte("\n")) {
		t.Fatalf("record was not compacted: %q", canonicalRecord)
	}
	line, err := encodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(line, []byte("\n")) != 1 || line[len(line)-1] != '\n' {
		t.Fatalf("frame is not exactly one JSONL line: %q", line)
	}
	decoded, err := decodeFrame(bytes.TrimSuffix(line, []byte("\n")))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SHA256 != frame.SHA256 || !bytes.Equal(decoded.Record, canonicalRecord) {
		t.Fatalf("decoded frame differs: got=%+v canonical=%s", decoded, canonicalRecord)
	}
}

func TestFrameRejectsIdentityAndChecksumDrift(t *testing.T) {
	if _, _, err := buildFrame("rec_arg", []byte(`{"id":"rec_body"}`)); err == nil {
		t.Fatal("embedded id mismatch unexpectedly accepted")
	}
	frame, _, err := buildFrame("rec_checksum", []byte(`{"id":"rec_checksum","schema_version":"compound.receipt.v0.1","created_at":"2026-07-15T00:00:00Z"}`))
	if err != nil {
		t.Fatal(err)
	}
	frame.SHA256 = "00"
	if err := validateFrame(frame); err == nil {
		t.Fatal("checksum drift unexpectedly accepted")
	} else {
		var invalid *FrameInvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("checksum error type = %T, want *FrameInvalidError: %v", err, err)
		}
	}
}

func TestFamilyUsesSchemaBeforeIDPrefixAndFallsBackToPrefix(t *testing.T) {
	if got := familyOf([]byte(`{"schema_version":"compound.transition.v0.1"}`), "rec_misleading"); got != "transition" {
		t.Fatalf("schema family = %q, want transition", got)
	}
	if got := familyOf([]byte(`{"id":"hook_fallback"}`), "hook_fallback"); got != "hook" {
		t.Fatalf("prefix family = %q, want hook", got)
	}
	if got := familyOf([]byte(`{"id":"unknown"}`), "unknown"); got != "other" {
		t.Fatalf("unknown family = %q, want other", got)
	}
}

// TestBuildFrameFailsClosedOnIDPrefixSchemaFamilyDisagreement proves the E4
// routing mismatch: familyOf(record, id) used by the WRITE path (buildFrame)
// resolves family schema-first, while every READ path resolves family via
// familyOf(nil, id) -- id-prefix ONLY, because a read has no record bytes yet
// to consult. A frame whose id-prefix (e.g. "rec_" -> receipt) disagrees with
// its schema-derived family (e.g. "compound.transition.v0.1" -> transition) is
// written into one family's segment but a same-id read looks in the OTHER
// family's segment and misses it -- silent data loss, never surfaced. The fix
// must reject such a frame at build time rather than writing it somewhere a
// same-id read can never find it.
func TestBuildFrameFailsClosedOnIDPrefixSchemaFamilyDisagreement(t *testing.T) {
	id := "rec_mismatched"
	record := []byte(`{"id":"` + id + `","schema_version":"compound.transition.v0.1","created_at":"2026-07-15T00:00:00Z"}`)

	// Precondition proving the disagreement is real, not a typo in the test:
	// the write-side family (schema-first) and the read-side family (id-prefix
	// only) name two DIFFERENT families for the exact same id+record.
	writeFamily := familyOf(record, id)
	readFamily := familyOf(nil, id)
	if writeFamily == readFamily {
		t.Fatalf("test fixture does not exercise a disagreement: write family = read family = %q", writeFamily)
	}
	if writeFamily != "transition" || readFamily != "receipt" {
		t.Fatalf("unexpected families: write=%q read=%q, want write=transition read=receipt", writeFamily, readFamily)
	}

	if _, _, err := buildFrame(id, record); err == nil {
		t.Fatalf("buildFrame accepted a frame whose id-prefix family (%q) disagrees with its schema family (%q) -- it would be written under %q but every read routes it to %q and misses it", readFamily, writeFamily, writeFamily, readFamily)
	} else {
		var invalid *FrameInvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("disagreement error type = %T, want *FrameInvalidError: %v", err, err)
		}
	}
}
