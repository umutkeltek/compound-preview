package home

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const PrivateCustodyUsage = "usage: compound home private-custody repair [--home <path>|--local|--global] [--recover-stage <.artifact-N=artifact:sha256:DIGEST>]... [--dry-run] [--json]\n"

// RunPrivateCustodyCLI owns the nested private-custody grammar and rendering.
// cmd/compound only selects the home subcommand so the command boundary stays
// a thin dispatch layer.
func RunPrivateCustodyCLI(args []string, stdout, stderr io.Writer) int {
	if privateCustodyHasHelp(args) {
		fmt.Fprint(stdout, PrivateCustodyUsage)
		return 0
	}
	flagHome := ""
	dryRun, jsonOut, local, global := false, false, false, false
	var recoverStages []PrivateStageRecovery
	positionals := make([]string, 0, 2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--dry-run":
			dryRun = true
		case "--json":
			jsonOut = true
		case "--local":
			local = true
		case "--global":
			global = true
		case "--recover-stage":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "home private-custody repair: --recover-stage requires NAME=REF")
				return 2
			}
			name, ref, ok := strings.Cut(args[i+1], "=")
			if !ok {
				fmt.Fprintln(stderr, "home private-custody repair: --recover-stage requires NAME=REF")
				return 2
			}
			recoverStages = append(recoverStages, PrivateStageRecovery{Name: name, ExpectedRef: ref})
			i++
		case "--home":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintln(stderr, "home private-custody repair: --home requires a value")
				return 2
			}
			flagHome = args[i+1]
			i++
		default:
			if strings.HasPrefix(arg, "-") {
				fmt.Fprintf(stderr, "home private-custody repair: unknown arg %s\n", arg)
				return 2
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 2 || positionals[0] != "private-custody" || positionals[1] != "repair" {
		fmt.Fprint(stderr, PrivateCustodyUsage)
		return 2
	}
	if local && global || flagHome != "" && (local || global) {
		fmt.Fprintln(stderr, "home private-custody repair: choose exactly one of --home, --local, or --global")
		return 2
	}
	if err := validatePrivateStageSelections(recoverStages); err != nil {
		fmt.Fprintln(stderr, "home private-custody repair:", err)
		return 2
	}
	homePath, err := Resolve(flagHome)
	if local {
		homePath, err = LocalPath()
	}
	if err != nil {
		fmt.Fprintln(stderr, "home private-custody repair:", err)
		return 1
	}
	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{DryRun: dryRun, RecoverStages: recoverStages})
	if jsonOut {
		err = json.NewEncoder(stdout).Encode(report)
	} else {
		err = WritePrivateCustodyReport(stdout, report)
	}
	if err != nil {
		fmt.Fprintln(stderr, "home private-custody repair:", err)
		return 1
	}
	if !report.OK || report.Counts.Manual > 0 {
		return 1
	}
	return 0
}

func privateCustodyHasHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			return true
		}
	}
	return false
}

func WritePrivateCustodyReport(w io.Writer, report PrivateCustodyReport) error {
	if report.SchemaVersion == "" {
		return nil
	}
	clean := "DIRTY"
	if report.OK {
		clean = "ok"
	}
	if _, err := fmt.Fprintln(w, "private_custody:", clean); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "private_custody.root: %s\n", report.Root); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "private_custody.status: %s\n", report.Status); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "private_custody.checked: %d drift=%d manual=%d repaired=%d\n", report.Counts.Checked, report.Counts.Drift, report.Counts.Manual, report.Counts.Repaired); err != nil {
		return err
	}
	for _, finding := range report.Findings {
		if finding.Status == "ok" {
			continue
		}
		bits := []string{finding.Kind, finding.Status, finding.Path}
		if finding.Actual != "" {
			bits = append(bits, "actual="+finding.Actual)
		}
		if finding.Action != "" {
			bits = append(bits, "action="+finding.Action)
		}
		if finding.Detail != "" {
			bits = append(bits, "detail="+finding.Detail)
		}
		if _, err := fmt.Fprintf(w, "private_custody.finding: %s\n", strings.Join(bits, " ")); err != nil {
			return err
		}
	}
	for _, action := range report.Actions {
		if _, err := fmt.Fprintf(w, "private_custody.action: %s %s %s %s\n", action.Kind, action.Status, action.Path, action.Detail); err != nil {
			return err
		}
	}
	return nil
}
