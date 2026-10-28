package main

import (
	"strings"
	"testing"
)

func TestWorkPrivateSourcePublishNativeCLIAdmissionAndHelp(t *testing.T) {
	plan := []string{"--work", "work_fixture", "--actor", "codex:fixture", "--repo", "/synthetic/source", "--review-action", "act_fixture"}
	if workCommandNeedsWritableStore("private-source-publish", plan) {
		t.Fatal("publication planning opens writable store")
	}
	if workCommandNeedsWritableStore("private-source-publish", []string{"--release-tag", "v1.2.3", "--request", "publication.json"}) {
		t.Fatal("release tag planning opens writable store")
	}
	if !workCommandNeedsWritableStore("private-source-publish", []string{"--apply", "--request", "request.json"}) {
		t.Fatal("native publication effect did not open writable store")
	}
	if workCommandNeedsWritableStore("private-source-publish", []string{"--apply"}) {
		t.Fatal("invalid publication command opens writable store")
	}
	help := workHelpText("private-source-publish")
	if !strings.Contains(help, "--review-action") || !strings.Contains(help, "--request") || !strings.Contains(help, "--apply") || !strings.Contains(help, "--release-tag") {
		t.Fatal("native publication help is incomplete")
	}
}
