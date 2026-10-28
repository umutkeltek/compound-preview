package hookplan

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"compound/internal/harness"
	"compound/internal/lanemanifest"
)

// GovernedFullRosterRefusal returns a refusal reason when a hook-governed lane's
// observed hook roster is not full, and "" when the lane may proceed. It is the
// governed-spawn half of the roster contract: the manifest installs the roster
// (`hook install --lane-file`), and governed execution requires the full roster
// so governance is preserved where it matters. The gate is READ-ONLY -- it
// refuses, it never installs; installing on spawn would make a governed advance
// mutate the lane's settings as a side effect.
//
// A lane with no observable hook profile (settings file not on disk) is skipped,
// not refused: its availability is already governed by the tuple preflight and
// its mutation boundary by permission mode, so the roster requirement only binds
// lanes that actually publish one. An unreadable settings path IS refused --
// unknown is not clean, and an unreadable path is exactly the drift this gate
// exists to catch.
func GovernedFullRosterRefusal(harnessName string) string {
	base, err := os.UserHomeDir()
	if err != nil {
		return fmt.Sprintf("resolve user home for roster check: %v", err)
	}
	settingsPath, ok := LaneHookSettingsPath(base, harnessName)
	if !ok {
		return "" // not a hook-governed lane
	}
	registered, err := harness.ObserveRegisteredHookEvents(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return "" // no profile installed on this machine; tuple + permission mode govern
	}
	if err != nil {
		return fmt.Sprintf("observe lane hook roster at %s: %v", settingsPath, err)
	}
	fullEvents, err := RosterEventSet(lanemanifest.RosterFull)
	if err != nil {
		return err.Error()
	}
	var missing []string
	for event := range fullEvents {
		if !registered[event] {
			missing = append(missing, event)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return fmt.Sprintf(
		"full hook roster is not installed for %s (missing %s); governed execution requires the full roster",
		harnessName, strings.Join(missing, ", "),
	)
}

// GovernedFullRosterDoThis is the remediation compound offers when the gate refuses.
func GovernedFullRosterDoThis(harnessName string) []string {
	return []string{
		fmt.Sprintf("compound hook install --harness %s --roster full", harnessName),
		"compound hook install --lane-file <lane-manifest> --roster full --config-dir <lane-profile>",
	}
}

// GovernedRosterGate is the full fail-closed payload for a governed spawn whose
// lane does not carry the full hook roster. cmd/compound renders it as a
// gateRefusal; the strings live here so the refusal contract stays with the gate
// and the CLI caller stays a thin renderer.
type GovernedRosterGate struct {
	Command  string
	Gate     string
	Refused  string
	Tried    []string
	DoThis   []string
	Prevents string
}

// GovernedFullRosterGate returns the renderable refusal when a hook-governed
// lane's observed roster is not full, and nil when the lane may proceed.
func GovernedFullRosterGate(harnessName string) *GovernedRosterGate {
	reason := GovernedFullRosterRefusal(harnessName)
	if reason == "" {
		return nil
	}
	return &GovernedRosterGate{
		Command:  "compound work advance --execute",
		Gate:     "governed_full_roster",
		Refused:  fmt.Sprintf("%s cannot be governed yet: %s", harnessName, reason),
		Tried:    []string{"observed the lane's installed hook roster"},
		DoThis:   GovernedFullRosterDoThis(harnessName),
		Prevents: "governed execution under a lane whose governance hooks are not all installed; the minimal floor lacks the outcome/receipt/user-prompt telemetry governed execution requires.",
	}
}
