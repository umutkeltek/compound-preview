package browser

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The seam between B4's heal (the reader) and B3's credential provider (the
// writer). The round-trip test replaces the fake `compound` with a shim that
// execs this test binary into runCredentialProvider, so heal's request bytes
// and the real provider's reply bytes meet over a real pipe.

const (
	providerHelperEnv    = "COMPOUND_TEST_CREDENTIAL_PROVIDER_HELPER"
	providerHelperExeEnv = "COMPOUND_TEST_CREDENTIAL_PROVIDER_EXE"
	providerHelperNowEnv = "COMPOUND_TEST_CREDENTIAL_NOW"
)

// TestCredentialProviderHelperProcess is the real `compound browser
// credential-provider` when the shim execs this binary; a no-op otherwise.
func TestCredentialProviderHelperProcess(t *testing.T) {
	if os.Getenv(providerHelperEnv) != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) < 2 || args[0] != "browser" || args[1] != "credential-provider" {
		fmt.Fprint(os.Stderr, credentialProviderUsage)
		os.Exit(2)
	}
	if raw := os.Getenv(providerHelperNowEnv); raw != "" {
		unix, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			os.Exit(90)
		}
		credentialNow = func() time.Time { return time.Unix(unix, 0).UTC() }
	}
	os.Exit(runCredentialProvider(args[2:], os.Stdin, os.Stdout, os.Stderr))
}

func TestCredentialProviderHelperRejectsWrongCommand(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"browser"}, {"browser", "unknown"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			argv := append([]string{"-test.run=^TestCredentialProviderHelperProcess$", "--"}, args...)
			cmd := exec.Command(exe, argv...)
			cmd.Env = append(os.Environ(), providerHelperEnv+"=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 2 {
				t.Fatalf("helper exit = %v, want misuse exit 2", err)
			}
			if stdout.Len() != 0 || stderr.String() != credentialProviderUsage {
				t.Fatalf("helper output: stdout=%q stderr=%q, want portable provider usage %q", stdout.String(), stderr.String(), credentialProviderUsage)
			}
		})
	}
}

func TestHealRoundTripThroughRealCredentialProvider(t *testing.T) {
	f := newHealFixture(t, false)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(f.binDir, "compound"),
		"#!/bin/sh\nexec \"$"+providerHelperExeEnv+"\" -test.run='^TestCredentialProviderHelperProcess$' -- \"$@\"\n")
	t.Setenv(providerHelperExeEnv, exe)
	t.Setenv(providerHelperEnv, "1")
	pinned := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	t.Setenv(providerHelperNowEnv, strconv.FormatInt(pinned.Unix(), 10))

	const seed = "JBSWY3DPEHPK3PXP"
	wantOTP, err := totpFromBase32(seed, pinned)
	if err != nil {
		t.Fatal(err)
	}
	accounts := filepath.Join(f.home, "secrets", "accounts")
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# synthetic\nusername=operator@example.test\npassword=" + fakeSecret + "\ntotp_seed=" + seed + "\n"
	if err := os.WriteFile(filepath.Join(accounts, fakeCredentialRef), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f.writeRecipe("login", []map[string]any{
		{"action": "navigate", "descriptor": map[string]any{"url": "https://app.example.test/login"}},
		{"action": "fill", "descriptor": map[string]any{"role": "textbox", "name": "Email"}, "value": "{{secret:username}}"},
		{"action": "fill", "descriptor": map[string]any{"role": "textbox", "name": "Password"}, "value": "{{secret:password}}"},
		{"action": "fill", "descriptor": map[string]any{"role": "textbox", "name": "One-time code"}, "value": "{{secret:totp}}"},
		{"action": "click", "descriptor": map[string]any{"role": "button", "name": "Sign in"}},
	})
	f.snapshot(0, map[string][2]string{"e3": {"textbox", "Email"}, "e4": {"textbox", "Password"}, "e6": {"textbox", "One-time code"}, "e5": {"button", "Sign in"}})
	f.state("login-ref", "e5")

	code, out, errOut := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 0 || res.Status != "healed" || !res.LoggedIn {
		t.Fatalf("heal through the real provider: exit=%d %+v err=%s", code, res, errOut)
	}
	for n, want := range map[int]string{
		1: `["fill","@e3","operator@example.test"]`,
		2: `["fill","@e4","` + fakeSecret + `"]`,
		3: `["fill","@e6","` + wantOTP + `"]`,
	} {
		if got := f.readState("batch-" + strconv.Itoa(n) + ".stdin"); !strings.Contains(got, want) {
			t.Fatalf("batch %d stdin = %q, want it to carry %s", n, got, want)
		}
	}
	if strings.Contains(out, fakeSecret) || strings.Contains(errOut, fakeSecret) {
		t.Fatalf("secret reached heal output")
	}

	// The provider audited every hand-out, names only.
	file, err := os.Open(credentialAuditPath(f.home))
	if err != nil {
		t.Fatalf("provider wrote no audit: %v", err)
	}
	defer file.Close()
	served := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), fakeSecret) {
			t.Fatalf("audit line carries a value: %s", scanner.Text())
		}
		var e credentialAuditEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatalf("audit line %q: %v", scanner.Text(), err)
		}
		if e.Outcome != "served" || e.Profile != fakeProfileID || e.RefName != fakeCredentialRef || e.Source != "file" {
			t.Fatalf("audit entry = %+v", e)
		}
		served++
	}
	if served != 2 {
		t.Fatalf("audit served lines = %d, want 2 (one prefetch, one fresh otp at step time)", served)
	}
}

// TestCredentialResolveWireRoundTrip pipes heal's request bytes into the real
// provider in-process and reads its reply with heal's parser.
func TestCredentialResolveWireRoundTrip(t *testing.T) {
	body, err := credentialResolveRequestJSON(fakeProfileID, fakeCredentialRef)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"protocol":"agent-browser.plugin.v1","type":"credential.resolve","capability":"credential.read","request":{"profileName":"example-profile","itemRef":"example-login","url":null}}`
	if string(body) != want {
		t.Fatalf("request = %s\nwant      %s", body, want)
	}
	if bare, _ := credentialResolveRequestJSON(fakeProfileID, ""); !strings.Contains(string(bare), `"itemRef":null`) {
		t.Fatalf("a row without credential_ref must send itemRef null: %s", bare)
	}

	f := newHealFixture(t, false)
	t.Setenv("PATH", f.binDir) // no bws: the file source answers
	pinned := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	saved := credentialNow
	credentialNow = func() time.Time { return pinned }
	t.Cleanup(func() { credentialNow = saved })
	const seed = "JBSWY3DPEHPK3PXP"
	wantOTP, err := totpFromBase32(seed, pinned)
	if err != nil {
		t.Fatal(err)
	}
	accounts := filepath.Join(f.home, "secrets", "accounts")
	if err := os.MkdirAll(accounts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(accounts, fakeCredentialRef), []byte("email=operator@example.test\npassword="+fakeSecret+"\ntotp_seed="+seed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resolve := func(request []byte) (providedCredential, error, string) {
		var out, errOut bytes.Buffer
		if code := runCredentialProvider([]string{"--home", f.home}, bytes.NewReader(request), &out, &errOut); code != 0 {
			t.Fatalf("provider exit %d: %s", code, errOut.String())
		}
		cred, err := parseCredentialProviderReply(out.Bytes())
		return cred, err, out.String()
	}
	cred, err, _ := resolve(body)
	if err != nil {
		t.Fatalf("parse provider reply: %v", err)
	}
	for name, want := range map[string]string{"username": "operator@example.test", "password": fakeSecret, "otp": wantOTP} {
		if got := cred.field(credentialReplyField(name)); got != want {
			t.Fatalf("field %s = %q, want %q", name, got, want)
		}
	}
	if credentialReplyField("totp") != "otp" || credentialReplyField("totp_code") != "otp" || credentialReplyField("email") != "" {
		t.Fatalf("field mapping drifted")
	}

	// A refusal reads as credential_unavailable with the provider's names-only reason.
	mismatch, _ := credentialResolveRequestJSON(fakeProfileID, "another-ref")
	_, err, raw := resolve(mismatch)
	if err == nil || reasonOf(err) != "credential_unavailable" || !strings.Contains(err.Error(), "itemRef does not match") {
		t.Fatalf("refusal = %v (raw %s)", err, raw)
	}
	if strings.Contains(err.Error(), fakeSecret) {
		t.Fatalf("refusal echoed a value: %v", err)
	}
}

func TestHealTOTPStepWithoutProviderOTPQueuesTwoFactorWithoutAttempt(t *testing.T) {
	f := newHealFixture(t, true) // the fake provider serves no otp unless $D/otp exists
	f.writeRecipe("login", []map[string]any{
		{"action": "navigate", "descriptor": map[string]any{"url": "https://app.example.test/login"}},
		{"action": "fill", "descriptor": map[string]any{"role": "textbox", "name": "Email"}, "value": "{{secret:username}}"},
		{"action": "fill", "descriptor": map[string]any{"role": "textbox", "name": "Password"}, "value": "{{secret:password}}"},
		{"action": "fill", "descriptor": map[string]any{"role": "textbox", "name": "One-time code"}, "value": "{{secret:totp}}"},
		{"action": "click", "descriptor": map[string]any{"role": "button", "name": "Sign in"}},
	})
	f.snapshot(0, map[string][2]string{"e3": {"textbox", "Email"}, "e4": {"textbox", "Password"}, "e6": {"textbox", "One-time code"}, "e5": {"button", "Sign in"}})
	f.state("login-ref", "e5")
	code, out, errOut := f.cli("heal", "--profile", fakeProfileID, "--json")
	var res healResult
	decodeJSONOut(t, out, &res)
	if code != 3 || res.Reason != "two_factor_required" {
		t.Fatalf("exit=%d %+v err=%s", code, res, errOut)
	}
	if _, ok := f.registryRow()["heal_attempts"]; ok {
		t.Fatalf("a missing otp burned a heal attempt")
	}
	if f.countArgv("[batch]") != 0 || f.countArgv("[click]") != 0 {
		t.Fatalf("login page driven without a second factor: %v", f.argvLines())
	}
	queue := readQueue(t, f.home)
	if len(queue) != 1 || queue[0].Reason != "two_factor_required" {
		t.Fatalf("queue = %+v", queue)
	}
}
