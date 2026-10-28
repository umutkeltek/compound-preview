//go:build darwin

package browser

import (
	"strings"
	"testing"
)

func TestAddInstanceFindings_OrphanIdleSwiftshaderFires(t *testing.T) {
	inst := browserInstance{
		UUID:          "abcd1234",
		MainPID:       9999,
		ParentABPID:   7000,
		ParentABAlive: true,
		ParentABCPU:   "0.0%",
		Swiftshader:   true,
		Elapsed:       "1d2h3m",
	}
	report := browserDoctorReport{
		StaleMin: 60,
		IdlePct:  5,
		Codes:    map[string]int{},
	}
	addInstanceFindings(&report, inst)
	saw := false
	for _, f := range report.Findings {
		if f.Code == "orphan-idle-swiftshader" {
			saw = true
			if len(f.ReapTarget) == 0 {
				t.Fatalf("orphan-idle-swiftshader must carry ReapTarget, got %+v", f)
			}
			if f.ReapTarget[0] != 9999 {
				t.Fatalf("ReapTarget[0] should be main pid 9999, got %d", f.ReapTarget[0])
			}
		}
	}
	if !saw {
		t.Fatalf("expected orphan-idle-swiftshader finding, got %+v", report.Findings)
	}
}

func TestAddInstanceFindings_OrphanDoesNotFireWhenParentBusy(t *testing.T) {
	inst := browserInstance{
		UUID:          "abcd1234",
		MainPID:       9999,
		ParentABPID:   7000,
		ParentABAlive: true,
		ParentABCPU:   "47.0%",
		Swiftshader:   true,
		Elapsed:       "1d2h3m",
	}
	report := browserDoctorReport{StaleMin: 60, IdlePct: 5, Codes: map[string]int{}}
	addInstanceFindings(&report, inst)
	for _, f := range report.Findings {
		if f.Code == "orphan-idle-swiftshader" {
			t.Fatalf("must NOT fire when parent is busy (47%%): %+v", f)
		}
	}
}

func TestAddInstanceFindings_ParentMissingFires(t *testing.T) {
	inst := browserInstance{
		UUID: "abcd1234", MainPID: 9999,
		ParentABPID: 7000, ParentABAlive: false, ParentABCPU: "",
		Swiftshader: true, Elapsed: "1h",
	}
	report := browserDoctorReport{StaleMin: 60, IdlePct: 5, Codes: map[string]int{}}
	addInstanceFindings(&report, inst)
	got := false
	for _, f := range report.Findings {
		if f.Code == "parent-missing" {
			got = true
		}
	}
	if !got {
		t.Fatalf("parent-missing must fire when ab dead, got %+v", report.Findings)
	}
}

func TestAddInstanceFindings_NonOrchestratedFires(t *testing.T) {
	inst := browserInstance{
		UUID: "abcd1234", MainPID: 9999,
		ParentABPID: 0, ParentABAlive: false,
		Swiftshader: false, Elapsed: "5m",
	}
	report := browserDoctorReport{StaleMin: 60, IdlePct: 5, Codes: map[string]int{}}
	addInstanceFindings(&report, inst)
	got := false
	for _, f := range report.Findings {
		if f.Code == "non-orchestrated" {
			got = true
		}
	}
	if !got {
		t.Fatalf("non-orchestrated must fire when no ab ancestor, got %+v", report.Findings)
	}
}

func TestAddInstanceFindings_StaleInstanceFires(t *testing.T) {
	inst := browserInstance{
		UUID: "abcd1234", MainPID: 9999,
		ParentABPID: 0, ParentABAlive: false,
		Swiftshader: false, Elapsed: "3d0h0m",
	}
	report := browserDoctorReport{StaleMin: 60, IdlePct: 5, Codes: map[string]int{}}
	addInstanceFindings(&report, inst)
	got := false
	for _, f := range report.Findings {
		if f.Code == "stale-instance" {
			got = true
		}
	}
	if !got {
		t.Fatalf("stale-instance must fire for 3d0h0m elapsed with stale_min=60, got %+v", report.Findings)
	}
}

func TestIsChromeShape_OnlyMatchesChromeForTesting(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"chrome-for-testing", "/Users/tester/.agent-browser/browsers/chrome/.../MacOS/Google Chrome for Testing --type=gpu-process", true},
		{"personal-chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --type=gpu-process", false},
		{"brave", "/Applications/Brave Browser.app/.../chrome_crashpad_handler", false},
		{"electron", "/Applications/Cursor.app/.../chrome_crashpad_handler", false},
		{"puppeteer", "Puppeteer internal stuff", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isChromeShape(browserProcess{Command: c.cmd})
			if got != c.want {
				t.Fatalf("isChromeShape(%q) = %v, want %v", c.cmd, got, c.want)
			}
		})
	}
}

func TestClassifyHelper(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"... --type=gpu-process ...", "gpu-process"},
		{"... --type=renderer ...", "renderer"},
		{"...MacOS/Google Chrome for Testing --remote-debugging-port=0...", "main"},
		{"... Google Chrome for Testing Helper (GPU) ...", "gpu"},
		{"... Google Chrome for Testing Helper (Renderer) ...", "renderer"},
	}
	for _, c := range cases {
		got := classifyHelper(c.cmd)
		if got != c.want {
			t.Fatalf("classifyHelper(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestExtractUserDataUUID(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{`... --user-data-dir=/var/folders/zg/.../agent-browser-chrome-9fbccb12-86b8-4f1f-8da2-da60b6dbaa17 ...`, "9fbccb12-86b8-4f1f-8da2-da60b6dbaa17"},
		{`... --user-data-dir=/tmp/foo ...`, "foo"},
		{`... no user data dir ...`, ""},
	}
	for _, c := range cases {
		got := extractUserDataUUID(c.cmd)
		if got != c.want {
			t.Fatalf("extractUserDataUUID(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
	if !strings.Contains(extractUserDataDir(`... --user-data-dir=/tmp/x ...`), "/tmp/x") {
		t.Fatalf("extractUserDataDir broken: %q", extractUserDataDir(`... --user-data-dir=/tmp/x ...`))
	}
}

func TestParseEtime(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"05:30", 330},
		{"01:05:30", 3930},
		{"3-04:05:30", int64(3*86400 + 4*3600 + 5*60 + 30)},
		{"-01:00", 0}, // we treat unknown negatives as 0; not the user's primary path
		{"", 0},
	}
	for _, c := range cases {
		got := parseEtime(c.in)
		// normalize the negative case expectations
		if c.in == "-01:00" && got != 0 {
			t.Fatalf("parseEtime(%q) = %d, want 0", c.in, got)
		}
		if c.in != "-01:00" && got != c.want {
			t.Fatalf("parseEtime(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
