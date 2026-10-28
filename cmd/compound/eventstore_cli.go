package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"compound/internal/eventstore"
	"compound/internal/home"
)

const eventStoreMigrateUsage = "usage: compound event-store migrate (--dry-run | --apply | --verify) [--batch N] [--json] [--home P]"

// cmdEventStore hosts the physical-layout operations of the event store seam.
// It exposes the legacy loose-record migration, governed post-completion repair,
// and frozen-stream rebaseline; the verb name is deliberately distinct from
// the read-only `event` trace surface.
func cmdEventStore(args []string) {
	os.Exit(runEventStore(args, os.Stdout, os.Stderr))
}

func runEventStore(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: compound event-store migrate ... | rebaseline ... | repair ...")
		return 2
	}
	if hasUsageFlag(args) {
		fmt.Fprintln(stdout, "usage: compound event-store migrate ... | rebaseline ... | repair ...")
		return 0
	}
	switch args[0] {
	case "migrate":
		return runEventStoreMigrate(args[1:], stdout, stderr)
	case "rebaseline":
		return runEventStoreRebaseline(args[1:], stdout, stderr)
	case "repair":
		return runEventStoreRepair(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "event-store: unknown subcommand", args[0])
		fmt.Fprintln(stderr, "usage: compound event-store migrate ... | rebaseline ... | repair ...")
		return 2
	}
}

func runEventStoreMigrate(args []string, stdout, stderr io.Writer) int {
	if hasUsageFlag(args) {
		fmt.Fprintln(stdout, eventStoreMigrateUsage)
		return 0
	}
	var dryRun, apply, verify, jsonOut bool
	homeFlag := ""
	batch := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dryRun = true
		case "--apply":
			apply = true
		case "--verify":
			verify = true
		case "--json":
			jsonOut = true
		case "--batch":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, eventStoreMigrateUsage)
				return 2
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				fmt.Fprintln(stderr, "event-store migrate: --batch needs a positive integer")
				return 2
			}
			batch = n
		case "--home":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				fmt.Fprintln(stderr, eventStoreMigrateUsage)
				return 2
			}
			i++
			homeFlag = strings.TrimSpace(args[i])
		default:
			fmt.Fprintln(stderr, "event-store migrate: unknown arg", args[i])
			fmt.Fprintln(stderr, eventStoreMigrateUsage)
			return 2
		}
	}
	if boolCount(dryRun, apply, verify) != 1 {
		fmt.Fprintln(stderr, "event-store migrate: exactly one of --dry-run, --apply, --verify is required")
		fmt.Fprintln(stderr, eventStoreMigrateUsage)
		return 2
	}

	homePath, err := home.Resolve(homeFlag)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err, "(run `compound home init`)")
		return 1
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	opts := eventstore.MigrateOptions{BatchSize: batch}
	switch {
	case dryRun:
		opts.Mode = eventstore.ModeDryRun
	case apply:
		opts.Mode = eventstore.ModeApply
	case verify:
		opts.Mode = eventstore.ModeVerify
	}

	report, migErr := store.Migrate(opts)
	if jsonOut {
		encodeEventStoreJSON(stdout, report)
	} else {
		printMigrateReport(stdout, report)
	}
	if migErr != nil {
		if errors.Is(migErr, eventstore.ErrMigrationConflict) {
			fmt.Fprintln(stderr, "event-store migrate: HALTED on id conflict —", migErr)
		} else {
			fmt.Fprintln(stderr, "event-store migrate:", migErr)
		}
		return 1
	}

	// Exit-code contract: dry-run always reports (0 unless it surfaced a conflict);
	// apply/verify signal a closed, healthy completion gate with 0 and anything
	// else (incomplete, or a post-completion legacy writer) with 1 so scripts and
	// CI can gate on it.
	if dryRun {
		if report.Conflicts > 0 {
			return 1
		}
		return 0
	}
	if !report.Completion.Complete || report.Completion.LegacyWriterActive {
		return 1
	}
	return 0
}

func encodeEventStoreJSON(w io.Writer, report eventstore.MigrateReport) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(report)
}

func printMigrateReport(w io.Writer, report eventstore.MigrateReport) {
	fmt.Fprintf(w, "# event-store migrate (%s)\n\n", report.Mode)
	fmt.Fprintf(w, "- discovered: %d\n", report.Discovered)
	switch report.Mode {
	case "dry-run":
		fmt.Fprintf(w, "- would_migrate: %d\n", report.Migrated)
		fmt.Fprintf(w, "- would_skip_already_present: %d\n", report.AlreadyPresent)
		fmt.Fprintf(w, "- would_reject: %d\n", report.Rejected)
		if report.Conflicts > 0 {
			fmt.Fprintf(w, "- conflicts: %d %v\n", report.Conflicts, report.ConflictIDs)
		}
	default:
		fmt.Fprintf(w, "- migrated: %d\n", report.Migrated)
		fmt.Fprintf(w, "- already_present: %d\n", report.AlreadyPresent)
		fmt.Fprintf(w, "- rejected: %d\n", report.Rejected)
		fmt.Fprintf(w, "- archived: %d\n", report.Archived)
	}
	for _, family := range sortedMapKeys(report.PerFamily) {
		fmt.Fprintf(w, "- family/%s: %d\n", family, report.PerFamily[family])
	}
	for _, rej := range report.Rejections {
		fmt.Fprintf(w, "- rejected/%s: %s\n", rej.ID, rej.Reason)
	}
	if report.Mode != "dry-run" {
		comp := report.Completion
		fmt.Fprintf(w, "\n## completion gate\n\n")
		fmt.Fprintf(w, "- complete: %t\n", comp.Complete)
		fmt.Fprintf(w, "- legacy_discovered: %d\n", comp.DiscoveredExpected)
		fmt.Fprintf(w, "- remaining_loose: %d\n", comp.RemainingLoose)
		fmt.Fprintf(w, "- rollup_digest_matches_plan: %t\n", comp.RollupDigest == comp.ExpectedDigest)
		fmt.Fprintf(w, "- repair_manifests: %d\n", comp.RepairManifests)
		fmt.Fprintf(w, "- repaired_records: %d\n", comp.RepairedRecords)
		fmt.Fprintf(w, "- divergent_archived: %d\n", comp.DivergentArchived)
		fmt.Fprintf(w, "- post_plan_rows: %d\n", comp.PostPlanRows)
		if comp.LegacyWriterActive {
			fmt.Fprintf(w, "- status: degraded:legacy_writer_active\n")
		}
		for _, reason := range comp.Reasons {
			fmt.Fprintf(w, "- blocked: %s\n", reason)
		}
	}
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}
