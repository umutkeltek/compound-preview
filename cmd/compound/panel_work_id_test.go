package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"compound/internal/home"
	"compound/internal/panel"
	"compound/internal/work"
)

func TestPanelPlanCLIBindsExistingWorkID(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	captureStdout(t, func() {
		cmdRoute([]string{"import-model-capabilities", filepath.Join("..", "..", "fixtures", "panel", "model-capabilities.yaml")})
	})
	captureStdout(t, func() {
		cmdPanel([]string{"import-lenses", filepath.Join("..", "..", "fixtures", "panel", "lens-manifest.yaml")})
	})
	seedPanelCLITarget(t, homePath, "mcand_example_api_clean")
	workStore, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := workStore.Create(work.CreateInput{
		Owner:            "cli-task-owner",
		Actor:            "cli-task-owner",
		DesiredOutcome:   "Review the selected capability evidence.",
		SuccessPredicate: "Panel output stays on the existing task.",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create parent work: %v", err)
	}

	out := captureStdout(t, func() {
		cmdPanel([]string{
			"plan", "--work-id", parent.ID,
			"--target-kind", "memory_candidate",
			"--target", "mcand_example_api_clean",
			"--state", "parked",
			"--not-privacy-sensitive", "synthetic fixtures",
			"--max-items", "1",
			"--json",
		})
	})
	var planned panel.Run
	if err := json.Unmarshal([]byte(out), &planned); err != nil {
		t.Fatalf("parse bound panel plan: %v\n%s", err, out)
	}
	if planned.WorkID != parent.ID || !planned.WorkBound {
		t.Fatalf("bound panel plan = work %q bound=%t, want %q/true", planned.WorkID, planned.WorkBound, parent.ID)
	}
}
