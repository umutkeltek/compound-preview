package policy

import (
	"path/filepath"
	"strconv"
	"strings"
)

// The routine-read and scoped local-write lanes for shell commands.
//
// classifyShellAction first checks the operator roster, then recognizes scoped
// local writes and routine reads. Everything else is classified by its worst
// segment, including executable command substitutions, and unknown verbs keep
// their security classification.
//
// Why the verb and not the command text: substring matching cannot tell
// `grep "^func Test"` (which contains "nc ") from netcat, and cannot tell
// `echo "curl x"` from egress. It also cannot tell `ls a; ls b` — a pair of
// reads — from a composed attack, which is why the pre-existing rule of
// "contains a shell metacharacter, therefore unknown, therefore hold" turned
// every ordinary read into an operator prompt. docs/security/prompt-injection.md
// promises that routine reads stay automatic; holding them trains the operator
// to click through holds, which defeats the boundary it is protecting.
//
// Decomposition respects quotes and separates executable substitutions from
// arguments. Heredoc bodies are stdin, not additional command segments. The
// local-write recognizer adds scoped mutations without expanding the read table.
//
// Adding a verb to any read table requires that the verb cannot write, execute,
// or egress — including through its own flags (`sed -i`, `find -exec`).

const (
	shellClassRead          = "read"
	shellClassGoverned      = "governed"
	shellClassInstall       = "install"
	shellClassUnknown       = "unknown"
	shellClassNetwork       = "network"
	shellClassDestructive   = "destructive"
	shellClassOperatorGated = "operator_gated"
)

// shellClassSeverity orders escalation when one command mixes classes: the
// worst segment decides the whole command.
var shellClassSeverity = map[string]int{
	shellClassRead:          0,
	shellClassGoverned:      1,
	shellClassInstall:       2,
	shellClassUnknown:       3,
	shellClassNetwork:       4,
	shellClassDestructive:   5,
	shellClassOperatorGated: 6,
}

// shellSubstitutionPlaceholder stands in for a command substitution body in the
// outer segment. It is not a verb in any table, so a substitution used AS the
// verb (`$(echo ls) x`) leaves the routine lane.
const shellSubstitutionPlaceholder = "compound_substitution_value"

var shellReadVerbs = map[string]bool{
	// Location and metadata.
	"cd": true, "pwd": true, "ls": true, "eza": true, "tree": true, "stat": true,
	"file": true, "basename": true, "dirname": true, "realpath": true, "readlink": true,
	"du": true, "df": true, "uptime": true, "date": true, "whoami": true, "hostname": true,
	"uname": true, "arch": true, "printenv": true, "which": true, "id": true, "groups": true,
	"sw_vers": true, "vm_stat": true, "ps": true, "lsof": true, "nproc": true,
	// Host inventory. Both dump hardware/driver state to stdout and have no
	// write, exec, or egress flag, so a capability probe ("what machine is this,
	// how much RAM") stays routine instead of prompting the operator.
	"system_profiler": true, "ioreg": true,
	// Emit and format.
	"echo": true, "printf": true, "true": true, "false": true,
	// Condition builtins. `[ -d dir ]` / `test -f file` inspect the filesystem
	// and produce only an exit status.
	"[": true, "test": true, "[[": true,
	// Read and transform text.
	"cat": true, "bat": true, "head": true, "tail": true, "wc": true, "nl": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "column": true, "rev": true,
	"fold": true, "paste": true, "join": true, "comm": true, "expand": true, "unexpand": true,
	"diff": true, "cmp": true, "diff3": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true,
	"jq": true, "yq": true,
	// Digest and inspect bytes.
	"md5": true, "md5sum": true, "shasum": true, "sha1sum": true, "sha256sum": true,
	"base64": true, "xxd": true, "od": true, "strings": true,
	// First-party read-only scanner.
	"redact": true,
}

var shellDestructiveVerbs = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "shred": true, "srm": true,
	"dd": true, "diskutil": true, "fdisk": true, "newfs": true, "truncate": true,
}

var shellNetworkVerbs = map[string]bool{
	"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true, "socat": true,
	"telnet": true, "ssh": true, "scp": true, "sftp": true, "rsync": true, "ftp": true,
	"http": true, "https": true, "httpie": true,
	"gh": true, "glab": true, "aws": true, "gcloud": true, "az": true,
	"wrangler": true, "vercel": true, "netlify": true,
}

// shellNestedReadVerbs holds first-party CLIs whose read-only subcommands are
// known. A nil value list means every invocation of that subcommand is a read.
// A verb absent here leaves the routine lane, which is why governed execution
// (`compound work advance --execute`) still reaches the operator.
var shellNestedReadVerbs = map[string]map[string][]string{
	"mcporter": {
		"list": nil,
	},
	"asc": {
		"doctor":   nil,
		"status":   nil,
		"validate": nil,
	},
	"os": {
		"status":     nil,
		"doctor":     nil,
		"capability": nil,
		"skills":     nil,
		"route":      nil,
	},
}

var gitReadSubcommands = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "rev-parse": true,
	"rev-list": true, "ls-files": true, "ls-tree": true, "merge-base": true,
	"describe": true, "blame": true, "annotate": true, "cat-file": true,
	"for-each-ref": true, "shortlog": true, "reflog": true, "count-objects": true,
	"name-rev": true, "check-ignore": true, "check-attr": true, "diff-tree": true,
	"diff-index": true, "grep": true, "show-ref": true, "symbolic-ref": true,
	"var": true, "version": true, "help": true, "whatchanged": true, "cherry": true,
	"verify-commit": true, "verify-tag": true,
}

var gitNetworkSubcommands = map[string]bool{
	"push": true, "pull": true, "fetch": true, "clone": true, "ls-remote": true,
	"submodule": true, "request-pull": true, "send-email": true, "svn": true,
}

// gitGlobalFlagsWithValue take a separate argument, so the value must not be
// mistaken for the subcommand (`git -C /path status`).
var gitGlobalFlagsWithValue = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true,
}

// gitRefQueryFlagsWithValue take a separate argument on the ref-listing
// subcommands, where the presence of a bare operand is what distinguishes
// listing from creating. Without these, `git branch -a --contains <sha>` reads
// the SHA as a new branch name and a plain listing gets held.
var gitRefQueryFlagsWithValue = map[string]bool{
	"--contains": true, "--no-contains": true,
	"--merged": true, "--no-merged": true,
	"--points-at": true, "--sort": true, "--format": true,
	"--color": true, "--abbrev": true, "-n": true,
}

// goGlobalFlagsWithValue mirrors gitGlobalFlagsWithValue: `go -C <dir> test`
// puts the directory where the subcommand would otherwise sit, so without this
// the path is read as the subcommand and a plain test run gets held.
var goGlobalFlagsWithValue = map[string]bool{
	"-C": true,
}

var goReadSubcommands = map[string]bool{
	"test": true, "vet": true, "build": true, "list": true, "env": true,
	"doc": true, "version": true,
}

// shellVersionProbeVerbs are interpreters and compilers deliberately kept OUT of
// shellReadVerbs because they execute arbitrary input (`python3 -c`, `node -e`),
// but whose version probe is a pure read. "Is python3 installed, and which one"
// is the single most common capability check an agent makes before planning
// work, and holding it trains the operator to click through holds -- the exact
// failure docs/security/prompt-injection.md warns about.
//
// The table is an explicit roster, NOT a general "any verb plus --version" rule.
// A general rule would admit any planted binary simply by naming it with a
// version flag, which is precisely what unknown-verb fail-closed exists to stop.
// Membership here is not permission to run the interpreter; see
// shellVersionProbeIsRead for the argument restriction that keeps it that way.
// Deliberately absent: node, bun, and tsc. Each already has a STRICTER owner
// that must keep winning -- bun routes through shellPackageManagerClass, and
// tsc/node require an exact trusted executable token
// (shellVerificationExecutableTrusted), which is what stops a planted `tsc` on
// PATH. Adding them here would reach past those rules.
var shellVersionProbeVerbs = map[string]bool{
	"python": true, "python3": true, "deno": true,
	"ruby": true, "perl": true, "php": true, "java": true, "javac": true,
	"rustc": true, "swiftc": true,
}

// shellVersionProbeVersionFlags are the only arguments a version probe may carry.
// --help/-h are excluded on purpose: for several of these binaries a bare -h
// takes a value (and for unknown-to-us builds could mean anything), while
// --version/-V have one meaning across the roster -- print a version and exit.
var shellVersionProbeVersionFlags = map[string]bool{
	"--version": true, "-V": true,
}

// shellVersionProbeIsRead reports whether a rostered interpreter is being asked
// only for its version. The flag must be the SOLE argument: `python3 --version`
// prints and exits, while `python3 --version -c 'code'` reaches the interpreter
// (some builds accept trailing work after a version flag) and must stay unknown.
// Dynamic arguments are rejected because a substitution could supply the code.
// shellPackageManagerVersionProbe reports whether a package-manager invocation
// is exactly `<pm> --version`. Same sole-argument rule as the interpreter
// roster: `npm --version install evil` must not read as a version probe.
func shellPackageManagerVersionProbe(verb string, args []string) bool {
	if !shellPackageManagerVerbs[verb] || len(args) != 1 {
		return false
	}
	arg := strings.TrimSpace(args[0])
	if strings.Contains(arg, shellSubstitutionPlaceholder) {
		return false
	}
	return arg == "--version"
}

// shellPackageManagerVerbs mirrors the verbs shellPackageManagerClass switches
// on. Kept as a table so the version probe cannot drift out of sync with the
// install-classification path it front-runs.
var shellPackageManagerVerbs = map[string]bool{
	"npm": true, "pnpm": true, "yarn": true, "bun": true, "bunx": true,
	"brew": true, "pip": true, "pip3": true, "gem": true, "cargo": true,
	"go": true, "uv": true, "pipx": true, "apt": true, "apt-get": true,
}

func shellVersionProbeIsRead(verb string, args []string) bool {
	if !shellVersionProbeVerbs[verb] || len(args) != 1 {
		return false
	}
	arg := strings.TrimSpace(args[0])
	if strings.Contains(arg, shellSubstitutionPlaceholder) {
		return false
	}
	return shellVersionProbeVersionFlags[arg]
}

func classifyShellAction(
	classification ActionClassification,
	command string,
	firstPartyCommands FirstPartyCommandCatalog,
	cwd string,
	forbiddenPaths ...[]string,
) ActionClassification {
	if strings.TrimSpace(command) == "" {
		classification.Known = false
		classification.Operation = "unknown"
		classification.MutationClass = "security"
		classification.Reversibility = "hard"
		classification.BlastRadius = "host"
		return classification
	}
	segments, ok := shellLex(command)
	if !ok || len(segments) == 0 {
		classification = shellClassificationFor(classification, shellClassUnknown)
		// A command the lexer could not decompose classifies exactly as it did
		// before -- unknown, fail closed to hold. What changes is that the hold
		// is not clearable: an autonomy envelope may widen what a lane does with
		// commands the boundary READ, never with commands it could not read.
		classification.OperatorGated = true
		return classification
	}
	// The gated roster runs ahead of every narrower recognizer, because those
	// recognizers can only DOWNGRADE: `git push --force origin main` would
	// otherwise be classified by the generic network lane and become clearable.
	for _, segment := range segments {
		if shellSegmentOperatorGated(segment, cwd) {
			return shellClassificationFor(classification, shellClassOperatorGated)
		}
		if shellSegmentCapturesWorkInProgress(segment, cwd) {
			classification.CapturesWorkInProgress = true
		}
	}
	paths := DefaultConfig().ForbiddenPaths
	if len(forbiddenPaths) > 0 {
		paths = append(paths, forbiddenPaths[0]...)
	}
	if local, ok := shellLocalWriteCommand(classification, segments, firstPartyCommands, cwd, paths); ok {
		local.EvidenceRefs = []string{"command:" + actionHash([]byte(command))}
		return local
	}
	if len(segments) == 1 {
		if shellExistingRemotePush(segments[0]) {
			classification = shellClassificationFor(classification, shellClassNetwork)
			classification.Operation = "git_push_existing_remote"
			classification.Destination = "git-remote:existing"
			return classification
		}
		if destination, ok := shellNoClobberWorkspaceCopyDestination(segments[0]); ok && cwd == "" {
			classification = shellClassificationFor(classification, shellClassGoverned)
			classification.TargetKind = "path"
			classification.Destination = destination
			return classification
		}
		if destination, ok := shellWorkspaceMkdirDestination(segments[0]); ok && cwd == "" {
			classification = shellClassificationFor(classification, shellClassGoverned)
			classification.TargetKind = "path"
			classification.Destination = destination
			return classification
		}
	}
	worst := shellClassRead
	for _, segment := range segments {
		class := shellSegmentClass(segment, firstPartyCommands)
		if shellClassSeverity[class] > shellClassSeverity[worst] {
			worst = class
		}
	}
	return applyShellVerbCatalog(shellClassificationFor(classification, worst), command, segments, firstPartyCommands)
}

// shellExistingRemotePush recognizes only the narrow standing-grant shape:
// the conventional existing "origin" remote with exactly one simple source
// ref. An omitted ref can consume force/delete refspecs from repository config,
// so even `git push origin` stays held. Force/delete/mirror/bulk/config-changing
// flags, URL/path destinations, refspecs, alternate remotes, git global flags,
// wrappers, and composed commands likewise stay in the generic network lane.
func shellExistingRemotePush(segment shellSegment) bool {
	if segment.writes || segment.obfuscated || len(segment.words) < 2 ||
		shellVerbName(segment.words[0]) != "git" || segment.words[1] != "push" {
		return false
	}

	args := segment.words[2:]
	return len(args) == 2 && args[0] == "origin" && shellSimplePushSource(args[1])
}

func shellSimplePushSource(source string) bool {
	if source == "" || strings.HasPrefix(source, "-") ||
		strings.HasPrefix(source, "+") || strings.HasPrefix(source, ":") {
		return false
	}
	for _, r := range source {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '/' || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func shellNoClobberWorkspaceCopyDestination(segment shellSegment) (string, bool) {
	if segment.obfuscated || segment.writes || len(segment.words) < 3 ||
		shellVerbName(segment.words[0]) != "cp" {
		return "", false
	}
	recursive, clone, noClobber := false, false, false
	operands := make([]string, 0, len(segment.words)-1)
	flagsDone := false
	for _, word := range segment.words[1:] {
		if !flagsDone && word == "--" {
			flagsDone = true
			continue
		}
		if !flagsDone && strings.HasPrefix(word, "-") {
			switch word {
			case "-R", "-r":
				recursive = true
			case "-c":
				clone = true
			case "-n":
				noClobber = true
			case "-p":
			default:
				return "", false
			}
			continue
		}
		flagsDone = true
		if strings.Contains(word, shellSubstitutionPlaceholder) {
			return "", false
		}
		operands = append(operands, word)
	}
	if !recursive || !clone || !noClobber || len(operands) < 2 {
		return "", false
	}
	return shellWorkspaceRelativeDestination(operands[len(operands)-1])
}

func shellWorkspaceMkdirDestination(segment shellSegment) (string, bool) {
	if segment.obfuscated || segment.writes || len(segment.words) != 3 ||
		shellVerbName(segment.words[0]) != "mkdir" ||
		(segment.words[1] != "-p" && segment.words[1] != "--parents") {
		return "", false
	}
	return shellWorkspaceRelativeDestination(segment.words[2])
}

func shellWorkspaceRelativeDestination(destination string) (string, bool) {
	if strings.Contains(destination, shellSubstitutionPlaceholder) {
		return "", false
	}
	clean := filepath.Clean(destination)
	if filepath.IsAbs(clean) || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) ||
		strings.HasPrefix(destination, "~") {
		return "", false
	}
	return clean, true
}

func shellClassificationFor(classification ActionClassification, class string) ActionClassification {
	switch class {
	case shellClassRead:
		classification.Operation = "local_command"
		classification.MutationClass = "local_reversible"
		classification.Reversibility = "easy"
		classification.BlastRadius = "local"
	case shellClassGoverned:
		classification.Operation = "governed_command"
		classification.MutationClass = "local_reversible"
		classification.Reversibility = "easy"
		classification.BlastRadius = "local"
	case shellClassNetwork:
		classification.Operation = "network_command"
		classification.RequiredSink = "network"
		classification.MutationClass = "external_side_effect"
		classification.Reversibility = "hard"
		classification.BlastRadius = "internet"
	case shellClassDestructive:
		classification.Operation = "destructive_command"
		classification.MutationClass = "destructive"
		classification.Reversibility = "irreversible"
		classification.BlastRadius = "host"
	case shellClassInstall:
		classification.Operation = "install_command"
		classification.RequiredSink = "install"
		classification.MutationClass = "security"
		classification.Reversibility = "manual"
		classification.BlastRadius = "host"
	case shellClassOperatorGated:
		classification.Operation = OperatorGatedOperation
		classification.OperatorGated = true
		classification.MutationClass = "destructive"
		classification.Reversibility = "irreversible"
		classification.BlastRadius = "host"
	default:
		classification.Known = false
		classification.Operation = "unknown_command"
		classification.MutationClass = "security"
		classification.Reversibility = "hard"
		classification.BlastRadius = "host"
	}
	return classification
}

// shellSegment is one executable segment of a command: the words it runs,
// whether it redirects output somewhere that keeps the data, and whether a word
// was spliced together across a quote boundary.
type shellSegment struct {
	words      []string
	writes     bool
	obfuscated bool
	dynamic    bool
	stdin      bool
	// writeTargets are the literal redirection destinations this segment keeps
	// data in. The operator-gated roster runs first; the local-write recognizer
	// then validates every target before admitting a reversible write.
	writeTargets []string
}

// shellLex decomposes a command into its executable segments in one pass,
// honoring quoting and backslash escapes the way the shell does, and collecting
// each command-substitution body as a segment of its own.
//
// Quote fidelity is a correctness requirement in both directions: `go test -run
// 'TestA|TestB'` must stay one segment (the pipe is data), and `echo "; curl x"`
// must stay one segment for the same reason — the shell runs no curl there.
//
// It reports false when the command cannot be lexed — a NUL byte, an
// unterminated quote or substitution, backgrounding, or process substitution —
// so anything unanalyzable fails closed instead of being read as a bag of words.
func shellLex(command string) ([]shellSegment, bool) {
	if strings.ContainsRune(command, 0) {
		return nil, false
	}
	segments := []shellSegment{}
	current := shellSegment{}
	word := strings.Builder{}
	trailing := []shellSegment{}
	heredocs := []shellHeredoc{}

	endWord := func() {
		if word.Len() > 0 {
			current.words = append(current.words, word.String())
			word.Reset()
		}
	}
	endSegment := func() {
		endWord()
		if len(current.words) > 0 || current.writes {
			segments = append(segments, current)
		}
		current = shellSegment{}
	}

	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '\\':
			if i+1 < len(runes) {
				if runes[i+1] == '\n' {
					return nil, false
				}
				i++
				word.WriteRune(runes[i])
				continue
			}
			word.WriteRune(r)
		case '\'':
			body, end, ok := shellReadQuoted(runes, i)
			if !ok {
				return nil, false
			}
			// An empty quote pair, or a word that continues right after the
			// closing quote, splices a verb out of fragments. Nothing routine
			// writes a command that way, so the splice itself is the signal:
			// the segment leaves the routine lane even though the assembled
			// word resolves to a real program.
			if body == "" || shellWordContinues(runes, end) {
				current.obfuscated = true
			}
			word.WriteString(body)
			i = end
		case '"':
			// Double quotes do not suppress command substitution, so the body
			// is scanned rather than taken literally.
			body, nested, end, ok := shellReadDoubleQuoted(runes, i)
			if !ok {
				return nil, false
			}
			trailing = append(trailing, nested...)
			if body == "" || shellWordContinues(runes, end) {
				current.obfuscated = true
			}
			if strings.Contains(body, shellSubstitutionPlaceholder) || strings.ContainsRune(body, '$') {
				current.dynamic = true
			}
			word.WriteString(body)
			i = end
		case '`':
			body, end, ok := shellReadBacktick(runes, i)
			if !ok {
				return nil, false
			}
			nested, ok := shellLex(body)
			if !ok {
				return nil, false
			}
			trailing = append(trailing, nested...)
			current.dynamic = true
			word.WriteString(shellSubstitutionPlaceholder)
			i = end
		case '$':
			current.dynamic = true
			if i+1 < len(runes) && runes[i+1] == '(' {
				body, end, ok := shellReadSubstitution(runes, i+1)
				if !ok {
					return nil, false
				}
				nested, ok := shellLex(body)
				if !ok {
					return nil, false
				}
				trailing = append(trailing, nested...)
				word.WriteString(shellSubstitutionPlaceholder)
				i = end
				continue
			}
			word.WriteRune(r)
		case '{', '}', '*', '?', '[':
			// Unquoted brace, parameter, and pathname expansions can turn one
			// apparent verification operand into new flags or commands.
			current.dynamic = true
			word.WriteRune(r)
		case '=':
			if i+1 < len(runes) && runes[i+1] == '(' {
				return nil, false // zsh =(command) process substitution
			}
			word.WriteRune(r)
		case '(', ')':
			return nil, false // grouping and zsh executable glob qualifiers
		case '<', '>':
			if r == '<' && i+1 < len(runes) && runes[i+1] == '<' && (i+2 == len(runes) || runes[i+2] != '<') {
				if shellAllDigits(word.String()) {
					word.Reset()
				} else {
					endWord()
				}
				doc, end, ok := shellReadHeredoc(runes, i)
				if !ok {
					return nil, false
				}
				heredocs = append(heredocs, doc)
				current.stdin = true
				i = end
				continue
			}
			if i+1 < len(runes) && runes[i+1] == '(' {
				return nil, false // process substitution runs a command
			}
			end, writes, ok := shellReadRedirection(runes, i, &word, &current)
			if !ok {
				return nil, false
			}
			if writes {
				current.writes = true
			}
			i = end
		case '&':
			if i+1 < len(runes) && runes[i+1] == '&' {
				endSegment()
				i++
				continue
			}
			if i+1 < len(runes) && runes[i+1] == '>' {
				end, writes, ok := shellReadRedirection(runes, i+1, &word, &current)
				if !ok {
					return nil, false
				}
				if writes {
					current.writes = true
				}
				i = end
				continue
			}
			return nil, false // backgrounding detaches the command
		case '|':
			endSegment()
			if i+1 < len(runes) && runes[i+1] == '|' {
				i++
			}
		case ';', '\r':
			endSegment()
		case '\n':
			endSegment()
			if len(heredocs) > 0 {
				end, ok := shellConsumeHeredocs(runes, i+1, heredocs)
				if !ok {
					return nil, false
				}
				i = end - 1
				heredocs = nil
			}
		case ' ', '\t':
			endWord()
		default:
			word.WriteRune(r)
		}
	}
	endSegment()
	if len(heredocs) > 0 {
		return nil, false
	}
	return append(segments, trailing...), true
}

// shellReadQuoted returns the body of the quoted span starting at open, with
// backslash escapes resolved inside double quotes only, as the shell does.
func shellReadQuoted(runes []rune, open int) (string, int, bool) {
	quote := runes[open]
	body := strings.Builder{}
	for i := open + 1; i < len(runes); i++ {
		if quote == '"' && runes[i] == '\\' && i+1 < len(runes) {
			next := runes[i+1]
			if shellDoubleQuoteEscapes(next) {
				i++
				if next != '\n' {
					body.WriteRune(next)
				}
			} else {
				body.WriteRune('\\')
			}
			continue
		}
		if runes[i] == quote {
			return body.String(), i, true
		}
		body.WriteRune(runes[i])
	}
	return "", 0, false
}

func shellDoubleQuoteEscapes(next rune) bool {
	switch next {
	case '$', '`', '"', '\\', '\n':
		return true
	default:
		return false
	}
}

// shellReadDoubleQuoted returns the literal text of a double-quoted span with
// every command substitution replaced by a placeholder, plus the segments those
// substitutions run. `"$(cmd)"` executes cmd, so the quotes hide nothing and the
// inner command must be classified on its own.
func shellReadDoubleQuoted(runes []rune, open int) (string, []shellSegment, int, bool) {
	text := strings.Builder{}
	nested := []shellSegment{}
	for i := open + 1; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			if i+1 < len(runes) {
				next := runes[i+1]
				if shellDoubleQuoteEscapes(next) {
					i++
					if next != '\n' {
						text.WriteRune(next)
					}
				} else {
					text.WriteRune('\\')
				}
			}
		case '"':
			return text.String(), nested, i, true
		case '`':
			body, end, ok := shellReadBacktick(runes, i)
			if !ok {
				return "", nil, 0, false
			}
			segments, ok := shellLex(body)
			if !ok {
				return "", nil, 0, false
			}
			nested = append(nested, segments...)
			text.WriteString(shellSubstitutionPlaceholder)
			i = end
		case '$':
			if i+1 < len(runes) && runes[i+1] == '(' {
				body, end, ok := shellReadSubstitution(runes, i+1)
				if !ok {
					return "", nil, 0, false
				}
				segments, ok := shellLex(body)
				if !ok {
					return "", nil, 0, false
				}
				nested = append(nested, segments...)
				text.WriteString(shellSubstitutionPlaceholder)
				i = end
				continue
			}
			text.WriteRune(runes[i])
		default:
			text.WriteRune(runes[i])
		}
	}
	return "", nil, 0, false
}

func shellReadBacktick(runes []rune, open int) (string, int, bool) {
	for i := open + 1; i < len(runes); i++ {
		if runes[i] == '\\' && i+1 < len(runes) {
			i++
			continue
		}
		if runes[i] == '`' {
			return string(runes[open+1 : i]), i, true
		}
	}
	return "", 0, false
}

// shellReadSubstitution returns the body of a $( … ) span, tracking nesting and
// skipping quoted spans so a `)` inside a string does not close it early.
func shellReadSubstitution(runes []rune, open int) (string, int, bool) {
	depth := 0
	for i := open; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			i++
		case '\'', '"':
			_, end, ok := shellReadQuoted(runes, i)
			if !ok {
				return "", 0, false
			}
			i = end
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return string(runes[open+1 : i]), i, true
			}
		}
	}
	return "", 0, false
}

// shellReadRedirection consumes a redirection operator and its target. A
// discard to /dev/null and a descriptor duplication keep no data, so they are
// not writes; anything else is. The leading file-descriptor digit belongs to
// the operator, not to the preceding word.
func shellReadRedirection(runes []rune, start int, word *strings.Builder, current *shellSegment) (int, bool, bool) {
	if digits := word.String(); digits != "" && shellAllDigits(digits) {
		word.Reset()
	}
	i := start
	inputOnly := runes[i] == '<'
	compoundOperator := false
	for i+1 < len(runes) && (runes[i+1] == '>' || runes[i+1] == '<') {
		compoundOperator = true
		i++
		if runes[i] == '>' {
			inputOnly = false
		}
	}
	descriptorDuplication := false
	if i+1 < len(runes) && runes[i+1] == '&' {
		// Only <&number and >&number duplicate descriptors. In zsh,
		// >>&word appends stdout and stderr to word, even when word looks
		// numeric, while <> is read/write. Both must remain writes.
		descriptorDuplication = !compoundOperator
		i++
	}
	for i+1 < len(runes) && (runes[i+1] == ' ' || runes[i+1] == '\t') {
		i++
	}
	target := strings.Builder{}
	for i+1 < len(runes) {
		next := runes[i+1]
		if next == ' ' || next == '\t' || next == ';' || next == '|' || next == '\n' || next == '&' {
			break
		}
		if (next == '=' || next == '<' || next == '>') && i+2 < len(runes) && runes[i+2] == '(' {
			return 0, false, false
		}
		if strings.ContainsRune("*?[]()#^", next) {
			return 0, false, false
		}
		if next == '\'' || next == '"' {
			body, end, ok := shellReadQuoted(runes, i+1)
			if !ok {
				return 0, false, false
			}
			if target.Len() > 0 || shellWordContinues(runes, end) {
				current.obfuscated = true
			}
			target.WriteString(body)
			i = end
			continue
		}
		i++
		target.WriteRune(next)
	}
	if target.Len() == 0 {
		return i, false, false
	}
	if !shellLiteralLocalPath(target.String()) {
		current.obfuscated = true
	}
	if descriptorDuplication {
		targetValue := target.String()
		return i, targetValue != "-" && !shellAllDigits(targetValue), true
	}
	if inputOnly {
		if strings.HasPrefix(target.String(), "/dev/tcp/") || strings.HasPrefix(target.String(), "/dev/udp/") {
			current.obfuscated = true
		}
		return i, false, true
	}
	writes := target.String() != "/dev/null"
	if writes {
		current.writeTargets = append(current.writeTargets, target.String())
	}
	return i, writes, true
}

// shellWordContinues reports whether the character after a closing quote keeps
// building the same word, rather than ending it.
func shellWordContinues(runes []rune, closing int) bool {
	if closing+1 >= len(runes) {
		return false
	}
	switch runes[closing+1] {
	case ' ', '\t', '\n', '\r', ';', '|', '&', '<', '>', ')':
		return false
	}
	return true
}

func shellAllDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

// shellControlKeywords are shell grammar, not commands. `for … do … done` and
// `if … then … fi` execute nothing themselves; the loop body's own segments are
// classified individually, exactly like `;` and `&&` composition. Holding a
// loop whose every body command is a read is the boundary breaking its own
// contract — it teaches the operator to click through holds.
var shellControlKeywords = map[string]bool{
	"for": true, "do": true, "done": true,
	"if": true, "then": true, "elif": true, "else": true, "fi": true,
	"while": true, "until": true, "case": true, "esac": true,
	"{": true, "}": true, "!": true,
}

// shellStripControlKeywords removes leading control-flow words. It refuses the
// `for X in <words>` header unless every word is literal, so a loop cannot
// smuggle execution through its iteration list.
func shellStripControlKeywords(words []string) ([]string, bool) {
	for len(words) > 0 && shellControlKeywords[words[0]] {
		head := words[0]
		words = words[1:]
		if head != "for" && head != "while" && head != "until" && head != "case" {
			continue
		}
		// Consume the whole `VAR in word...` header. Any substitution placeholder
		// in the iteration list means the list is computed rather than literal,
		// so the loop stops being statically readable and keeps its hold.
		for len(words) > 0 && !shellControlKeywords[words[0]] {
			if strings.Contains(words[0], shellSubstitutionPlaceholder) {
				return nil, false
			}
			words = words[1:]
		}
		// `in` is only meaningful inside this header, so it is consumed here
		// rather than treated as a standalone control token.
		if len(words) > 0 && words[0] == "in" {
			words = words[1:]
			for len(words) > 0 && !shellControlKeywords[words[0]] {
				if strings.Contains(words[0], shellSubstitutionPlaceholder) {
					return nil, false
				}
				words = words[1:]
			}
		}
	}
	return words, true
}

func shellSegmentClass(segment shellSegment, firstPartyCommands FirstPartyCommandCatalog) string {
	if segment.writes || segment.obfuscated {
		return shellClassUnknown
	}
	if stripped, ok := shellStripControlKeywords(segment.words); !ok {
		return shellClassUnknown
	} else if len(stripped) < len(segment.words) {
		if len(stripped) == 0 {
			// A pure control token (`done`, `fi`) runs nothing.
			return shellClassRead
		}
		segment.words = stripped
	}
	if shellCommandLookupIsRead(segment.words) {
		return shellClassRead
	}
	fields, prefixesSafe := shellStripCommandPrefixes(segment.words)
	if !prefixesSafe {
		return shellClassUnknown
	}
	prefixLength := len(segment.words) - len(fields)
	prefix := segment.words[:prefixLength]
	if shellPrefixChangesExecution(prefix) {
		return shellClassUnknown
	}
	if len(fields) == 0 {
		if shellPrefixHasAssignment(prefix) {
			return shellClassUnknown
		}
		return shellClassRead
	}
	verb := shellVerbName(fields[0])
	if shellDynamicArgumentsNeedHold(verb) && segment.dynamic {
		return shellClassUnknown
	}
	if !shellVerificationExecutableTrusted(fields[0], verb) {
		return shellClassUnknown
	}
	if shellVerificationVerbRequiresCleanPrefix(verb) && shellPrefixHasAssignment(prefix) {
		return shellClassUnknown
	}
	return shellVerbClass(verb, fields[1:], firstPartyCommands)
}

func shellCommandLookupIsRead(fields []string) bool {
	if len(fields) != 3 || fields[0] != "command" || (fields[1] != "-v" && fields[1] != "-V") {
		return false
	}
	name := fields[2]
	if name == "" || strings.Contains(name, shellSubstitutionPlaceholder) {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// shellPrefixChangesExecution holds prefixes that can replace the executable
// or inject startup/build instructions before the classified verb runs.
func shellPrefixChangesExecution(prefix []string) bool {
	for _, token := range prefix {
		if !shellLooksLikeAssignment(token) {
			continue
		}
		name, _, _ := strings.Cut(strings.Trim(token, `"'`), "=")
		switch name {
		case "PATH", "path", "BASH_ENV", "ENV", "BASHOPTS", "SHELLOPTS",
			"MAKEFILES", "MAKEFLAGS", "SWIFT_EXEC", "SWIFTC_EXEC",
			"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH":
			return true
		}
	}
	return false
}

func shellPrefixHasAssignment(prefix []string) bool {
	for _, token := range prefix {
		if shellLooksLikeAssignment(token) {
			return true
		}
	}
	return false
}

func shellVerificationVerbRequiresCleanPrefix(verb string) bool {
	switch verb {
	case "sed", "bash", "swift", "make", "xcodebuild", "tsc", "bun", "node", "xattr":
		return true
	default:
		return false
	}
}

func shellDynamicArgumentsNeedHold(verb string) bool {
	switch verb {
	case "sed", "bash", "swift", "make", "xcodebuild", "tsc", "bun", "node", "xattr":
		return true
	default:
		return false
	}
}

func shellVerificationExecutableTrusted(token, verb string) bool {
	token = strings.TrimSpace(strings.Trim(token, `"'`))
	switch verb {
	case "sed":
		return token == "sed" || token == "/usr/bin/sed" || token == "/bin/sed"
	case "bash":
		return token == "bash" || token == "/bin/bash"
	case "swift":
		return token == "swift" || token == "/usr/bin/swift"
	case "make":
		return token == "make" || token == "/usr/bin/make"
	case "xcodebuild":
		return token == "xcodebuild" || token == "/usr/bin/xcodebuild"
	case "tsc":
		return token == "./node_modules/.bin/tsc"
	case "bun":
		return token == "bun"
	case "node":
		return token == "node"
	case "xattr":
		return token == "xattr" || token == "/usr/bin/xattr"
	case "compound":
		return token == "compound"
	default:
		return true
	}
}

// shellStripCommandPrefixes drops environment assignments and the wrappers that
// merely run another command, so the classified verb is the one that acts.
// `sudo` is deliberately absent: it changes privilege, so it is not routine.
func shellStripCommandPrefixes(fields []string) ([]string, bool) {
	for len(fields) > 0 {
		head := fields[0]
		if shellLooksLikeAssignment(head) {
			fields = fields[1:]
			continue
		}
		wrapper := shellVerbName(head)
		switch wrapper {
		case "env":
			if !shellWrapperExecutableTrusted(head, wrapper) {
				return nil, false
			}
			fields = fields[1:]
			for len(fields) > 0 && fields[0] == "-u" {
				if len(fields) < 3 || !shellEnvironmentName(fields[1]) {
					return nil, false
				}
				fields = fields[2:]
			}
			if len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
				return nil, false
			}
			continue
		case "timeout", "gtimeout":
			if !shellWrapperExecutableTrusted(head, wrapper) {
				return nil, false
			}
			fields = fields[1:]
			if len(fields) == 0 || !shellLooksLikeDuration(fields[0]) {
				return nil, false
			}
			fields = fields[1:]
			if len(fields) == 0 {
				return nil, false
			}
			continue
		case "nice", "nohup", "stdbuf", "time", "command", "builtin", "exec":
			if !shellWrapperExecutableTrusted(head, wrapper) {
				return nil, false
			}
			fields = fields[1:]
			if wrapper == "nice" && len(fields) > 0 && fields[0] == "-n" {
				if len(fields) < 3 {
					return nil, false
				}
				if _, err := strconv.Atoi(fields[1]); err != nil {
					return nil, false
				}
				fields = fields[2:]
			}
			if len(fields) == 0 || strings.HasPrefix(fields[0], "-") {
				return nil, false
			}
			continue
		}
		break
	}
	return fields, true
}

func shellEnvironmentName(name string) bool {
	if name == "" || strings.ContainsRune(name, '=') || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	return shellLooksLikeAssignment(name + "=value")
}

func shellWrapperExecutableTrusted(token, wrapper string) bool {
	token = strings.TrimSpace(strings.Trim(token, `"'`))
	return token == wrapper
}

func shellLooksLikeAssignment(token string) bool {
	index := strings.Index(token, "=")
	if index <= 0 || strings.HasPrefix(token, "-") {
		return false
	}
	for _, r := range token[:index] {
		if r != '_' && !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func shellLooksLikeDuration(token string) bool {
	if token == "" {
		return false
	}
	digits := 0
	for i, r := range token {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r == 's' || r == 'm' || r == 'h' || r == 'd') && i == len(token)-1:
		case r == '.':
		default:
			return false
		}
	}
	return digits > 0
}

// shellVerbName reduces a command word to the program it names, so a path or a
// wrapper cannot hide the verb (`/usr/bin/curl` is curl). Quotes are trimmed
// only at the edges; an interior quote pair is left intact, so a quote-split
// verb never resolves to an allowlisted one.
func shellVerbName(token string) string {
	token = strings.TrimSpace(token)
	token = strings.TrimLeft(token, "({!")
	token = strings.Trim(token, `"'`)
	if index := strings.LastIndex(token, "/"); index >= 0 && index+1 < len(token) {
		token = token[index+1:]
	}
	return strings.ToLower(token)
}

func shellVerbClass(verb string, args []string, firstPartyCommands FirstPartyCommandCatalog) string {
	switch verb {
	case "":
		return shellClassUnknown
	case "compound":
		return shellCompoundClass(args, firstPartyCommands)
	case "codex":
		if len(args) == 1 && args[0] == "--version" {
			return shellClassRead
		}
		return shellClassUnknown
	case "git":
		return shellGitClass(args)
	case "go":
		return shellGoClass(args)
	case "gofmt":
		return shellGofmtClass(args)
	case "sb":
		return shellSwitchbackClass(args)
	case "xargs":
		return shellXargsClass(args, firstPartyCommands)
	case "bash":
		return shellBashSyntaxClass(args)
	case "swift":
		return shellSwiftClass(args)
	case "xcodebuild":
		return shellXcodebuildClass(args)
	case "xcrun":
		return shellXcrunClass(args)
	case "install.sh":
		return shellClassInstall
	case "make":
		return shellMakeVerificationClass(args)
	case "tsc":
		if len(args) == 1 && args[0] == "--noEmit" {
			return shellClassRead
		}
		return shellClassUnknown
	case "bun":
		if shellProofSmokeScript(args) {
			return shellClassRead
		}
	case "node":
		if len(args) == 2 && args[0] == "--experimental-strip-types" &&
			shellProofSmokeScript(args[1:]) {
			return shellClassRead
		}
		return shellClassUnknown
	case "xattr":
		return shellXattrClass(args)
	case "sed":
		if shellSedIsReadOnly(args) {
			return shellClassRead
		}
		return shellClassUnknown
	case "find":
		return shellFindClass(args)
	case "fd":
		if shellFlagAbsent(args, "-x", "--exec", "-X", "--exec-batch") {
			return shellClassRead
		}
		return shellClassUnknown
	case "awk", "gawk", "nawk":
		for _, arg := range args {
			// AWK's `print > file` and `printf >> file` write from inside the
			// program, beyond shell-level redirection detection. Without an AWK
			// parser, any `>` in the program fails closed; comparisons may be
			// held, but a file write must never enter the routine-read lane.
			if strings.Contains(arg, "system(") || strings.Contains(arg, "close(") || strings.Contains(arg, ">") {
				return shellClassUnknown
			}
		}
		return shellClassRead
	}
	if strings.HasPrefix(verb, "mkfs") {
		return shellClassDestructive
	}
	if shellDestructiveVerbs[verb] {
		return shellClassDestructive
	}
	if shellNetworkVerbs[verb] {
		return shellClassNetwork
	}
	if class, ok := shellPackageManagerClass(verb, args); ok {
		return class
	}
	if subcommands, ok := shellNestedReadVerbs[verb]; ok {
		return shellNestedClass(subcommands, args)
	}
	if shellReadVerbs[verb] {
		return shellClassRead
	}
	if shellVersionProbeIsRead(verb, args) {
		return shellClassRead
	}
	return shellClassUnknown
}

func shellProofSmokeScript(args []string) bool {
	if len(args) != 1 {
		return false
	}
	script := strings.TrimSpace(args[0])
	if script == "" || filepath.IsAbs(script) || strings.ContainsAny(script, "*?[") ||
		strings.Contains(script, shellSubstitutionPlaceholder) {
		return false
	}
	clean := filepath.Clean(script)
	if clean != script || filepath.Dir(clean) != "scripts" {
		return false
	}
	base := filepath.Base(clean)
	return base == "smoke.mjs" || strings.HasSuffix(base, "-smoke.mjs")
}

func shellXattrClass(args []string) string {
	if len(args) == 0 {
		return shellClassUnknown
	}
	flagsDone := false
	hasOperand := false
	for _, arg := range args {
		if !flagsDone && arg == "--" {
			flagsDone = true
			continue
		}
		if !flagsDone && strings.HasPrefix(arg, "--") {
			if arg != "--help" {
				return shellClassUnknown
			}
			continue
		}
		if !flagsDone && strings.HasPrefix(arg, "-") {
			flags := strings.TrimPrefix(arg, "-")
			if flags == "" {
				return shellClassUnknown
			}
			for _, flag := range flags {
				switch flag {
				case 'h', 'l', 'p', 'r', 's', 'v', 'x':
				case 'c', 'd', 'w':
					return shellClassUnknown
				default:
					return shellClassUnknown
				}
			}
			continue
		}
		hasOperand = true
	}
	if !hasOperand {
		return shellClassUnknown
	}
	return shellClassRead
}

func shellBashSyntaxClass(args []string) string {
	if len(args) != 2 || args[0] != "-n" {
		return shellClassUnknown
	}
	script := strings.TrimSpace(args[1])
	if script == "" || strings.HasPrefix(script, "-") {
		return shellClassUnknown
	}
	return shellClassRead
}

func shellGofmtClass(args []string) string {
	if shellFlagAbsent(args, "-w") {
		return shellClassRead
	}
	files := 0
	for _, arg := range args {
		switch arg {
		case "-w", "-s":
			continue
		}
		if strings.HasPrefix(arg, "-") ||
			strings.ContainsAny(arg, "*?[") ||
			!strings.HasSuffix(strings.ToLower(arg), ".go") {
			return shellClassUnknown
		}
		if _, ok := shellWorkspaceRelativeDestination(arg); !ok {
			return shellClassUnknown
		}
		files++
	}
	if files == 0 {
		return shellClassUnknown
	}
	return shellClassGoverned
}

func shellSwiftClass(args []string) string {
	if len(args) == 1 {
		switch args[0] {
		case "--version", "-version":
			return shellClassRead
		}
	}
	if len(args) == 2 && args[0] == "package" {
		switch args[1] {
		case "describe", "dump-package":
			return shellClassRead
		}
	}
	if len(args) == 0 {
		return shellClassUnknown
	}

	subcommand := args[0]
	if subcommand != "build" && subcommand != "test" {
		return shellClassUnknown
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--configuration", "-c":
			if i+1 >= len(args) {
				return shellClassUnknown
			}
			i++
			value := args[i]
			if value != "debug" && value != "release" {
				return shellClassUnknown
			}
		case "--jobs", "-j":
			if i+1 >= len(args) || !shellAllDigits(args[i+1]) {
				return shellClassUnknown
			}
			i++
		case "--product", "--target":
			if subcommand != "build" || i+1 >= len(args) ||
				strings.TrimSpace(args[i+1]) == "" ||
				strings.HasPrefix(args[i+1], "-") {
				return shellClassUnknown
			}
			i++
		case "--filter", "--skip":
			if subcommand != "test" || i+1 >= len(args) ||
				strings.TrimSpace(args[i+1]) == "" ||
				strings.HasPrefix(args[i+1], "-") {
				return shellClassUnknown
			}
			i++
		case "--parallel", "--no-parallel", "--list-tests", "--enable-code-coverage":
			if subcommand != "test" {
				return shellClassUnknown
			}
		default:
			return shellClassUnknown
		}
	}
	return shellClassRead
}

func shellMakeVerificationClass(args []string) string {
	if len(args) != 1 {
		return shellClassUnknown
	}
	switch args[0] {
	case "--version", "-v", "--help", "-h":
		return shellClassRead
	case "test", "build", "app", "verify-direct-release":
		return shellClassRead
	case "install":
		return shellClassInstall
	default:
		return shellClassUnknown
	}
}

func shellXcodebuildClass(args []string) string {
	actions := map[string]bool{
		"build":                 true,
		"test":                  true,
		"build-for-testing":     true,
		"test-without-building": true,
	}
	queryFlags := map[string]bool{
		"-help":                    true,
		"-usage":                   true,
		"-version":                 true,
		"-showsdks":                true,
		"-list":                    true,
		"-showbuildsettings":       true,
		"-showdestinations":        true,
		"-showbuildsettingstiming": true,
	}
	flagsWithValues := map[string]bool{
		"-project":                          true,
		"-workspace":                        true,
		"-scheme":                           true,
		"-target":                           true,
		"-configuration":                    true,
		"-sdk":                              true,
		"-arch":                             true,
		"-destination":                      true,
		"-destination-timeout":              true,
		"-jobs":                             true,
		"-parallel-testing-worker-count":    true,
		"-maximum-parallel-testing-workers": true,
		"-only-testing":                     true,
		"-skip-testing":                     true,
		"-testplan":                         true,
		"-deriveddatapath":                  true,
		"-resultbundlepath":                 true,
		"-resultstreampath":                 true,
		"-clonedsourcepackagesdirpath":      true,
		"-packagecachepath":                 true,
	}
	allowedFlags := map[string]bool{
		"-quiet":                             true,
		"-verbose":                           true,
		"-dry-run":                           true,
		"-parallelizetargets":                true,
		"-skipunavailableactions":            true,
		"-disableautomaticpackageresolution": true,
		"-onlyusepackageversionsfromresolvedfile": true,
	}

	actionCount := 0
	queryCount := 0
	signingDisabled := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		lower := strings.ToLower(arg)
		switch {
		case actions[lower]:
			actionCount++
		case queryFlags[lower]:
			queryCount++
		case flagsWithValues[lower]:
			if i+1 >= len(args) ||
				strings.TrimSpace(args[i+1]) == "" ||
				strings.HasPrefix(args[i+1], "-") {
				return shellClassUnknown
			}
			i++
		case allowedFlags[lower]:
		case arg == "CODE_SIGNING_ALLOWED=NO":
			signingDisabled = true
		default:
			return shellClassUnknown
		}
	}

	switch {
	case actionCount == 1 && queryCount == 0 && signingDisabled:
		return shellClassRead
	case actionCount == 0 && queryCount > 0 && !signingDisabled:
		return shellClassRead
	default:
		return shellClassUnknown
	}
}

func shellXcrunClass(args []string) string {
	for len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "--sdk", "--toolchain":
			if len(args) < 2 {
				return shellClassUnknown
			}
			args = args[2:]
		case "--verbose", "--no-cache", "--log":
			args = args[1:]
		default:
			goto classified
		}
	}

classified:
	if len(args) == 0 {
		return shellClassUnknown
	}
	switch strings.ToLower(args[0]) {
	case "--find":
		if len(args) == 2 {
			return shellClassRead
		}
	case "--show-sdk-path", "--show-sdk-version", "--show-sdk-build-version",
		"--show-sdk-platform-path", "--show-sdk-platform-version":
		if len(args) == 1 {
			return shellClassRead
		}
	case "simctl":
		if len(args) >= 2 {
			switch strings.ToLower(args[1]) {
			case "list", "listapps", "getenv", "help":
				return shellClassRead
			}
		}
	case "xcdevice":
		if len(args) >= 2 && strings.EqualFold(args[1], "list") {
			return shellClassRead
		}
	case "xctrace":
		if len(args) >= 2 && strings.EqualFold(args[1], "list") {
			return shellClassRead
		}
	case "devicectl":
		if len(args) >= 2 && strings.EqualFold(args[1], "list") {
			return shellClassRead
		}
		if len(args) >= 3 && strings.EqualFold(args[1], "device") &&
			strings.EqualFold(args[2], "info") {
			return shellClassRead
		}
	}
	return shellClassUnknown
}

func shellCompoundClass(args []string, firstPartyCommands FirstPartyCommandCatalog) string {
	if len(args) == 1 && args[0] == "--version" {
		return shellClassRead
	}
	command, rest := shellFirstOperand(args, nil)
	subcommand, _ := shellFirstOperand(rest, nil)
	admission, ok := firstPartyCommands.LookupSubcommandWithFlags(command, subcommand, rest)
	if !ok {
		return shellClassUnknown
	}
	switch admission {
	case FirstPartyCommandRead:
		return shellClassRead
	case FirstPartyCommandGoverned:
		return shellClassGoverned
	case FirstPartyCommandInstall:
		return shellClassInstall
	default:
		return shellClassUnknown
	}
}

func shellNestedClass(subcommands map[string][]string, args []string) string {
	sub, rest := shellFirstOperand(args, nil)
	if sub == "" {
		return shellClassRead
	}
	allowed, ok := subcommands[sub]
	if !ok {
		return shellClassUnknown
	}
	if allowed == nil {
		return shellClassRead
	}
	next, _ := shellFirstOperand(rest, nil)
	for _, candidate := range allowed {
		if next == candidate {
			return shellClassRead
		}
	}
	return shellClassUnknown
}

func shellGitClass(args []string) string {
	sub, rest := shellFirstOperand(args, gitGlobalFlagsWithValue)
	switch {
	case sub == "":
		return shellClassUnknown
	case sub == "clean", sub == "rm", sub == "filter-branch", sub == "filter-repo":
		return shellClassDestructive
	case sub == "reset" && !shellFlagAbsent(rest, "--hard"):
		return shellClassDestructive
	case sub == "update-ref" && !shellFlagAbsent(rest, "-d", "--delete"):
		return shellClassDestructive
	case sub == "branch" && !shellFlagAbsent(rest, "-d", "-D", "--delete"):
		return shellClassDestructive
	case sub == "add":
		return shellClassGoverned
	case sub == "commit":
		if !shellFlagAbsent(rest, "--amend") {
			return shellClassUnknown
		}
		return shellClassGoverned
	case sub == "rebase":
		if !shellFlagAbsent(rest, "-x", "--exec") {
			return shellClassUnknown
		}
		return shellClassGoverned
	case sub == "merge":
		if !shellFlagAbsent(rest, "--ff-only") {
			return shellClassGoverned
		}
		return shellClassUnknown
	case gitNetworkSubcommands[sub]:
		return shellClassNetwork
	case gitReadSubcommands[sub]:
		return shellClassRead
	case sub == "bundle":
		// `git bundle verify` validates and inspects an existing bundle.
		// Creation and import forms can write, so they remain unknown/held.
		action, _ := shellFirstOperand(rest, nil)
		if action == "verify" {
			return shellClassRead
		}
		return shellClassUnknown
	case sub == "branch":
		// Listing is a read; naming an operand creates or renames a ref. Query
		// flags take a value, and that value is not an operand.
		// With --list/-l, remaining operands are patterns rather than branch
		// names, provided no mutation flag is also present.
		if !shellFlagAbsent(rest, "--list", "-l") &&
			shellFlagAbsent(rest, "-m", "-M", "-c", "-C", "--move", "--copy", "--set-upstream-to", "-u", "--unset-upstream", "--edit-description") {
			return shellClassRead
		}
		if operand, _ := shellFirstOperand(rest, gitRefQueryFlagsWithValue); operand != "" {
			return shellClassUnknown
		}
		if shellFlagAbsent(rest, "-m", "-M", "-c", "-C", "--move", "--copy",
			"--set-upstream-to", "-u", "--unset-upstream", "--edit-description") {
			return shellClassRead
		}
		return shellClassUnknown
	case sub == "tag":
		if operand, _ := shellFirstOperand(rest, gitRefQueryFlagsWithValue); operand != "" {
			return shellClassUnknown
		}
		return shellClassRead
	case sub == "worktree", sub == "stash":
		next, _ := shellFirstOperand(rest, nil)
		switch next {
		case "list", "show":
			return shellClassRead
		case "add":
			if sub == "worktree" {
				return shellClassGoverned
			}
		case "remove", "drop", "clear", "prune":
			return shellClassDestructive
		}
		return shellClassUnknown
	case sub == "remote":
		next, _ := shellFirstOperand(rest, nil)
		if next == "" || next == "show" || next == "get-url" {
			return shellClassRead
		}
		return shellClassUnknown
	case sub == "config":
		if !shellFlagAbsent(rest, "--get", "--get-all", "--get-regexp", "--list", "-l") {
			return shellClassRead
		}
		return shellClassUnknown
	case sub == "reflog":
		if !shellFlagAbsent(rest, "expire", "delete") {
			return shellClassDestructive
		}
		return shellClassRead
	}
	return shellClassUnknown
}

// switchbackReadSubcommands are the `sb` verbs that only report state. Every
// other verb — run, apply, and the rest of the lane-mutating surface — stays
// unknown so it keeps its hold.
var switchbackReadSubcommands = map[string]bool{
	"pulse": true, "usage": true, "status": true, "list": true, "show": true,
	"doctor": true, "diff": true, "plan": true,
}

// shellSwitchbackClass reads `sb <verb> [sub]`. `sb profile doctor|list|show|diff|plan`
// is a report; `sb profile apply` is not, so the second operand decides when the
// first is a noun rather than a verb.
func shellSwitchbackClass(args []string) string {
	verb, rest := shellFirstOperand(args, nil)
	if verb == "" {
		return shellClassUnknown
	}
	if switchbackReadSubcommands[verb] {
		return shellClassRead
	}
	// Noun-first forms (`sb profile doctor`, `sb body status`, `sb lane list`)
	// carry their read/write verb in the second operand.
	switch verb {
	case "profile", "body", "lane", "registry", "capture", "account":
		sub, _ := shellFirstOperand(rest, nil)
		if switchbackReadSubcommands[sub] {
			return shellClassRead
		}
	}
	return shellClassUnknown
}

// shellXargsClass classifies `xargs [flags] <command> ...` by the command it
// will actually run: `find … | xargs grep -l …` is a read because grep is. An
// xargs with no command runs `echo`, and an untrusted or write-class payload
// keeps its hold.
func shellXargsClass(args []string, firstPartyCommands FirstPartyCommandCatalog) string {
	fields := args
	for len(fields) > 0 && strings.HasPrefix(fields[0], "-") {
		// -I/-J/-n/-P/-s take a value; consuming it keeps the payload aligned.
		switch fields[0] {
		case "-I", "-J", "-n", "-P", "-s", "-L":
			if len(fields) < 2 {
				return shellClassUnknown
			}
			fields = fields[2:]
		case "-0", "-r", "-t", "-p":
			fields = fields[1:]
		default:
			return shellClassUnknown
		}
	}
	if len(fields) == 0 {
		// Bare `xargs` echoes its input.
		return shellClassRead
	}
	payload, prefixesSafe := shellStripCommandPrefixes(fields)
	if !prefixesSafe || len(payload) == 0 {
		return shellClassUnknown
	}
	verb := shellVerbName(payload[0])
	if !shellVerificationExecutableTrusted(payload[0], verb) {
		return shellClassUnknown
	}
	return shellVerbClass(verb, payload[1:], firstPartyCommands)
}

func shellGoClass(args []string) string {
	sub, rest := shellFirstOperand(args, goGlobalFlagsWithValue)
	switch {
	case sub == "":
		return shellClassUnknown
	case sub == "install":
		return shellClassInstall
	case sub == "mod":
		next, _ := shellFirstOperand(rest, nil)
		if next == "graph" || next == "why" || next == "verify" {
			return shellClassRead
		}
		return shellClassUnknown
	case goReadSubcommands[sub]:
		return shellClassRead
	}
	return shellClassUnknown
}

func shellPackageManagerClass(verb string, args []string) (string, bool) {
	sub, rest := shellFirstOperand(args, nil)
	// A package manager asked only for its version installs nothing. This is
	// checked before the subcommand switch because that switch returns
	// (unknown, true) for anything it does not recognize, which swallowed
	// `npm --version` and `pip3 --version` -- routine "is this toolchain here"
	// probes -- into an operator prompt. Only the long flag is honored: -v means
	// verbose to pip and version to npm, so the short form stays unrecognized.
	if shellPackageManagerVersionProbe(verb, args) {
		return shellClassRead, true
	}
	switch verb {
	case "npm", "pnpm", "yarn", "bun", "bunx":
		switch sub {
		case "add", "install", "i", "ci", "create", "dlx", "x":
			return shellClassInstall, true
		case "test":
			return shellClassRead, true
		case "run":
			next, _ := shellFirstOperand(rest, nil)
			switch next {
			case "test", "lint", "build", "typecheck", "check":
				return shellClassRead, true
			}
			return shellClassUnknown, true
		}
		return shellClassUnknown, true
	case "brew":
		switch sub {
		case "install", "upgrade", "reinstall", "tap", "uninstall":
			return shellClassInstall, true
		case "list", "info", "--version", "config", "outdated":
			return shellClassRead, true
		}
		return shellClassUnknown, true
	case "pip", "pip3", "gem", "cargo", "apt", "apt-get", "dnf", "yum":
		switch sub {
		case "install", "upgrade", "uninstall", "remove":
			return shellClassInstall, true
		case "list", "show", "freeze", "search":
			return shellClassRead, true
		case "test", "build", "check", "clippy":
			if verb == "cargo" {
				return shellClassRead, true
			}
		}
		return shellClassUnknown, true
	}
	return "", false
}

func shellFindClass(args []string) string {
	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "-delete":
			return shellClassDestructive
		case "-exec", "-execdir", "-ok", "-okdir", "-fls", "-fprint", "-fprint0", "-fprintf":
			return shellClassUnknown
		}
	}
	return shellClassRead
}

// shellSedIsReadOnly parses sed's option/script boundary before inspecting
// scripts. Once a positional script has been seen, later positional operands
// are input filenames and must not be mistaken for sed commands.
func shellSedIsReadOnly(args []string) bool {
	scriptSeen := false
	options := true

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if options && arg == "--" {
			options = false
			continue
		}

		if options && strings.HasPrefix(arg, "--") {
			switch {
			case arg == "--in-place", strings.HasPrefix(arg, "--in-place="),
				arg == "--file", strings.HasPrefix(arg, "--file="):
				return false
			case arg == "--expression":
				if i+1 >= len(args) {
					return false
				}
				i++
				if shellSedScriptWrites(args[i]) {
					return false
				}
				scriptSeen = true
			case strings.HasPrefix(arg, "--expression="):
				if shellSedScriptWrites(strings.TrimPrefix(arg, "--expression=")) {
					return false
				}
				scriptSeen = true
			case arg == "--quiet", arg == "--silent", arg == "--regexp-extended",
				arg == "--separate", arg == "--unbuffered", arg == "--null-data",
				arg == "--posix", arg == "--debug", arg == "--sandbox":
				continue
			case (arg == "--help" || arg == "--version") && len(args) == 1:
				return true
			default:
				return false
			}
			continue
		}

		if options && strings.HasPrefix(arg, "-") && arg != "-" {
			short := strings.TrimPrefix(arg, "-")
			for j := 0; j < len(short); j++ {
				switch short[j] {
				case 'i', 'I', 'f':
					return false
				case 'e':
					expression := short[j+1:]
					if expression == "" {
						if i+1 >= len(args) {
							return false
						}
						i++
						expression = args[i]
					}
					if shellSedScriptWrites(expression) {
						return false
					}
					scriptSeen = true
					j = len(short)
				case 'n', 'E', 'r', 's', 'u', 'z':
					continue
				default:
					return false
				}
			}
			continue
		}

		if !scriptSeen {
			if shellSedScriptWrites(arg) {
				return false
			}
			scriptSeen = true
		}
	}
	return scriptSeen
}

func shellSedScriptWrites(script string) bool {
	if shellSedSubstitutionWrites(script) {
		return true
	}
	for i := 0; i < len(script); i++ {
		switch script[i] {
		case 'w', 'W', 'e', 'r', 'R':
		default:
			continue
		}
		if i == 0 {
			return true
		}
		switch script[i-1] {
		case ' ', '\t', '\n', ';', '{', '}', '/', '!':
			return true
		}
		if shellSedHasSimpleAddressBefore(script, i) {
			return true
		}
		if shellSedHasMixedRangeAddressBefore(script, i) {
			return true
		}
		if shellSedHasRegexAddressBefore(script, i) {
			return true
		}
		if shellSedHasAlternateRegexAddressBefore(script, i) {
			return true
		}
	}
	return false
}

func shellSedSubstitutionWrites(script string) bool {
	for i := 0; i+1 < len(script); i++ {
		if script[i] != 's' {
			continue
		}
		delimiter := script[i+1]
		if delimiter == '\\' || delimiter == '\n' {
			continue
		}
		patternEnd, ok := shellSedDelimitedEnd(script, i+2, delimiter)
		if !ok {
			continue
		}
		replacementEnd, ok := shellSedDelimitedEnd(script, patternEnd+1, delimiter)
		if !ok {
			continue
		}
	flags:
		for j := replacementEnd + 1; j < len(script); j++ {
			switch script[j] {
			case ';', '\n':
				break flags
			case 'w', 'W', 'e':
				return true
			}
		}
	}
	return false
}

func shellSedDelimitedEnd(script string, start int, delimiter byte) (int, bool) {
	for i := start; i < len(script); i++ {
		if script[i] == '\\' {
			i++
			continue
		}
		if script[i] == delimiter {
			return i, true
		}
	}
	return 0, false
}

func shellSedHasSimpleAddressBefore(script string, commandIndex int) bool {
	start := commandIndex
	for start > 0 {
		previous := script[start-1]
		if previous == '\n' || previous == ';' || previous == '{' || previous == '}' {
			break
		}
		start--
	}

	prefix := strings.TrimSpace(script[start:commandIndex])
	if prefix == "" {
		return false
	}
	hasAddress := false
	for i := 0; i < len(prefix); i++ {
		switch {
		case prefix[i] >= '0' && prefix[i] <= '9', prefix[i] == '$':
			hasAddress = true
		case prefix[i] == ' ', prefix[i] == '\t', prefix[i] == ',',
			prefix[i] == '~', prefix[i] == '+', prefix[i] == '!':
			continue
		default:
			return false
		}
	}
	return hasAddress
}

func shellSedHasMixedRangeAddressBefore(script string, commandIndex int) bool {
	start := commandIndex
	for start > 0 {
		previous := script[start-1]
		if previous == '\n' || previous == ';' || previous == '{' || previous == '}' {
			break
		}
		start--
	}

	prefix := strings.TrimSpace(script[start:commandIndex])
	prefix = strings.TrimSpace(strings.TrimSuffix(prefix, "!"))
	comma := strings.LastIndex(prefix, ",")
	if comma < 0 {
		return false
	}
	second := strings.TrimSpace(prefix[comma+1:])
	if second == "$" {
		return true
	}
	if strings.HasPrefix(second, "+") || strings.HasPrefix(second, "~") {
		second = second[1:]
	}
	if first, step, ok := strings.Cut(second, "~"); ok {
		return shellAllDigits(first) && shellAllDigits(step)
	}
	return shellAllDigits(second)
}

func shellSedHasRegexAddressBefore(script string, commandIndex int) bool {
	prefix := strings.TrimSpace(script[:commandIndex])
	prefix = strings.TrimSpace(strings.TrimSuffix(prefix, "!"))
	for candidateStart := 0; candidateStart < len(prefix); candidateStart++ {
		if candidateStart > 0 && !strings.ContainsRune(",;\n{}", rune(prefix[candidateStart-1])) {
			continue
		}
		for candidateStart < len(prefix) && (prefix[candidateStart] == ' ' || prefix[candidateStart] == '\t') {
			candidateStart++
		}
		if candidateStart >= len(prefix) {
			break
		}

		candidate := prefix[candidateStart:]
		delimiterIndex := 0
		if candidate[0] == '\\' {
			if len(candidate) < 3 {
				continue
			}
			delimiterIndex = 1
		} else if candidate[0] != '/' {
			continue
		}
		delimiter := candidate[delimiterIndex]
		regexStart := delimiterIndex + 1
		end, ok := shellSedDelimitedEnd(candidate, regexStart, delimiter)
		if !ok {
			continue
		}
		modifiers := strings.TrimSpace(candidate[end+1:])
		if modifiers == "" || strings.Trim(modifiers, "IiMm") == "" {
			return true
		}
	}
	return false
}

func shellSedHasAlternateRegexAddressBefore(script string, commandIndex int) bool {
	start := commandIndex
	for start > 0 {
		previous := script[start-1]
		if previous == '\n' || previous == ';' || previous == '{' || previous == '}' {
			break
		}
		start--
	}

	prefix := strings.TrimSpace(script[start:commandIndex])
	prefix = strings.TrimSpace(strings.TrimSuffix(prefix, "!"))
	for i := 0; i+2 < len(prefix); i++ {
		if prefix[i] != '\\' {
			continue
		}
		delimiter := prefix[i+1]
		end, ok := shellSedDelimitedEnd(prefix, i+2, delimiter)
		if ok && end == len(prefix)-1 {
			return true
		}
	}
	return false
}

// shellFirstOperand returns the first non-flag argument, skipping flags and the
// values of flags known to take one.
func shellFirstOperand(args []string, flagsWithValue map[string]bool) (string, []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return strings.ToLower(strings.Trim(arg, `"'`)), args[i+1:]
		}
		if flagsWithValue[arg] && !strings.Contains(arg, "=") {
			i++
		}
	}
	return "", nil
}

func shellFlagAbsent(args []string, flags ...string) bool {
	for _, arg := range args {
		name := arg
		if index := strings.Index(arg, "="); index > 0 {
			name = arg[:index]
		}
		for _, flag := range flags {
			if name == flag {
				return false
			}
		}
	}
	return true
}
