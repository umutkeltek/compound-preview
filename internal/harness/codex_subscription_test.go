package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func writeSyntheticCodexAuth(t *testing.T, dir string, body any, mode os.FileMode) []byte {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), data, mode); err != nil {
		t.Fatal(err)
	}
	return data
}

func syntheticChatGPTAuth() map[string]any {
	return map[string]any{
		"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "last_refresh": "2026-09-01T00:00:00Z",
		"tokens": map[string]any{
			"access_token": "synthetic-access-token", "id_token": syntheticIDToken(),
			"refresh_token": "synthetic-refresh-token", "account_id": "synthetic-account-id",
		},
	}
}

var syntheticIDTokenExpiry = time.Now().Add(time.Hour).Unix()

func syntheticIDToken() string {
	encode := base64.RawURLEncoding.EncodeToString
	payload := fmt.Sprintf(`{"sub":"synthetic-user","email":"fixture@example.invalid","exp":%d}`, syntheticIDTokenExpiry)
	return encode([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		encode([]byte(payload)) + "." +
		encode([]byte("synthetic-signature"))
}

func fixtureCodexAdapter(store *runattempt.Store) *CodexAdapter {
	a := NewCodexAdapter(store)
	a.verifyNativeExecutable = func(string) error { return nil }
	return a
}

func TestCodexNativeVerifierRefusesShellWrapperNamedCodex(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPATH(t, bin)
	if err := verifyNativeCodexExecutable(path); err == nil {
		t.Fatal("shell wrapper accepted as native executable")
	}
	a := NewCodexAdapter(runattempt.Open(t.TempDir()))
	_, err := a.Preflight(context.Background(), RunInput{PermissionMode: "plan"})
	if err == nil || !strings.Contains(err.Error(), "native codex") {
		t.Fatalf("plan mode accepted shell wrapper: %v", err)
	}
}

func TestCodexSubscriptionExecutableReplacementBeforeStartFailsClosed(t *testing.T) {
	source := t.TempDir()
	writeSyntheticCodexAuth(t, source, syntheticChatGPTAuth(), 0o600)
	bin := t.TempDir()
	path := filepath.Join(bin, "codex")
	marker := filepath.Join(t.TempDir(), "launched")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf launched > \"$LAUNCH_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPATH(t, bin)
	t.Setenv("CODEX_HOME", source)
	t.Setenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION", "1")
	t.Setenv("LAUNCH_MARKER", marker)
	out, err := fixtureCodexAdapter(runattempt.Open(t.TempDir())).Run(context.Background(), RunInput{
		WorkID: "work_executable_drift", Harness: "codex", Workspace: t.TempDir(), Command: "codex",
		Prompt: "fixture", RequestedModelID: "gpt-fixture", RequestedProviderRoute: "chatgpt-subscription", Now: time.Now().UTC(), preflighted: true,
		StartGuard: func(_ context.Context, start func() error) error {
			if err := os.Rename(path, path+".old"); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf launched > \"$LAUNCH_MARKER\"\n"), 0o755); err != nil {
				return err
			}
			return start()
		},
	})
	if err == nil || !strings.Contains(err.Error(), "changed before start") || out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("replacement was not rejected: out=%+v err=%v", out, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("replacement launched: %v", err)
	}
}

func TestCodexSubscriptionBindingUsesIsolatedSnapshot(t *testing.T) {
	source := t.TempDir()
	before := writeSyntheticCodexAuth(t, source, syntheticChatGPTAuth(), 0o600)
	capture := filepath.Join(t.TempDir(), "captured-auth.json")
	homeLog := filepath.Join(t.TempDir(), "isolated-home")
	argvLog := filepath.Join(t.TempDir(), "argv")
	bin := t.TempDir()
	script := "#!/bin/sh\ncp \"$CODEX_HOME/auth.json\" \"$AUTH_CAPTURE\" || exit 71\nprintf '%s' \"$CODEX_HOME\" > \"$HOME_CAPTURE\"\nprintf '%s\\n' \"$@\" > \"$ARGV_CAPTURE\"\n[ -z \"$OPENAI_API_KEY\" ] || exit 72\nprintf 'subscription fixture ok\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	withPATH(t, bin)
	t.Setenv("CODEX_HOME", source)
	t.Setenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION", "1")
	t.Setenv("OPENAI_API_KEY", "synthetic-paid-fallback-key")
	t.Setenv("AUTH_CAPTURE", capture)
	t.Setenv("HOME_CAPTURE", homeLog)
	t.Setenv("ARGV_CAPTURE", argvLog)
	output, err := fixtureCodexAdapter(runattempt.Open(t.TempDir())).Run(context.Background(), RunInput{
		WorkID: "work_subscription_fixture", Harness: "codex", Workspace: t.TempDir(),
		Prompt: "synthetic prompt", Command: "codex", RequestedModelID: "gpt-fixture", RequestedProviderRoute: "chatgpt-subscription", Now: time.Now().UTC(), preflighted: true,
		ExecutionSelectionSnapshot: &runattempt.ExecutionSelectionSnapshot{
			Source: "override", Harness: "codex", RequestedModelID: "gpt-fixture", RequestedEffort: "high",
		},
	})
	if err != nil || output.ExitCode != 0 {
		t.Fatalf("subscription run: output=%+v err=%v", output, err)
	}
	isolatedBytes, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var isolated struct {
		AuthMode    string `json:"auth_mode"`
		APIKey      any    `json:"OPENAI_API_KEY"`
		LastRefresh string `json:"last_refresh"`
		Tokens      struct {
			Access  string `json:"access_token"`
			ID      string `json:"id_token"`
			Refresh string `json:"refresh_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(isolatedBytes, &isolated); err != nil {
		t.Fatal(err)
	}
	if isolated.AuthMode != "chatgpt" || isolated.APIKey != nil || isolated.LastRefresh != "2026-09-01T00:00:00Z" || isolated.Tokens.Access != "synthetic-access-token" || isolated.Tokens.ID != syntheticIDToken() || isolated.Tokens.Account != "synthetic-account-id" || isolated.Tokens.Refresh != "" {
		t.Fatalf("isolated subscription shape invalid: mode=%q apiKey=%v access=%t id=%t account=%t refresh=%t", isolated.AuthMode, isolated.APIKey, isolated.Tokens.Access != "", isolated.Tokens.ID != "", isolated.Tokens.Account != "", isolated.Tokens.Refresh != "")
	}
	after, err := os.ReadFile(filepath.Join(source, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("source auth changed")
	}
	isolationPath, err := os.ReadFile(homeLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(isolationPath) == source {
		t.Fatal("child used native auth home")
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(argv), "-a\nnever\n-s\nread-only\n-m\ngpt-fixture\n-c\nmodel_reasoning_effort=\"high\"\nexec\n") {
		t.Fatalf("Codex read-only argv missing: %q", argv)
	}
	if !strings.Contains(string(argv), "--ignore-user-config\n--ignore-rules\n-c\nmodel_provider=\"openai\"\n--json\n") {
		t.Fatalf("Codex subscription provider/config isolation missing: %q", argv)
	}
	if output.RunAttempt.ExecutionSelection == nil || output.RunAttempt.ExecutionSelection.AppliedEffort != "high" || output.RunAttempt.ExecutionSelection.EffortEvidence != "argv:model_reasoning_effort" {
		t.Fatalf("native effort not attested on attempt: %+v", output.RunAttempt.ExecutionSelection)
	}
	if _, err := os.Stat(string(isolationPath)); !os.IsNotExist(err) {
		t.Fatalf("isolated auth home remains after run: %v", err)
	}
	transcript, err := os.ReadFile(output.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic-access-token", syntheticIDToken(), "synthetic-refresh-token", "synthetic-paid-fallback-key"} {
		if strings.Contains(string(transcript), secret) || strings.Contains(string(argv), secret) || strings.Contains(strings.Join(output.RunAttempt.ProofRefs, " "), secret) {
			t.Fatal("secret leaked to transcript or proof refs")
		}
	}
	second, err := fixtureCodexAdapter(runattempt.Open(t.TempDir())).Run(context.Background(), RunInput{
		WorkID: "work_subscription_fixture_second", Harness: "codex", Workspace: t.TempDir(),
		Prompt: "synthetic second prompt", Command: "codex", RequestedModelID: "gpt-fixture", RequestedProviderRoute: "chatgpt-subscription", Now: time.Now().UTC(), preflighted: true,
	})
	if err != nil || second.ExitCode != 0 {
		t.Fatalf("second subscription run: output=%+v err=%v", second, err)
	}
	secondHome, err := os.ReadFile(homeLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(secondHome) == string(isolationPath) {
		t.Fatal("distinct runs reused isolated auth home")
	}
}

func TestCodexSubscriptionBindingRefusesUnsafeSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "missing"},
		{name: "api key", prepare: func(t *testing.T, dir string) {
			auth := syntheticChatGPTAuth()
			auth["OPENAI_API_KEY"] = "synthetic-paid-key"
			writeSyntheticCodexAuth(t, dir, auth, 0o600)
		}},
		{name: "wrong mode", prepare: func(t *testing.T, dir string) {
			auth := syntheticChatGPTAuth()
			auth["auth_mode"] = "apikey"
			writeSyntheticCodexAuth(t, dir, auth, 0o600)
		}},
		{name: "non private", prepare: func(t *testing.T, dir string) {
			writeSyntheticCodexAuth(t, dir, syntheticChatGPTAuth(), 0o644)
		}},
		{name: "symlink", prepare: func(t *testing.T, dir string) {
			outside := t.TempDir()
			writeSyntheticCodexAuth(t, outside, syntheticChatGPTAuth(), 0o600)
			if err := os.Symlink(filepath.Join(outside, "auth.json"), filepath.Join(dir, "auth.json")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := t.TempDir()
			if tc.prepare != nil {
				tc.prepare(t, source)
			}
			bin := t.TempDir()
			marker := filepath.Join(t.TempDir(), "launched")
			if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nprintf launched > \"$LAUNCH_MARKER\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			withPATH(t, bin)
			t.Setenv("CODEX_HOME", source)
			t.Setenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION", "1")
			t.Setenv("LAUNCH_MARKER", marker)
			out, err := fixtureCodexAdapter(runattempt.Open(t.TempDir())).Run(context.Background(), RunInput{
				WorkID: "work_subscription_refusal", Harness: "codex", Workspace: t.TempDir(), Prompt: "fixture", Command: "codex", RequestedModelID: "gpt-fixture", RequestedProviderRoute: "chatgpt-subscription", Now: time.Now().UTC(), preflighted: true,
			})
			if err == nil || !strings.Contains(err.Error(), "codex subscription binding") {
				t.Fatalf("unsafe source did not fail closed: output=%+v err=%v", out, err)
			}
			if strings.Contains(err.Error(), "synthetic-") || strings.Contains(err.Error(), source) {
				t.Fatalf("auth secret/path in error: %v", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("child launched on unsafe auth: %v", err)
			}
			if out.RunAttempt.ID == "" || out.RunAttempt.Status != runattempt.StatusFailed {
				t.Fatalf("failed binding lacks terminal attempt: %+v", out.RunAttempt)
			}
		})
	}
}

func TestCodexSubscriptionBindingRefusesCallerAuthOverride(t *testing.T) {
	for _, env := range []string{"CODEX_HOME=/tmp/other", "OPENAI_API_KEY=synthetic-paid-key"} {
		t.Run(strings.SplitN(env, "=", 2)[0], func(t *testing.T) {
			t.Setenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION", "1")
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			withPATH(t, bin)
			out, err := fixtureCodexAdapter(runattempt.Open(t.TempDir())).Run(context.Background(), RunInput{
				WorkID: "work_override_refusal", Harness: "codex", Workspace: t.TempDir(), Env: []string{env}, Now: time.Now().UTC(),
			})
			if err == nil || out.RunAttempt.ID != "" {
				t.Fatalf("explicit auth override should refuse before mint: output=%+v err=%v", out, err)
			}
		})
	}
}

func TestCodexSubscriptionBindingCleansUpAfterStartError(t *testing.T) {
	source := t.TempDir()
	writeSyntheticCodexAuth(t, source, syntheticChatGPTAuth(), 0o600)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/this-interpreter-does-not-exist\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPATH(t, bin)
	store := runattempt.Open(t.TempDir())
	workspace := t.TempDir()
	tempSpace := t.TempDir()
	t.Setenv("TMPDIR", tempSpace)
	t.Setenv("CODEX_HOME", source)
	t.Setenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION", "1")
	out, err := fixtureCodexAdapter(store).Run(context.Background(), RunInput{
		WorkID: "work_subscription_start_error", Harness: "codex", Workspace: workspace, Command: "codex", RequestedModelID: "gpt-fixture", RequestedProviderRoute: "chatgpt-subscription", preflighted: true,
		Prompt: "fixture", Now: time.Now().UTC(),
	})
	if err == nil || out.RunAttempt.Status != runattempt.StatusFailed {
		t.Fatalf("start error not terminal: output=%+v err=%v", out, err)
	}
	entries, err := os.ReadDir(tempSpace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("isolated auth home survived start error: %v", entries)
	}
}

func TestCodexSubscriptionBindingCleansUpAfterCancellation(t *testing.T) {
	source := t.TempDir()
	writeSyntheticCodexAuth(t, source, syntheticChatGPTAuth(), 0o600)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nprintf '%s' \"$$\" > \"$PID_CAPTURE\"\nprintf '%s' \"$CODEX_HOME\" > \"$HOME_CAPTURE\"\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withPATH(t, bin)
	marked := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME_CAPTURE", marked)
	t.Setenv("PID_CAPTURE", marked+".pid")
	t.Setenv("CODEX_HOME", source)
	t.Setenv("COMPOUND_CODEX_USE_EXISTING_SUBSCRIPTION", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := runattempt.Open(t.TempDir())
	workspace := t.TempDir()
	type result struct {
		out RunOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := fixtureCodexAdapter(store).Run(ctx, RunInput{
			WorkID: "work_subscription_cancel", Harness: "codex", Workspace: workspace,
			Command: "codex", Prompt: "fixture", RequestedModelID: "gpt-fixture", RequestedProviderRoute: "chatgpt-subscription", Now: time.Now().UTC(), preflighted: true,
		})
		done <- result{out, err}
	}()
	var home []byte
	deadline := time.After(3 * time.Second)
	for len(home) == 0 {
		var err error
		home, err = os.ReadFile(marked)
		if err == nil && len(home) != 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("fixture child did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case got := <-done:
		if got.out.RunAttempt.Status != runattempt.StatusFailed {
			t.Fatalf("cancel attempt status: %+v, err=%v", got.out.RunAttempt, got.err)
		}
	case <-time.After(5 * time.Second):
		pid, _ := os.ReadFile(marked + ".pid")
		processes, _ := exec.Command("ps", "-axo", "pid=,ppid=,pgid=,stat=,comm=").Output()
		for _, line := range strings.Split(string(processes), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[2] == string(pid) {
				t.Logf("cancelled process group: %s", line)
			}
		}
		t.Fatal("cancelled run did not finish")
	}
	if _, err := os.Stat(string(home)); !os.IsNotExist(err) {
		t.Fatalf("isolated auth home remains after cancellation: %v", err)
	}
}

func TestCodexSubscriptionSnapshotParsesAsChatGPT(t *testing.T) {
	command, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("native Codex CLI unavailable")
	}
	source := t.TempDir()
	writeSyntheticCodexAuth(t, source, syntheticChatGPTAuth(), 0o600)
	t.Setenv("CODEX_HOME", source)
	isolation := t.TempDir()
	if err := writeCodexSubscriptionSnapshot(isolation); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(command, "login", "status")
	cmd.Env = codexSubscriptionChildEnv(nil, isolation)
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Logged in using ChatGPT") {
		t.Fatalf("synthetic subscription snapshot not recognized: status=%q err=%v", output, err)
	}
}

func TestCodexSelectedModelAndEffortRefuseAmbiguousNativeInputs(t *testing.T) {
	for _, tc := range []struct {
		name, model, effort string
	}{
		{name: "display label", model: "codex (gpt-5.5)"},
		{name: "unsupported effort", model: "gpt-fixture", effort: "ultra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := RunInput{
				WorkID: "work_native_selection_refusal", Harness: "codex", Command: "codex", Workspace: t.TempDir(),
				RequestedModelID: tc.model, Now: time.Now().UTC(), preflighted: true,
				ExecutionSelectionSnapshot: &runattempt.ExecutionSelectionSnapshot{
					Source: "override", Harness: "codex", RequestedModelID: tc.model, RequestedEffort: tc.effort,
				},
			}
			out, err := NewCodexAdapter(runattempt.Open(t.TempDir())).Run(context.Background(), in)
			if err == nil || out.RunAttempt.ID != "" {
				t.Fatalf("ambiguous selection reached run: output=%+v err=%v", out, err)
			}
		})
	}
}

func TestCodexWrapperCannotClaimUnappliedNativeEffort(t *testing.T) {
	out, err := NewCodexAdapter(runattempt.Open(t.TempDir())).Run(context.Background(), RunInput{
		WorkID: "work_wrapper_effort", Harness: "codex", Command: "fixture-codex-wrapper",
		Workspace: t.TempDir(), RequestedModelID: "codex (gpt-5.5)", Now: time.Now().UTC(), preflighted: true,
		ExecutionSelectionSnapshot: &runattempt.ExecutionSelectionSnapshot{
			Source: "saved", SelectionRevision: 1, Harness: "codex", RequestedModelID: "codex (gpt-5.5)", RequestedEffort: "high",
		},
	})
	if err == nil || out.RunAttempt.ID != "" || !strings.Contains(err.Error(), "cannot attest") {
		t.Fatalf("wrapper falsely accepted native effort: output=%+v err=%v", out, err)
	}
}
