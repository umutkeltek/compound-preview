package main

import (
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/runattempt"
)

func TestRunAttemptListHumanOutputShowsOrigin(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}

	store := runattempt.Open(homePath)
	attempt, err := store.Mint("work_origin_fixture", "agent:test", "codex", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, runattempt.OriginClaimReservation); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdRunAttempt([]string{"list"})
	})
	if !strings.Contains(out, "origin="+runattempt.OriginClaimReservation) {
		t.Fatalf("human list omits RunAttempt origin:\n%s", out)
	}
}
