package harness

import (
	"fmt"
	"strings"
)

// OhMyPiApprovalArgs translates the governed PermissionMode into the harness
// argv fragment oh-my-pi's CLI actually understands. The mapping is the
// single source of truth for the posture flag a governed dispatch passes;
// buildCmd calls it for every oh_my_pi launch and never inherits the host's
// config default.
//
// The OMP CLI takes `--approval-mode <mode>` with the values
// yolo|write|always-ask. Anything else must fail closed here: a typo or an
// unsupported mode that silently degraded to always-ask would re-create the
// defect this file exists to fix — a headless omp process whose write tool
// was held by an interactive permission prompt and never advanced.
//
//	bypass       -> --approval-mode=yolo (mutation without an interactive prompt)
//	acceptEdits  -> --approval-mode=write (mutations allowed, no prompt)
//	plan | default | "" -> --approval-mode=always-ask (default; non-mutation prompt)
//	anything else -> error
//
// The empty-string default is "always-ask" rather than yolo because plan /
// default / "" is the conservative posture a caller passes when they have
// not opted into mutation; a launch that needs mutation must say so
// explicitly. This matches claudePermissionArgs's conservative default.
func OhMyPiApprovalArgs(permissionMode string) ([]string, error) {
	switch strings.TrimSpace(permissionMode) {
	case "bypass":
		return []string{"--approval-mode=yolo"}, nil
	case "acceptEdits":
		return []string{"--approval-mode=write"}, nil
	case "plan", "default", "", "oneshot":
		return []string{"--approval-mode=always-ask"}, nil
	default:
		return nil, fmt.Errorf("oh_my_pi: unknown permission mode %q (recognized: bypass, acceptEdits, plan, default)", permissionMode)
	}
}

// OhMyPiHookArgs returns the `--hook=<path>` argv fragment that loads the
// Compound TS shim into omp's hook surface. The shim is the only mechanism
// that gives omp a real PreToolUse veto (the CLI itself has no permission
// gate that fires headless). When the shim is not loaded, every compound
// hold becomes fail_open — and that is exactly the silent path this packet
// exists to close.
//
// An empty / whitespace shim path is REFUSED: a `bypass` launch without a
// loadable shim would have no enforcement surface at all, and a caller who
// set bypass while the shim is missing has clearly lost the contract. The
// buildCmd caller surfaces this as a launch-readiness refusal BEFORE
// process start so no RunAttempt is minted for an unguarded run.
func OhMyPiHookArgs(shimPath string) ([]string, error) {
	if strings.TrimSpace(shimPath) == "" {
		return nil, fmt.Errorf("oh_my_pi: hook shim path is required (refused empty path)")
	}
	return []string{"--hook=" + strings.TrimSpace(shimPath)}, nil
}
