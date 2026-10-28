package hookplan

import (
	"fmt"
	"os"
	"strings"

	"compound/internal/lanemanifest"
)

// InstallRequest is the resolved selection for `compound hook install`: which
// harnesses to install into, which roster (nil = every compiled binding), and
// the isolated profile override. Keeping this resolution in hookplan keeps the
// CLI thin and the cmd/compound surface within its budget; the two ways to
// select (a lane manifest via --lane-file, or an explicit --harness) converge
// here.
type InstallRequest struct {
	Harnesses     []string
	ConfigDir     string
	AllowedEvents map[string]bool
}

// ResolveInstallRequest turns the CLI's --lane-file / --harness / --roster /
// --config-dir selections into an install request. A --lane-file derives the
// install harness and the hook roster from the manifest (--roster overrides the
// manifest's declaration); an explicit --harness narrows to that harness, with
// --roster applied as an optional event filter. --lane-file and --harness are
// mutually exclusive.
func ResolveInstallRequest(laneFile, harnessFlag, rosterFlag, configDir string) (InstallRequest, error) {
	rosterEvent := func(roster string) (map[string]bool, error) {
		if strings.TrimSpace(roster) == "" {
			return nil, nil
		}
		return RosterEventSet(lanemanifest.Roster(roster))
	}
	var harnesses []string
	roster := strings.TrimSpace(rosterFlag)
	if strings.TrimSpace(laneFile) != "" {
		if strings.TrimSpace(harnessFlag) != "" {
			return InstallRequest{}, fmt.Errorf("--lane-file and --harness are mutually exclusive")
		}
		body, err := os.ReadFile(laneFile)
		if err != nil {
			return InstallRequest{}, fmt.Errorf("read lane manifest: %w", err)
		}
		manifest, err := lanemanifest.Parse(body)
		if err != nil {
			return InstallRequest{}, err
		}
		installHarness, err := manifest.InstallHarness()
		if err != nil {
			return InstallRequest{}, err
		}
		roster = string(manifest.Hooks.Roster)
		if strings.TrimSpace(rosterFlag) != "" {
			roster = strings.TrimSpace(rosterFlag)
		}
		harnesses = []string{installHarness}
	} else {
		switch strings.TrimSpace(harnessFlag) {
		case "", "all":
		case "claude-code", "claude-zai", "claude-minimax", "claude-qwen", "codex", "oh-my-pi":
			harnesses = []string{strings.TrimSpace(harnessFlag)}
		default:
			return InstallRequest{}, fmt.Errorf("--harness must be claude-code, claude-zai, claude-minimax, claude-qwen, codex, oh-my-pi, or all")
		}
	}
	allowedEvents, err := rosterEvent(roster)
	if err != nil {
		return InstallRequest{}, err
	}
	if strings.TrimSpace(configDir) != "" && (len(harnesses) != 1 || (harnesses[0] != "claude-code" && harnesses[0] != "claude-zai" && harnesses[0] != "claude-minimax" && harnesses[0] != "claude-qwen" && harnesses[0] != "oh-my-pi")) {
		return InstallRequest{}, fmt.Errorf("--config-dir requires one isolated-profile harness: claude-code, claude-zai, claude-minimax, claude-qwen, or oh-my-pi")
	}
	return InstallRequest{Harnesses: harnesses, ConfigDir: strings.TrimSpace(configDir), AllowedEvents: allowedEvents}, nil
}
