package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDelegateCLIRejectsExecutionSelectorsWithoutProfile(t *testing.T) {
	t.Parallel()
	selectors := []string{
		"--role", "executor",
		"--capability-class", "build-implement",
		"--client-harness", "codex",
		"--transport", "switchback",
		"--route-alias", "auto/build",
		"--model", "fixture/model",
		"--semantic-effort", "deep",
		"--effort", "xhigh",
		"--orchestration", "standard",
		"--privacy-ceiling", "private",
		"--launch-profile", "claude-minimax",
	}
	for i := 0; i < len(selectors); i += 2 {
		flag, value := selectors[i], selectors[i+1]
		t.Run(strings.TrimPrefix(flag, "--"), func(t *testing.T) {
			_, err := parseDelegateCLI([]string{"work_abc", "--harness", "codex", flag, value})
			if err == nil || !strings.Contains(err.Error(), "--execution-profile") {
				t.Fatalf("%s without profile error = %v", flag, err)
			}
		})
	}
}

func TestParseDelegateCLIMapsExecutionProfileSelectors(t *testing.T) {
	t.Parallel()
	options, err := parseDelegateCLI([]string{
		"work_abc",
		"--harness", "claude_code",
		"--execution-profile", "coding-implementation",
		"--role", "executor",
		"--capability-class", "build-implement",
		"--client-harness", "claude-code",
		"--transport", "switchback",
		"--route-alias", "auto/build",
		"--model", "fixture/model",
		"--semantic-effort", "deep",
		"--effort", "xhigh",
		"--orchestration", "standard",
		"--privacy-ceiling", "private",
		"--launch-profile", "claude-minimax",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := options.executionProfile
	if profile.ProfileID != "coding-implementation" ||
		profile.Role != "executor" ||
		profile.CapabilityClass != "build-implement" ||
		profile.ClientHarnessID != "claude-code" ||
		profile.TransportID != "switchback" ||
		profile.RouteAlias != "auto/build" ||
		profile.ModelConstraint != "fixture/model" ||
		profile.SemanticEffort != "deep" ||
		profile.NativeEffort != "xhigh" ||
		profile.OrchestrationPolicy != "standard" ||
		profile.PrivacyCeiling != "private" {
		t.Fatalf("selectors were not mapped exactly: %+v", profile)
	}
	launchProfile := reflect.ValueOf(profile).FieldByName("LaunchProfileID")
	if !launchProfile.IsValid() || launchProfile.String() != "claude-minimax" {
		t.Fatalf("launch profile selector was not mapped exactly: %+v", profile)
	}
}
