package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const workRouteDeviationCLIBoundarySubEnv = "COMPOUND_WORK_ROUTE_DEVIATION_CLI_BOUNDARY_SUB"

// TestWorkCreateCLIJudgeDeviationRefusedWithoutPrediction is the S3 depth test at the CLI seam: a
// `work create` declaring a judge-grade route deviation WITHOUT a staked prediction is refused at
// admission (die -> exit 1) and the error names the rule. No --deviation-card is passed, proving
// that card absence is NOT a bypass. Runs in a subprocess because the refusal path calls os.Exit.
func TestWorkCreateCLIJudgeDeviationRefusedWithoutPrediction(t *testing.T) {
	if os.Getenv(workRouteDeviationCLIBoundarySubEnv) == "judge-no-prediction" {
		cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "p",
			"--deviation-lane", "scout/chat", "--deviation-risk", "judge"})
		return
	}
	homePath := initializedTestHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkCreateCLIJudgeDeviationRefusedWithoutPrediction$")
	cmd.Env = append(os.Environ(), workRouteDeviationCLIBoundarySubEnv+"=judge-no-prediction", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("judge deviation without prediction must exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("want die() exit 1, got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "route_deviation graded") {
		t.Fatalf("refusal must name the route_deviation rule:\n%s", output)
	}
	if strings.Contains(string(output), "created ") {
		t.Fatalf("refused create must not report success:\n%s", output)
	}
}

// TestWorkCreateCLIJudgeDeviationWithCardStillRefused proves the S3 anti-bypass at the CLI seam:
// supplying --deviation-card (the advisory EV card ref) does NOT buy admission -- a judge-grade
// deviation without a staked prediction is refused whether or not the card is present.
func TestWorkCreateCLIJudgeDeviationWithCardStillRefused(t *testing.T) {
	if os.Getenv(workRouteDeviationCLIBoundarySubEnv) == "judge-card-no-prediction" {
		cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "p",
			"--deviation-lane", "scout/chat", "--deviation-risk", "judge", "--deviation-card", "evcard_abc123"})
		return
	}
	homePath := initializedTestHome(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkCreateCLIJudgeDeviationWithCardStillRefused$")
	cmd.Env = append(os.Environ(), workRouteDeviationCLIBoundarySubEnv+"=judge-card-no-prediction", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("judge deviation with a card but no prediction must exit non-zero, got err=%v output=%s", err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("want die() exit 1, got %d output=%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "route_deviation graded") {
		t.Fatalf("refusal must name the route_deviation rule even with a card present:\n%s", output)
	}
}

// TestWorkCreateCLIJudgeDeviationAdmittedWithPrediction proves the admitted arm at the CLI seam: a
// judge-grade deviation WITH a staked prediction is admitted, and the created envelope carries the
// route_deviation marker through to disk (round-trip via `work show`). Runs in-process because the
// success path does not call os.Exit.
func TestWorkCreateCLIJudgeDeviationAdmittedWithPrediction(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)

	out := captureStdout(t, func() {
		cmdWork([]string{"create", "--owner", "compound-core", "--predicate", "judge deviation admits with a stake",
			"--deviation-lane", "scout/chat", "--deviation-class", "judge-critique", "--deviation-risk", "judge",
			"--predicted-outcome", "completed", "--predicted-move-type", "rule_delta"})
	})
	created := firstJSONObject(t, out)
	var workID string
	if err := json.Unmarshal(created["id"], &workID); err != nil || workID == "" {
		t.Fatalf("create output missing id:\n%s", out)
	}

	showOut := captureStdout(t, func() {
		cmdWork([]string{"show", workID, "--json"})
	})
	shown := firstJSONObject(t, showOut)
	var deviation struct {
		Lane            string `json:"lane"`
		CapabilityClass string `json:"capability_class"`
		CardRef         string `json:"card_ref"`
		RiskGrade       string `json:"risk_grade"`
	}
	if err := json.Unmarshal(shown["route_deviation"], &deviation); err != nil {
		t.Fatalf("show output missing route_deviation field: %v\n%s", err, showOut)
	}
	if deviation.Lane != "scout/chat" || deviation.CapabilityClass != "judge-critique" || deviation.RiskGrade != "judge" {
		t.Fatalf("route_deviation = %+v, want {scout/chat judge-critique <empty> judge}", deviation)
	}
	if deviation.CardRef != "" {
		t.Fatalf("route_deviation card_ref = %q, want empty (no card was supplied)", deviation.CardRef)
	}
}
