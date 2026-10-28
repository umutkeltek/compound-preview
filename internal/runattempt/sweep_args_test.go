package runattempt

import (
	"strings"
	"testing"
	"time"
)

func TestParseSweepArgs(t *testing.T) {
	validID := "ratt_20260910T120000Z_deadbeef"
	parsed, err := ParseSweepArgs([]string{"--attempt", validID, "--threshold", "2m", "--home", "/tmp/home", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.HasAttempt || parsed.AttemptID != validID || parsed.Threshold != 2*time.Minute || parsed.HomePath != "/tmp/home" || !parsed.JSON {
		t.Fatalf("parsed = %+v", parsed)
	}
	firstValue, err := ParseSweepArgs([]string{"--threshold", "", "--threshold", "2m", "--home", "", "--home", "/tmp/home"})
	if err != nil {
		t.Fatal(err)
	}
	if firstValue.Threshold != DefaultSweepThreshold || firstValue.HomePath != "" {
		t.Fatalf("repeated first-value semantics = %+v", firstValue)
	}

	for _, args := range [][]string{
		{"--attempt"},
		{"--attempt", validID, "--attempt", validID},
		{"--attempt", "not-an-attempt"},
		{"--attempt", "../" + validID},
		{"--threshold", "0"},
		{"--threshold", "not-a-duration"},
		{"--no-probe"},
		{"unexpected"},
	} {
		if _, err := ParseSweepArgs(args); err == nil {
			t.Fatalf("ParseSweepArgs(%q) unexpectedly succeeded", args)
		}
	}

	_, err = ParseSweepArgs([]string{"--attempt", "--json"})
	if err == nil || !strings.Contains(err.Error(), "another flag") {
		t.Fatalf("flag-as-value error = %v", err)
	}
}
