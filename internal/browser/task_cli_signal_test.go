//go:build darwin || linux

package browser

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// The public compound binary must supply the production signal context all
// the way through Work finalization and Jev's owned process group.
func TestPublicBrowserGoalSIGTERMPreservesPartialAttempt(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "compound")
	build := exec.Command("go", "build", "-o", bin, "./cmd/compound")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public compound CLI: %v\n%s", err, output)
	}
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", testTime()); err != nil {
		t.Fatal(err)
	}
	req := testTask(homePath)
	if _, err := SetJevEnabled(homePath, req.WorkID, true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	marker := filepath.Join(stubDir, "first-step")
	stub := filepath.Join(stubDir, "bridge.py")
	stubSource := `import base64, hashlib, json, os, subprocess, sys, time
def send(**value):
    print(json.dumps(value), flush=True)
send(event="ready")
request = json.loads(sys.stdin.readline())
contract_bytes = base64.b64decode(request["contract_json"], validate=True)
if hashlib.sha256(contract_bytes).hexdigest() != request["contract_digest"]:
    sys.exit(2)
contract = json.loads(contract_bytes)
if contract["NonGoals"] != request["non_goals"] or contract["Goal"] != request["goal"]:
    sys.exit(2)
send(event="predict_request", step=1)
if not json.loads(sys.stdin.readline()).get("allow"):
    sys.exit(2)
send(event="proposal", step=1, kind="done", role="terminal", url=request["url"])
if not json.loads(sys.stdin.readline()).get("allow"):
    sys.exit(2)
send(event="acted", step=1, status="done", url=request["url"])
child = subprocess.Popen(["sleep", "30"])
with open(os.environ["COMPOUND_TEST_JEV_STEP_MARKER"], "w") as file:
    # Publish an empty file first to exercise the reader's readiness boundary.
    file.flush()
    time.sleep(0.1)
    file.write(str(child.pid))
while True:
    time.sleep(1)
`
	if err := os.WriteFile(stub, []byte(stubSource), 0o600); err != nil {
		t.Fatal(err)
	}
	uv := filepath.Join(stubDir, "uv")
	if err := os.WriteFile(uv, []byte("#!/bin/sh\nexec python3 \"$COMPOUND_TEST_JEV_STUB\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "browser", "goal", "--json", "--home", homePath, "--work", req.WorkID, "--actor", req.Actor, "--url", req.URL, "--goal", req.Goal, "--expect-text", req.ExpectText, "--max-steps", "2", "--provider-budget-usd", "0.10")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "PATH="+stubDir+":"+os.Getenv("PATH"), "COMPOUND_TEST_JEV_STUB="+stub, "COMPOUND_TEST_JEV_STEP_MARKER="+marker, "TYPESAFE_API_KEY=synthetic", "TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST=0.01")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(15 * time.Second)
	var childPID int
	var observedMarker []byte
	var markerParseErr error
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil {
			observedMarker = append([]byte(nil), data...)
			candidate, parseErr := strconv.Atoi(string(data))
			markerParseErr = parseErr
			if parseErr == nil && candidate > 1 {
				childPID = candidate
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("public CLI did not complete first step; marker=%q parse=%v stdout=%s stderr=%s", observedMarker, markerParseErr, stdout.String(), stderr.String())
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled public CLI reported success")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("public CLI did not finalize after SIGTERM")
	}
	var result TaskResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil || result.Verified || result.Steps != 1 || result.RunAttemptID == "" || result.WorkState != "verifying" {
		t.Fatalf("partial CLI result = %+v, decode=%v stdout=%s stderr=%s", result, err, stdout.String(), stderr.String())
	}
	attempt, err := runattempt.Open(homePath).Get(result.RunAttemptID)
	if err != nil || attempt.Status != runattempt.StatusFailed {
		t.Fatalf("cancelled attempt = %+v, %v", attempt, err)
	}
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Get(req.WorkID)
	if err != nil || w.State != "verifying" || !containsString(w.RunAttemptIDs, result.RunAttemptID) {
		t.Fatalf("partial work = %+v, %v", w, err)
	}
	childDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(childDeadline) {
		if err := syscall.Kill(childPID, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(childPID, 0); err == nil {
		t.Fatalf("synthetic bridge descendant %d survived cancellation", childPID)
	}
}
