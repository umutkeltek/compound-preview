package lanes

import (
	"fmt"
	"os"
	"strings"

	"compound/internal/home"
	"compound/internal/hookplan"
)

// Readiness means the native refresh owner can handle this lane, not that its
// hooks, instruction pack, or provider have been refreshed or proven.
const statusRefreshReady = "ready"

// statusRefreshExternal marks a lane a separate adapter installs. Such a lane
// records no invocation_revision, and its hook reaches the binary through a
// stable path rather than a pinned identity, so publishing a new binary cannot
// leave it half-activated. It is reported with the command that owns it, never
// refreshed by the native writer, and never gates publication: making it a
// blocking verdict would be unsatisfiable, because no branch here can grant a
// readiness the native writer has no way to deliver.
const statusRefreshExternal = "external"

func preflightLaneRefresh(opts RefreshLaneOptions, harnessName string) LaneVerdict {
	harnessName = strings.TrimSpace(harnessName)
	v := LaneVerdict{Harness: harnessName, Status: StatusError}
	switch hookInstallHarness(harnessName) {
	case "claude-code", "claude-zai", "claude-minimax", "claude-qwen", "codex":
		if _, err := hookplan.InstallHooksWithRoster(opts.BaseDir, opts.BinaryPath, "global",
			[]string{hookInstallHarness(harnessName)}, "", nil, true, nil); err != nil {
			v.Detail = "native hook refresh preflight: " + err.Error()
			return v
		}
		v.Status = statusRefreshReady
		v.Detail = "native hook writer available; no refresh performed"
	case "oh_my_pi", "oh-my-pi":
		v.Status = statusRefreshExternal
		v.Detail = "separate adapter owns this lane: the oh-my-pi shim is installed by compound hook install --harness oh-my-pi and invokes the binary through its install path, so publication neither refreshes nor invalidates it"
		v.RepairCommand = "compound hook install --harness oh-my-pi"
	case "opencode":
		v.Status = statusRefreshExternal
		v.Detail = "separate adapter owns this lane: the OpenCode plugin and registration are installed by adapters/opencode/install.ts and resolve the binary at call time, so publication neither refreshes nor invalidates it"
		v.RepairCommand = "bun adapters/opencode/install.ts"
	default:
		v.Detail = "no native hook refresh owner for this harness; resolve its adapter activation contract before retrying"
	}
	return v
}

// PreflightInstalledRefresh is read-only, including on an absent home. Validate
// the complete required set before the first native config or authority write.
func PreflightInstalledRefresh(opts RefreshLaneOptions) ([]LaneVerdict, error) {
	names, err := InstalledHarnesses(opts.HomePath)
	if err != nil {
		return nil, err
	}
	verdicts := make([]LaneVerdict, 0, len(names))
	for _, name := range names {
		verdicts = append(verdicts, preflightLaneRefresh(opts, name))
	}
	return verdicts, nil
}

// refreshPreflightReady gates the first native write. Only lanes the native
// writer actually owns can withhold that permission; an externally-owned lane
// is reported but cannot veto, or an enrolled adapter the writer never touches
// would block every publication on this home forever.
func refreshPreflightReady(verdicts []LaneVerdict) bool {
	for _, v := range verdicts {
		if v.Status == statusRefreshExternal {
			continue
		}
		if v.Status != statusRefreshReady {
			return false
		}
	}
	return true
}

func runCLIRefreshPreflight(opts CLIRefreshOpts) error {
	// The installer asks before initializing a genuinely absent home. Existing
	// homes still need the same manifest integrity check as ordinary refresh.
	if _, err := os.Lstat(opts.HomePath); err == nil {
		if err := home.RequireMutationCompatible(opts.HomePath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	refreshOpts := RefreshLaneOptions{HomePath: opts.HomePath, BaseDir: opts.BaseDir, BinaryPath: opts.BinaryPath, Now: opts.Now}
	var verdicts []LaneVerdict
	if opts.HarnessName != "" {
		verdicts = []LaneVerdict{preflightLaneRefresh(refreshOpts, opts.HarnessName)}
	} else {
		var err error
		verdicts, err = PreflightInstalledRefresh(refreshOpts)
		if err != nil {
			return err
		}
	}
	if opts.JSON {
		if err := PrintVerdicts(os.Stdout, verdicts, true); err != nil {
			return err
		}
	} else {
		for _, v := range verdicts {
			fmt.Fprintf(os.Stdout, "refresh-preflight: %s %s: %s\n", v.Harness, v.Status, v.Detail)
		}
	}
	if !refreshPreflightReady(verdicts) {
		return fmt.Errorf("refresh preflight refused; no hook or instruction-pack refresh performed")
	}
	return nil
}
