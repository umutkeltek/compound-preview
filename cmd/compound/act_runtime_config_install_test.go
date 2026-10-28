package main

import (
	"bytes"
	"strings"
	"testing"

	"compound/internal/policy"
)

func TestRunActDispatchesRuntimeConfigInstallWithoutNativeEffects(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAct([]string{"runtime-config-install", "execute", "--plan-ref", "fixture-ref", "--actor", "fixture", "--home", "relative"}, &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "runtime-config-install (plan") {
		t.Fatal("typed owner was not dispatched")
	}
}

func TestRuntimeConfigInstallCommandAdmissionIsGovernedAndAdjacentActCommandsRemainHeld(t *testing.T) {
	for _, which := range []string{"runtime-config-install", "arbitrary-installer", "repoint"} {
		r := policy.ClassifyToolAction(policy.ToolActionRequest{Tool: "Bash", Arguments: map[string]any{"command": "compound act " + which + " --home /synthetic/home"}, FirstPartyCommands: firstPartyCommandCatalog()})
		if which == "runtime-config-install" {
			if !r.Known || r.Operation != "governed_command" || r.MutationClass != "local_reversible" {
				t.Fatalf("typed native owner not registered: %+v", r)
			}
		} else if r.Known || r.MutationClass != "security" {
			t.Fatalf("adjacent command escaped existing floor: %+v", r)
		}
	}
}
