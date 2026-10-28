//go:build darwin

package schedule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic launchd plist covering every key the projection reads. Written
// to a temp dir so tests never consult a real LaunchAgents directory.
const testPlistXML = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>ai.compound.test-schedule</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/bin/true</string>
		<string>--flag</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>7</integer>
		<key>Minute</key>
		<integer>30</integer>
		<key>Weekday</key>
		<integer>2</integer>
	</dict>
	<key>EnvironmentVariables</key>
	<dict>
		<key>SCHEDULE_TEST_TOKEN</key>
		<string>synthetic-secret</string>
	</dict>
	<key>StandardOutPath</key>
	<string>/tmp/compound-schedule-test.log</string>
</dict>
</plist>
`

func TestReadPlistEntryParsesEveryProjectedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ai.compound.test-schedule.plist")
	if err := os.WriteFile(path, []byte(testPlistXML), 0o644); err != nil {
		t.Fatal(err)
	}
	e, err := readPlistEntry(path)
	if err != nil {
		t.Fatalf("readPlistEntry: %v", err)
	}
	if e.Label != "ai.compound.test-schedule" {
		t.Errorf("Label = %q", e.Label)
	}
	if len(e.ProgramArgs) != 2 || e.ProgramArgs[0] != "/usr/bin/true" || e.ProgramArgs[1] != "--flag" {
		t.Errorf("ProgramArgs = %v", e.ProgramArgs)
	}
	if !e.RunAtLoad {
		t.Error("RunAtLoad = false, want true")
	}
	if e.Calendar == nil || e.Calendar.Hour != 7 || e.Calendar.Minute != 30 || e.Calendar.Weekday != 2 {
		t.Errorf("Calendar = %+v", e.Calendar)
	}
	if e.Env["SCHEDULE_TEST_TOKEN"] != "synthetic-secret" {
		t.Errorf("Env keys = %d, SCHEDULE_TEST_TOKEN missing", len(e.Env))
	}
	if e.StdOutPath != "/tmp/compound-schedule-test.log" {
		t.Errorf("StdOutPath = %q", e.StdOutPath)
	}
	if e.DisabledByExt {
		t.Error("DisabledByExt = true for a live plist")
	}
}

func TestCollectLaunchAgentsProjectsDisabledExtension(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "live.plist"), []byte(testPlistXML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "off.plist.disabled"), []byte(testPlistXML), 0o644); err != nil {
		t.Fatal(err)
	}
	es, err := collectLaunchAgents(dir)
	if err != nil {
		t.Fatalf("collectLaunchAgents: %v", err)
	}
	if len(es) != 2 {
		t.Fatalf("entries = %d, want 2 (disabled extension must still project)", len(es))
	}
	var disabled int
	for _, e := range es {
		if e.DisabledByExt {
			disabled++
		}
	}
	if disabled != 1 {
		t.Errorf("disabled-by-extension entries = %d, want 1", disabled)
	}
}

func TestParseCrontabLinesLabelsAreUniquePerCommand(t *testing.T) {
	in := "# comment line\n" +
		"PROJECT=/tmp/somewhere\n" +
		"0 * * * * /usr/local/bin/job-one --run\n" +
		"0 * * * * /usr/local/bin/job-two --run\n" +
		"0 * * * * /usr/local/bin/job-one --run\n"
	es := parseCrontabLines(in)
	if len(es) != 3 {
		t.Fatalf("entries = %d, want 3 (comment + env stanza skipped)", len(es))
	}
	if es[0].Label == es[1].Label {
		t.Errorf("same-schedule different-command labels collide: %q", es[0].Label)
	}
	if es[0].Label != es[2].Label {
		t.Errorf("identical lines got unstable labels: %q vs %q", es[0].Label, es[2].Label)
	}
}

func TestValidateScheduleFlagsFailsClosedOnUnknown(t *testing.T) {
	cases := []struct {
		sub    string
		args   []string
		maxPos int
		wantOK bool
	}{
		{"list", []string{"--json", "--source", "user", "--namespace", "compound"}, 0, true},
		{"list", []string{"--include-env-values"}, 0, true},
		{"list", []string{"--sorce", "user"}, 0, false},
		{"list", []string{"stray-positional"}, 0, false},
		{"show", []string{"my-label", "--json"}, 1, true},
		{"show", []string{"a", "b"}, 1, false},
		{"show", []string{"--source", "user"}, 1, false},
		{"doctor", []string{"--stale-days", "30", "--json"}, 0, true},
		{"doctor", []string{"--stale-day", "30"}, 0, false},
	}
	for _, c := range cases {
		err := validateScheduleFlags(c.sub, c.args, c.maxPos)
		if (err == nil) != c.wantOK {
			t.Errorf("validateScheduleFlags(%s, %v) err=%v, wantOK=%v", c.sub, c.args, err, c.wantOK)
		}
	}
}

// TestValidateScheduleSourceCoversClauseA locks clause (a): the canonical set
// is accepted, every other value is rejected with an error that names the
// set so the operator can see what they should have typed. The default
// ("all") is intentionally NOT exercised here — cmdScheduleList only calls
// this helper when --source is passed explicitly.
func TestValidateScheduleSourceCoversClauseA(t *testing.T) {
	for _, ok := range scheduleValidSources {
		if err := validateScheduleSource(ok); err != nil {
			t.Errorf("validateScheduleSource(%q) = %v, want nil", ok, err)
		}
	}
	bad := []string{"", "all ", "user,system", "USER", "launchd", "anything-bogus"}
	for _, b := range bad {
		err := validateScheduleSource(b)
		if err == nil {
			t.Errorf("validateScheduleSource(%q) = nil, want error", b)
			continue
		}
		msg := err.Error()
		// Error message must name the valid set so the operator can recover.
		for _, ok := range scheduleValidSources {
			if !strings.Contains(msg, ok) {
				t.Errorf("error for %q does not name valid source %q: %s", b, ok, msg)
			}
		}
		if !strings.Contains(msg, "--source") {
			t.Errorf("error for %q does not mention --source flag: %s", b, msg)
		}
	}
}

func TestPrepareScheduleEntriesRedactsEnvValuesByDefault(t *testing.T) {
	in := []ScheduleEntry{
		{Label: "a", Namespace: "compound", Env: map[string]string{"TOKEN": "synthetic-secret", "PATH": "/bin"}},
		{Label: "b", Namespace: "other"},
	}
	out := prepareScheduleEntries(in, "", false)
	if len(out) != 2 {
		t.Fatalf("entries = %d, want 2", len(out))
	}
	if out[0].Env != nil {
		t.Errorf("Env values survived default redaction: %d keys", len(out[0].Env))
	}
	if len(out[0].EnvKeys) != 2 || out[0].EnvKeys[0] != "PATH" || out[0].EnvKeys[1] != "TOKEN" {
		t.Errorf("EnvKeys = %v, want sorted [PATH TOKEN]", out[0].EnvKeys)
	}

	kept := prepareScheduleEntries(in, "", true)
	if kept[0].Env["TOKEN"] != "synthetic-secret" {
		t.Error("opt-in --include-env-values lost env values")
	}

	filtered := prepareScheduleEntries(in, "compound", false)
	if len(filtered) != 1 || filtered[0].Label != "a" {
		t.Errorf("namespace filter through the shared seam wrong: %d entries", len(filtered))
	}
}
