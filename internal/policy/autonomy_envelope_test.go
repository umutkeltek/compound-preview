package policy

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
)

// These tests run through the SAME two functions the live PreToolUse hook uses
// -- MonitorToolAction, which calls Evaluate -- rather than through the
// classifier alone. That is deliberate: the defect these tests pin was never a
// misclassification. `bun install` classified correctly as an install command
// every time. What was missing was that a shell intent reached the grant
// matcher with an EMPTY target path, so no path-scoped grant could ever cover
// it, and the class baseline was the only thing left to decide. A test against
// the classifier would have stayed green through the whole defect.

func envelopeNow() time.Time {
	return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
}

// envelopeConfig is the shape the operator actually runs: L3 autonomy carrying
// its authority ref, and ONE standing grant that names a project subtree and
// nothing else. It deliberately does not enumerate mutation classes,
// reversibilities, or blast radii -- if the envelope only worked for a grant
// that pre-listed every class a build can produce, it would not be an envelope.
func envelopeConfig(scope string) Config {
	cfg := DefaultConfig()
	cfg.OperatorAutonomyLevel = AutonomyLevelFullProjectScope
	cfg.OperatorAutonomyAuthorityRef = OperatorStateAuthorityRef
	cfg.StandingGrants = []StandingGrant{{
		ID:         "grant_project_scope",
		TargetPath: filepath.ToSlash(scope) + "/**",
	}}
	return cfg
}

func monitorShell(t *testing.T, cfg Config, cwd, command string) MonitoredAction {
	t.Helper()
	return MonitorToolAction(ToolActionRequest{
		WorkID:             "work_envelope",
		Actor:              "claude_minimax",
		Harness:            "claude_minimax",
		Tool:               "Bash",
		Cwd:                cwd,
		Arguments:          map[string]any{"command": command},
		ToolSchemaRevision: "builtin:bash",
	}, cfg, envelopeNow())
}

// TestShellIntentCarriesCwdAsItsBlastScope pins the seam the rest of the file
// depends on. Without a target path the grant matcher reports "target path
// empty outside target_path", which is the exact miss that made every mutating
// command in a granted project fall back to its class baseline.
func TestShellIntentCarriesCwdAsItsBlastScope(t *testing.T) {
	scope := t.TempDir()
	cwd := filepath.Join(scope, "cmd", "app")
	got := monitorShell(t, envelopeConfig(scope), cwd, "go test ./...")

	if got.Intent.Target.Path != filepath.Clean(cwd) {
		t.Fatalf("shell intent target path=%q, want the cwd %q", got.Intent.Target.Path, filepath.Clean(cwd))
	}
	if got.Intent.Target.Kind != "command" {
		t.Fatalf("target kind=%q, want command (the cwd is the SCOPE, not a new target kind)", got.Intent.Target.Kind)
	}
	for _, reason := range got.Decision.Reasons {
		if strings.Contains(reason, "target path empty") {
			t.Fatalf("grant still misses on an empty target path: %q", reason)
		}
	}

	// A tool that names its own file must keep naming it: overwriting a
	// filesystem target with the cwd would widen the field forbidden-path
	// matching depends on.
	file := MonitorToolAction(ToolActionRequest{
		WorkID: "work_envelope", Actor: "lane", Harness: "lane", Tool: "Read",
		Cwd: cwd, Arguments: map[string]any{"file_path": filepath.Join(scope, "main.go")},
		ToolSchemaRevision: "builtin:read",
	}, envelopeConfig(scope), envelopeNow())
	if file.Intent.Target.Path == filepath.Clean(cwd) {
		t.Fatalf("filesystem intent target path was overwritten with the cwd %q", cwd)
	}
}

// TestAutonomyEnvelopeClearsOrdinaryProjectWorkInScope is the falsifier for the
// freeze. Every command here reached an unattended lane as an operator prompt
// with nobody to answer it; all of them must come back Allowed while the lane
// works inside the granted subtree.
//
// viaEnvelope records WHICH of the two mechanisms carries each command, and the
// test asserts it rather than accepting either. Once a shell intent states its
// cwd, the ordinary grant match covers everything whose reversibility and blast
// radius were already inside the grant's terms -- `bun install` is security
// class, but manual/host, so the grant itself answers it. Only the commands the
// grant genuinely CANNOT reach (hard reversibility, unknown classification)
// need the envelope. Pinning the split is what stops the envelope from quietly
// becoming the path everything takes.
func TestAutonomyEnvelopeClearsOrdinaryProjectWorkInScope(t *testing.T) {
	scope := t.TempDir()
	cfg := envelopeConfig(scope)
	cwd := filepath.Join(scope, "repo")

	for _, tc := range []struct {
		command     string
		class       string
		viaEnvelope bool
	}{
		{command: "go test ./...", class: "local_reversible"},
		{command: "git commit -am x", class: "local_reversible"},
		{command: "bun install", class: "security"},
		{command: `python3 -c "print(1)"`, class: "local_reversible"},
		{command: "echo hi > out.txt", class: "local_reversible"},
		{command: "python3 -m example_module", class: "security", viaEnvelope: true},
		{command: "git push origin main", class: "external_side_effect", viaEnvelope: true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			got := monitorShell(t, cfg, cwd, tc.command)
			if !Allowed(got.Decision) {
				t.Fatalf("outcome=%q reasons=%v; want an allowing decision inside the envelope",
					got.Decision.Outcome, got.Decision.Reasons)
			}
			if got.Classification.MutationClass != tc.class {
				t.Fatalf("mutation_class=%q, want %q -- the envelope must CLEAR the class, never relabel it",
					got.Classification.MutationClass, tc.class)
			}
			if got.Decision.StandingGrantID != "grant_project_scope" {
				t.Fatalf("standing_grant_id=%q, want the grant that authorized it", got.Decision.StandingGrantID)
			}
			if len(got.Decision.Obligations) == 0 {
				t.Fatalf("cleared action carries no obligations; the autonomous path must stay accountable")
			}
			named := false
			for _, reason := range got.Decision.Reasons {
				if strings.Contains(reason, "autonomy envelope L3") &&
					strings.Contains(reason, "grant_project_scope") &&
					strings.Contains(reason, "classification=") {
					named = true
				}
			}
			if named != tc.viaEnvelope {
				t.Fatalf("cleared via envelope=%v, want %v; reasons=%v", named, tc.viaEnvelope, got.Decision.Reasons)
			}
		})
	}
}

// TestAutonomyEnvelopeStopsAtItsEdge: the same commands, one directory outside
// the grant. If these also cleared, the "envelope" would just be an off switch.
func TestAutonomyEnvelopeStopsAtItsEdge(t *testing.T) {
	scope := t.TempDir()
	outside := t.TempDir()
	cfg := envelopeConfig(scope)

	for _, command := range []string{"bun install", "python3 -m example_module", "rm -rf build"} {
		t.Run(command, func(t *testing.T) {
			got := monitorShell(t, cfg, outside, command)
			if Allowed(got.Decision) {
				t.Fatalf("outcome=%q; a command run outside %q must not be cleared by that grant",
					got.Decision.Outcome, scope)
			}
		})
	}
}

// TestOperatorGatedShapesSurviveTheEnvelope is the other half of the boundary.
// Every entry is a shape whose first wrong move cannot be undone by re-running
// the lane, so a WIDER envelope must not reach it -- these run in-scope, at L3,
// inside the grant, and must still hold.
func TestOperatorGatedShapesSurviveTheEnvelope(t *testing.T) {
	scope := t.TempDir()
	outside := t.TempDir()
	cfg := envelopeConfig(scope)
	cwd := filepath.Join(scope, "repo")

	for _, tc := range []struct {
		name    string
		command string
	}{
		{"force push", "git push --force origin main"},
		{"force-with-lease push", "git push --force-with-lease origin main"},
		{"delete refspec", "git push origin :old-branch"},
		{"delete flag", "git push --delete origin old-branch"},
		{"mirror push", "git push --mirror origin"},
		{"force refspec", "git push origin +main:main"},
		{"filter-branch", "git filter-branch --all"},
		{"filter-repo", "git filter-repo --path x"},
		{"git -C force push", "git -C /repo push --force origin main"},
		{"sudo", "sudo systemsetup -setremotelogin on"},
		{"doas", "doas rm build"},
		{"diskutil", "diskutil eraseDisk JHFS+ x disk2"},
		{"dd", "dd if=/dev/zero of=/dev/disk2"},
		{"mkfs", "mkfs.ext4 /dev/sda1"},
		{"newfs", "newfs_hfs /dev/disk2"},
		{"tmutil delete", "tmutil deletelocalsnapshots /"},
		{"rm -rf root", "rm -rf /"},
		{"rm -rf home tilde", "rm -rf ~"},
		{"rm -rf HOME var", "rm -rf $HOME"},
		{"rm -rf credential dir", "rm -rf ~/.ssh"},
		{"rm -rf outside envelope", "rm -rf " + outside},
		{"rm -r glob", "rm -r *"},
		{"write pem", "echo k > server.pem"},
		{"write id_rsa", "cat k > id_rsa"},
		{"write dotenv", "echo A=1 > .env.production"},
		{"gated segment in a chain", "go build ./... && sudo make install"},
		{"gated inside a writing segment", "sudo rm -rf / > log.txt"},
		// Wrapper bypasses: each of these puts the gated verb somewhere other
		// than the first word, and the classifier underneath unwraps them.
		{"env wrapper", "env FOO=1 sudo make install"},
		{"nice wrapper", "nice sudo make install"},
		{"absolute path verb", "/usr/bin/sudo make install"},
		{"xargs payload", "xargs rm -rf /"},
		{"xargs flags then payload", "xargs -n 1 -I {} rm -rf /"},
		{"gated inside a command substitution", "echo $(sudo make install)"},
		{"rm escaping upward", "rm -rf ../elsewhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := monitorShell(t, cfg, cwd, tc.command)
			if Allowed(got.Decision) {
				t.Fatalf("outcome=%q reasons=%v; an operator-gated shape must never be cleared by an envelope",
					got.Decision.Outcome, got.Decision.Reasons)
			}
			if !got.Classification.OperatorGated {
				t.Fatalf("classification does not mark the shape operator-gated: %+v", got.Classification)
			}
		})
	}
}

// TestOperatorGatedRosterStaysTight is the counterweight. A roster that gates
// the near misses is just the freeze again under a new name: the operator would
// still be prompted for ordinary work, and would still learn to click through.
// Every command here is one keystroke from a gated shape and must still clear.
func TestOperatorGatedRosterStaysTight(t *testing.T) {
	scope := t.TempDir()
	cfg := envelopeConfig(scope)
	cwd := filepath.Join(scope, "repo")

	for _, tc := range []struct {
		name    string
		command string
	}{
		{"ordinary push", "git push origin main"},
		{"push a normal refspec", "git push origin main:main"},
		{"pull is not a rewrite", "git pull --rebase origin main"},
		{"branch delete is local", "git branch -d old-branch"},
		{"rm inside the envelope", "rm -rf build"},
		{"rm a nested path inside", "rm -rf ./dist/cache"},
		{"rm without a recursive flag", "rm /etc/hosts"},
		{"write an ordinary file", "echo hi > out.txt"},
		{"write a file merely containing env", "echo hi > environment.md"},
		{"a verb that merely starts with dd", "ddtrace-run pytest"},
		{"tmutil that does not delete", "tmutil listlocalsnapshots /"},
		{"xargs over a read", "xargs grep -l TODO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := monitorShell(t, cfg, cwd, tc.command)
			if got.Classification.OperatorGated {
				t.Fatalf("near-miss command was gated: %q -> %+v", tc.command, got.Classification)
			}
			if !Allowed(got.Decision) {
				t.Fatalf("outcome=%q reasons=%v; ordinary in-scope work must not reach the operator",
					got.Decision.Outcome, got.Decision.Reasons)
			}
		})
	}
}

// TestUnreadableCommandIsNotAClearedCommand: a command the lexer cannot
// decompose used to hold as "unknown", and unknown is exactly the floor the
// envelope clears. Without this, `rm -rf / &` would clear -- backgrounding is
// enough to make the lexer give up.
func TestUnreadableCommandIsNotAClearedCommand(t *testing.T) {
	scope := t.TempDir()
	cfg := envelopeConfig(scope)
	cwd := filepath.Join(scope, "repo")

	for _, command := range []string{"rm -rf / &", "(rm -rf build)", "go test ./... &"} {
		t.Run(command, func(t *testing.T) {
			got := monitorShell(t, cfg, cwd, command)
			if Allowed(got.Decision) {
				t.Fatalf("outcome=%q; a command the boundary could not read must not be cleared", got.Decision.Outcome)
			}
			if got.Classification.MutationClass != "security" || got.Classification.Known {
				t.Fatalf("unreadable command was reclassified rather than left unknown: %+v", got.Classification)
			}
		})
	}
}

// TestAutonomyEnvelopeNeedsTheOperatorToHaveSetIt: the envelope is the
// operator's decision, not a default. Below L3, or without the authority ref
// that says who set the level, nothing clears.
// The probe uses an unclassified module run: it is one of the commands the ordinary
// grant match genuinely cannot reach (hard reversibility), so only the envelope
// can clear it, and each subtest below is therefore actually testing the
// envelope rather than the grant path underneath it.
func TestAutonomyEnvelopeNeedsTheOperatorToHaveSetIt(t *testing.T) {
	scope := t.TempDir()
	cwd := filepath.Join(scope, "repo")
	const probe = "python3 -m example_module"

	t.Run("L2 does not clear", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		cfg.OperatorAutonomyLevel = "L2"
		if got := monitorShell(t, cfg, cwd, probe); Allowed(got.Decision) {
			t.Fatalf("outcome=%q; only L3 opens the envelope", got.Decision.Outcome)
		}
	})

	t.Run("L3 without an authority ref does not clear", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		cfg.OperatorAutonomyAuthorityRef = ""
		if got := monitorShell(t, cfg, cwd, probe); Allowed(got.Decision) {
			t.Fatalf("outcome=%q; an autonomy level nobody signed must not widen anything", got.Decision.Outcome)
		}
	})

	t.Run("no path-scoped grant does not clear", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		cfg.StandingGrants = nil
		if got := monitorShell(t, cfg, cwd, probe); Allowed(got.Decision) {
			t.Fatalf("outcome=%q; L3 alone is not an envelope, the grant draws the scope", got.Decision.Outcome)
		}
	})

	t.Run("no cwd does not clear", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		if got := monitorShell(t, cfg, "", probe); Allowed(got.Decision) {
			t.Fatalf("outcome=%q; an intent that states no scope is not inside one", got.Decision.Outcome)
		}
	})

	// "You may read this tree" is not an authorization to mutate it. Without
	// this rule the narrowest grant in a registry could open the widest
	// envelope in it, which is the opposite of what the operator drew.
	t.Run("a read grant does not draw an envelope", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		cfg.StandingGrants = []StandingGrant{{
			ID: "grant_read_only", Capability: "filesystem.read",
			MutationClass: "read", TargetPath: filepath.ToSlash(scope) + "/**",
		}}
		if got := monitorShell(t, cfg, cwd, probe); Allowed(got.Decision) {
			t.Fatalf("outcome=%q; a read grant opened a mutation envelope", got.Decision.Outcome)
		}
	})

	// Provenance: with several path-scoped grants in force, the decision must
	// name the one that makes the clearance coherent, not whichever row sorted
	// first. An auditor reading "cleared inside <a followup-task grant>" learns
	// something misleading about why an install was allowed.
	t.Run("the strongest grant over the path is the one named", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		cfg.StandingGrants = []StandingGrant{
			{ID: "grant_read", Capability: "filesystem.read", MutationClass: "read",
				TargetPath: filepath.ToSlash(scope) + "/**"},
			{ID: "grant_followup_task", Capability: "control.followup_task",
				MutationClass: "local_reversible", TargetPath: filepath.ToSlash(scope) + "/**"},
			{ID: "grant_destructive_shell", Capability: "shell.destructive_command",
				MutationClass: "destructive", TargetPath: filepath.ToSlash(scope) + "/**"},
		}
		got := monitorShell(t, cfg, cwd, probe)
		if !Allowed(got.Decision) {
			t.Fatalf("outcome=%q reasons=%v", got.Decision.Outcome, got.Decision.Reasons)
		}
		if got.Decision.StandingGrantID != "grant_destructive_shell" {
			t.Fatalf("named grant=%q, want grant_destructive_shell -- the decision must cite the "+
				"strongest true statement backing it", got.Decision.StandingGrantID)
		}
	})

	// The counterpart: a grant pinned to ONE capability still draws the
	// envelope, because pre-naming every capability a build invokes is the
	// allowlist this change retires. matchGrant keeps enforcing the pin on the
	// ordinary path; the envelope is the wider statement layered over it.
	t.Run("a capability-pinned mutation grant draws the envelope", func(t *testing.T) {
		cfg := envelopeConfig(scope)
		cfg.StandingGrants = []StandingGrant{{
			ID: "grant_destructive_shell", Capability: "shell.destructive_command",
			MutationClass: "destructive", TargetPath: filepath.ToSlash(scope) + "/**",
		}}
		got := monitorShell(t, cfg, cwd, probe)
		if !Allowed(got.Decision) {
			t.Fatalf("outcome=%q reasons=%v; a mutation grant over the path draws the envelope",
				got.Decision.Outcome, got.Decision.Reasons)
		}
		if got.Decision.StandingGrantID != "grant_destructive_shell" {
			t.Fatalf("standing_grant_id=%q; the decision must name the grant that drew the envelope",
				got.Decision.StandingGrantID)
		}
	})
}

// TestAutonomyEnvelopeNeverClearsADeny walks the outcomes the envelope must not
// touch at all, through Evaluate rather than the monitor, because Evaluate is
// what the hook re-runs against the loaded runtime policy.
func TestAutonomyEnvelopeNeverClearsADeny(t *testing.T) {
	scope := t.TempDir()
	cfg := envelopeConfig(scope)
	now := envelopeNow()

	t.Run("forbidden path", func(t *testing.T) {
		ai := envelope.ActionIntent{
			Header: envelope.Header{ID: "act_forbidden"}, Actor: "lane", Capability: "filesystem.write", Action: "write_file",
			Target:        envelope.ActionTarget{Kind: "file", Path: filepath.Join(scope, "app.key")},
			MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local",
		}
		if pd := Evaluate(ai, cfg, now); pd.Outcome != "deny" {
			t.Fatalf("outcome=%q, want deny; a forbidden path beats every envelope", pd.Outcome)
		}
	})

	t.Run("security floor deny", func(t *testing.T) {
		ai := envelope.ActionIntent{
			Header: envelope.Header{ID: "act_deny_floor"}, Actor: "lane", Capability: "shell.local_command", Action: "local_command",
			Target:        envelope.ActionTarget{Kind: "command", Path: scope},
			MutationClass: "local_reversible", Reversibility: "easy", BlastRadius: "local",
			SecurityFloorOutcome: "deny",
			SecurityReasons:      []string{"classifier marked content malicious; classifier may quarantine but never grant"},
		}
		if pd := Evaluate(ai, cfg, now); pd.Outcome != "deny" {
			t.Fatalf("outcome=%q, want deny", pd.Outcome)
		}
	})

	t.Run("suspicious classifier hold is not clearable", func(t *testing.T) {
		ai := envelope.ActionIntent{
			Header: envelope.Header{ID: "act_suspicious"}, Actor: "lane", Capability: "shell.install_command", Action: "install_command",
			Target:        envelope.ActionTarget{Kind: "command", Path: scope},
			MutationClass: "security", Reversibility: "manual", BlastRadius: "host",
			SecurityFloorOutcome: "hold",
			SecurityReasons:      []string{"classifier marked content suspicious; classifier may raise burden but never grant"},
		}
		if pd := Evaluate(ai, cfg, now); Allowed(pd) {
			t.Fatalf("outcome=%q; an untrusted-content signal is not operator friction", pd.Outcome)
		}
	})
}

// TestAutonomyEnvelopeLeavesContextFreeToRaise: the envelope runs BEFORE live
// context, so a dirty repo or a placement miss can still escalate an action the
// envelope cleared. If it ran after, context would have stopped being able to
// raise caution at all.
func TestAutonomyEnvelopeLeavesContextFreeToRaise(t *testing.T) {
	scope := t.TempDir()
	cfg := envelopeConfig(scope)
	// Inside the grant, and with a reversibility the grant itself cannot cover,
	// so the envelope is what clears it and the escalation below is measuring
	// the envelope rather than the plain grant path.
	ai := envelope.ActionIntent{
		Header: envelope.Header{ID: "act_ctx", IDs: envelope.IDs{WorkID: "work_ctx"}},
		Actor:  "lane", Capability: "shell.network_command", Action: "network_command",
		Target:        envelope.ActionTarget{Kind: "command", Path: filepath.Join(scope, "repo")},
		MutationClass: "external_side_effect", Reversibility: "hard", BlastRadius: "internet",
	}
	if pd := Evaluate(ai, cfg, envelopeNow()); !Allowed(pd) {
		t.Fatalf("baseline outcome=%q, want the envelope to clear it first", pd.Outcome)
	}
	// The escalating observation has to be one that actually applies to this
	// action. A dirty repo alone no longer escalates a command that cannot touch
	// a working tree (see wip_capture_shell.go) -- so the intent below is marked
	// as capturing, which is the case that still re-escalates. The invariant
	// under test is unchanged: context runs AFTER the envelope and can raise.
	capturing := ai
	capturing.CapturesWorkInProgress = true
	raised := EvaluateWithContext(capturing, cfg, EvaluationContext{
		SourceControl: &SourceControlContext{
			Status: "dirty", GitRoot: scope, ChangedPaths: 3,
			SessionOwnershipKnown: true, SessionForeignChangedPaths: 3,
		},
	}, envelopeNow())
	if Allowed(raised) {
		t.Fatalf("outcome=%q; live context must still be able to raise caution on a cleared action", raised.Outcome)
	}

	// And a placement miss, which has nothing to do with working-tree state,
	// must still raise on the ordinary (non-capturing) intent.
	placement := EvaluateWithContext(ai, cfg, EvaluationContext{
		Placement: &PlacementGuardContext{
			Mode: "block", Confidence: "block", OwnerRef: "owner://elsewhere",
			TargetRef: filepath.Join(scope, "repo"), Reason: "wrong owner",
		},
	}, envelopeNow())
	if Allowed(placement) {
		t.Fatalf("outcome=%q; a placement block must still raise on a cleared action", placement.Outcome)
	}
}
