package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"compound/internal/policy"
)

// `compound presence` is registered read, and its read projection earned that
// classification (60f14751 moved it to work.OpenReadOnly). But `presence
// announce` is a subcommand of the same command, and it opens a WRITER store
// and appends a presence session. With no subcommand admission declared, the
// base row answers for it, so a mutation was admitted as a routine read and ran
// without ever reaching a PolicyDecision.
//
// This is the admission bug class Compound exists to eliminate — the classifier
// must answer for the subcommand that acts, not for the command's friendliest
// path.
func TestPresenceAnnounceIsGovernedNotRead(t *testing.T) {
	t.Parallel()

	catalog := firstPartyCommandCatalog()
	for _, testCase := range []struct {
		command string
		want    string
		why     string
	}{
		{
			command: "compound presence announce --actor a --session s --harness claude_code",
			want:    "governed_command",
			why:     "announce opens a writer store and appends a presence session",
		},
		{
			command: "compound presence --json",
			want:    "local_command",
			why:     "the read projection must stay automatic",
		},
		{
			command: "compound presence --json --include-stale --ttl 15m",
			want:    "local_command",
			why:     "read flags must not be mistaken for a subcommand",
		},
	} {
		t.Run(testCase.command, func(t *testing.T) {
			got := policy.ClassifyToolAction(policy.ToolActionRequest{
				Tool:               "Bash",
				Arguments:          map[string]any{"command": testCase.command},
				FirstPartyCommands: catalog,
			})
			if !got.Known || got.Operation != testCase.want {
				t.Fatalf("%s: %q => operation %q, want %q (%+v)",
					testCase.why, testCase.command, got.Operation, testCase.want, got)
			}
		})
	}
}

// The class guard. A command registered read must not be able to reach a WRITER
// store at all, unless the mutating path sits behind a subcommand declared
// governed — otherwise the base read row answers for the mutation, which is how
// `presence announce` escaped.
//
// Reachability is computed over the real package call graph, not by scanning
// the handler's file: cmdPlacement and cmdTransition merely SHARE main.go with
// unrelated writers, and a guard that fails on them is a guard that gets
// deleted. The handler is located through the registry itself
// (runtime.FuncForPC), so renaming a command or moving a file cannot silently
// drop coverage, and the writer may sit any number of calls deep — presence's
// sits in cmdPresenceAnnounce, one level down.
func TestReadClassifiedCommandsCannotReachAWriterStore(t *testing.T) {
	t.Parallel()

	calls, writers := parsePackageCallGraph(t)
	for name, command := range compoundCommandRegistry() {
		if command.Admission != policy.FirstPartyCommandRead || command.Run == nil {
			continue
		}
		if declaresGovernedSubcommand(command) {
			// The mutating path is declared, so the classifier answers for it.
			continue
		}
		handler := handlerFuncName(command.Run)
		if handler == "" {
			continue
		}
		if via, reaches := reachesWriter(handler, calls, writers); reaches {
			t.Errorf(
				"%q is registered read but reaches a writer store via %s: swap it to work.OpenReadOnly, or declare the mutating subcommand governed",
				name, strings.Join(via, " -> "),
			)
		}
	}
}

func declaresGovernedSubcommand(command compoundCommand) bool {
	for _, admission := range command.SubcommandAdmission {
		if admission == policy.FirstPartyCommandGoverned ||
			admission == policy.FirstPartyCommandHold {
			return true
		}
	}
	return false
}

// handlerFuncName reduces the registered handler to its bare package-local
// name: "compound/cmd/compound.cmdPresence" -> "cmdPresence".
func handlerFuncName(run func([]string)) string {
	pc := reflect.ValueOf(run).Pointer()
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return ""
	}
	name := fn.Name()
	if index := strings.LastIndex(name, "."); index >= 0 {
		name = name[index+1:]
	}
	// A closure ("cmdX.funcN", as retiredCommand builds) has no body to walk.
	if strings.HasPrefix(name, "func") {
		return ""
	}
	return name
}

// parsePackageCallGraph returns, for every top-level func in package main, the
// package-local funcs it calls, plus the set of funcs that open a writer store
// directly.
func parsePackageCallGraph(t *testing.T) (map[string][]string, map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	calls := map[string][]string{}
	writers := map[string]bool{}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				name := fn.Name.Name
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch target := call.Fun.(type) {
					case *ast.Ident:
						calls[name] = append(calls[name], target.Name)
					case *ast.SelectorExpr:
						if ident, ok := target.X.(*ast.Ident); ok &&
							ident.Name == "work" && target.Sel.Name == "Open" {
							writers[name] = true
						}
					}
					return true
				})
			}
		}
	}
	if len(writers) == 0 {
		t.Fatal("call graph found no work.Open caller at all: the guard would pass vacuously")
	}
	return calls, writers
}

func reachesWriter(start string, calls map[string][]string, writers map[string]bool) ([]string, bool) {
	type step struct {
		name string
		path []string
	}
	seen := map[string]bool{start: true}
	queue := []step{{name: start, path: []string{start}}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if writers[current.name] {
			return current.path, true
		}
		for _, next := range calls[current.name] {
			if seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, step{
				name: next,
				path: append(append([]string{}, current.path...), next),
			})
		}
	}
	return nil, false
}
