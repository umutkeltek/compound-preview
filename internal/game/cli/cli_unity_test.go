package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCmdGameUnityScaffoldJSON(t *testing.T) {
	stdout, stderr := captureStdoutStderr(t, func() {
		_ = cmdGameUnityScaffold([]string{"hypothesis_draft_test-game", "--json"})
	})
	if stderr != "" {
		t.Fatalf("stderr should be empty: %q", stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("parse: %v\nbody: %s", err, stdout)
	}
	if got["schema"] != "compound/game-unity-blueprint.v0.1" {
		t.Errorf("schema: %v", got["schema"])
	}
	if got["unity_version"] != "2022.3.20f1" {
		t.Errorf("unity_version default: %v", got["unity_version"])
	}
	if got["render_pipeline"] != "URP" {
		t.Errorf("render_pipeline default: %v", got["render_pipeline"])
	}
	// All three boundary strings must appear verbatim.
	boundary, _ := got["boundary"].([]any)
	need := map[string]bool{
		"this command emits blueprint spec only; does not write any files to disk in v0":                                                  false,
		"creating Unity project directories requires explicit human approval via --materialize, which is gated FirstPartyCommandGoverned": false,
		"Unity editor binary path is not consulted, downloaded, or invoked":                                                               false,
	}
	for _, b := range boundary {
		bs, _ := b.(string)
		if _, ok := need[bs]; ok {
			need[bs] = true
		}
	}
	for k, found := range need {
		if !found {
			t.Errorf("boundary string missing: %q", k)
		}
	}
}

func TestCmdGameUnityScaffoldHuman(t *testing.T) {
	stdout, _ := captureStdoutStderr(t, func() {
		_ = cmdGameUnityScaffold([]string{"hypothesis_draft_my-game"})
	})
	if !strings.Contains(stdout, "compound/game-unity-blueprint.v0.1") {
		t.Errorf("schema not in stdout: %s", stdout)
	}
	if !strings.Contains(stdout, "hypothesis_draft_my-game") {
		t.Errorf("idea id not in stdout: %s", stdout)
	}
	if !strings.Contains(stdout, "does not write any files to disk in v0") {
		t.Errorf("boundary not in stdout: %s", stdout)
	}
}

func TestCmdGameUnityScaffoldRequiresIdeaID(t *testing.T) {
	code, _ := runGameCmd(t, func() int {
		return cmdGameUnityScaffold([]string{"--json"})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestCmdGameUnityScaffoldRejectsFlagShapedID(t *testing.T) {
	code, _ := runGameCmd(t, func() int {
		return cmdGameUnityScaffold([]string{"--materialize", "--json"})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestCmdGameUnityDispatchesUnknownSubcommand(t *testing.T) {
	code, _ := runGameCmd(t, func() int {
		return cmdGameUnity([]string{"frob"})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestCmdGameUnityRequiresSubcommand(t *testing.T) {
	code, _ := runGameCmd(t, func() int {
		return cmdGameUnity([]string{})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestCmdGameUnityRejectsUnknownFlag(t *testing.T) {
	code, _ := runGameCmd(t, func() int {
		return cmdGameUnity([]string{"scaffold", "x", "--bogus"})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}
