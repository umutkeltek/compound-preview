package steamweb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeAppDetailsSingle(t *testing.T) {
	body := readFixtureSteamweb(t, "appdetail_440.json")
	got, err := decodeAppDetails(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	entry, ok := got["440"]
	if !ok {
		t.Fatalf("missing appid 440 entry")
	}
	if !entry.Success || entry.Data == nil || entry.Data.Name != "Team Fortress 2" {
		t.Fatalf("entry mismatch: %+v", entry)
	}
	if len(entry.Data.Genres) != 2 {
		t.Fatalf("expected 2 genres, got %d", len(entry.Data.Genres))
	}
}

func TestDecodeAppDetailsMalformed(t *testing.T) {
	if _, err := decodeAppDetails([]byte("{not valid json")); err == nil {
		t.Fatalf("expected decode error")
	}
}

func TestDecodeAppDetailsNotFound(t *testing.T) {
	body := readFixtureSteamweb(t, "appdetail_404.json")
	got, err := decodeAppDetails(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	entry := got["9999999"]
	if entry.Success || entry.Data != nil {
		t.Fatalf("expected success=false and data nil")
	}
}

func readFixtureSteamweb(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}
