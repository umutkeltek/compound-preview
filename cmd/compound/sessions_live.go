package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/work"
)

const sessionsLiveUsage = `usage: compound sessions live [--json] [--ghosts-only] [--home DIR]

Reconciles what run attempts CLAIM about their liveness against what is
OBSERVABLE right now, and names the disagreement.

  live         bound session is recorded AND observed running
  ghost        attempt is non-terminal but its recorded session is gone
  unbound      non-terminal execution with no recorded session binding
  reservation  claim-time placeholder; not a session, never judged as one
  unknown      the probe failed; no terminal state is guessed`

func cmdSessionsLive(args []string) {
	if code := runSessionsLive(args, os.Stdout, os.Stderr, time.Now()); code != 0 {
		os.Exit(code)
	}
}

func runSessionsLive(args []string, stdout, stderr io.Writer, now time.Time) int {
	return runSessionsLiveWith(args, stdout, stderr, now, nil, nil)
}

// runSessionsLiveWith is the seam tests use to inject both the
// compound-side runner and the multiplexer-side observer. nil
// arguments mean "use the production defaults from the work package".
func runSessionsLiveWith(args []string, stdout, stderr io.Writer, now time.Time, runner work.SessionRunner, mux work.MultiplexerSource) int {
	jsonOut, ghostsOnly, homeFlag := false, false, ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			jsonOut = true
		case "--ghosts-only":
			ghostsOnly = true
		case "--home":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(stderr, sessionsLiveUsage)
				return 2
			}
			homeFlag = strings.TrimSpace(args[i])
		case "-h", "--help":
			fmt.Fprintln(stdout, sessionsLiveUsage)
			return 0
		default:
			fmt.Fprintln(stderr, sessionsLiveUsage)
			return 2
		}
	}

	homePath, err := home.Resolve(homeFlag)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err, "(run `compound home init`)")
		return 1
	}
	store, err := work.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	report, err := work.SessionsLiveReconcile(store, homePath, work.SessionsLiveReconcileOptions{
		Runner:      runner,
		Multiplexer: mux,
		Now:         now,
	})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if ghostsOnly {
		report.FilterGhostsOnly()
	}
	if jsonOut {
		if err := report.WriteJSON(stdout); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	if len(report.Rows) == 0 {
		fmt.Fprintln(stdout, "no non-terminal run attempts")
		return 0
	}
	report.WriteText(stdout, now)
	return 0
}
