package cli

import (
	"bytes"
	"compound/internal/act"
	"compound/internal/home"
	"compound/internal/policy"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRuntimeConfigInstallUsesGloballyResolvedHome(t *testing.T) {
	h := t.TempDir()
	t.Setenv("COMPOUND_HOME", h)
	prior := newRuntimeConfigInstallOwner
	defer func() { newRuntimeConfigInstallOwner = prior }()
	spy := &runtimeInstallerCLISpy{}
	newRuntimeConfigInstallOwner = func(got string) (runtimeConfigInstallOwner, error) {
		if got != h {
			t.Fatalf("home=%q", got)
		}
		return spy, nil
	}
	var stdout, stderr bytes.Buffer
	code := RunRuntimeConfigInstall([]string{"execute", "--plan-ref", "fixture-ref", "--actor", "fixture", "--json"}, &stdout, &stderr)
	if code != 0 || spy.executions != 1 {
		t.Fatalf("code=%d err=%s", code, stderr.String())
	}
	code = RunRuntimeConfigInstall([]string{"recovery-plan", "--plan-ref", "original-ref", "--operation", "rollback", "--work", "work_recovery", "--actor", "fixture", "--json"}, &stdout, &stderr)
	if code != 0 || spy.recoveries != 1 {
		t.Fatalf("recovery code=%d err=%s", code, stderr.String())
	}
}

type runtimeInstallerCLISpy struct{ plans, executions, recoveries, qualificationPlans, qualifications int }

func (s *runtimeInstallerCLISpy) PlanCaptureQualification(act.RuntimeCaptureQualificationRequest, string, string) (act.RuntimeCaptureQualificationPlanResult, error) {
	s.qualificationPlans++
	return act.RuntimeCaptureQualificationPlanResult{}, nil
}
func (s *runtimeInstallerCLISpy) QualifyCapture(string, string) (act.RuntimeCaptureQualificationResult, error) {
	s.qualifications++
	return act.RuntimeCaptureQualificationResult{}, nil
}

func (s *runtimeInstallerCLISpy) Plan(policy.RuntimeConfigInstallRequest, string, string) (act.RuntimeConfigInstallPlanResult, error) {
	s.plans++
	return act.RuntimeConfigInstallPlanResult{}, nil
}
func (s *runtimeInstallerCLISpy) PlanRecovery(string, string, string, string) (act.RuntimeConfigInstallPlanResult, error) {
	s.recoveries++
	return act.RuntimeConfigInstallPlanResult{}, nil
}
func (s *runtimeInstallerCLISpy) Execute(context.Context, string, string) (act.RuntimeConfigInstallResult, error) {
	s.executions++
	return act.RuntimeConfigInstallResult{}, nil
}

func TestRuntimeConfigInstallUsageRejectsUnknownFlags(t *testing.T) {
	var stderr bytes.Buffer
	code := RunRuntimeConfigInstall([]string{"plan", "--rogue", "x", "--work", "/w", "--request-file", "/r.json", "--actor", "fixture", "--home", "/h"}, &bytes.Buffer{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "compound act runtime-config-install") {
		t.Fatalf("expected usage refusal for unknown flag, code=%d stderr=%q", code, stderr.String())
	}
}

func TestRuntimeConfigInstallUsageRequiresAbsoluteHome(t *testing.T) {
	var stderr bytes.Buffer
	code := RunRuntimeConfigInstall([]string{"execute", "--plan-ref", "x", "--actor", "fixture", "--home", "relative"}, &bytes.Buffer{}, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "compound act runtime-config-install") {
		t.Fatalf("expected usage refusal for relative home, code=%d stderr=%q", code, stderr.String())
	}
}

func TestRuntimeConfigCLIGlobalParserToOwner(t *testing.T) {
	t.Setenv("COMPOUND_HOME", t.TempDir())
	h := t.TempDir()
	file := filepath.Join(h, "metadata.json")
	installFile := filepath.Join(h, "install.json")
	installRaw, _ := json.Marshal(policy.RuntimeConfigInstallRequest{SchemaVersion: policy.RuntimeConfigInstallSchema})
	if err := os.WriteFile(installFile, installRaw, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(act.RuntimeCaptureQualificationRequest{SchemaVersion: "compound.runtime_capture_qualification_request.v0.1"})
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	old := newRuntimeConfigInstallOwner
	defer func() { newRuntimeConfigInstallOwner = old }()
	for _, op := range []string{"plan", "execute", "recovery-plan", "qualify-capture-plan", "qualify-capture"} {
		spy := &runtimeInstallerCLISpy{}
		newRuntimeConfigInstallOwner = func(got string) (runtimeConfigInstallOwner, error) {
			if got != h {
				t.Fatalf("global home lost: %s", got)
			}
			return spy, nil
		}
		args := []string{"act", "runtime-config-install", op, "--home", h, "--actor", "fixture", "--json"}
		if op == "qualify-capture-plan" || op == "plan" {
			requestFile := file
			if op == "plan" {
				requestFile = installFile
			}
			args = append(args, "--work", "work_fixture", "--request-file", requestFile)
		} else {
			args = append(args, "--plan-ref", "fixture-ref")
		}
		if op == "recovery-plan" {
			args = append(args, "--work", "work_recovery", "--operation", "rollback")
		}
		prepared, err := home.PrepareGlobalCLIArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		for _, arg := range prepared {
			if arg == "--home" {
				t.Fatal("global parser retained --home")
			}
		}
		var out, errs bytes.Buffer
		if code := RunRuntimeConfigInstall(prepared[2:], &out, &errs); code != 0 || errs.Len() != 0 {
			t.Fatalf("%s code=%d %s", op, code, errs.String())
		}
		if spy.plans+spy.executions+spy.recoveries+spy.qualificationPlans+spy.qualifications != 1 {
			t.Fatal("wrong dispatch")
		}
	}
}

func TestRuntimeCaptureQualificationCLIRejectsMalformedBeforeOwner(t *testing.T) {
	old := newRuntimeConfigInstallOwner
	defer func() { newRuntimeConfigInstallOwner = old }()
	newRuntimeConfigInstallOwner = func(string) (runtimeConfigInstallOwner, error) {
		t.Fatal("malformed input reached owner")
		return nil, nil
	}
	h := t.TempDir()
	for _, op := range []string{"qualify-capture-plan", "qualify-capture"} {
		for _, extra := range [][]string{{"--rogue", "x"}, {"--actor", "other"}, {"--json", "--json"}} {
			args := []string{op, "--actor", "fixture", "--home", h}
			if op == "qualify-capture-plan" {
				args = append(args, "--work", "work_fixture", "--request-file", filepath.Join(h, "request.json"))
			} else {
				args = append(args, "--plan-ref", "fixture-ref")
			}
			var out, errs bytes.Buffer
			if code := RunRuntimeConfigInstall(append(args, extra...), &out, &errs); code != 2 {
				t.Fatalf("%s malformed flags code=%d", op, code)
			}
		}
	}
}
