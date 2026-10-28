package hook

import (
	"encoding/json"
	"testing"

	"compound/internal/eventstore"
)

func readHookEventRecord(t *testing.T, home, id string, target any) []byte {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(id)
	if err != nil {
		t.Fatalf("read event %s: %v", id, err)
	}
	if target != nil {
		if err := json.Unmarshal(record.Bytes, target); err != nil {
			t.Fatalf("parse event %s: %v", id, err)
		}
	}
	return record.Bytes
}

func hookEventRecordExists(t *testing.T, home, id string) bool {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	exists, err := store.Exists(id)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}
