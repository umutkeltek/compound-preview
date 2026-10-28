package policy

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
)

// Desktop surface: WHERE an action lands on the machine the operator is using,
// independent of what it mutates. The operator works on this Mac while agents
// run, so a click that is "local_reversible" inside an isolated browser profile
// and the same click delivered to the operator's own Chrome or the frontmost
// window are different actions. Mutation class cannot tell them apart; this
// dimension does.
//
// Only operator_browser and foreground_gui can take the desktop away from the
// operator. They are held in EVERY posture, because the operator is at the
// keyboard precisely when a takeover hurts most: an interactive posture says a
// human is present, not that the human agreed to lose the screen. The one thing
// that lifts the hold is a standing grant whose desktop_surface names that
// surface. The isolated rungs and background_gui never change a decision.
const (
	DesktopSurfaceNone             = "none"
	DesktopSurfaceIsolatedHeadless = "isolated_headless"
	DesktopSurfaceIsolatedVisible  = "isolated_visible"
	DesktopSurfaceBackgroundGUI    = "background_gui"
	DesktopSurfaceOperatorBrowser  = "operator_browser"
	DesktopSurfaceForegroundGUI    = "foreground_gui"
)

// Posture words, restated from harness (which imports this package and so
// cannot be imported here). internal/harness pins the two spellings equal.
const (
	PostureInteractive = "interactive"
	PostureUnattended  = "unattended"
)

var desktopSurfaceRank = map[string]int{
	DesktopSurfaceNone:             0,
	DesktopSurfaceIsolatedHeadless: 1,
	DesktopSurfaceIsolatedVisible:  2,
	DesktopSurfaceBackgroundGUI:    3,
	DesktopSurfaceOperatorBrowser:  4,
	DesktopSurfaceForegroundGUI:    5,
}

// DesktopSurfaceTakesOperatorDesktop reports whether a surface acts on the
// operator's live session rather than an isolated or backgrounded one.
func DesktopSurfaceTakesOperatorDesktop(surface string) bool {
	return surface == DesktopSurfaceOperatorBrowser || surface == DesktopSurfaceForegroundGUI
}

// DesktopSurfaceUnattendedReason is the exact hold reason for a desktop-taking
// action on an unattended lane. It is deliberately NOT on the autonomy
// envelope's clearable list: a standing project scope says what a lane may
// change, not that it may take the operator's screen.
func DesktopSurfaceUnattendedReason(surface string) string {
	return "desktop_surface:" + surface + ":unattended"
}

// DesktopSurfaceAttendedReason is the exact hold reason for a desktop-taking
// action where the operator is (or may be) at the keyboard and no standing
// grant names the surface. Like the unattended reason it is not clearable by
// the autonomy envelope.
func DesktopSurfaceAttendedReason(surface string) string {
	return "desktop_surface:" + surface + ":attended"
}

// desktopSurfaceGranted reports whether an eligible standing grant names the
// surface. Only an explicit desktop_surface match counts; capability, mutation
// class and every other grant field are irrelevant to this lift.
func desktopSurfaceGranted(grants []StandingGrant, surface string, now time.Time) bool {
	for _, g := range grants {
		if strings.TrimSpace(g.DesktopSurface) != surface {
			continue
		}
		if g.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, g.ExpiresAt); err != nil || !now.Before(t) {
				continue
			}
		}
		if _, inactive := grantNotYetActive(g, now); inactive {
			continue
		}
		return true
	}
	return false
}

// DesktopSurfaceObligation is the duty a granted desktop-taking action
// carries. It names the surface so the receipt says which desktop was used.
func DesktopSurfaceObligation(surface string) string {
	return "desktop_surface:" + surface + ": operator-live surface; record it on the Receipt"
}

// postureUnattended treats only an empty posture (a caller that predates the
// field) as interactive. A posture spelled any other way than the two known
// words is not evidence that a human is present.
func postureUnattended(posture string) bool {
	switch strings.ToLower(strings.TrimSpace(posture)) {
	case "", PostureInteractive:
		return false
	}
	return true
}

// applyDesktopSurfaceObligation attaches the interactive obligation to an
// outcome that is already allowed. It never lowers a hold or deny: the
// unattended case reaches here as a security-floor hold and passes untouched.
func applyDesktopSurfaceObligation(pd envelope.PolicyDecision, ai envelope.ActionIntent, now time.Time) envelope.PolicyDecision {
	surface := strings.TrimSpace(ai.DesktopSurface)
	if !DesktopSurfaceTakesOperatorDesktop(surface) {
		return pd
	}
	obligation := DesktopSurfaceObligation(surface)
	switch pd.Outcome {
	case "allow":
		pd.Outcome = "allow_with_obligations"
		pd.Obligations = mergeStrings(pd.Obligations, obligation)
		pd.Reasons = append(pd.Reasons, fmt.Sprintf("desktop_surface:%s acts on the operator's live desktop; obligation attached", surface))
		pd.ID = idFor("pol_", ai.ID, pd.Outcome, "desktop-surface", surface, ts(now))
	case "allow_with_obligations":
		pd.Obligations = mergeStrings(pd.Obligations, obligation)
	}
	return pd
}

// classifyDesktopSurface reads the surface from the tool name and, for shell
// and PTY tools, from the command words. Server-specific MCP prefixes are
// checked before generic name fragments so that, for example, Claude-in-Chrome's
// own `computer` tool stays operator_browser.
func classifyDesktopSurface(tool string, arguments map[string]any) string {
	switch {
	case strings.HasPrefix(tool, "mcp__claude-in-chrome__"), strings.HasPrefix(tool, "mcp__claude_in_chrome__"):
		return DesktopSurfaceOperatorBrowser
	case strings.HasPrefix(tool, "mcp__cmux-cua__"), strings.HasPrefix(tool, "mcp__cmux_cua__"):
		return cmuxCUADesktopSurface(toolOperation(tool), arguments)
	case isComputerUseTool(tool):
		return DesktopSurfaceForegroundGUI
	case strings.HasPrefix(tool, "mcp__browser__"), strings.Contains(tool, "playwright"):
		return DesktopSurfaceIsolatedVisible
	case isPTYTool(tool):
		return shellDesktopSurface(stringArgument(arguments, "chars"))
	case isShellTool(tool):
		return shellDesktopSurface(stringArgument(arguments, "command", "cmd"))
	}
	return DesktopSurfaceNone
}

func isComputerUseTool(tool string) bool {
	return tool == "computer" ||
		strings.Contains(tool, "computer_use") || strings.Contains(tool, "computer-use") ||
		(strings.HasPrefix(tool, "mcp__") && toolOperation(tool) == "computer")
}

// cmuxCUADesktopSurface: cmux-cua acts on backgrounded windows by contract.
// bring_to_front, hotkey, and press_key are foreground by name; every other
// tool becomes foreground when the call asks for foreground delivery. Batch and
// replay tools carry their actions inside the arguments, where a foreground
// step cannot be read without parsing the batch, so they fail closed.
func cmuxCUADesktopSurface(operation string, arguments map[string]any) string {
	switch operation {
	case "bring_to_front", "hotkey", "press_key", "perform_actions", "replay_trajectory":
		return DesktopSurfaceForegroundGUI
	}
	if cmuxCUAForegroundRequested(arguments) {
		return DesktopSurfaceForegroundGUI
	}
	return DesktopSurfaceBackgroundGUI
}

func cmuxCUAForegroundRequested(arguments map[string]any) bool {
	for _, key := range []string{"delivery_mode", "dispatch"} {
		if strings.EqualFold(stringArgument(arguments, key), "foreground") {
			return true
		}
	}
	for _, key := range []string{"foreground", "activate"} {
		if strings.EqualFold(stringArgument(arguments, key), "true") {
			return true
		}
	}
	return false
}

// shellDesktopSurface returns the worst surface any segment of the command
// reaches. A command the lexer cannot read is already an operator-gated hold
// in the shell classifier, so it contributes no surface of its own here.
func shellDesktopSurface(command string) string {
	if strings.TrimSpace(command) == "" {
		return DesktopSurfaceNone
	}
	segments, ok := shellLex(command)
	if !ok {
		return DesktopSurfaceNone
	}
	worst := DesktopSurfaceNone
	for _, segment := range segments {
		if surface := shellSegmentDesktopSurface(segment.words); desktopSurfaceRank[surface] > desktopSurfaceRank[worst] {
			worst = surface
		}
	}
	return worst
}

func shellSegmentDesktopSurface(words []string) string {
	if stripped, ok := shellStripControlKeywords(words); ok {
		words = stripped
	}
	if fields, ok := shellStripCommandPrefixes(words); ok {
		words = fields
	}
	if len(words) == 0 {
		return DesktopSurfaceNone
	}
	args := words[1:]
	switch shellVerbName(words[0]) {
	case "agent-browser":
		return agentBrowserDesktopSurface(args)
	case "peekaboo":
		return peekabooDesktopSurface(args)
	case "osascript":
		return osascriptDesktopSurface(args)
	case "open":
		return openDesktopSurface(args)
	}
	return DesktopSurfaceNone
}

// agentBrowserDesktopSurface: its own profile, headless by default. Attaching
// to an existing browser (CDP, auto-connect, connect) reaches the operator's.
func agentBrowserDesktopSurface(args []string) string {
	headed := false
	for _, arg := range args {
		flag, _, _ := strings.Cut(strings.ToLower(arg), "=")
		switch flag {
		case "--cdp", "--auto-connect", "--connect", "connect":
			// `connect <port|url>` is the positional form of --cdp. Any bare
			// word "connect" fails closed to the operator's browser.
			return DesktopSurfaceOperatorBrowser
		case "--headed":
			headed = true
		}
	}
	if headed {
		return DesktopSurfaceIsolatedVisible
	}
	return DesktopSurfaceIsolatedHeadless
}

// peekabooDesktopSurface: capture and listing work on background windows;
// input, focus, and anything not on the capture list can front a window, so an
// unlisted subcommand fails closed to foreground.
func peekabooDesktopSurface(args []string) string {
	var positional []string
	for _, arg := range args {
		if strings.EqualFold(arg, "--foreground") {
			return DesktopSurfaceForegroundGUI
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, strings.ToLower(arg))
		}
	}
	if len(positional) == 0 {
		return DesktopSurfaceBackgroundGUI
	}
	switch positional[0] {
	case "image", "see", "list":
		return DesktopSurfaceBackgroundGUI
	}
	return DesktopSurfaceForegroundGUI
}

// osascriptDesktopSurface reads inline -e scripts. `activate`, or System Events
// synthesizing input, fronts an app. A script from a file or stdin cannot be
// read here and fails closed to foreground.
func osascriptDesktopSurface(args []string) string {
	var script []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-e" && i+1 < len(args) {
			script = append(script, args[i+1])
			i++
		}
	}
	if len(script) == 0 {
		return DesktopSurfaceForegroundGUI
	}
	body := strings.ToLower(strings.Join(script, "\n"))
	if strings.Contains(body, "activate") {
		return DesktopSurfaceForegroundGUI
	}
	if strings.Contains(body, "system events") &&
		(strings.Contains(body, "keystroke") || strings.Contains(body, "key code") || strings.Contains(body, "click")) {
		return DesktopSurfaceForegroundGUI
	}
	return DesktopSurfaceBackgroundGUI
}

// openDesktopSurface: `open -a App` / `open -b bundle.id` launches and fronts an
// application. Short flags may be clustered (`-na`).
func openDesktopSurface(args []string) string {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") &&
			(strings.ContainsRune(arg, 'a') || strings.ContainsRune(arg, 'b')) {
			return DesktopSurfaceForegroundGUI
		}
	}
	return DesktopSurfaceNone
}
