package policy

import (
	"path/filepath"
	"strings"
)

// The operator-gated shell roster: the short list of shell shapes an operator
// autonomy envelope may NEVER auto-clear.
//
// The envelope (see autonomy_envelope.go) exists because an unattended lane that
// prompts on every ordinary build command is not governed, it is frozen. But an
// envelope that clears EVERY hold is not an envelope, it is an off switch. This
// file is the other half: the shapes whose first wrong move cannot be undone by
// re-running the lane, so they keep reaching the operator no matter how wide the
// envelope around them is.
//
// Membership is deliberately a tight, named roster rather than a heuristic:
//
//   - `git push` with a history-rewriting or ref-deleting flag or refspec —
//     force, force-with-lease, force-if-includes, delete, mirror, prune, a `+ref`
//     force refspec, or a `:ref` deletion refspec. A published history is not
//     local state; re-running the lane cannot restore it.
//   - `git filter-branch` / `git filter-repo` — whole-history rewrites.
//   - `sudo` / `doas` — the envelope is scoped to a project subtree; privilege
//     escalation leaves that subtree by definition.
//   - `diskutil`, `dd`, `mkfs*`, `newfs*`, `tmutil delete*` — device- and
//     snapshot-level operations, which take the recovery path down with them.
//   - `rm` with a recursive/force flag aimed at `/`, `~`, `$HOME`, a credential
//     directory, or any path that does not resolve INSIDE the working directory
//     the envelope is scoped to.
//   - a redirection that keeps data in a `*.pem`, `id_rsa*`, or `.env*` file —
//     writing a credential is not a local-reversible edit.
//
// Scope limit, stated rather than implied: the write rule reads the shell's own
// redirection targets, which is the write channel the lexer models. A write
// performed BY a program (`cp k k.pem`) is not seen here; it is caught, if at
// all, by the forbidden-path rule on a filesystem intent.
const (
	// OperatorGatedOperation is the classification Operation for a gated shell
	// command. It rides the ActionIntent as `action`, so an auditor reading a
	// stored envelope can tell a gated hold from an ordinary one.
	OperatorGatedOperation = "operator_gated_command"

	// OperatorGatedFloorReason is the security-floor reason a gated command
	// carries. autonomyEnvelopeBlocked matches on it, so the floor and the
	// envelope share one owner for "this hold does not clear".
	OperatorGatedFloorReason = "operator-gated command shape stays with the operator; an autonomy envelope does not clear it"

	// UnreadableCommandFloorReason marks a command the lexer could not
	// decompose. It classifies exactly as it did before (unknown/security), but
	// the envelope must not clear it: "we could not read this" is not the same
	// claim as "we read this and it was ordinary".
	UnreadableCommandFloorReason = "shell command could not be decomposed; an unreadable command is not a cleared command"
)

var operatorGatedVerbs = map[string]bool{
	"sudo": true, "doas": true,
	"diskutil": true, "dd": true,
}

// operatorGatedGitSubcommands are whole-history rewrites: gated on the
// subcommand alone, with no flag able to make them safe.
var operatorGatedGitSubcommands = map[string]bool{
	"filter-branch": true, "filter-repo": true,
}

// operatorGatedPushFlags rewrite or delete published refs.
var operatorGatedPushFlags = map[string]bool{
	"-f": true, "--force": true, "--force-with-lease": true,
	"--force-if-includes": true, "--delete": true, "-d": true,
	"--mirror": true, "--prune": true,
}

// operatorGatedCredentialDirs are directories whose contents are credentials.
// Named explicitly so a command run FROM the home directory cannot reach them
// with a relative path that resolves "inside" the envelope.
var operatorGatedCredentialDirs = map[string]bool{
	".ssh": true, ".aws": true, ".gnupg": true,
}

// operatorGatedRmFlags are the flags that turn `rm` from a single-file delete
// into a subtree delete or an unstoppable one.
func operatorGatedRmFlag(arg string) bool {
	switch arg {
	case "--recursive", "--force", "--dir":
		return true
	}
	if len(arg) < 2 || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	return strings.ContainsAny(arg[1:], "rRfd")
}

// shellSegmentOperatorGated reports whether one segment names a gated shape.
// cwd is the directory the envelope is scoped to; an empty cwd means no path is
// provably inside it, so every recursive rm target reads as out of scope.
func shellSegmentOperatorGated(segment shellSegment, cwd string) bool {
	for _, target := range segment.writeTargets {
		if operatorGatedWriteTarget(target) {
			return true
		}
	}
	return operatorGatedWords(segment.words, cwd, 0)
}

// operatorGatedWords peels the wrappers that put a gated verb somewhere other
// than the first word. Without this, `env sudo rm -rf /` and `xargs rm -rf /`
// read as an ordinary `env`/`xargs` invocation here, then get UNWRAPPED by the
// classifier underneath and land in a class the envelope clears -- so the gate
// would be bypassable by prefixing the command. depth bounds the recursion;
// a wrapper chain deeper than that is not a shape anyone needs, and refusing to
// unwrap it further is the fail-closed answer.
func operatorGatedWords(words []string, cwd string, depth int) bool {
	if depth > 4 {
		return true
	}
	fields, prefixesSafe := shellStripCommandPrefixes(words)
	if !prefixesSafe {
		// An unsafe prefix already leaves the routine lane as unknown; it is not
		// this roster's call to make, and claiming it here would relabel every
		// odd wrapper as destructive.
		return false
	}
	if len(fields) == 0 {
		return false
	}
	verb := shellVerbName(fields[0])
	args := fields[1:]
	if operatorGatedVerbs[verb] {
		return true
	}
	if strings.HasPrefix(verb, "mkfs") || strings.HasPrefix(verb, "newfs") {
		return true
	}
	switch verb {
	case "tmutil":
		return len(args) > 0 && strings.HasPrefix(args[0], "delete")
	case "git":
		return operatorGatedGit(args)
	case "rm":
		return operatorGatedRm(args, cwd)
	case "xargs":
		return operatorGatedWords(operatorGatedXargsPayload(args), cwd, depth+1)
	}
	return false
}

// operatorGatedXargsPayload skips xargs' own flags to reach the command it
// runs. It mirrors shellXargsClass's flag table so the two cannot drift into
// disagreeing about where the payload starts.
func operatorGatedXargsPayload(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-I", "-J", "-n", "-P", "-s", "-L":
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		default:
			args = args[1:]
		}
	}
	return args
}

func operatorGatedGit(args []string) bool {
	subcommand, rest, ok := gitSubcommandAndArgs(args)
	if !ok {
		return false
	}
	if operatorGatedGitSubcommands[subcommand] {
		return true
	}
	if subcommand != "push" {
		return false
	}
	for _, arg := range rest {
		if operatorGatedPushFlags[arg] {
			return true
		}
		// `--force-with-lease=<ref>` and friends carry their value inline.
		if index := strings.Index(arg, "="); index > 0 && operatorGatedPushFlags[arg[:index]] {
			return true
		}
		if operatorGatedPushRefspec(arg) {
			return true
		}
	}
	return false
}

// operatorGatedPushRefspec recognizes the two refspec shapes that do not just
// add commits: a leading `+` (force) and an empty source (`:ref`, delete).
func operatorGatedPushRefspec(arg string) bool {
	if strings.HasPrefix(arg, "-") {
		return false
	}
	if strings.HasPrefix(arg, "+") {
		return true
	}
	source, _, found := strings.Cut(arg, ":")
	return found && strings.TrimSpace(source) == ""
}

// gitSubcommandAndArgs skips the git global flags (and their separate values)
// so `git -C /repo push --force` is not read as a push-free invocation.
func gitSubcommandAndArgs(args []string) (string, []string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return arg, args[i+1:], true
		}
		if gitGlobalFlagsWithValue[arg] {
			i++
		}
	}
	return "", nil, false
}

func operatorGatedRm(args []string, cwd string) bool {
	recursive := false
	targets := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--" {
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			if operatorGatedRmFlag(arg) {
				recursive = true
			}
			continue
		}
		targets = append(targets, arg)
	}
	if !recursive {
		return false
	}
	if len(targets) == 0 {
		return true
	}
	for _, target := range targets {
		if !operatorGatedRmTargetInScope(target, cwd) {
			return true
		}
	}
	return false
}

// operatorGatedRmTargetInScope reports whether a recursive-delete target
// provably resolves INSIDE cwd. It answers "provably inside", not "probably
// safe": a home-rooted path, an absolute path elsewhere, a `..` escape, a glob,
// a variable, a substitution, or a credential directory all answer false, so
// the gate stays on whatever the boundary cannot pin down.
func operatorGatedRmTargetInScope(target, cwd string) bool {
	target = strings.TrimSpace(target)
	cwd = strings.TrimSpace(cwd)
	if target == "" || cwd == "" || !filepath.IsAbs(cwd) {
		return false
	}
	if strings.ContainsAny(target, "*?[$`") ||
		strings.Contains(target, shellSubstitutionPlaceholder) ||
		strings.HasPrefix(target, "~") {
		return false
	}
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(cwd, resolved)
	}
	resolved = filepath.Clean(resolved)
	for _, segment := range strings.Split(filepath.ToSlash(resolved), "/") {
		if operatorGatedCredentialDirs[segment] {
			return false
		}
	}
	cwd = filepath.Clean(cwd)
	if resolved == cwd {
		// Deleting the envelope itself is not an in-scope edit of it.
		return false
	}
	return strings.HasPrefix(resolved, cwd+string(filepath.Separator))
}

// operatorGatedWriteTarget reports whether a redirection destination is a
// credential-shaped file.
func operatorGatedWriteTarget(target string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(target)))
	if base == "" || base == "." || base == string(filepath.Separator) {
		return false
	}
	return strings.HasSuffix(base, ".pem") ||
		strings.HasPrefix(base, "id_rsa") ||
		strings.HasPrefix(base, ".env")
}
