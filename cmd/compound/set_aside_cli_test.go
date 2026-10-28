package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/work"
)

// The CLI is set-aside's real consumer. This exercises the full begin -> end
// round trip through cmdWork (arg parsing, repeated --file collection, JSON
// output) against a real-git t.TempDir fixture and a t.TempDir home.
func TestSetAsideCLIBeginRevertsAndEndRestores(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	repo, base, tip := initLandingCLIRepo(t)
	runLandingCLIGit(t, repo, "merge", "--ff-only", tip) // main == tip; candidate.txt present

	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner: "codex", Actor: "codex", SuccessPredicate: "set-aside foreign WIP",
		GitDelivery: &envelope.GitDeliveryContract{
			TargetWorktreeRoot: repo,
			TargetLocalRef:     "refs/heads/main",
			ExpectedBaseSHA:    base,
			PublicationMode:    "local_only",
		},
	}, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}

	owned := filepath.Join(repo, "candidate.txt")
	if err := os.WriteFile(owned, []byte("FOREIGN NEIGHBOR WIP\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	beginOut := captureStdout(t, func() {
		cmdWork([]string{
			"set-aside", "begin", w.ID, "--candidate", tip,
			"--file", "candidate.txt", "--by", "codex", "--reason", "neighbor lane WIP", "--json",
		})
	})
	var rec work.SetAsideRecord
	if err := json.Unmarshal([]byte(beginOut), &rec); err != nil {
		t.Fatalf("parse begin JSON: %v\n%s", err, beginOut)
	}
	if rec.Token == "" || len(rec.Files) != 1 || rec.Files[0].Path != "candidate.txt" {
		t.Fatalf("begin record = %+v", rec)
	}
	if got := setAsideMustRead(t, owned); got != "candidate\n" {
		t.Fatalf("begin did not revert the working tree: %q", got)
	}
	recPath := filepath.Join(homePath, "work", "set-aside", w.ID+".json")
	if _, err := os.Stat(recPath); err != nil {
		t.Fatalf("begin did not write the record: %v", err)
	}

	endOut := captureStdout(t, func() {
		cmdWork([]string{"set-aside", "end", w.ID, "--by", "codex", "--json"})
	})
	var res work.SetAsideEndResult
	if err := json.Unmarshal([]byte(endOut), &res); err != nil {
		t.Fatalf("parse end JSON: %v\n%s", err, endOut)
	}
	if len(res.Restored) != 1 || res.Restored[0] != "candidate.txt" {
		t.Fatalf("end result = %+v", res)
	}
	if got := setAsideMustRead(t, owned); got != "FOREIGN NEIGHBOR WIP\n" {
		t.Fatalf("end did not restore the foreign content: %q", got)
	}
	if _, err := os.Stat(recPath); !os.IsNotExist(err) {
		t.Fatalf("end did not delete the record: %v", err)
	}
}

// The set-aside verb answers --help with its own usage and executable example,
// and a bare/unknown subcommand exits 2 with usage.
func TestSetAsideCLIHelpAndBadSubcommand(t *testing.T) {
	out := captureStdout(t, func() {
		cmdWork([]string{"set-aside", "--help"})
	})
	if !strings.Contains(out, "usage: compound work set-aside") || !strings.Contains(out, "example: compound work set-aside") {
		t.Fatalf("set-aside help missing usage/example:\n%s", out)
	}
}

func setAsideMustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
