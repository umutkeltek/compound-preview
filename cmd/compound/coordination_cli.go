package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/hookplan"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

const coordinationUsage = `usage: compound coordination [--home P] [--actor A] [--json]
       compound coordination measure --report drep_... [--home P] [--json]`

// cmdCoordination is the generic, READ-ONLY coordination surface: "I am
// working on this, that actor is working on that, and I am waiting on it to
// start that job." It projects the planning half `work continue` runs
// (workbench + candidate prep + continueWait + the dispatch-plan lane-capacity
// gate) for every actor without executing any of it — no claim, advance,
// admit, dispatch-step, or record. Consumed by the console's Me / Others /
// Waiting-on view, the terminal, or another agent.
func cmdCoordination(args []string) {
	if hasUsageFlag(args) {
		fmt.Println(coordinationUsage)
		return
	}
	if len(args) > 0 && args[0] == "measure" {
		cmdCoordinationMeasure(args[1:])
		return
	}
	jsonOut := false
	actor := ""
	homeArg := ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			jsonOut = true
		case "--home":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, coordinationUsage)
				os.Exit(2)
			}
			homeArg = strings.TrimSpace(args[i])
		case "--actor":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, coordinationUsage)
				os.Exit(2)
			}
			actor = strings.TrimSpace(args[i])
		default:
			fmt.Fprintln(os.Stderr, coordinationUsage)
			os.Exit(2)
		}
	}

	homePath, err := home.Resolve(homeArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	// Read-only by registry contract: looking must not materialize work/ or
	// events/ (see work.OpenReadOnly); `measure` keeps its own opener below.
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	// One request-scoped, read-only capacity observation: every eligible actor
	// reads each capacity source at most once; dispatch/delegate keep their own guard.
	now := time.Now()
	report, err := store.Coordination(now, work.CoordinationOptions{
		Actor:         actor,
		CapacityGuard: routeevidence.NewCoordinationCapacityObservation(routeevidence.LaneCapacityConfig{}, homePath, now).Guard(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if jsonOut {
		operatorCoordination, err := hookplan.ReadOperatorCoordination(hookplan.OperatorCoordinationRequest{
			HomePath: homePath,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		payload := struct {
			work.CoordinationReport
			OperatorCoordination hookplan.OperatorCoordinationProjection `json:"operator_coordination"`
		}{
			CoordinationReport:   report,
			OperatorCoordination: operatorCoordination,
		}
		if err := json.NewEncoder(os.Stdout).Encode(payload); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	printCoordination(report)
}

func cmdCoordinationMeasure(args []string) {
	jsonOut := false
	homeArg := ""
	reportID := ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			jsonOut = true
		case "--home":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, coordinationUsage)
				os.Exit(2)
			}
			homeArg = strings.TrimSpace(args[i])
		case "--report":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, coordinationUsage)
				os.Exit(2)
			}
			reportID = strings.TrimSpace(args[i])
		default:
			fmt.Fprintln(os.Stderr, coordinationUsage)
			os.Exit(2)
		}
	}
	if reportID == "" {
		fmt.Fprintln(os.Stderr, coordinationUsage)
		os.Exit(2)
	}
	homePath, err := home.Resolve(homeArg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	receipt, err := hookplan.MeasureDeliveryOutcome(homePath, reportID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(receipt); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Printf("coordination measurement %s report=%s\n", receipt.ID, receipt.DeliveryReportID)
	fmt.Printf("- caused_action_refs: %s\n", coordinationRefs(receipt.CausedActionRefs))
	fmt.Printf("- observed_action_refs: %s\n", coordinationRefs(receipt.ObservedActionRefs))
	fmt.Printf("- observed_run_attempt_refs: %s\n", coordinationRefs(receipt.ObservedRunAttemptRefs))
	fmt.Printf("- observed_work_refs: %s\n", coordinationRefs(receipt.ObservedWorkRefs))
}

func coordinationRefs(refs []string) string {
	if len(refs) == 0 {
		return "(none)"
	}
	return strings.Join(refs, ", ")
}

func printCoordination(report work.CoordinationReport) {
	actorLabel := report.Actor
	if actorLabel == "" {
		actorLabel = "(no actors)"
	}
	mode := "chosen: most-present actor"
	if report.ActorExplicit {
		mode = "explicit"
	}
	fmt.Printf("coordination — me=%s (%s)  generated_at=%s\n\n", actorLabel, mode, report.GeneratedAt)

	printCoordinationFace("ME · ", report.Me)

	fmt.Println("\nOTHERS")
	if len(report.Others) == 0 {
		fmt.Println("  (no other actors)")
	}
	for _, face := range report.Others {
		printCoordinationFace("  · ", face)
	}

	fmt.Println("\nWAITING-ON")
	if len(report.Waits) == 0 {
		fmt.Println("  nothing is waiting on anything")
	}
	for _, edge := range report.Waits {
		target := edge.Work
		if target == "" {
			target = "—"
		}
		line := fmt.Sprintf("  %-12s waits  %-28s %s", edge.Actor, edge.Reason, target)
		if len(edge.BlockedOn) > 0 {
			line += "  [" + strings.Join(edge.BlockedOn, ", ") + "]"
		}
		if edge.Lane != nil {
			line += "  lane=" + coordinationLaneSummary(edge.Lane)
		}
		fmt.Println(line)
	}

	if len(report.Integrity) > 0 {
		fmt.Println("\nINTEGRITY")
		for _, note := range report.Integrity {
			fmt.Println("  " + note)
		}
	}
}

func printCoordinationFace(label string, face work.ActorCoordination) {
	actor := face.Actor
	if actor == "" {
		actor = "(none)"
	}
	fmt.Printf("%s%s  open=%d in_progress=%d needs_review=%d\n", label, actor, face.Open, face.InProgress, face.NeedsReview)
	if face.Doing != nil {
		fmt.Printf("      doing:  %s — %s (%s)\n", face.Doing.WorkID, face.Doing.Title, face.Doing.State)
		if strings.TrimSpace(face.Doing.NextAction) != "" {
			fmt.Printf("              %s\n", face.Doing.NextAction)
		}
	} else {
		// Idle actors compress to one line; the wait truth stays in --json.
		if !face.CanContinue {
			fmt.Printf("      doing:  idle (no safe continue step: %s)\n", firstNonEmptyCLI(face.WaitReason, "no open work"))
		} else {
			fmt.Printf("      doing:  idle (could continue: %s)\n", face.NextAction)
		}
		return
	}
	if face.CanContinue {
		fmt.Printf("      can continue: %s\n", face.NextAction)
	} else {
		fmt.Printf("      waiting: %s\n", firstNonEmptyCLI(face.WaitReason, "no safe continue step"))
		if len(face.BlockedOn) > 0 {
			fmt.Printf("      blocked on: %s\n", strings.Join(face.BlockedOn, ", "))
		}
	}
	if face.Lane != nil {
		fmt.Printf("      lane: %s\n", coordinationLaneSummary(face.Lane))
	}
}

func coordinationLaneSummary(lane *work.CoordinationLane) string {
	parts := []string{}
	if strings.TrimSpace(lane.State) != "" {
		parts = append(parts, lane.State)
	}
	parts = append(parts, fmt.Sprintf("remaining=%.0f", lane.Remaining))
	if lane.Refused {
		parts = append(parts, "refused")
	}
	if lane.ConfirmationRequired {
		parts = append(parts, "confirmation_required")
	}
	if strings.TrimSpace(lane.Message) != "" {
		parts = append(parts, lane.Message)
	}
	return strings.Join(parts, " ")
}

func firstNonEmptyCLI(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
