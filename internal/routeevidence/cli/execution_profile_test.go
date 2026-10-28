package routeevidencecli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/routeevidence"
)

func TestExecutionProfileJSONDoesNotCreateCOMPOUNDHOME(t *testing.T) {
	missingHome := filepath.Join(t.TempDir(), "must-remain-absent")
	t.Setenv("COMPOUND_HOME", missingHome)

	out := captureExecProfileStdout(t, func() {
		TryExecutionProfileCommand([]string{
			"execution-profile",
			"--profile", "mining-bulk",
			"--role", "miner",
			"--capability-class", "classify-extract",
			"--harness", "codex",
			"--transport", "switchback",
			"--model", "openai/gpt-5.6-sol",
			"--json",
		})
	})
	if _, err := os.Stat(missingHome); !os.IsNotExist(err) {
		t.Fatalf("execution-profile command created COMPOUND_HOME: %v", err)
	}

	var plan routeevidence.ExecutionProfilePlan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("parse standalone JSON: %v\n%s", err, out)
	}
	if plan.ProfileID != "mining-bulk" ||
		plan.ClientHarnessID != "codex" ||
		plan.TransportID != "switchback" ||
		plan.ModelConstraint != "openai/gpt-5.6-sol" ||
		plan.SemanticEffort != "economy" ||
		plan.NativeEffort.Requested != "low" {
		t.Fatalf("CLI output lost requested/resolved dimensions: %+v", plan)
	}
	if plan.EffectiveProvider != routeevidence.UnknownUntilReceipt || plan.EffectiveModel != routeevidence.UnknownUntilReceipt {
		t.Fatalf("CLI output overclaimed effective execution: %+v", plan)
	}
}

func TestExecutionProfileRejectsClaudeUltraWithPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunExecutionProfileCommand([]string{
		"--profile", "coding-implementation",
		"--harness", "claude-code",
		"--transport", "native",
		"--effort", "ultra",
		"--json",
	}, &stdout, &stderr)
	if code == 0 || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got := stderr.String(); !strings.Contains(got, "requested_intent.native_effort") || !strings.Contains(got, "claude-code") {
		t.Fatalf("missing path-addressed compatibility reason: %s", got)
	}
}

func TestExecutionProfileListsBuiltIns(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunExecutionProfileCommand([]string{"--list", "--json"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var profiles []routeevidence.ExecutionProfileDefinition
	if err := json.Unmarshal(stdout.Bytes(), &profiles); err != nil {
		t.Fatalf("parse list JSON: %v\n%s", err, stdout.String())
	}
	if len(profiles) != 5 || profiles[0].ID != "coding-implementation" {
		t.Fatalf("built-in catalog is not deterministic: %+v", profiles)
	}
}

func TestExecutionProfileUsageMentionsProfileFlag(t *testing.T) {
	if !strings.Contains(ExecutionProfileUsage(), "execution-profile") || !strings.Contains(ExecutionProfileUsage(), "--profile") {
		t.Fatalf("execution-profile usage omits --profile:\n%s", ExecutionProfileUsage())
	}
}

func captureExecProfileStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	type readResult struct {
		out []byte
		err error
	}
	readDone := make(chan readResult, 1)
	go func() {
		out, err := io.ReadAll(r)
		readDone <- readResult{out: out, err: err}
	}()

	os.Stdout = w
	defer func() {
		os.Stdout = old
		_ = w.Close()
		_ = r.Close()
	}()
	fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res := <-readDone
	if res.err != nil {
		t.Fatal(res.err)
	}
	return string(res.out)
}
