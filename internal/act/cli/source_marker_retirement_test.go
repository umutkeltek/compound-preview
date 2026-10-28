package cli

import (
	"bytes"
	"testing"
)

func TestSourceMarkerRetirementRejectsInjectedAuthority(t *testing.T) {
	for _, extra := range [][]string{{"--run-attempt", "ratt_fixture"}, {"--decision", "pol_fixture"}, {"--command", "printf x"}, {"--transport", "generic"}, {"--credential", "synthetic-value"}} {
		args := append([]string{"execute", "--plan-ref", "sha256:fixture", "--actor", "fixture", "--home", t.TempDir()}, extra...)
		var out, err bytes.Buffer
		if RunSourceMarkerRetirement(args, &out, &err) != 2 || out.Len() != 0 {
			t.Fatal("accepted caller-supplied authority or body")
		}
	}
	var out, err bytes.Buffer
	if RunSourceMarkerRetirement([]string{"recovery-plan", "--plan-ref", "sha256:fixture", "--operation", "rollback", "--work", "work_fixture", "--actor", "fixture", "--home", t.TempDir()}, &out, &err) != 2 {
		t.Fatal("accepted unowned rollback effect")
	}
}
