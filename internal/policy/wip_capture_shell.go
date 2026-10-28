package policy

import (
	"os"
	"path/filepath"
	"strings"
)

// Which shell shapes actually put someone else's uncommitted work at risk.
//
// The source-control gate holds a mutation when the repository carries dirty
// paths this session does not own. That rule is right for the action it was
// written against -- a write that lands on top of unreviewed changes -- and
// wrong for almost everything else, because it keys on the STATE OF THE REPO
// rather than on what the action touches. An operator with fifteen repositories
// carrying work in progress therefore had every lane held on every mutation the
// moment it entered any of them, including `git fetch`, a test run, and an
// install: commands that cannot alter a working tree at all.
//
// A hold that fires on actions which cannot cause the harm is not a narrower
// gate, it is a broader one, and it is the reason the gate reads as friction
// instead of protection.
//
// This file names the shapes that CAN cause it. Each either records foreign
// changes into history or the index, or destroys them:
//
//   - `git commit` with -a/--all/--include and no pathspec -- sweeps every
//     tracked modification, including ones this session never made, into a
//     commit. Plain `git commit` (index only) and `git commit <pathspec>` are
//     deliberate about what they capture.
//   - `git add` with a sweeping selector (-A/--all/-u/--update/./..), a glob, or
//     a directory operand. `git add <file>` names what it stages.
//   - `git stash` in any form -- it removes working-tree changes wholesale.
//   - `git checkout -- <path>` and `git restore` -- overwrite the working tree
//     from another revision.
//   - `git reset --hard` and `git clean` -- discard uncommitted work outright.
//
// Everything else is left alone: fetch, pull, push, status, log, diff, branch
// listing, test runs, builds, package installs, interpreters. None of them can
// capture or destroy a working-tree change.

var wipSweepingAddSelectors = map[string]bool{
	"-A": true, "--all": true, "-u": true, "--update": true,
	"--no-ignore-removal": true, "--ignore-removal": true,
}

// wipCommitSweepFlags make `git commit` pick up tracked modifications the
// invocation never named.
var wipCommitSweepFlags = map[string]bool{
	"-a": true, "--all": true, "--include": true, "-i": true,
}

// shellSegmentCapturesWorkInProgress reports whether one segment is one of the
// shapes above. cwd resolves a relative `git add` operand so a directory can be
// told from a file; an unresolvable operand fails closed to sweeping.
func shellSegmentCapturesWorkInProgress(segment shellSegment, cwd string) bool {
	fields, prefixesSafe := shellStripCommandPrefixes(segment.words)
	if !prefixesSafe || len(fields) == 0 {
		return false
	}
	if shellVerbName(fields[0]) != "git" {
		return false
	}
	subcommand, args, ok := gitSubcommandAndArgs(fields[1:])
	if !ok {
		return false
	}
	switch subcommand {
	case "stash", "clean":
		return true
	case "restore":
		// `--staged` alone only unstages; it leaves the working tree intact.
		return !wipRestoreIsStagedOnly(args)
	case "reset":
		return wipFlagPresent(args, "--hard")
	case "checkout":
		// A branch switch is not a working-tree overwrite; `checkout -- <path>`
		// is. The `--` separator is what distinguishes them.
		return wipFlagPresent(args, "--")
	case "commit":
		return wipCommitSweeps(args)
	case "add":
		return wipAddSweeps(args, cwd)
	}
	return false
}

func wipRestoreIsStagedOnly(args []string) bool {
	staged := false
	for _, arg := range args {
		switch arg {
		case "--staged", "-S":
			staged = true
		case "--worktree", "-W":
			return false
		}
	}
	return staged
}

func wipFlagPresent(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag {
			return true
		}
	}
	return false
}

// wipCommitSweeps reports whether a commit would pick up modifications it did
// not name. A commit carrying an explicit pathspec is deliberate even with -a.
func wipCommitSweeps(args []string) bool {
	sweeps := false
	pathspec := false
	for _, arg := range args {
		if strings.Contains(arg, shellSubstitutionPlaceholder) {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		if index := strings.Index(arg, "="); index > 0 {
			arg = arg[:index]
		}
		if wipCommitSweepFlags[arg] {
			sweeps = true
			continue
		}
		// Bundled short flags such as `-am`.
		if len(arg) > 1 && !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg[1:], "ai") {
			sweeps = true
		}
	}
	if !sweeps {
		return false
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			pathspec = i+1 < len(args)
			break
		}
		if wipCommitFlagTakesValue(args[i]) {
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			// Every flag value is consumed above, so a bare operand here is a
			// pathspec.
			pathspec = true
			break
		}
	}
	return !pathspec
}

// wipCommitFlagTakesValue reports whether a commit flag consumes the NEXT
// argument. Bundled short groups matter as much as the exact forms: `git commit
// -am msg` puts the message where a naive scan reads a pathspec, which would
// have made the single most common sweeping commit look deliberate.
func wipCommitFlagTakesValue(arg string) bool {
	switch arg {
	case "-m", "--message", "-F", "--file", "--author", "--date", "-C", "--reuse-message",
		"-c", "--reedit-message", "--fixup", "--squash", "-t", "--template",
		"--cleanup", "-S", "--gpg-sign", "--trailer":
		return true
	}
	if len(arg) < 2 || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	// A bundled group takes a value only if its LAST letter is the value-taking
	// one: `-am msg` does, `-ammsg` carries the value inline and does not.
	return strings.ContainsRune("mFCct", rune(arg[len(arg)-1]))
}

// wipAddSweeps reports whether a `git add` invocation stages more than the
// paths it names.
func wipAddSweeps(args []string, cwd string) bool {
	operands := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			operands = append(operands, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			base := arg
			if index := strings.Index(base, "="); index > 0 {
				base = base[:index]
			}
			if wipSweepingAddSelectors[base] {
				return true
			}
			// A bundled short group such as `-Au`.
			if len(base) > 1 && !strings.HasPrefix(base, "--") && strings.ContainsAny(base[1:], "Au") {
				return true
			}
			continue
		}
		operands = append(operands, arg)
	}
	if len(operands) == 0 {
		// A bare `git add` stages nothing, but `git add` with only flags we did
		// not recognize is not something to guess about.
		return false
	}
	for _, operand := range operands {
		if wipAddOperandIsBroad(operand, cwd) {
			return true
		}
	}
	return false
}

// wipAddOperandIsBroad reports whether one `git add` operand can reach beyond a
// single named file. A glob, a dot, a trailing separator, a substitution, or a
// directory all can; so does anything the boundary cannot resolve.
func wipAddOperandIsBroad(operand, cwd string) bool {
	operand = strings.TrimSpace(operand)
	if operand == "" || operand == "." || operand == ".." ||
		strings.Contains(operand, shellSubstitutionPlaceholder) ||
		strings.ContainsAny(operand, "*?[") ||
		strings.HasSuffix(operand, string(filepath.Separator)) {
		return true
	}
	resolved := operand
	if !filepath.IsAbs(resolved) {
		if strings.TrimSpace(cwd) == "" {
			return true
		}
		resolved = filepath.Join(cwd, resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		// A path that does not exist cannot be staged, so this is either a typo
		// or a pathspec the boundary cannot read. Neither is worth guessing on.
		return true
	}
	return info.IsDir()
}
