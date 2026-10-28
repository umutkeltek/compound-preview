package main

import (
	"fmt"
	"os"

	"compound/internal/profiles"
)

func cmdProfiles(args []string) {
	usage := "usage: compound profiles <generate|check> [--installed-only]"
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if hasHelpFlag(args) {
		fmt.Println(usage)
		return
	}
	if len(args) > 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	check := false
	installedOnly := false
	switch args[0] {
	case "generate":
	case "check":
		check = true
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if len(args) == 2 {
		if args[1] != "--installed-only" || !check {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		installedOnly = true
	}

	catalog, err := profiles.LoadCatalog()
	if err != nil {
		fmt.Fprintln(os.Stderr, "profiles:", err)
		os.Exit(1)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "profiles:", err)
		os.Exit(1)
	}
	report, err := profiles.Sync(catalog.Roles, profiles.TargetsForHome(homeDir), profiles.SyncOptions{Check: check, InstalledOnly: installedOnly})
	if err != nil {
		fmt.Fprintln(os.Stderr, "profiles:", err)
		os.Exit(1)
	}
	for _, line := range report.Summary {
		fmt.Println(line)
	}
	fmt.Println()
	fmt.Println(profiles.FinalLine(report))
	if check && report.Drift > 0 {
		os.Exit(1)
	}
}
