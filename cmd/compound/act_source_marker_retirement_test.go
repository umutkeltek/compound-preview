package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActSourceMarkerRetirementUsesClosedRequestParser(t *testing.T) {
	dir := t.TempDir()
	request := filepath.Join(dir, "request.json")
	if e := os.WriteFile(request, []byte("{\"credential_value\":\"synthetic-private-body\"}"), 0600); e != nil {
		t.Fatal(e)
	}
	var out, err bytes.Buffer
	code := runActWithInput([]string{"source-marker-retirement", "plan", "--request-file", request, "--work", "work_fixture", "--actor", "fixture", "--home", dir}, strings.NewReader(""), &out, &err)
	if code != 1 || out.Len() != 0 || !strings.Contains(err.String(), "source_marker_retirement:request") || strings.Contains(err.String(), "synthetic-private-body") {
		t.Fatal("real act dispatch did not enforce closed marker request decoder")
	}
}
