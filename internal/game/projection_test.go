package game

import (
	"encoding/json"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"compound/internal/game/source/gamesource"
	"compound/internal/sourceobs"
)

var seedRevCounter int64

func seedObs(t *testing.T, home string, adapterID, sourceSys, kind, sourceKey, subject, statement string, evidence []string, when time.Time) {
	t.Helper()
	it := gamesource.RawObservation{
		Kind:       kind,
		SourceKey:  sourceKey,
		Subject:    subject,
		Statement:  statement,
		Confidence: 1.0,
		Evidence:   evidence,
	}
	rev := "test-rev-" + strconv.FormatInt(atomic.AddInt64(&seedRevCounter, 1), 10)
	obs, err := gamesource.BuildObservation(adapterID, sourceSys, rev, it, when)
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
	if _, err := sourceobs.Apply(home, obs, when); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
}

func TestProjectGenresGroupsBySourceKey(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Team Fortress 2 is in genre "Action" on appid 440`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:abc"}, now.Add(-2*time.Hour))
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Dota 2 is in genre "Action" on appid 570`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:def"}, now.Add(-1*time.Hour))

	report, err := ProjectGenres(home, now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if report.Schema != SchemaGameGenres {
		t.Errorf("schema mismatch: %q", report.Schema)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(report.Entries))
	}
	e := report.Entries[0]
	if e.SourceKey != "action" {
		t.Errorf("source_key: %q", e.SourceKey)
	}
	if e.Label != "Action" {
		t.Errorf("label: %q", e.Label)
	}
	if e.ObservationCount != 2 {
		t.Errorf("observation_count: got %d, want 2", e.ObservationCount)
	}
	if len(e.Apps) != 2 {
		t.Errorf("apps: got %d, want 2 (%v)", len(e.Apps), e.Apps)
	}
}

func TestProjectCompaniesMergesRoles(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"company", "valve", "Valve", `Valve develops appid 440 (Team Fortress 2)`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:abc"}, now.Add(-2*time.Hour))
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"company", "valve", "Valve", `Valve publishes appid 440 (Team Fortress 2)`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:def"}, now.Add(-1*time.Hour))

	report, err := ProjectCompanies(home, now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(report.Entries))
	}
	e := report.Entries[0]
	if len(e.Roles) != 2 {
		t.Fatalf("expected 2 roles, got %d (%v)", len(e.Roles), e.Roles)
	}
	if e.Roles[0] != "developer" || e.Roles[1] != "publisher" {
		t.Errorf("roles: %v", e.Roles)
	}
}

func TestProjectGenresMarksStale(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "rpg", "RPG", `Some game is in genre "RPG" on appid 1`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:old"},
		now.Add(-30*24*time.Hour))
	report, err := ProjectGenres(home, now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if len(report.Entries) != 1 || !report.Entries[0].Stale {
		t.Errorf("expected stale entry; got %+v", report.Entries)
	}
}

func TestProjectGenresEmptyHome(t *testing.T) {
	home := t.TempDir()
	report, err := ProjectGenres(home, time.Now())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if len(report.Entries) != 0 {
		t.Errorf("expected empty entries")
	}
	if report.Schema != SchemaGameGenres {
		t.Errorf("schema mismatch: %q", report.Schema)
	}
}

func TestProjectGenresMixedSources(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"company", "valve", "Valve", `Valve develops appid 440 (Team Fortress 2)`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:w"}, now.Add(-2*time.Hour))
	seedObs(t, home, gamesource.AdapterIDSteamSpy, gamesource.SourceSystemSteamSpy,
		"company", "valve", "Valve", `steamspy lists Valve as developer of appid 440`,
		[]string{"https://steamspy.com/api.php?request=appdetails", "revision:s"}, now.Add(-1*time.Hour))
	report, err := ProjectCompanies(home, now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if len(report.Entries) != 1 || report.Entries[0].Source != "both" {
		t.Errorf("expected one entry with source=both; got %+v", report.Entries)
	}
	if report.Entries[0].ObservationCount != 2 {
		t.Errorf("observation_count: %d", report.Entries[0].ObservationCount)
	}
}

func TestProjectTrendingTopN(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
			"genre", "action", "Action", `Game is in genre "Action" on appid 440`,
			[]string{"https://store.steampowered.com/api/appdetails", "revision:r"},
			now.Add(-time.Duration(i+1)*time.Hour))
	}
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "strategy", "Strategy", `Game is in genre "Strategy" on appid 570`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:r2"},
		now.Add(-30*time.Minute))
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "ancient", "Ancient", `Game is in genre "Ancient" on appid 1`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:r3"},
		now.Add(-30*24*time.Hour))

	report, err := ProjectTrending(home, now, 10, 7)
	if err != nil {
		t.Fatalf("trending: %v", err)
	}
	if report.Schema != SchemaGameTrending {
		t.Errorf("schema: %q", report.Schema)
	}
	if len(report.Entries) != 2 {
		t.Fatalf("expected 2 entries (ancient excluded by window), got %d", len(report.Entries))
	}
	if report.Entries[0].SourceKey != "action" || report.Entries[0].Hits != 3 {
		t.Errorf("top entry: %+v", report.Entries[0])
	}
	if report.Entries[1].SourceKey != "strategy" || report.Entries[1].Hits != 1 {
		t.Errorf("second entry: %+v", report.Entries[1])
	}
}

func TestCatalogJSONStableAcrossRuns(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC()
	seedObs(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Game is in genre "Action" on appid 440`,
		[]string{"https://store.steampowered.com/api/appdetails", "revision:a"},
		now.Add(-1*time.Hour))
	r1, err := ProjectGenres(home, now)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	r2, err := ProjectGenres(home, now)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(r2)
	if len(b1) != len(b2) {
		t.Errorf("byte length differs: %d vs %d", len(b1), len(b2))
	}
}

func TestParseGameSubject(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantKey  string
		wantLab  string
		wantOk   bool
	}{
		{"game:genre:action::Action", "genre", "action", "Action", true},
		{"game:company:valve::Valve", "company", "valve", "Valve", true},
		{"game:app:440::Hades", "app", "440", "Hades", true},
		{"game:genre:action", "genre", "action", "", true},
		{"", "", "", "", false},
		{"not-game:genre:x", "", "", "", false},
		{"game:genre", "", "", "", false},
	}
	for _, c := range cases {
		k, key, lab, _, ok := parseGameSubject(c.in)
		if ok != c.wantOk || k != c.wantKind || key != c.wantKey || lab != c.wantLab {
			t.Errorf("parseGameSubject(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.in, k, key, lab, ok, c.wantKind, c.wantKey, c.wantLab, c.wantOk)
		}
	}
}
