package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"compound/internal/home"
	"compound/internal/work"
)

const (
	workShowGlobalHomeChildEnv  = "COMPOUND_TEST_WORK_SHOW_GLOBAL_HOME_CHILD"
	workShowUnknownFlagChildEnv = "COMPOUND_TEST_WORK_SHOW_UNKNOWN_FLAG_CHILD"
)

func TestWorkShowGlobalHomeUsesRealWorkID(t *testing.T) {
	if os.Getenv(workShowGlobalHomeChildEnv) == "1" {
		os.Args = []string{
			"compound",
			"work",
			"show",
			"--home",
			os.Getenv("COMPOUND_TEST_TARGET_HOME"),
			os.Getenv("COMPOUND_TEST_TARGET_WORK_ID"),
			"--json",
		}
		main()
		return
	}

	targetHome := t.TempDir()
	if err := home.Init(targetHome, "test", cliTestNow); err != nil {
		t.Fatalf("init target home: %v", err)
	}
	targetStore, err := work.Open(targetHome)
	if err != nil {
		t.Fatalf("open target work store: %v", err)
	}
	created, err := targetStore.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "global home preprocessing preserves the real work id",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create target work: %v", err)
	}

	fallbackHome := t.TempDir()
	if err := home.Init(fallbackHome, "test", cliTestNow); err != nil {
		t.Fatalf("init fallback home: %v", err)
	}
	t.Setenv("COMPOUND_HOME", fallbackHome)

	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkShowGlobalHomeUsesRealWorkID$")
	cmd.Env = append(os.Environ(),
		workShowGlobalHomeChildEnv+"=1",
		"COMPOUND_TEST_TARGET_HOME="+targetHome,
		"COMPOUND_TEST_TARGET_WORK_ID="+created.ID,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("work show with global --home failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), created.ID) {
		t.Fatalf("work show output did not resolve the real work id %s:\n%s", created.ID, out)
	}
}

func TestWorkShowRejectsUnknownFlag(t *testing.T) {
	if os.Getenv(workShowUnknownFlagChildEnv) == "1" {
		cmdWork([]string{"show", "--bogus", os.Getenv("COMPOUND_TEST_TARGET_WORK_ID"), "--json"})
		return
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	created, err := store.Create(work.CreateInput{
		Owner:            "compound-core",
		SuccessPredicate: "work show rejects unknown flags",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	t.Setenv("COMPOUND_HOME", homePath)

	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkShowRejectsUnknownFlag$")
	cmd.Env = append(os.Environ(),
		workShowUnknownFlagChildEnv+"=1",
		"COMPOUND_TEST_TARGET_WORK_ID="+created.ID,
	)
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("work show must reject --bogus, got err=%v output:\n%s", err, out)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("work show --bogus exit = %d, want 2; output:\n%s", exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "unknown flag --bogus") {
		t.Fatalf("work show --bogus output missing hard error:\n%s", out)
	}
}

func TestWorkShowJSONIsOneObjectWithLifecycle(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work: %v", err)
	}
	created, err := store.Create(work.CreateInput{Owner: "compound-core", SuccessPredicate: "show emits one JSON object"}, cliTestNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}

	out := captureStdout(t, func() {
		cmdWork([]string{"show", created.ID, "--json"})
	})
	decoder := json.NewDecoder(strings.NewReader(out))
	var shown map[string]json.RawMessage
	if err := decoder.Decode(&shown); err != nil {
		t.Fatalf("decode work show: %v\n%s", err, out)
	}
	if _, ok := shown["lifecycle"]; !ok {
		t.Fatalf("work show JSON omits lifecycle:\n%s", out)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("work show JSON has trailing data: %v\n%s", err, out)
	}
}
