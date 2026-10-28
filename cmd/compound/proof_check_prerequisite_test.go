package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProofCheckConsolePrerequisitesBeforeRuntime(t *testing.T) {
	for _, tc := range []struct{ name, shape, reason string }{
		{"missing dependencies", "missing", "console_deps_missing"},
		{"non-directory dependencies", "file", "console_deps_missing"},
		{"missing compiler", "directory", "console_compiler_missing"},
		{"non-executable compiler", "compiler", "console_compiler_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initCompoundCommandGitRepo(t)
			modules := filepath.Join(repo, "console", "node_modules")
			if tc.shape != "missing" {
				if err := os.MkdirAll(filepath.Dir(modules), 0o755); err != nil {
					t.Fatal(err)
				}
				if tc.shape == "file" {
					if err := os.WriteFile(modules, []byte("not a directory"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.MkdirAll(filepath.Join(modules, ".bin"), 0o755); err != nil {
						t.Fatal(err)
					}
					if tc.shape == "compiler" {
						if err := os.WriteFile(filepath.Join(modules, ".bin", "tsc"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			previous := proofCheckRuntimeExecutable
			resolutions := 0
			proofCheckRuntimeExecutable = func(string) (string, error) {
				resolutions++
				return "", errors.New("runtime resolver reached before readiness")
			}
			defer func() { proofCheckRuntimeExecutable = previous }()
			report, err := runProofCheckContext(context.Background(), repo, false, "", false, false, time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want early %s refusal, got report=%+v err=%v", tc.reason, report, err)
			}
			if resolutions != 0 || len(report.Steps) != 0 {
				t.Fatalf("unready proof reached execution: resolutions=%d steps=%d", resolutions, len(report.Steps))
			}
		})
	}
}

func TestProofCheckPrerequisitesDoNotChangeDryRunInventory(t *testing.T) {
	repo := initCompoundCommandGitRepo(t)
	report, err := runProofCheckContext(context.Background(), repo, true, "", false, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	want, err := proofCheckSteps("")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Steps) != len(want) {
		t.Fatalf("dry run inventory = %d, want %d", len(report.Steps), len(want))
	}
	for _, step := range report.Steps {
		if step.Status != "planned" {
			t.Fatalf("dry run claimed execution: %+v", step)
		}
	}
}
