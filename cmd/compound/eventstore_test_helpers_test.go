package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/eventstore"
)

func readTestPath(t *testing.T, path string) []byte {
	t.Helper()
	if homePath, id, ok := legacyEventTestPath(path); ok {
		store, err := eventstore.Open(homePath)
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.Read(id)
		if err != nil {
			t.Fatal(err)
		}
		return record.Bytes
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readTestEventJSON(t *testing.T, homePath, id string, target any) {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(record.Bytes, target); err != nil {
		t.Fatal(err)
	}
}

func testEventExists(t *testing.T, homePath, id string) bool {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	exists, err := store.Exists(id)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func testEventEntries(t *testing.T, homePath string, prefixes ...string) []eventstore.Entry {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: prefixes})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func legacyEventTestPath(path string) (homePath, id string, ok bool) {
	clean := filepath.Clean(path)
	if filepath.Base(filepath.Dir(clean)) != "events" || !strings.HasSuffix(filepath.Base(clean), ".json") {
		return "", "", false
	}
	id = strings.TrimSuffix(filepath.Base(clean), ".json")
	if _, err := eventstore.ReferenceForID(id); err != nil {
		return "", "", false
	}
	return filepath.Dir(filepath.Dir(clean)), id, true
}
