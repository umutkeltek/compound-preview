package negatives

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"compound/internal/candidates"
)

func writeCLIFailurePattern(t *testing.T, homePath, id, subject, statement, detail string) {
	t.Helper()
	doc := map[string]any{
		"id":         id,
		"kind":       string(candidates.FailurePattern),
		"subject":    subject,
		"statement":  statement,
		"detail":     detail,
		"confidence": 0.8,
		"verdict":    candidates.Park,
	}
	dir := filepath.Join(homePath, candidates.InboxDir(candidates.FailurePattern))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunCLIBuildAndListJSON(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	writeCLIFailurePattern(t, homePath, "fp_cli_negatives", "cli-negatives-check", "cli negatives statement", "cli-negatives-detail")

	emptyOut := captureStdout(t, func() { RunCLI([]string{"list", "--json"}) })
	var empty Projection
	if err := json.Unmarshal([]byte(emptyOut), &empty); err != nil {
		t.Fatalf("parse empty list: %v\n%s", err, emptyOut)
	}
	if len(empty.Records) != 0 {
		t.Fatalf("expected no records before build, got %+v", empty)
	}

	buildOut := captureStdout(t, func() { RunCLI([]string{"build", "--json"}) })
	var built Projection
	if err := json.Unmarshal([]byte(buildOut), &built); err != nil {
		t.Fatalf("parse build: %v\n%s", err, buildOut)
	}
	if len(built.Records) != 1 || built.Records[0].RefID != "fp_cli_negatives" {
		t.Fatalf("expected one built record, got %+v", built)
	}

	listOut := captureStdout(t, func() { RunCLI([]string{"list", "--json"}) })
	var list Projection
	if err := json.Unmarshal([]byte(listOut), &list); err != nil {
		t.Fatalf("parse list: %v\n%s", err, listOut)
	}
	if len(list.Records) != 1 || list.Records[0].RefID != "fp_cli_negatives" {
		t.Fatalf("expected the persisted record readable via list, got %+v", list)
	}
}

func captureStdout(t *testing.T, fn func()) string {
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
