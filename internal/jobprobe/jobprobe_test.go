package jobprobe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/authority"
)

var probeUnitNow = time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)

// fakeRunner returns canned verdicts keyed by JobID (falling back to Name), so
// tests never touch the real system. A missing key is an explicit probe error.
type fakeRunner struct {
	byKey map[string]ProbeResult
	errs  map[string]string
}

func (f fakeRunner) Probe(target ProbeTarget, _ time.Time) (ProbeResult, error) {
	key := target.JobID
	if key == "" {
		key = target.Name
	}
	if msg, ok := f.errs[key]; ok {
		return ProbeResult{}, &probeErr{msg}
	}
	if res, ok := f.byKey[key]; ok {
		return res, nil
	}
	return ProbeResult{}, &probeErr{"no fake verdict for " + key}
}

type probeErr struct{ msg string }

func (e *probeErr) Error() string { return e.msg }

func importUnitRoster(t *testing.T, home, body string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "cron-jobs.json")
	if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ImportJobRoster(home, source, probeUnitNow); err != nil {
		t.Fatalf("import roster: %v", err)
	}
}

func writeTargets(t *testing.T, home string, targets ...ProbeTarget) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(ProbeTargetsPath(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(ProbeTargetsFile{SchemaVersion: ProbeTargetsSchema, Targets: targets}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProbeTargetsPath(home), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectRefreshesRosterFromFakeProbe(t *testing.T) {
	home := t.TempDir()
	importUnitRoster(t, home, `{
  "version": 1,
  "jobs": [
    {"job_id": "worker-a", "name": "worker a", "enabled": true, "state": "scheduled", "last_status": "ok"}
  ]
}`)
	writeTargets(t, home, ProbeTarget{JobID: "worker-a", Name: "worker a", Kind: KindLocalLaunchd, Unit: "com.example.worker-a"})

	runner := fakeRunner{byKey: map[string]ProbeResult{
		"worker-a": {Alive: false, Detail: "fake: unit not loaded"},
	}}
	report, err := Collect(home, runner, probeUnitNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if report.TargetCount != 1 || report.ProbedCount != 1 || report.DeadCount != 1 {
		t.Fatalf("unexpected collect counts: %+v", report)
	}
	if report.Refresh.FailureRefreshedCount != 1 {
		t.Fatalf("collect must refresh the roster failure state: %+v", report.Refresh)
	}
	roster, err := authority.LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if roster.Jobs[0].LastStatus != "error" || roster.Jobs[0].ProbeState != "dead" {
		t.Fatalf("row not refreshed to observed failure: %+v", roster.Jobs[0])
	}
}

func TestCollectMissingTargetsFileIsCleanNoOp(t *testing.T) {
	home := t.TempDir()
	importUnitRoster(t, home, `{"version":1,"jobs":[{"job_id":"x","name":"x","enabled":true,"state":"scheduled","last_status":"ok"}]}`)
	report, err := Collect(home, fakeRunner{}, probeUnitNow)
	if err != nil {
		t.Fatalf("collect with no targets must not error: %v", err)
	}
	if report.TargetCount != 0 || report.Refresh.MatchedCount != 0 {
		t.Fatalf("no-targets run must be a no-op, got %+v", report)
	}
	if len(report.Warnings) == 0 {
		t.Fatalf("no-targets run should warn")
	}
}

func TestCollectProbeErrorDoesNotFabricateFailure(t *testing.T) {
	home := t.TempDir()
	importUnitRoster(t, home, `{"version":1,"jobs":[{"job_id":"worker-b","name":"worker b","enabled":true,"state":"scheduled","last_status":"ok"}]}`)
	writeTargets(t, home, ProbeTarget{JobID: "worker-b", Kind: KindLocalCron, Match: "worker-b"})
	runner := fakeRunner{errs: map[string]string{"worker-b": "crontab unavailable"}}
	report, err := Collect(home, runner, probeUnitNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if report.ErrorCount != 1 || report.DeadCount != 0 {
		t.Fatalf("a probe error must not be counted as a dead unit: %+v", report)
	}
	roster, err := authority.LoadJobRosterAuthority(home)
	if err != nil {
		t.Fatal(err)
	}
	if roster.Jobs[0].LastStatus != "ok" {
		t.Fatalf("a probe error must not fabricate a roster failure, got %q", roster.Jobs[0].LastStatus)
	}
}

func TestValidateTargetRejectsIncompleteTargets(t *testing.T) {
	cases := []ProbeTarget{
		{Kind: KindLocalLaunchd, Unit: "u"},  // missing job_id + name
		{JobID: "j", Kind: KindLocalLaunchd}, // launchd missing unit
		{JobID: "j", Kind: KindLocalCron},    // cron missing match
		{JobID: "j", Kind: KindRemote},       // remote missing command
		{JobID: "j", Kind: "bogus"},          // unknown kind
	}
	for i, tc := range cases {
		if err := validateTarget(tc); err == nil {
			t.Fatalf("case %d should be rejected: %+v", i, tc)
		}
	}
	ok := []ProbeTarget{
		{JobID: "j", Kind: KindLocalLaunchd, Unit: "u"},
		{Name: "n", Kind: KindLocalCron, Match: "m"},
		{JobID: "j", Kind: KindRemote, ProbeCommand: []string{"true"}},
	}
	for i, tc := range ok {
		if err := validateTarget(tc); err != nil {
			t.Fatalf("valid case %d rejected: %v", i, err)
		}
	}
}

// fakeCommandRunner records the exact argv and returns canned exit codes, so the
// SystemProbeRunner shell-out parsing is proven without touching launchctl /
// crontab / any real host.
type fakeCommandRunner struct {
	calls  [][]string
	stdout string
	code   int
	runErr error
}

func (f *fakeCommandRunner) Run(name string, args ...string) (string, int, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.stdout, f.code, f.runErr
}

func TestSystemProbeRunnerLaunchd(t *testing.T) {
	loaded := &fakeCommandRunner{code: 0}
	res, err := SystemProbeRunner{Cmd: loaded}.Probe(ProbeTarget{Kind: KindLocalLaunchd, Unit: "com.example.svc"}, probeUnitNow)
	if err != nil || !res.Alive {
		t.Fatalf("exit 0 launchctl must read alive: res=%+v err=%v", res, err)
	}
	if len(loaded.calls) != 1 || loaded.calls[0][0] != "launchctl" || loaded.calls[0][2] != "com.example.svc" {
		t.Fatalf("unexpected launchctl argv: %+v", loaded.calls)
	}
	notLoaded := &fakeCommandRunner{code: 113}
	res, err = SystemProbeRunner{Cmd: notLoaded}.Probe(ProbeTarget{Kind: KindLocalLaunchd, Unit: "com.example.svc"}, probeUnitNow)
	if err != nil || res.Alive {
		t.Fatalf("non-zero launchctl must read dead: res=%+v err=%v", res, err)
	}
}

func TestSystemProbeRunnerCron(t *testing.T) {
	present := &fakeCommandRunner{code: 0, stdout: "0 3 * * * /usr/local/bin/example-worker\n"}
	res, err := SystemProbeRunner{Cmd: present}.Probe(ProbeTarget{Kind: KindLocalCron, Match: "example-worker"}, probeUnitNow)
	if err != nil || !res.Alive {
		t.Fatalf("matching crontab must read alive: res=%+v err=%v", res, err)
	}
	absent := &fakeCommandRunner{code: 0, stdout: "0 3 * * * /usr/local/bin/other\n"}
	res, err = SystemProbeRunner{Cmd: absent}.Probe(ProbeTarget{Kind: KindLocalCron, Match: "example-worker"}, probeUnitNow)
	if err != nil || res.Alive {
		t.Fatalf("absent crontab entry must read dead: res=%+v err=%v", res, err)
	}
}

func TestSystemProbeRunnerRemoteUsesDeclaredCommand(t *testing.T) {
	up := &fakeCommandRunner{code: 0}
	target := ProbeTarget{Kind: KindRemote, HostRef: "host-alias", ProbeCommand: []string{"declared-probe", "--check", "svc"}}
	res, err := SystemProbeRunner{Cmd: up}.Probe(target, probeUnitNow)
	if err != nil || !res.Alive {
		t.Fatalf("remote exit 0 must read alive: res=%+v err=%v", res, err)
	}
	if len(up.calls) != 1 || up.calls[0][0] != "declared-probe" || up.calls[0][2] != "svc" {
		t.Fatalf("remote probe must run the declared argv verbatim: %+v", up.calls)
	}
	down := &fakeCommandRunner{code: 1}
	res, err = SystemProbeRunner{Cmd: down}.Probe(target, probeUnitNow)
	if err != nil || res.Alive {
		t.Fatalf("remote non-zero must read dead: res=%+v err=%v", res, err)
	}
}
