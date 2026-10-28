package mem

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAppendReadRoundTrip(t *testing.T) {
	home := t.TempDir()

	evs := []RecallEvent{
		{Ts: "2024-01-01T00:00:00Z", Session: "sess-1", QueryHash: "abc123", ClaimIDs: []string{"c1", "c2"}, Engine: "engine-a"},
		{Ts: "2024-01-02T00:00:00Z", Session: "sess-2", QueryHash: "def456"},
	}

	for _, ev := range evs {
		if err := AppendRecallEvent(home, ev); err != nil {
			t.Fatalf("AppendRecallEvent(%+v): %v", ev, err)
		}
	}

	got, err := ReadRecallEvents(home)
	if err != nil {
		t.Fatalf("ReadRecallEvents: %v", err)
	}

	if len(got) != len(evs) {
		t.Fatalf("got %d events, want %d", len(got), len(evs))
	}

	for i := range evs {
		if !reflect.DeepEqual(got[i], evs[i]) {
			t.Errorf("event[%d]: got %+v, want %+v", i, got[i], evs[i])
		}
	}
}

func TestAppendIsAdditive(t *testing.T) {
	home := t.TempDir()

	ev1 := RecallEvent{Ts: "2024-01-01T00:00:00Z", QueryHash: "h1"}
	if err := AppendRecallEvent(home, ev1); err != nil {
		t.Fatalf("first append: %v", err)
	}

	ev2 := RecallEvent{Ts: "2024-01-02T00:00:00Z", QueryHash: "h2"}
	if err := AppendRecallEvent(home, ev2); err != nil {
		t.Fatalf("second append: %v", err)
	}

	got, err := ReadRecallEvents(home)
	if err != nil {
		t.Fatalf("ReadRecallEvents: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}

	if !reflect.DeepEqual(got[0], ev1) {
		t.Errorf("first event: got %+v, want %+v", got[0], ev1)
	}
	if !reflect.DeepEqual(got[1], ev2) {
		t.Errorf("second event: got %+v, want %+v", got[1], ev2)
	}
}

func TestReadNoFile(t *testing.T) {
	home := t.TempDir()

	got, err := ReadRecallEvents(home)
	if err != nil {
		t.Fatalf("expected nil error when file does not exist, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %d events", len(got))
	}
	if got == nil {
		t.Fatal("expected non-nil slice")
	}
}

func TestMalformedLine(t *testing.T) {
	home := t.TempDir()

	dir := home + "/ledger"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create ledger dir: %v", err)
	}

	f, err := os.Create(home + "/ledger/recall-events.jsonl")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	f.WriteString(`{"ts":"2024-01-01T00:00:00Z","query_hash":"h1"}` + "\n")
	f.WriteString(`not json` + "\n")
	f.Close()

	_, err = ReadRecallEvents(home)
	if err == nil {
		t.Fatal("expected error for malformed line")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should mention line number, got: %v", err)
	}
}

func TestAppendRejectsMissingTs(t *testing.T) {
	home := t.TempDir()
	ev := RecallEvent{QueryHash: "h1"}
	if err := AppendRecallEvent(home, ev); err == nil {
		t.Fatal("expected error for missing ts")
	}
}

func TestAppendRejectsNonRFC3339Ts(t *testing.T) {
	home := t.TempDir()
	ev := RecallEvent{Ts: "2024-01-01", QueryHash: "h1"}
	err := AppendRecallEvent(home, ev)
	if err == nil {
		t.Fatal("expected error for non-RFC3339 ts")
	}
	if !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("error should mention RFC3339, got %v", err)
	}
}

func TestAppendRejectsMissingQueryHash(t *testing.T) {
	home := t.TempDir()
	ev := RecallEvent{Ts: "2024-01-01T00:00:00Z"}
	if err := AppendRecallEvent(home, ev); err == nil {
		t.Fatal("expected error for missing query_hash")
	}
}
