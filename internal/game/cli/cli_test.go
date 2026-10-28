package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/game/source/gamesource"
	"compound/internal/sourceobs"
)

func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	prev := os.Getenv("COMPOUND_HOME")
	t.Setenv("COMPOUND_HOME", home)
	t.Cleanup(func() { _ = os.Setenv("COMPOUND_HOME", prev) })
	return home
}

func seedGameObsCli(t *testing.T, home, adapterID, sourceSys, kind, sourceKey, subject, statement string, when time.Time) {
	t.Helper()
	it := gamesource.RawObservation{
		Kind:       kind,
		SourceKey:  sourceKey,
		Subject:    subject,
		Statement:  statement,
		Confidence: 1.0,
		Evidence:   []string{"https://example.test/api", "revision:test"},
	}
	rev := "cli-test-" + time.Now().Format(time.RFC3339Nano)
	obs, err := gamesource.BuildObservation(adapterID, sourceSys, rev, it, when)
	if err != nil {
		t.Fatalf("seed build: %v", err)
	}
	if _, err := sourceobs.Apply(home, obs, when); err != nil {
		t.Fatalf("seed apply: %v", err)
	}
}

func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, rOut)
		stdout = b.String()
		close(outDone)
	}()
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, rErr)
		stderr = b.String()
		close(errDone)
	}()
	fn()
	_ = wOut.Close()
	_ = wErr.Close()
	<-outDone
	<-errDone
	os.Stdout = origOut
	os.Stderr = origErr
	return stdout, stderr
}

func TestCmdGameGenresJSONStable(t *testing.T) {
	home := withTempHome(t)
	now := time.Now().UTC()
	seedGameObsCli(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Team Fortress 2 is in genre "Action" on appid 440`,
		now.Add(-2*time.Hour))
	seedGameObsCli(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Dota 2 is in genre "Action" on appid 570`,
		now.Add(-1*time.Hour))
	var code int
	stdout, stderr := captureStdio(t, func() {
		code = cmdGameGenres([]string{"--json"})
	})
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("parse json: %v\nbody: %s", err, stdout)
	}
	if got["schema"] != "compound/game-genres.v0.1" {
		t.Errorf("schema: %v", got["schema"])
	}
	entries, _ := got["entries"].([]any)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(entries))
	}
}

func TestCmdGameDoctorEmptyHome(t *testing.T) {
	withTempHome(t)
	var code int
	stdout, stderr := captureStdio(t, func() {
		code = cmdGameDoctor([]string{"--json"})
	})
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("parse json: %v\nbody: %s", err, stdout)
	}
	if got["schema"] != "compound/game-doctor.v0.1" {
		t.Errorf("schema: %v", got["schema"])
	}
	codes, _ := got["codes"].(map[string]any)
	if codes["no_observations"] != float64(1) {
		t.Errorf("expected no_observations=1, got %v", codes)
	}
}

func TestCmdGameCompaniesJSON(t *testing.T) {
	home := withTempHome(t)
	now := time.Now().UTC()
	seedGameObsCli(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"company", "valve", "Valve", `Valve develops appid 440 (Team Fortress 2)`,
		now.Add(-2*time.Hour))
	seedGameObsCli(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"company", "valve", "Valve", `Valve publishes appid 440 (Team Fortress 2)`,
		now.Add(-1*time.Hour))
	var code int
	stdout, stderr := captureStdio(t, func() {
		code = cmdGameCompanies([]string{"--json"})
	})
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("parse json: %v\nbody: %s", err, stdout)
	}
	if got["schema"] != "compound/game-companies.v0.1" {
		t.Errorf("schema: %v", got["schema"])
	}
}

func TestCmdGameTrendingJSON(t *testing.T) {
	home := withTempHome(t)
	now := time.Now().UTC()
	seedGameObsCli(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Game is in genre "Action" on appid 440`,
		now.Add(-1*time.Hour))
	var code int
	stdout, stderr := captureStdio(t, func() {
		code = cmdGameTrending([]string{"--limit", "5", "--days", "7", "--json"})
	})
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("parse json: %v\nbody: %s", err, stdout)
	}
	if got["schema"] != "compound/game-trending.v0.1" {
		t.Errorf("schema: %v", got["schema"])
	}
}

func TestCmdGameObserveEmptyAppIDs(t *testing.T) {
	withTempHome(t)
	var code int
	_, stderr := captureStdio(t, func() {
		code = cmdGameObserve([]string{"steamweb"})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "--appids") {
		t.Errorf("expected --appids hint, got %q", stderr)
	}
}

func TestCmdGameObserveUnknownAdapter(t *testing.T) {
	withTempHome(t)
	var code int
	_, stderr := captureStdio(t, func() {
		code = cmdGameObserve([]string{"steamdbscrape", "--appids", "440"})
	})
	if code != 1 {
		t.Errorf("expected exit 1, got %d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "unknown adapter") {
		t.Errorf("expected unknown adapter message, got %q", stderr)
	}
}

func TestCmdGameObserveRejectsInvalidAppIDs(t *testing.T) {
	withTempHome(t)
	for _, in := range []string{"abc", "0", "-1", "440,xxx"} {
		var code int
		_, stderr := captureStdio(t, func() {
			code = cmdGameObserve([]string{"steamweb", "--appids", in})
		})
		if code != 2 {
			t.Errorf("appids=%q: expected exit 2, got %d stderr=%s", in, code, stderr)
		}
	}
}

func TestCmdGameRejectsUnknownFlag(t *testing.T) {
	withTempHome(t)
	cases := [][]string{
		{"genres", "--bad"},
		{"doctor", "--bogus"},
		{"trending", "--no-such-flag"},
	}
	for _, args := range cases {
		var code int
		_, stderr := captureStdio(t, func() {
			switch args[0] {
			case "genres":
				code = cmdGameGenres(args[1:])
			case "doctor":
				code = cmdGameDoctor(args[1:])
			case "trending":
				code = cmdGameTrending(args[1:])
			}
		})
		if code != 2 {
			t.Errorf("args=%v: expected exit 2, got %d stderr=%s", args, code, stderr)
		}
		if !strings.Contains(stderr, "unknown flag") {
			t.Errorf("args=%v: expected unknown flag, got %q", args, stderr)
		}
	}
}

func TestCmdGameDoctorSeedsSeesObs(t *testing.T) {
	home := withTempHome(t)
	now := time.Now().UTC()
	seedGameObsCli(t, home, gamesource.AdapterIDSteamWeb, gamesource.SourceSystemSteamWeb,
		"genre", "action", "Action", `Game is in genre "Action" on appid 440`,
		now.Add(-2*time.Hour))
	var code int
	stdout, stderr := captureStdio(t, func() {
		code = cmdGameDoctor([]string{"--json"})
	})
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("parse: %v\n%s", err, stdout)
	}
	counts, _ := got["counts"].(map[string]any)
	if counts["total_game_observations"] != float64(1) {
		t.Errorf("total_game_observations: %v", counts)
	}
	if counts["source:steamweb"] != float64(1) {
		t.Errorf("source:steamweb: %v", counts)
	}
}

func TestParseGameAppIDs(t *testing.T) {
	cases := []struct {
		in      string
		wantLen int
		wantErr bool
	}{
		{"440", 1, false},
		{"440,570", 2, false},
		{" 440 , 570 ", 2, false},
		{"", 0, true},
		{"abc", 0, true},
		{"0", 0, true},
		{"-1", 0, true},
		{"440,xxx", 0, true},
	}
	for _, c := range cases {
		got, err := parseGameAppIDs(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseGameAppIDs(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && len(got) != c.wantLen {
			t.Errorf("parseGameAppIDs(%q) got %d items, want %d", c.in, len(got), c.wantLen)
		}
	}
}

func TestGameSplitCSV(t *testing.T) {
	got := gameSplitCSV("a, b , ,c")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

func TestValidateGameFlags(t *testing.T) {
	if err := validateGameFlags("doctor", []string{"--json"}, map[string]bool{"--json": true}); err != nil {
		t.Errorf("valid flag rejected: %v", err)
	}
	if err := validateGameFlags("doctor", []string{"--bogus"}, map[string]bool{"--json": true}); err == nil {
		t.Errorf("invalid flag accepted")
	}
	if err := validateGameFlags("observe", []string{"steamweb", "--appids", "440"},
		map[string]bool{"--appids": true}); err != nil {
		t.Errorf("observe with positional + valid flag rejected: %v", err)
	}
}

func TestGameCliNeverHitsNetworkInTests(t *testing.T) {
	withTempHome(t)
	cases := []func() int{
		func() int { var c int; captureStdio(t, func() { c = cmdGameDoctor([]string{"--json"}) }); return c },
		func() int { var c int; captureStdio(t, func() { c = cmdGameGenres([]string{"--json"}) }); return c },
		func() int { var c int; captureStdio(t, func() { c = cmdGameCompanies([]string{"--json"}) }); return c },
		func() int { var c int; captureStdio(t, func() { c = cmdGameTrending([]string{"--json"}) }); return c },
	}
	for i, fn := range cases {
		if code := fn(); code != 0 {
			t.Errorf("read-only subcommand %d unexpectedly tried network: exit %d", i, code)
		}
	}
}
