package main

import (
	"bytes"
	"strings"
	"testing"

	"compound/internal/policy"
)

func TestRunActDispatchesCredentialVerificationWithoutNativeEffects(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAct([]string{"credential-verify", "execute", "--plan-ref", "fixture-ref"}, &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "credential-verify (plan") {
		t.Fatal("typed owner was not dispatched")
	}
}

func TestCredentialVerificationCommandAdmissionIsGovernedAndAdjacentActCommandsRemainHeld(t *testing.T) {
	for _, which := range []string{"credential-verify", "arbitrary-verifier", "repoint"} {
		r := policy.ClassifyToolAction(policy.ToolActionRequest{Tool: "Bash", Arguments: map[string]any{"command": "compound act " + which + " --home /synthetic/home"}, FirstPartyCommands: firstPartyCommandCatalog()})
		if which == "credential-verify" {
			if !r.Known || r.Operation != "governed_command" || r.MutationClass != "local_reversible" {
				t.Fatalf("typed native owner not registered: %+v", r)
			}
		} else if r.Known || r.MutationClass != "security" {
			t.Fatalf("adjacent command escaped existing floor: %+v", r)
		}
	}
}
