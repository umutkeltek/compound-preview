// Package proofscope derives the go-test package scope for a sealed base/head
// commit pair. The result is either Full with one reason, or Affected with an
// exact sorted list of import paths to test. Derive is a pure function of two
// git trees and the changed paths between them; DeriveGit reads git objects
// only, never the worktree, COMPOUND_HOME or the network. The package certifies
// nothing itself: `compound proof-check --presubmit` turns a scope into its
// non-certifying go-test runs (Presubmit).
//
// Known residual blind spots (the nightly full run is the backstop):
//   - tests that locate the repository by other means than the markers below:
//     environment variables, absolute paths, or subprocesses that discover the
//     root without runtime.Caller, os.Getwd, "--show-toplevel", "GOMOD",
//     "compound/..." or a ".." path literal;
//   - non-test production code that reads repo-relative files (a design-time
//     scan found two such literals, both in cmd/compound, itself a root reader).
package proofscope

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Mode is the scope decision.
type Mode string

const (
	Full     Mode = "full"
	Affected Mode = "affected"
)

const modulePath = "compound"

// ClaimVerifierPath is the claim-protocol marker: its presence at head means
// testbattery can verify a scope claim. p2 creates it.
const ClaimVerifierPath = "scripts/testbattery/scope_claim.go"

type Scope struct {
	Mode        Mode     `json:"mode"`
	Reason      string   `json:"reason"`
	Base        string   `json:"base"`
	Head        string   `json:"head"`
	Packages    []string `json:"packages,omitempty"`     // Affected only: sorted, unique import paths to test
	Changed     []string `json:"changed,omitempty"`      // packages that own a changed path (explanatory)
	RootReaders []string `json:"root_readers,omitempty"` // always-run set at head (explanatory)
	// Reached is Changed plus every package reached from it by prod-import
	// edges and one hop of test edges; PathReaders are the packages whose tests
	// name a changed path. Packages is their union with RootReaders. Presubmit
	// reads them separately because it defers the always-run set.
	Reached     []string `json:"reached,omitempty"`
	PathReaders []string `json:"path_readers,omitempty"`
}

// Snapshot is one commit's tree. Base needs Paths only.
type Snapshot struct {
	Paths  []string          // every tracked path (blobs, symlinks, gitlinks)
	Source map[string][]byte // head: go.mod and every *.go blob; nil for base
}

// pinnedRootReaders are operator-named repo-invariant checkers. The pin
// guards against detector regressions.
var pinnedRootReaders = []string{"internal/arch", "internal/contract"}

var moduleFiles = map[string]bool{"go.mod": true, "go.sum": true, "go.work": true, "go.work.sum": true}

var scopeOwners = []string{"internal/proofscope/", "scripts/testbattery/"}

// FullScope is the full-run decision with one reason.
func FullScope(base, head, reason string) Scope {
	return Scope{Mode: Full, Reason: reason, Base: base, Head: head}
}

// isGoSource applies the testbattery source-inventory package rules: a .go
// file whose basename does not start with "." or "_", where no directory
// component starts with "." or "_" or equals testdata or vendor.
func isGoSource(p string) bool {
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") || part == "testdata" || part == "vendor" {
			return false
		}
	}
	base := parts[len(parts)-1]
	return strings.HasSuffix(base, ".go") && !strings.HasPrefix(base, ".") && !strings.HasPrefix(base, "_")
}

func packageDirs(paths []string) map[string]bool {
	dirs := make(map[string]bool)
	for _, p := range paths {
		if isGoSource(p) {
			dirs[path.Dir(p)] = true
		}
	}
	return dirs
}

func importPath(dir string) string {
	if dir == "." {
		return modulePath
	}
	return modulePath + "/" + dir
}

// moduleDirective returns the go.mod module path, or "" when absent.
func moduleDirective(gomod []byte) string {
	for _, line := range strings.Split(string(gomod), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			if unquoted, err := strconv.Unquote(fields[1]); err == nil {
				return unquoted
			}
			return fields[1]
		}
	}
	return ""
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pkgFacts is what one head package's files say about its edges.
type pkgFacts struct {
	prodImports map[string]bool // head package dirs imported by non-test files
	testImports map[string]bool // head package dirs imported or named by _test.go files
	rootReader  bool
	targets     map[string]bool // repo-relative paths its tests read
}

// Derive applies the rules in order; the first match wins.
func Derive(base, head Snapshot, changed []string) Scope {
	full := func(reason string) Scope { return FullScope("", "", reason) }

	gomod, ok := head.Source["go.mod"]
	if !ok || moduleDirective(gomod) != modulePath {
		return full("module-path")
	}
	changedSet := make(map[string]bool, len(changed))
	for _, p := range changed {
		changedSet[p] = true
	}
	changedPaths := sortedKeys(changedSet)
	if len(changedPaths) == 0 {
		return full("empty-diff")
	}
	for _, p := range changedPaths {
		if moduleFiles[path.Base(p)] {
			return full("module-files:" + p)
		}
		for _, owner := range scopeOwners {
			if strings.HasPrefix(p, owner) {
				return full("scope-owner:" + p)
			}
		}
		if !strings.Contains(p, "/") && !strings.HasSuffix(p, ".md") {
			return full("root-file:" + p)
		}
	}
	headDirs, baseDirs := packageDirs(head.Paths), packageDirs(base.Paths)
	for _, dir := range sortedKeys(baseDirs) {
		if !headDirs[dir] {
			return full("package-removed:" + dir)
		}
	}

	headPkgByImport := make(map[string]string, len(headDirs))
	for dir := range headDirs {
		headPkgByImport[importPath(dir)] = dir
	}
	known := pathPrefixes(head.Paths, base.Paths, changedPaths)
	facts := make(map[string]*pkgFacts, len(headDirs))
	for dir := range headDirs {
		facts[dir] = &pkgFacts{prodImports: map[string]bool{}, testImports: map[string]bool{}, targets: map[string]bool{}}
	}
	headPaths := append([]string(nil), head.Paths...)
	sort.Strings(headPaths)
	for _, p := range headPaths {
		if !isGoSource(p) {
			continue
		}
		src, ok := head.Source[p]
		if !ok {
			return full("parse-error:" + p)
		}
		if err := scanFile(p, src, facts[path.Dir(p)], headPkgByImport, known); err != nil {
			return full("parse-error:" + p)
		}
	}

	// Seeds: every head package dir equal to or an ancestor of a changed
	// path's directory.
	seeds := make(map[string]bool)
	for _, p := range changedPaths {
		for dir := path.Dir(p); ; dir = path.Dir(dir) {
			if headDirs[dir] {
				seeds[dir] = true
			}
			if dir == "." {
				break
			}
		}
	}
	// R: seeds plus every package that transitively prod-imports a member.
	importers := make(map[string][]string)
	for dir, f := range facts {
		for imported := range f.prodImports {
			importers[imported] = append(importers[imported], dir)
		}
	}
	reached := make(map[string]bool)
	queue := sortedKeys(seeds)
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if reached[dir] {
			continue
		}
		reached[dir] = true
		queue = append(queue, importers[dir]...)
	}
	// Tested: R plus one hop of test edges into R.
	tested := make(map[string]bool, len(reached))
	for dir := range reached {
		tested[dir] = true
	}
	for dir, f := range facts {
		for imported := range f.testImports {
			if reached[imported] {
				tested[dir] = true
				break
			}
		}
	}
	// Readers.
	rootReaders := make(map[string]bool)
	for _, dir := range pinnedRootReaders {
		if headDirs[dir] {
			rootReaders[dir] = true
		}
	}
	for dir, f := range facts {
		if f.rootReader {
			rootReaders[dir] = true
		}
	}
	all := make(map[string]bool, len(tested)+len(rootReaders))
	for dir := range tested {
		all[dir] = true
	}
	for dir := range rootReaders {
		all[dir] = true
	}
	pathReaders := make(map[string]bool)
	for dir, f := range facts {
		for target := range f.targets {
			if triggers(target, changedPaths) {
				all[dir] = true
				pathReaders[dir] = true
				break
			}
		}
	}
	if len(all) == len(headDirs) {
		return full("affected-all")
	}
	return Scope{
		Mode:        Affected,
		Reason:      "diff",
		Packages:    importPaths(all),
		Changed:     importPaths(seeds),
		RootReaders: importPaths(rootReaders),
		Reached:     importPaths(tested),
		PathReaders: importPaths(pathReaders),
	}
}

func importPaths(dirs map[string]bool) []string {
	var out []string
	for _, dir := range sortedKeys(dirs) {
		out = append(out, importPath(dir))
	}
	return out
}

// triggers reports whether a changed path equals target or lies under it.
func triggers(target string, changed []string) bool {
	for _, p := range changed {
		if p == target || strings.HasPrefix(p, target+"/") {
			return true
		}
	}
	return false
}

// pathPrefixes is every path and directory prefix in the given lists.
func pathPrefixes(lists ...[]string) map[string]bool {
	out := make(map[string]bool)
	for _, list := range lists {
		for _, p := range list {
			for q := p; q != "." && q != "/" && !out[q]; q = path.Dir(q) {
				out[q] = true
			}
		}
	}
	return out
}

// scanFile records one head Go file's import, literal and reader edges.
func scanFile(name string, src []byte, f *pkgFacts, headPkgByImport map[string]string, known map[string]bool) error {
	test := strings.HasSuffix(name, "_test.go")
	mode := parser.ImportsOnly
	if test {
		mode = 0
	}
	file, err := parser.ParseFile(token.NewFileSet(), name, src, mode)
	if err != nil {
		return err
	}
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return err
		}
		if dir, ok := headPkgByImport[imported]; ok && strings.HasPrefix(imported, modulePath+"/") {
			if test {
				f.testImports[dir] = true
			} else {
				f.prodImports[dir] = true
			}
		}
	}
	if !test {
		return nil
	}
	dir := path.Dir(name)
	addTarget := func(target string) {
		target = path.Clean(target)
		if target == ".." || strings.HasPrefix(target, "../") || path.IsAbs(target) {
			return // escapes the repository: a temp-dir traversal test
		}
		if target == "." {
			f.rootReader = true
			return
		}
		if known[target] {
			f.targets[target] = true
		}
	}
	callArgs := make(map[*ast.BasicLit]bool)
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok &&
				((x.Name == "runtime" && n.Sel.Name == "Caller") || (x.Name == "os" && n.Sel.Name == "Getwd")) {
				f.rootReader = true
			}
		case *ast.CallExpr:
			k, last := 0, -1
			for i, arg := range n.Args {
				if lit, ok := arg.(*ast.BasicLit); ok {
					callArgs[lit] = true
					if s, ok := stringLit(lit); ok && s == ".." {
						k, last = k+1, i
					}
				}
			}
			if k > 0 {
				parts := []string{dir}
				for range k {
					parts = append(parts, "..")
				}
				for _, arg := range n.Args[last+1:] {
					lit, ok := arg.(*ast.BasicLit)
					s, isString := stringLit(lit)
					if !ok || !isString {
						break
					}
					parts = append(parts, s)
				}
				addTarget(path.Join(parts...))
			}
		case *ast.BasicLit:
			s, ok := stringLit(n)
			if !ok {
				break
			}
			switch {
			case s == "--show-toplevel" || s == "GOMOD" || s == modulePath+"/...":
				f.rootReader = true
			case s == ".." && !callArgs[n]:
				addTarget(path.Join(dir, ".."))
			case strings.HasPrefix(s, "../"):
				addTarget(path.Join(dir, s))
			}
			if imported, ok := headPkgByImport[s]; ok {
				f.testImports[imported] = true
			}
		}
		return true
	})
	return nil
}

func stringLit(lit *ast.BasicLit) (string, bool) {
	if lit == nil || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
