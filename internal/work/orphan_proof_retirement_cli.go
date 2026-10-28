package work

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"compound/internal/home"
)

// RunOrphanProofRetirementCLI exposes only exact plan and operator-approved
// apply. It has no general journal deletion operation.
func RunOrphanProofRetirementCLI(operation string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("land-lease "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	homeArg := flags.String("home", "", "Compound home")
	workID := flags.String("work", "", "active recovery Work")
	reservationID := flags.String("reservation", "", "exact orphan reservation")
	planPath := flags.String("plan", "", "path to read-only exact plan JSON")
	actor := flags.String("actor", "", "recovery Work lease holder")
	operator := flags.String("operator", "", "current operator provenance")
	evidence := flags.String("evidence", "", "all-writer pause evidence")
	paused := flags.Bool("writers-paused", false, "all participating writers paused")
	expires := flags.String("expires", "", "pause expiry RFC3339, at most five minutes")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected arguments")
		return 2
	}
	root, err := home.Resolve(*homeArg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	store, err := Open(root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if operation == "orphan-plan" {
		if *workID == "" || *reservationID == "" {
			fmt.Fprintln(stderr, "--work and --reservation required")
			return 2
		}
		plan, err := store.PlanOrphanProofRetirement(*workID, *reservationID)
		if err != nil {
			fmt.Fprintln(stderr, "orphan proof retirement plan:", err)
			return 1
		}
		if err := json.NewEncoder(stdout).Encode(plan); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if operation != "orphan-apply" {
		fmt.Fprintln(stderr, "unsupported operation")
		return 2
	}
	if *workID == "" || *planPath == "" || *actor == "" {
		fmt.Fprintln(stderr, "--work, --plan, and --actor required")
		return 2
	}
	body, err := os.ReadFile(*planPath)
	if err != nil {
		fmt.Fprintln(stderr, "read plan:", err)
		return 1
	}
	var plan OrphanProofRetirementPlan
	if err := json.Unmarshal(body, &plan); err != nil {
		fmt.Fprintln(stderr, "decode plan:", err)
		return 2
	}
	end, err := time.Parse(time.RFC3339Nano, *expires)
	if err != nil {
		fmt.Fprintln(stderr, "invalid --expires:", err)
		return 2
	}
	decision, err := store.RetireOrphanProofCompletion(*workID, OrphanProofRetirementInput{
		Actor: *actor, Operator: *operator, EvidenceRef: *evidence,
		WritersPaused: *paused, ExpiresAt: end, Plan: plan,
	}, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "orphan proof retirement:", err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(decision); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
