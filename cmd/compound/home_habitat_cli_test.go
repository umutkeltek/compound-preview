package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func TestHomeDoctorSurfacesUnknownRootsAndBridgeDebt(t *testing.T) {
	t.Parallel()
	h := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(h, "local", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h, "close-plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h, "invented-feature-root"), 0o755); err != nil {
		t.Fatal(err)
	}
	doctorReport := home.Doctor(h)
	textOut := strings.Join(homeDoctorHabitatLines(doctorReport), "\n")
	for _, want := range []string{
		"habitat: DIRTY",
		"habitat_unknown_root: invented-feature-root",
		"habitat_bridge_root: close-plans",
	} {
		if !strings.Contains(textOut, want) {
			t.Fatalf("home doctor missing %q:\n%s", want, textOut)
		}
	}

	jsonOut, err := json.Marshal(buildHomeDoctorReport(doctorReport))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Habitat home.HabitatInventory `json:"habitat"`
		Verdict string                `json:"verdict"`
	}
	if err := json.Unmarshal(jsonOut, &decoded); err != nil {
		t.Fatalf("parse home doctor JSON: %v\n%s", err, jsonOut)
	}
	if decoded.Verdict != "fail" || decoded.Habitat.OK || len(decoded.Habitat.UnknownRoots) != 1 || decoded.Habitat.UnknownRoots[0] != "invented-feature-root" || len(decoded.Habitat.BridgeRoots) != 1 || decoded.Habitat.BridgeRoots[0] != "close-plans" {
		t.Fatalf("home doctor JSON lost habitat truth: %+v", decoded)
	}
}
