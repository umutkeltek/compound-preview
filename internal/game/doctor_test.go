package game

import (
	"testing"
	"time"

	"compound/internal/game/source/gamesource"
)

func TestDoctorEmptyHome(t *testing.T) {
	report := mustDoctor(t, t.TempDir(), time.Now())
	if report.Schema != SchemaGameDoctor {
		t.Errorf("schema: %q", report.Schema)
	}
	if report.Codes["no_observations"] != 1 {
		t.Errorf("expected no_observations=1, got %v", report.Codes)
	}
}

func TestDoctorFreshAndStale(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Game is in genre "Action" on appid 440`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:fresh"},
		now.Add(-2*time.Hour))
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "rpg", "RPG", `Game is in genre "RPG" on appid 1`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:stale"},
		now.Add(-30*24*time.Hour))

	report := mustDoctor(t, home, now)
	if report.Codes["stale_observation"] != 1 {
		t.Errorf("expected stale_observation=1, got %v", report.Codes)
	}
	if report.Counts["fresh"] != 1 {
		t.Errorf("expected fresh=1, got %v", report.Counts)
	}
	if report.Counts["stale"] != 1 {
		t.Errorf("expected stale=1, got %v", report.Counts)
	}
	if report.Counts["source:steamweb"] != 2 {
		t.Errorf("expected source:steamweb=2, got %v", report.Counts)
	}
}

func TestDoctorOnlyFresh(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedObs(t, home, gamesource.AdapterIDSteamSpy, gamesource.SourceSystemSteamSpy,
		"company", "valve", "Valve", `steamspy lists Valve as developer of appid 440`,
		[]string{"https://steamspy.com/api.php?request=appdetails", "revision:s1"},
		now.Add(-3*time.Hour))
	report := mustDoctor(t, home, now)
	if report.Codes["fresh"] != 1 {
		t.Errorf("expected fresh=1, got %v", report.Codes)
	}
	if report.Counts["source:steamspy"] != 1 {
		t.Errorf("expected source:steamspy=1, got %v", report.Counts)
	}
}

func mustDoctor(t *testing.T, home string, now time.Time) DoctorReport {
	t.Helper()
	r, err := Doctor(home, now)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	return r
}
