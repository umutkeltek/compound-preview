package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/runtimebinding"
)

const (
	runtimePulseDoctorChildEnv = "COMPOUND_TEST_RUNTIME_PULSE_DOCTOR_CHILD"
	runtimePulseRepairChildEnv = "COMPOUND_TEST_RUNTIME_PULSE_REPAIR_CHILD"
	runtimePulseLiveChildEnv   = "COMPOUND_TEST_RUNTIME_PULSE_LIVE_CHILD"
	runtimePulsePathEnv        = "COMPOUND_TEST_RUNTIME_PULSE_PATH"
)

type runtimePulseCLICheck struct {
	Name   string `json:"name"`
	Area   string `json:"area"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

func TestRuntimeDoctorSwitchbackPulse(t *testing.T) {
	t.Parallel()
	if os.Getenv(runtimePulseDoctorChildEnv) == "1" {
		cmdRuntime([]string{"doctor", "--switchback-pulse", os.Getenv(runtimePulsePathEnv)})
		return
	}

	receiptPath := writeRuntimePulseCLIReceipt(t, "ok", []runtimePulseCLICheck{
		{Name: "engine", Area: "engine", Level: "ok", Detail: "synthetic engine is healthy"},
		{Name: "headroom", Area: "headroom", Level: "ok", Detail: "synthetic optimizer is healthy"},
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeDoctorSwitchbackPulse$")
	cmd.Env = append(os.Environ(),
		runtimePulseDoctorChildEnv+"=1",
		runtimePulsePathEnv+"="+receiptPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime doctor --switchback-pulse failed: %v\n%s", err, output)
	}
	for _, want := range []string{
		"- service: service:switchback",
		"- owner: project:switchback",
		"- status: ok",
		"- live:engine: ok",
		"- live:headroom: ok",
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("runtime doctor output missing %q:\n%s", want, output)
		}
	}
}

func TestRuntimeRepairPlanSwitchbackPulseFailedHop(t *testing.T) {
	t.Parallel()
	if os.Getenv(runtimePulseRepairChildEnv) == "1" {
		cmdRuntime([]string{"repair-plan", "--switchback-pulse", os.Getenv(runtimePulsePathEnv)})
		return
	}

	homePath := t.TempDir()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	receiptPath := writeRuntimePulseCLIReceipt(t, "fail", []runtimePulseCLICheck{
		{Name: "engine", Area: "engine", Level: "ok", Detail: "synthetic engine is healthy"},
		{Name: "headroom", Area: "headroom", Level: "ok", Detail: "synthetic optimizer is healthy"},
		{Name: "config-drift", Area: "config", Level: "warn", Detail: "synthetic config differs"},
		{Name: "tap:synthetic capture/one", Area: "taps", Level: "fail", Detail: "synthetic-capture.invalid is not listening"},
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeRepairPlanSwitchbackPulseFailedHop$")
	cmd.Env = append(os.Environ(),
		runtimePulseRepairChildEnv+"=1",
		runtimePulsePathEnv+"="+receiptPath,
		"COMPOUND_HOME="+homePath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime repair-plan --switchback-pulse failed: %v\n%s", err, output)
	}
	for _, want := range []string{
		"# runtime repair-plan rrp_",
		"- service: service:switchback",
		"- failed_checks: 1",
		"- writeback_gate: owg_",
		"- receipt: rec_",
		"- transition: tr_",
		"- review_yaml:",
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("runtime repair-plan output missing %q:\n%s", want, output)
		}
	}

	planPath := outputValue(string(output), "- plan:")
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read runtime repair plan: %v\n%s", err, output)
	}
	var plan runtimebinding.RepairPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatalf("parse runtime repair plan: %v", err)
	}
	if plan.FailedCount != 1 || len(plan.FailedChecks) != 1 || plan.FailedChecks[0].Name != "live:tap:synthetic capture/one" {
		t.Fatalf("repair plan lost exact failed hop: %+v", plan)
	}
	if !strings.Contains(plan.FailedChecks[0].Detail, "synthetic-capture.invalid is not listening") {
		t.Fatalf("repair plan lost sanitized failed-hop evidence: %+v", plan.FailedChecks[0])
	}

	reviewPath := outputValue(string(output), "- review:")
	review, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatalf("read runtime repair review: %v", err)
	}
	if !strings.Contains(string(review), "does not mutate runtime owner files") {
		t.Fatalf("runtime repair review lost no-actuation boundary:\n%s", review)
	}
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "claims.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("pulse repair-plan must not promote claims, stat err=%v", err)
	}
}

func TestRuntimeDoctorSwitchbackPulseRejectsLiveFlag(t *testing.T) {
	t.Parallel()
	if os.Getenv(runtimePulseLiveChildEnv) == "1" {
		cmdRuntime([]string{"doctor", "--switchback-pulse", os.Getenv(runtimePulsePathEnv), "--live"})
		return
	}

	receiptPath := writeRuntimePulseCLIReceipt(t, "ok", []runtimePulseCLICheck{
		{Name: "engine", Area: "engine", Level: "ok", Detail: "synthetic engine is healthy"},
		{Name: "headroom", Area: "headroom", Level: "ok", Detail: "synthetic optimizer is healthy"},
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeDoctorSwitchbackPulseRejectsLiveFlag$")
	cmd.Env = append(os.Environ(),
		runtimePulseLiveChildEnv+"=1",
		runtimePulsePathEnv+"="+receiptPath,
	)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("pulse evidence combined with --live should exit 2, err=%v output=%s", err, output)
	}
	if !strings.Contains(string(output), "--switchback-pulse cannot be combined with --live") {
		t.Fatalf("pulse/live conflict missing actionable error: %s", output)
	}
}

func writeRuntimePulseCLIReceipt(t *testing.T, overall string, checks []runtimePulseCLICheck) string {
	t.Helper()
	summary := map[string]int{"ok": 0, "warn": 0, "fail": 0}
	for _, check := range checks {
		summary[check.Level]++
	}
	body, err := json.Marshal(map[string]any{
		"schema":      runtimebinding.SwitchbackPulseSchema,
		"ts":          time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		"mode":        "fast",
		"overall":     overall,
		"summary":     summary,
		"duration_ms": 25,
		"checks":      checks,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "switchback-pulse.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
