package operatornotice

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/reviewartifact"
)

const noticeUsage = "usage: compound notice flush [--transport buzz] [--home P] [--json]\n" +
	"       compound notice return-queue list|show <id>|ack <id> --by <actor> [--home P]\n" +
	"       compound notice deadman ping [--fail] [--home P]"

// ReviewsBuilder assembles the annotated, authority-stamped review queue for a
// home. It is injected because building it is reviewqueue.Build, which depends
// on six organs — see NotifyWithReviews for why this package must not import
// that chain itself.
type ReviewsBuilder func(homePath string, now time.Time) (*reviewartifact.QueueReport, error)

// Run is the organ entry point behind `compound notice` (the cmd-budget ratchet
// keeps cmd/compound a dispatch layer; the surface lives here).
//
// `notice flush` is the thing that makes the hold frontier reachable: without a
// caller, Notify is a function nobody runs, and the operator keeps clearing
// dialogs by hand. It stays a manual verb — emitting is governed by the act
// seam's standing grant, so a missing grant reports Refused rather than posting.
//
// Run keeps the review frontier out; RunWith is the wired entry point. Run
// exists for callers that genuinely have no review queue.
func Run(args []string) { RunWith(args, nil) }

// RunWith is Run with the review queue supplied. Without it the notifier sees
// workbench and executor holds only, which is a small minority of what actually
// needs the operator.
func RunWith(args []string, reviews ReviewsBuilder) {
	if len(args) > 0 {
		switch args[0] {
		case "return-queue":
			os.Exit(runHomeVerb(args[1:], func(homePath string, rest []string) int {
				return runReturnQueueCLI(homePath, rest, os.Stdout, os.Stderr, time.Now().UTC())
			}))
		case "deadman":
			os.Exit(runHomeVerb(args[1:], func(homePath string, rest []string) int {
				return runDeadmanCLI(homePath, rest, os.Stdout, os.Stderr)
			}))
		}
	}
	if len(args) == 0 || args[0] != "flush" {
		fmt.Fprintln(os.Stderr, noticeUsage)
		os.Exit(2)
	}
	transport, homePath, asJSON := "buzz", "", false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--transport":
			if i+1 < len(args) {
				transport = args[i+1]
				i++
			}
		case "--home":
			if i+1 < len(args) {
				homePath = args[i+1]
				i++
			}
		default:
			fmt.Fprintln(os.Stderr, noticeUsage)
			os.Exit(2)
		}
	}
	if strings.TrimSpace(homePath) == "" {
		resolved, err := home.Resolve("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "notice: resolve home: %v\n", err)
			os.Exit(1)
		}
		homePath = resolved
	}
	now := time.Now().UTC()
	var queue *reviewartifact.QueueReport
	if reviews != nil {
		built, berr := reviews(homePath, now)
		if berr != nil {
			// Fail rather than silently notifying against a partial frontier:
			// a flush that quietly saw only workbench holds is exactly the
			// failure this wiring exists to remove.
			fmt.Fprintf(os.Stderr, "notice: build review queue: %v\n", berr)
			os.Exit(1)
		}
		queue = built
	}
	report, err := NotifyWithReviews(homePath, transport, queue, now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "notice: %v\n", err)
		os.Exit(1)
	}
	if asJSON {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "notice: encode report: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(encoded))
		return
	}
	fmt.Printf("notice flush transport=%s scanned=%d emitted=%d already=%d refused=%d unnotifiable=%d doctor=%d\n",
		report.Transport, report.Scanned, report.Emitted, report.AlreadyNotified,
		report.Refused, report.Unnotifiable, report.DoctorCount)
	// A nonzero doctor count is a real signal (no standing grant, missing
	// transport config, unreadable home) and must not read as success.
	if report.DoctorCount > 0 {
		os.Exit(1)
	}
}

// runHomeVerb strips an optional `--home P` from args, resolves the home the
// same way flush does, and runs fn. It returns the exit code.
func runHomeVerb(args []string, fn func(homePath string, rest []string) int) int {
	homePath := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--home" {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, noticeUsage)
				return 2
			}
			homePath = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	if strings.TrimSpace(homePath) == "" {
		resolved, err := home.Resolve("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "notice: resolve home: %v\n", err)
			return 1
		}
		homePath = resolved
	}
	return fn(homePath, rest)
}
