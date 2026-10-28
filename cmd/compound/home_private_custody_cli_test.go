package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/home"
)

const homePrivateCustodyChildEnv = "COMPOUND_TEST_HOME_PRIVATE_CUSTODY_CHILD"

func TestHomePrivateCustodyCLIJSONTextAndExitCodes(t *testing.T) {
	t.Parallel()
	if os.Getenv(homePrivateCustodyChildEnv) == "doctor-json" {
		cmdHome([]string{"doctor", "--json"})
		os.Exit(0)
	}
	if os.Getenv(homePrivateCustodyChildEnv) == "repair-json" {
		cmdHome([]string{"private-custody", "repair", "--json"})
		os.Exit(0)
	}
	if os.Getenv(homePrivateCustodyChildEnv) == "repair-text" {
		cmdHome([]string{"private-custody", "repair", "--dry-run"})
		os.Exit(0)
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "global", cliTestNow); err != nil {
		t.Fatal(err)
	}
	body := []byte("cli custody")
	digest := digestHex(body)
	root := filepath.Join(homePath, "private", "artifacts", "sha256")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, digest), body, 0o644); err != nil {
		t.Fatal(err)
	}

	doctor := exec.Command(os.Args[0], "-test.run=^TestHomePrivateCustodyCLIJSONTextAndExitCodes$")
	doctor.Env = append(os.Environ(), homePrivateCustodyChildEnv+"=doctor-json", "COMPOUND_HOME="+homePath)
	out, err := doctor.CombinedOutput()
	if err == nil {
		t.Fatalf("doctor drift exit nil, want non-zero; output:\n%s", out)
	}
	var dr homeDoctorJSONReport
	if err := json.Unmarshal(out, &dr); err != nil {
		t.Fatalf("doctor json parse: %v\n%s", err, out)
	}
	if dr.PrivateCustody.SchemaVersion == "" || dr.PrivateCustody.Counts.Drift == 0 {
		t.Fatalf("doctor missing private custody drift: %+v", dr.PrivateCustody)
	}

	repairText := exec.Command(os.Args[0], "-test.run=^TestHomePrivateCustodyCLIJSONTextAndExitCodes$")
	repairText.Env = append(os.Environ(), homePrivateCustodyChildEnv+"=repair-text", "COMPOUND_HOME="+homePath)
	textOut, _ := repairText.CombinedOutput()
	if !strings.Contains(string(textOut), "private_custody.status") {
		t.Fatalf("repair text output missing private custody status:\n%s", textOut)
	}
	info, err := os.Lstat(filepath.Join(root, digest))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("dry-run mutated object mode=%04o err=%v", info.Mode().Perm(), err)
	}

	repair := exec.Command(os.Args[0], "-test.run=^TestHomePrivateCustodyCLIJSONTextAndExitCodes$")
	repair.Env = append(os.Environ(), homePrivateCustodyChildEnv+"=repair-json", "COMPOUND_HOME="+homePath)
	out, err = repair.CombinedOutput()
	if err != nil {
		t.Fatalf("repair json: %v\n%s", err, out)
	}
	var rr home.PrivateCustodyReport
	if err := json.Unmarshal(out, &rr); err != nil {
		t.Fatalf("repair json parse: %v\n%s", err, out)
	}
	if !rr.OK || !rr.Changed || rr.Counts.Repaired == 0 {
		t.Fatalf("repair report = %+v", rr)
	}
	if strings.Contains(string(out), `"Checked"`) || strings.Contains(string(out), `"Status"`) {
		t.Fatalf("repair JSON used unstable capitalized keys:\n%s", out)
	}
}

func TestHomePrivateCustodyCLIParserErrorsAndHelp(t *testing.T) {
	t.Parallel()
	if os.Getenv(homePrivateCustodyChildEnv) == "repair-bogus" {
		cmdHome([]string{"private-custody", "repair", "--bogus"})
		os.Exit(0)
	}
	if os.Getenv(homePrivateCustodyChildEnv) == "repair-missing-home" {
		cmdHome([]string{"private-custody", "repair", "--home"})
		os.Exit(0)
	}
	if os.Getenv(homePrivateCustodyChildEnv) == "repair-help" {
		cmdHome([]string{"private-custody", "--help"})
		os.Exit(0)
	}
	if os.Getenv(homePrivateCustodyChildEnv) == "old-grammar" {
		cmdHome([]string{"repair", "private-custody"})
		os.Exit(0)
	}
	if os.Getenv(homePrivateCustodyChildEnv) == "home-help" {
		cmdHome([]string{"--help"})
		os.Exit(0)
	}
	if os.Getenv(homePrivateCustodyChildEnv) == "top-help" {
		usage()
		os.Exit(0)
	}
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "global", cliTestNow); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		env  string
		want int
		text string
	}{
		{name: "bogus", env: "repair-bogus", want: 2, text: "home private-custody repair: unknown arg --bogus"},
		{name: "missing_home", env: "repair-missing-home", want: 2, text: "--home requires a value"},
		{name: "help", env: "repair-help", want: 0, text: "compound home private-custody repair"},
		{name: "home_help", env: "home-help", want: 0, text: "compact|private-custody"},
		{name: "top_help", env: "top-help", want: 0, text: "private-custody repair is explicit permission repair"},
		{name: "old_grammar", env: "old-grammar", want: 2, text: "compound home <init|doctor|where|backup|export|verify|restore|archive-history|restore-history|compact|private-custody|route-swap-repair>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHomePrivateCustodyCLIParserErrorsAndHelp$")
			cmd.Env = append(os.Environ(), homePrivateCustodyChildEnv+"="+tc.env, "COMPOUND_HOME="+homePath)
			out, err := cmd.CombinedOutput()
			got := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				got = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("child error: %v\n%s", err, out)
			}
			if got != tc.want || !strings.Contains(string(out), tc.text) {
				t.Fatalf("exit/text = %d/%q, want %d containing %q", got, out, tc.want, tc.text)
			}
		})
	}
}

func TestHomePrivateCustodyRepairTokenAsBackupArgumentIsNotDispatched(t *testing.T) {
	t.Parallel()
	if os.Getenv(homePrivateCustodyChildEnv) == "backup-target-repair" {
		cmdHome([]string{"backup", "--target", "repair"})
		os.Exit(0)
	}

	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := home.Init(homePath, "global", cliTestNow); err != nil {
		t.Fatal(err)
	}
	childDir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHomePrivateCustodyRepairTokenAsBackupArgumentIsNotDispatched$")
	cmd.Dir = childDir
	cmd.Env = append(os.Environ(), homePrivateCustodyChildEnv+"=backup-target-repair", "COMPOUND_HOME="+homePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("backup target named repair was misrouted: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "private-custody") {
		t.Fatalf("backup target named repair reached private-custody parser:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(childDir, "repair")); err != nil {
		t.Fatalf("backup target repair was not published: %v\n%s", err, out)
	}
}

func digestHex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
