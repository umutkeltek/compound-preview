package policy

import (
	"os"
	"path/filepath"
	"strings"
)

// This recognizer owns only literal local writes. Catalog combinations still
// obey the catalog's single-capability rule; unknown neighbours never inherit
// this exception. Classification describes consequences, not authorization.
func shellLocalWriteCommand(base ActionClassification, segments []shellSegment, firstParty FirstPartyCommandCatalog, cwd string, forbidden []string) (ActionClassification, bool) {
	if !filepath.IsAbs(cwd) {
		return base, false
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return base, false
	}
	scope := shellLocalScope{cwd: filepath.Clean(cwd), home: filepath.Clean(home), forbidden: forbidden}
	var targets []string
	script, desktop := false, false
	for _, segment := range segments {
		if segment.obfuscated {
			return base, false
		}
		// The read lane skips loop headers, so it cannot prove that those
		// headers leave cwd/environment unchanged for a subsequent write.
		if len(segment.words) > 0 && shellControlKeywords[segment.words[0]] {
			return base, false
		}
		words, ok := shellStripControlKeywords(segment.words)
		if !ok {
			return base, false
		}
		fields, ok := shellStripCommandPrefixes(words)
		if !ok {
			return base, false
		}
		prefix := words[:len(words)-len(fields)]
		if !shellLocalPrefixSafe(prefix) {
			return base, false
		}
		if len(fields) == 0 {
			if segment.writes || shellSegmentClass(segment, firstParty) != shellClassRead {
				return base, false
			}
			continue
		}
		verb, args := shellVerbName(fields[0]), fields[1:]
		// Changing cwd in a compound command needs control-flow tracking.
		// Keep existing read-only cd compositions, but never guess write roots.
		if verb == "cd" {
			return base, false
		}
		readSegment := segment
		readSegment.writes = false
		class := shellSegmentClass(readSegment, firstParty)
		if class == shellClassInstall || class == shellClassNetwork || class == shellClassDestructive || class == shellClassOperatorGated {
			return base, false
		}
		read := class == shellClassRead
		var writes []string
		if shellLocalWriteVerb(verb) {
			if !shellCatalogExecutableTrusted(fields[0]) {
				return base, false
			}
			operands, destinations, ok := shellLocalWriteOperands(verb, args)
			if !ok {
				return base, false
			}
			for _, operand := range operands {
				if _, ok := scope.path(operand); !ok {
					return base, false
				}
			}
			for _, target := range destinations {
				path, ok := scope.path(target)
				if !ok {
					return base, false
				}
				writes = append(writes, path)
			}
			// A relative symlink target is interpreted from the link's parent.
			if verb == "ln" && !filepath.IsAbs(operands[0]) {
				if _, ok := scope.path(filepath.Join(filepath.Dir(writes[0]), operands[0])); !ok {
					return base, false
				}
			}
		} else if !read {
			path, isDesktop, ok := scope.interpreter(fields[0], args, segment.stdin)
			if !ok {
				return base, false
			}
			writes = append(writes, path)
			script, desktop = true, desktop || isDesktop
		}
		if segment.writes {
			if len(segment.writeTargets) == 0 {
				return base, false
			}
			for _, target := range segment.writeTargets {
				path, ok := scope.path(target)
				if !ok {
					return base, false
				}
				writes = append(writes, path)
			}
		}
		targets = append(targets, writes...)
	}
	if len(targets) == 0 {
		return base, false
	}
	base = shellClassificationFor(base, shellClassGoverned)
	base.Known, base.TargetKind, base.Reversibility = true, "path", "manual"
	base.Destination = "path:" + shellCommonLocalTarget(targets)
	if script {
		base.Capability = "script.run.local_program"
		base.Operation = "script.run.local_program"
	}
	if desktop {
		base.BlastRadius = "desktop"
	}
	return base, true
}

func shellLocalPrefixSafe(prefix []string) bool {
	if shellPrefixChangesExecution(prefix) {
		return false
	}
	for _, token := range prefix {
		if token == "exec" || token == "builtin" || token == "command" {
			return false
		}
		name, _, assigned := strings.Cut(token, "=")
		if !assigned {
			continue
		}
		switch name {
		case "HOME", "PWD", "TMPDIR", "PYTHONPATH", "PYTHONHOME", "NODE_OPTIONS", "RUBYOPT", "PERL5OPT":
			return false
		}
	}
	return true
}

type shellLocalScope struct {
	cwd, home string
	forbidden []string
}

func shellLiteralLocalPath(path string) bool {
	for _, prefix := range []string{"~", "$HOME", "${HOME}", "$PWD", "${PWD}", "$TMPDIR", "${TMPDIR}"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
			break
		}
	}
	return !strings.Contains(path, shellSubstitutionPlaceholder) && !strings.ContainsAny(path, "$`*?[]{}~\\\n\r")
}

func (s shellLocalScope) path(raw string) (string, bool) {
	if raw == "" || raw == "-" || !shellLiteralLocalPath(raw) {
		return "", false
	}
	p := raw
	for _, prefix := range []struct{ token, root string }{
		{"~", s.home}, {"$HOME", s.home}, {"${HOME}", s.home},
		{"$PWD", s.cwd}, {"${PWD}", s.cwd}, {"$TMPDIR", os.Getenv("TMPDIR")}, {"${TMPDIR}", os.Getenv("TMPDIR")},
	} {
		if p == prefix.token || strings.HasPrefix(p, prefix.token+"/") {
			if !filepath.IsAbs(prefix.root) {
				return "", false
			}
			p = prefix.root + strings.TrimPrefix(p, prefix.token)
			break
		}
	}
	if !filepath.IsAbs(p) {
		p = s.cwd + string(filepath.Separator) + p
	}
	if !s.allowed(filepath.Clean(p), s.cwd, s.home) {
		return "", false
	}
	resolved, ok := shellResolveLocalPath(p)
	cwd, cwdOK := shellResolveLocalPath(s.cwd)
	home, homeOK := shellResolveLocalPath(s.home)
	if !ok || !cwdOK || !homeOK || !s.allowed(resolved, cwd, home) {
		return "", false
	}
	// Preserve the caller's root spelling (for example /var vs /private/var),
	// but do not let an interior symlink hide a mutation in the other root.
	if shellPathWithin(resolved, cwd) {
		rel, _ := filepath.Rel(cwd, resolved)
		return filepath.Join(s.cwd, rel), true
	}
	rel, _ := filepath.Rel(home, resolved)
	return filepath.Join(s.home, rel), true
}

func (s shellLocalScope) allowed(path, cwd, home string) bool {
	if !shellPathWithin(path, cwd) && !shellPathWithin(path, home) {
		return false
	}
	for _, root := range []string{"/etc", "/private/etc", "/usr", "/bin", "/sbin", "/Library", "/System"} {
		if shellPathWithin(path, root) {
			return false
		}
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".compound" || operatorGatedCredentialDirs[component] {
			return false
		}
	}
	if compoundHome := os.Getenv("COMPOUND_HOME"); filepath.IsAbs(compoundHome) && shellPathWithin(path, compoundHome) {
		return false
	}
	if operatorGatedWriteTarget(path) {
		return false
	}
	if _, forbidden := matchForbidden(s.forbidden, path); forbidden {
		return false
	}
	_, forbidden := matchForbidden(s.forbidden, path+"/")
	return !forbidden
}

func shellPathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Resolve the existing ancestor too: a not-yet-created file can still escape
// through a symlink in its parent path. Missing leaves are ordinary write targets.
func shellResolveLocalPath(path string) (string, bool) {
	var suffix []string
	for {
		_, err := os.Lstat(path)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return "", false
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, true
		}
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			return "", false
		}
		suffix = append(suffix, filepath.Base(path))
		// Do not clean '..' before resolving an existing symlink ancestor.
		path = path[:strings.LastIndex(path, string(filepath.Separator))]
		if path == "" {
			path = string(filepath.Separator)
		}
	}
}

func shellCommonLocalTarget(paths []string) string {
	root := paths[0]
	for _, path := range paths[1:] {
		for !shellPathWithin(path, root) {
			root = filepath.Dir(root)
		}
	}
	return root
}

func shellLocalWriteVerb(verb string) bool {
	switch verb {
	case "cp", "mv", "mkdir", "touch", "chmod", "ln", "tee":
		return true
	default:
		return false
	}
}

// Explicit option arities keep a flag value from being mistaken for a path.
// install and unmodelled options retain their existing classification.
func shellLocalWriteOperands(verb string, args []string) ([]string, []string, bool) {
	short := map[string]string{"cp": "Rrcnpfiv", "mv": "fniv", "mkdir": "pv", "touch": "acm", "chmod": "Rfv", "ln": "sfniv", "tee": "ai"}[verb]
	long := map[string]string{"cp": "preserve,no-clobber,recursive,force,interactive,verbose", "mv": "force,no-clobber,interactive,verbose", "mkdir": "parents,verbose", "touch": "no-create", "chmod": "recursive,verbose", "ln": "symbolic,force,interactive,verbose", "tee": "append,ignore-interrupts"}[verb]
	operands := []string{}
	options := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if options && arg == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(arg, "-") {
			if verb == "mkdir" && (arg == "-m" || arg == "--mode") {
				if i+1 == len(args) || !shellLocalMode(args[i+1]) {
					return nil, nil, false
				}
				i++
				continue
			}
			if strings.HasPrefix(arg, "--") {
				found := false
				for _, flag := range strings.Split(long, ",") {
					if arg == "--"+flag {
						found = true
					}
				}
				if !found {
					return nil, nil, false
				}
			} else {
				if len(arg) < 2 {
					return nil, nil, false
				}
				for _, flag := range arg[1:] {
					if !strings.ContainsRune(short, flag) {
						return nil, nil, false
					}
				}
			}
			continue
		}
		operands = append(operands, arg)
	}
	if verb == "chmod" {
		if len(operands) < 2 || !shellLocalMode(operands[0]) {
			return nil, nil, false
		}
		operands = operands[1:]
	}
	if len(operands) == 0 {
		return nil, nil, false
	}
	switch verb {
	case "cp", "mv", "ln":
		if len(operands) < 2 {
			return nil, nil, false
		}
		if verb == "ln" && len(operands) != 2 {
			return nil, nil, false
		}
		if verb == "mv" {
			return operands, operands, true
		}
		return operands, operands[len(operands)-1:], true
	default:
		return operands, operands, true
	}
}

func shellLocalMode(mode string) bool {
	if mode == "" {
		return false
	}
	for _, r := range mode {
		if !strings.ContainsRune("01234567ugoa+-=rwxXst,", r) {
			return false
		}
	}
	return true
}

func (s shellLocalScope) interpreter(token string, args []string, stdin bool) (string, bool, bool) {
	verb := shellVerbName(token)
	switch verb {
	case "python", "python3", "node", "bun", "deno", "ruby", "perl", "osascript", "go":
	default:
		return "", false, false
	}
	if !shellCatalogExecutableTrusted(token) || !shellVerificationExecutableTrusted(token, verb) {
		return "", false, false
	}
	desktop := verb == "osascript"
	inline := false
	if verb == "go" || verb == "deno" || verb == "bun" {
		if len(args) > 0 && args[0] == "run" {
			args = args[1:]
		} else if verb == "go" {
			return "", false, false
		}
	}
	if len(args) == 0 {
		inline = stdin && verb != "go"
	} else {
		switch args[0] {
		case "-":
			inline = verb != "go"
		case "-c":
			inline = (verb == "python" || verb == "python3") && len(args) >= 2
		case "-e", "--eval":
			inline = verb != "go" && verb != "python" && verb != "python3" && len(args) >= 2
		case "eval":
			inline = verb == "deno" && len(args) >= 2
		}
	}
	if inline {
		path, ok := s.path(".")
		return path, desktop, ok
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", false, false
	}
	if (verb == "bun" || verb == "deno") && !strings.Contains(args[0], "/") && filepath.Ext(args[0]) == "" {
		return "", false, false
	}
	if verb == "go" && !strings.HasPrefix(args[0], "./") && !filepath.IsAbs(args[0]) && !strings.HasSuffix(args[0], ".go") {
		return "", false, false
	}
	// Bare package names and URL schemes are not local script paths.
	if strings.Contains(args[0], ":") || strings.Contains(args[0], "@") {
		return "", false, false
	}
	path, ok := s.path(args[0])
	if !ok || !shellPathWithin(path, s.cwd) {
		return "", false, false
	}
	resolved, ok := shellResolveLocalPath(path)
	cwd, cwdOK := shellResolveLocalPath(s.cwd)
	if !ok || !cwdOK || !shellPathWithin(resolved, cwd) {
		return "", false, false
	}
	if verb == "go" && strings.HasSuffix(args[0], ".go") {
		for _, arg := range args[1:] {
			if !strings.HasSuffix(arg, ".go") {
				break
			}
			other, ok := s.path(arg)
			if !ok || !shellPathWithin(other, s.cwd) {
				return "", false, false
			}
		}
	}
	// The program executes in cwd; its source file is not its write scope.
	return s.cwd, desktop, true
}
