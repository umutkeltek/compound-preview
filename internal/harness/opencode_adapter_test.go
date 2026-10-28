package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"compound/internal/routeevidence"
	"compound/internal/runattempt"
)

const openCodeFakeNative = `#!/usr/bin/python3
import json,os,sys,stat
from pathlib import Path
prompt=sys.stdin.read()
cfg=json.loads(os.environ['OPENCODE_CONFIG_CONTENT'])
root=Path(os.environ['XDG_DATA_HOME']).parent
assert Path.cwd()==root/'project'
assert not any(k in os.environ for k in ['OPENCODE_CONFIG','NODE_OPTIONS','BUN_OPTIONS','ANTHROPIC_API_KEY','EVIL_ENV'])
assert cfg['model']==cfg['small_model']=='fixture/control'
assert cfg['permission']=={'*':'deny'} and cfg['plugin']==[] and cfg['mcp']=={}
for relative in ['config/opencode/opencode.json','search-home/.opencode/opencode.json']:
 assert stat.S_IMODE((root/relative).stat().st_mode)==0o400
 assert json.loads((root/relative).read_text())==cfg
auth=json.loads((root/'data/opencode/auth.json').read_text())
assert list(auth)==['fixture'] and auth['fixture']['type']=='api'
assert stat.S_IMODE((root/'data/opencode/auth.json').stat().st_mode)==0o600
key=auth['fixture']['key']
if prompt=='error':
 print(json.dumps({'type':'error','error':{'message':key}}));sys.exit(0)
if prompt=='failure':
 print(key);print(key,file=sys.stderr);sys.exit(7)
if prompt=='timeout':
 import time
 print('partial retained',flush=True);time.sleep(30);sys.exit(0)
print(json.dumps({'type':'step_start','part':{}}))
print(json.dumps({'type':'text','part':{'text':'CONTROL_OK '+key}}))
print(json.dumps({'type':'step_finish','part':{'reason':'stop'}}))
`

// The post-effort floor: opencode now advertises effort-passable for the
// snapshot (which the adapter narrows to "low" only, before mint), still
// forbids any session pinning, and still observes through its own argv.
// "effort supported" here is honest: the narrow supported-value validation
// lives in adapter/preflight, not the capability row.
func TestOpenCodePromptOnlySelectionFloor(t *testing.T) {
	capability, ok := SelectionCapabilityFor("opencode")
	if !ok || !capability.ModelPassable || !capability.EffortPassable || capability.EffortObservable || capability.SessionPinnable || capability.ObservedVia != "opencode_argv" {
		t.Fatalf("incorrect native selection floor: %+v", capability)
	}
	if err := CheckSelectionApplicable("opencode", &runattempt.ExecutionSelectionSnapshot{RequestedModelID: "fixture/control"}); err != nil {
		t.Fatalf("opencode refused its own model selection: %v", err)
	}
	if err := CheckSelectionApplicable("opencode", &runattempt.ExecutionSelectionSnapshot{RequestedEffort: "low"}); err != nil {
		t.Fatalf("opencode refused an in-scope effort selection: %v", err)
	}
	row := DefaultRegistry().Harnesses["opencode"]
	if !strings.Contains(strings.Join(row.Limitations, " "), "registered executor is prompt-only") {
		t.Fatal("interactive native capabilities were not separated from prompt-only execution")
	}
	if err := CheckSelectionApplicable("os-harness-entrypoints", &runattempt.ExecutionSelectionSnapshot{RequestedModelID: "fixture/control"}); err == nil {
		t.Fatal("generic OS adapter silently accepted native model selection")
	}
}

func openCodeFixture(t *testing.T, endpoint string) (*OpenCodeAdapter, RunInput) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("native prompt-only adapter requires macOS sandbox-exec")
	}
	dir := t.TempDir()
	command := filepath.Join(dir, "opencode-fixture")
	if err := os.WriteFile(command, []byte(openCodeFakeNative), 0o700); err != nil {
		t.Fatal(err)
	}
	return openCodeFixtureCommand(t, endpoint, command)
}

func openCodeFixtureCommand(t *testing.T, endpoint, command string) (*OpenCodeAdapter, RunInput) {
	t.Helper()
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "models.yaml")
	body := fmt.Sprintf(`version: 1
purpose: synthetic prompt-only tuple
capability_classes: [build-implement]
models:
  fixture/control:
    lane: synthetic
    harness: opencode
    provider_route: fixture
    provider_endpoint: %q
    aliases: [%q]
    tier: premium
    packet_ceiling: repo-scoped
    capabilities: {build-implement: {level: capable, evidence: fixture}}
routing_defaults:
  build-implement: [fixture/control]
`, endpoint, command)
	if err := os.WriteFile(source, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(home, source, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _, revision, err := routeevidence.LoadModelCapabilitiesSnapshot(home)
	if err != nil {
		t.Fatal(err)
	}
	a := NewOpenCodeAdapter(runattempt.Open(home))
	a.authPath = filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(a.authPath, []byte(`{"fixture":{"type":"api","key":"synthetic-selected-credential"},"other":{"type":"api","key":"synthetic-unrelated-credential"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in := RunInput{WorkID: "work_opencode_fixture", Workspace: t.TempDir(), Prompt: "Return CONTROL_OK", Now: time.Now(), RequestedModelID: "fixture/control", RequestedProviderRoute: "fixture", ExpectedModelRouterRevision: revision, ActionIntentID: "act_opencode_fixture", StartGuard: func(_ context.Context, start func() error) error { return start() }}
	return a, in
}

func TestOpenCodePreflightRefusesCallerAndTupleDriftBeforeMint(t *testing.T) {
	a, in := openCodeFixture(t, "https://provider.invalid/v1")
	for name, change := range map[string]func(*RunInput){
		"argv": func(in *RunInput) { in.Args = []string{"--continue"} }, "env": func(in *RunInput) { in.Env = []string{"EVIL_ENV=1"} }, "command": func(in *RunInput) { in.Command = "/bin/sh" }, "permission": func(in *RunInput) { in.PermissionMode = "bypass" }, "revision": func(in *RunInput) { in.ExpectedModelRouterRevision = "stale" }, "provider": func(in *RunInput) { in.RequestedProviderRoute = "other" }, "model": func(in *RunInput) { in.RequestedModelID = "fixture/missing" }, "size": func(in *RunInput) { in.Prompt = strings.Repeat("x", openCodePromptLimit+1) },
	} {
		t.Run(name, func(t *testing.T) {
			copy := in
			change(&copy)
			if _, err := a.Preflight(context.Background(), copy); err == nil {
				t.Fatal("unsafe preflight accepted")
			}
		})
	}
	prepared, err := a.Preflight(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.opencodeLaunch.Root); !os.IsNotExist(err) {
		t.Fatal("preflight created isolation files")
	}
	if err := os.WriteFile(prepared.Command, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), prepared); err == nil {
		t.Fatal("changed native executable launched")
	}
	files, _ := filepath.Glob(filepath.Join(a.store.Home(), "run_attempts", "ratt_*.json"))
	if len(files) != 0 {
		t.Fatal("preflight refusal minted an attempt")
	}
}

func TestOpenCodeSelectionSnapshotAndLaunchProfileMismatchRefused(t *testing.T) {
	a, in := openCodeFixture(t, "https://provider.invalid/v1")
	in.LaunchProfileID = "fixture/control"
	in.ExecutionSelectionSnapshot = &runattempt.ExecutionSelectionSnapshot{Source: "saved", SelectionRevision: 1, Harness: "opencode", LaunchProfileID: "fixture/control", LaunchProfileRevision: in.ExpectedModelRouterRevision, RequestedModelID: "fixture/control", ProviderRoute: "fixture", EffortEvidence: "unsupported:no effort requested", NativeSessionEvidence: "unsupported:fresh isolated run"}
	prepared, err := a.Preflight(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*runattempt.ExecutionSelectionSnapshot){
		"model":    func(s *runattempt.ExecutionSelectionSnapshot) { s.RequestedModelID = "fixture/other" },
		"provider": func(s *runattempt.ExecutionSelectionSnapshot) { s.ProviderRoute = "other" },
		"revision": func(s *runattempt.ExecutionSelectionSnapshot) { s.LaunchProfileRevision = "stale" },
		"profile":  func(s *runattempt.ExecutionSelectionSnapshot) { s.LaunchProfileID = "fixture/other" },
		"harness":  func(s *runattempt.ExecutionSelectionSnapshot) { s.Harness = "os-harness-entrypoints" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := prepared
			snapshot := *prepared.ExecutionSelectionSnapshot
			change(&snapshot)
			copy.ExecutionSelectionSnapshot = &snapshot
			if out, err := a.Run(context.Background(), copy); err == nil || out.RunAttempt.ID != "" {
				t.Fatal("selection drift minted or launched")
			}
		})
	}
	copy := in
	copy.LaunchProfileID = "unrelated-profile"
	if _, err := a.Preflight(context.Background(), copy); err == nil {
		t.Fatal("unrelated initial launch profile accepted")
	}
	// Prepared work can receive the wire's generic five-minute timeout after
	// preflight. Run must reapply the stronger bound before prepared validation.
	prepared.Timeout = 5 * time.Minute
	out, err := a.Run(context.Background(), prepared)
	if err != nil || out.RunAttempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("wire default was not safely bounded: %s %v", out.RunAttempt.Status, err)
	}
}

func TestOpenCodeRejectsDirtyEndpointBeforeMint(t *testing.T) {
	userinfo := &url.URL{Scheme: "https", Host: "provider.invalid", Path: "/v1", User: url.UserPassword("synthetic-user", "synthetic-key")}
	for _, endpoint := range []string{"http://provider.invalid/v1", userinfo.String(), "https://provider.invalid/v1?token=secret", "https://provider.invalid/v1#secret", "https://provider.invalid:444/v1", "https://provider.invalid/v1?", "https://provider.invalid/v1#"} {
		t.Run(endpoint, func(t *testing.T) {
			a, in := openCodeFixture(t, endpoint)
			if _, err := a.Preflight(context.Background(), in); err == nil {
				t.Fatal("dirty endpoint accepted")
			}
		})
	}
}

func TestOpenCodeIsolationLifecycleAndSanitizedEvidence(t *testing.T) {
	for _, mode := range []string{"success", "error", "failure", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			a, in := openCodeFixture(t, "https://provider.invalid/v1")
			t.Setenv("EVIL_ENV", "canary")
			t.Setenv("OPENCODE_CONFIG", "/synthetic/ambient.invalid")
			t.Setenv("ANTHROPIC_API_KEY", "synthetic-unrelated-credential")
			if mode != "success" {
				in.Prompt = mode
			}
			if mode == "timeout" {
				in.Timeout = 500 * time.Millisecond
			}
			before, _ := os.ReadFile(a.authPath)
			prepared, err := a.Preflight(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			root := prepared.opencodeLaunch.Root
			out, runErr := a.Run(context.Background(), prepared)
			if out.RunAttempt.ID == "" || (out.RunAttempt.Status == runattempt.StatusSucceeded) != (mode == "success") {
				t.Fatalf("wrong lifecycle: status=%s err=%v", out.RunAttempt.Status, runErr)
			}
			if (runErr != nil) != (mode == "error") {
				t.Fatalf("wrong infrastructure/error-event result: %v", runErr)
			}
			if out.TimedOut != (mode == "timeout") {
				t.Fatalf("timeout result=%v", out.TimedOut)
			}
			if out.RunAttempt.Workspace != filepath.Join(root, "project") || out.RunAttempt.Command != "/usr/bin/sandbox-exec" {
				t.Fatal("actual command/cwd misrecorded")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("isolated credential/search root retained")
			}
			after, _ := os.ReadFile(a.authPath)
			if string(after) != string(before) {
				t.Fatal("original native auth changed")
			}
			transcript, err := os.ReadFile(out.TranscriptPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(transcript), "synthetic-selected-credential") || strings.Contains(string(transcript), "synthetic-unrelated-credential") {
				t.Fatal("credential persisted in transcript")
			}
			if mode == "success" && !strings.Contains(string(transcript), "CONTROL_OK [REDACTED]") {
				t.Fatalf("missing complete sanitized text: %s", transcript)
			}
		})
	}
}

func TestOpenCodeOutputRequiresCompleteTextAndRejectsErrors(t *testing.T) {
	for name, events := range map[string][]any{
		"provider-error": {map[string]any{"type": "error"}},
		"missing-finish": {map[string]any{"type": "text", "part": map[string]string{"text": "partial"}}},
		"length":         {map[string]any{"type": "text", "part": map[string]string{"text": "partial"}}, map[string]any{"type": "step_finish", "part": map[string]string{"reason": "length"}}},
		"tool":           {map[string]any{"type": "tool_use"}},
	} {
		t.Run(name, func(t *testing.T) {
			var body strings.Builder
			for _, event := range events {
				b, _ := json.Marshal(event)
				fmt.Fprintf(&body, "stdout: %s\n", b)
			}
			path := filepath.Join(t.TempDir(), "transcript")
			_ = os.WriteFile(path, []byte(body.String()), 0o600)
			if validateOpenCodeTranscript(subprocessEvidenceContext{TranscriptPath: path}) == nil {
				t.Fatal("incomplete/error output accepted")
			}
		})
	}
}

func TestOpenCodePrepareAndStartFailuresCleanCredentialSnapshot(t *testing.T) {
	for _, phase := range []string{"prepare", "start"} {
		t.Run(phase, func(t *testing.T) {
			a, in := openCodeFixture(t, "https://provider.invalid/v1")
			if phase == "start" {
				a.sandboxCommand = filepath.Join(t.TempDir(), "missing-sandbox")
			}
			prepared, err := a.Preflight(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "prepare" {
				if os.Chmod(a.authPath, 0o400) != nil {
					t.Fatal("fixture mode")
				}
			}
			out, err := a.Run(context.Background(), prepared)
			if err == nil || out.RunAttempt.ID == "" || out.RunAttempt.Status != runattempt.StatusFailed || strings.Contains(err.Error(), "synthetic-selected-credential") {
				t.Fatalf("failure not terminal/content-free: %s %v", out.RunAttempt.Status, err)
			}
			if _, err := os.Stat(prepared.opencodeLaunch.Root); !os.IsNotExist(err) {
				t.Fatal("failed setup/start retained isolated credential root")
			}
		})
	}
}

func TestOpenCodeDeletionFailureCannotTerminalizeSucceeded(t *testing.T) {
	a, in := openCodeFixture(t, "https://provider.invalid/v1")
	prepared, err := a.Preflight(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	root := prepared.opencodeLaunch.Root
	t.Cleanup(func() {
		if err := removeOpenCodeRoot(root); err != nil {
			t.Error(err)
		}
	})
	a.removeRoot = func(string) error { return os.ErrPermission }
	out, err := a.Run(context.Background(), prepared)
	if err == nil || !strings.Contains(err.Error(), "OPENCODE_CLEANUP_FAILED") || out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("cleanup failure claimed success: %s %v", out.RunAttempt.Status, err)
	}
	if _, err := os.Stat(filepath.Join(root, "data/opencode/auth.json")); err != nil {
		t.Fatal("deletion-failure falsifier did not retain the snapshot")
	}
	stored, err := a.store.Get(out.RunAttempt.ID)
	if err != nil || stored.Status != runattempt.StatusFailed {
		t.Fatalf("durable cleanup failure=%s %v", stored.Status, err)
	}
}

func TestOpenCodeSandboxDeniesImmutableSubtreeWritesWithWritableModes(t *testing.T) {
	a, in := openCodeFixture(t, "https://provider.invalid/v1")
	prepared, err := a.Preflight(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	l := prepared.opencodeLaunch
	if err := os.Mkdir(l.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeOpenCodeRoot(l.Root); err != nil {
			t.Error(err)
		}
	})
	if err := prepareOpenCodeFiles(l, "synthetic-key"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"config", "config/opencode", "search-home", "search-home/.opencode"} {
		_ = os.Chmod(filepath.Join(l.Root, dir), 0o700)
	}
	for _, file := range []string{"config/opencode/opencode.json", "search-home/.opencode/opencode.json"} {
		_ = os.Chmod(filepath.Join(l.Root, file), 0o644)
	}
	script := `import os,sys
from pathlib import Path
root=Path(sys.argv[1]); denied=0
for relative in ['config/opencode','search-home/.opencode']:
 d=root/relative;p=d/'opencode.json'
 operations=[lambda:p.write_text('changed'),lambda:p.chmod(0o600),lambda:p.rename(d/'moved.json'),lambda:p.unlink(),lambda:(d/'new').write_text('changed'),lambda:(d/'new-dir').mkdir(),lambda:d.rmdir(),lambda:d.chmod(0o777),lambda:d.rename(d.with_name('moved-dir'))]
 for op in operations:
  try:op()
  except PermissionError:denied+=1
  except OSError as e:
   if e.errno==1:denied+=1
   else:raise
  else:raise Exception('immutable subtree operation permitted')
(root/'tmp'/'allowed').write_text('runtime write allowed')
assert denied==18
print('denied=18 runtime_write=allowed')
`
	cmd := exec.Command("/usr/bin/sandbox-exec", "-p", a.sandboxProfile(l), "/usr/bin/python3", "-c", script, l.Root)
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "denied=18 runtime_write=allowed") {
		t.Fatalf("immutable fence failed: %v %s", err, output)
	}
}
