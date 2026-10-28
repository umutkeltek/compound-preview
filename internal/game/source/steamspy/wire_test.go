package steamspy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeEstimate(t *testing.T) {
	body := readFixtureSpy(t, "appdetail_440.json")
	got, err := decodeEstimate(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AppID != "440" || got.Name != "Team Fortress 2" || got.Owners == "" {
		t.Errorf("decoded mismatch: %+v", got)
	}
}

func TestDecodeEstimateMalformed(t *testing.T) {
	if _, err := decodeEstimate([]byte("{not valid")); err == nil {
		t.Fatalf("expected error")
	}
}

func readFixtureSpy(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}
