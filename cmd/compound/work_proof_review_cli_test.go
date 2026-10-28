package main

import (
	"strings"
	"testing"
)

func TestWorkProofReviewNativeCLIAdmissionAndHelp(t *testing.T) {
	if !strings.Contains(genericWorkUsage, "proof-review|proof-manifest|proof-adopt") {
		t.Fatal("root work help omits native review entry")
	}
	if !workCommandNeedsWritableStore("proof-review", []string{"work_example", "--actor", "executor", "--file", "review.md"}) {
		t.Fatal("valid native review does not reach writable owner")
	}
	for _, args := range [][]string{
		{"work_example", "--actor", "executor"},
		{"work_example", "--actor", "executor", "--file", "review.md", "--review-ref", "foreign"},
	} {
		if workCommandNeedsWritableStore("proof-review", args) {
			t.Fatal("invalid review opens writable store")
		}
	}
	help := captureStdout(t, func() { cmdWork([]string{"proof-review", "--help"}) })
	for _, want := range []string{"compound work proof-review", "--file", "--actor", "--expected-policy", "--expected-action", "example:"} {
		if !strings.Contains(help, want) {
			t.Fatalf("native review help missing %s", want)
		}
	}
}
