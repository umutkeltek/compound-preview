package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	stdstrconv "strconv"
	"strings"
)

// checkEventStoreOwnership makes internal/eventstore the only package that may know the physical
// layout beneath COMPOUND_HOME/events. It walks the worktree rather than the Git index so an
// untracked production file cannot bypass the check.
func checkEventStoreOwnership(repoRoot string) []Finding {
	var files []string
	var findings []Finding
	for _, root := range []string{"internal", "cmd"} {
		absRoot := filepath.Join(repoRoot, root)
		if _, err := os.Stat(absRoot); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			findings = append(findings, Finding{"eventstore-ownership", Fail, "cannot inspect " + root + ": " + err.Error()})
			continue
		}
		err := filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, walkErr error) error {
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Clean(filepath.Dir(path)) == filepath.Join(repoRoot, "internal", "eventstore") {
				return nil
			}
			if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
				files = append(files, rel)
			}
			return nil
		})
		if err != nil {
			findings = append(findings, Finding{"eventstore-ownership", Fail, "cannot walk " + root + ": " + err.Error()})
		}
	}

	sort.Strings(files)
	var violations []eventStoreViolation
	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			findings = append(findings, Finding{"eventstore-ownership", Fail, "cannot read " + rel + ": " + err.Error()})
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, body, parser.AllErrors)
		if err != nil {
			findings = append(findings, Finding{"eventstore-ownership", Fail, "cannot parse " + rel + ": " + err.Error()})
			continue
		}
		violations = append(violations, inspectEventStoreFile(rel, fset, file)...)
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].rel != violations[j].rel {
			return violations[i].rel < violations[j].rel
		}
		if violations[i].line != violations[j].line {
			return violations[i].line < violations[j].line
		}
		return violations[i].reason < violations[j].reason
	})
	seen := map[string]bool{}
	for _, violation := range violations {
		key := violation.rel + ":" + stdstrconv.Itoa(violation.line) + ":" + violation.reason
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, Finding{
			Check:    "eventstore-ownership",
			Severity: Fail,
			Message:  violation.rel + ":" + stdstrconv.Itoa(violation.line) + " constructs or accesses the physical events layout outside internal/eventstore (" + violation.reason + ")",
		})
	}
	if len(findings) == 0 {
		findings = append(findings, Finding{"eventstore-ownership", Info, "internal/eventstore is the sole physical owner of the events layout"})
	}
	return findings
}

type eventStoreViolation struct {
	rel    string
	line   int
	reason string
}

type eventPathKind uint8

const (
	eventPathNone eventPathKind = iota
	eventPathComponent
	eventPathPhysical
)

func inspectEventStoreFile(rel string, fset *token.FileSet, file *ast.File) []eventStoreViolation {
	imports := eventStoreImports(file)
	globals := map[string]eventPathKind{}
	globalStrings := map[string]string{}
	var violations []eventStoreViolation
	record := func(node ast.Node, reason string) {
		violations = append(violations, eventStoreViolation{rel: rel, line: fset.Position(node.Pos()).Line, reason: reason})
	}

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, value := range valueSpec.Values {
				ast.Inspect(value, func(node ast.Node) bool {
					inspectEventStoreOperation(node, globals, globalStrings, imports, record)
					return true
				})
			}
			bindEventStoreValues(rel, valueSpec.Names, valueSpec.Values, globals, globalStrings, imports, record)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		lit, ok := node.(*ast.BasicLit)
		if ok && eventStoreLiteralKind(lit) == eventPathPhysical && !eventStoreLiteralVocabulary(rel, lit) {
			record(lit, "physical event path literal")
		}
		return true
	})

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		env := make(map[string]eventPathKind, len(globals))
		for name, kind := range globals {
			env[name] = kind
		}
		stringValues := make(map[string]string, len(globalStrings))
		for name, value := range globalStrings {
			stringValues[name] = value
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.ValueSpec:
				bindEventStoreValues(rel, n.Names, n.Values, env, stringValues, imports, record)
			case *ast.AssignStmt:
				names := make([]*ast.Ident, 0, len(n.Lhs))
				for _, lhs := range n.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						names = append(names, ident)
					}
				}
				bindEventStoreValues(rel, names, n.Rhs, env, stringValues, imports, record)
			case *ast.ReturnStmt:
				for _, result := range n.Results {
					if eventStoreExprKind(result, env, stringValues, imports) != eventPathNone && !eventStoreReturnVocabulary(rel, result) {
						record(result, "event path returned to caller")
					}
				}
			}
			inspectEventStoreOperation(node, env, stringValues, imports, record)
			return true
		})
	}
	return violations
}

func inspectEventStoreOperation(node ast.Node, env map[string]eventPathKind, stringValues map[string]string, imports map[string]string, record func(ast.Node, string)) {
	switch n := node.(type) {
	case *ast.BinaryExpr:
		if n.Op == token.ADD && eventStoreExprKind(n, env, stringValues, imports) == eventPathPhysical {
			record(n, "event path concatenation")
		}
	case *ast.CallExpr:
		if eventStoreImportedCall(n, imports, "path/filepath", "Join", "FromSlash") || eventStoreImportedCall(n, imports, "path", "Join") {
			if eventStoreArgsKind(n.Args, env, stringValues, imports) != eventPathNone {
				record(n, "event path construction")
			}
		}
		if eventStorePathFormattingCall(n, imports) && eventStoreArgsKind(n.Args, env, stringValues, imports) != eventPathNone {
			record(n, "event path formatting")
		}
		if eventStoreFilesystemCall(n, imports) && eventStoreArgsKind(n.Args, env, stringValues, imports) != eventPathNone {
			record(n, "event filesystem access")
		}
	}
}

func bindEventStoreValues(rel string, names []*ast.Ident, values []ast.Expr, env map[string]eventPathKind, stringValues map[string]string, imports map[string]string, record func(ast.Node, string)) {
	for i, name := range names {
		if name == nil || name.Name == "_" {
			continue
		}
		kind := eventPathNone
		var value ast.Expr
		if i < len(values) {
			value = values[i]
		} else if len(values) == 1 {
			value = values[0]
		}
		if value != nil {
			kind = eventStoreExprKind(value, env, stringValues, imports)
		}
		if folded, ok := eventStoreStaticString(value, stringValues); ok {
			stringValues[name.Name] = folded
		} else {
			delete(stringValues, name.Name)
		}
		if kind == eventPathNone {
			delete(env, name.Name)
			continue
		}
		env[name.Name] = kind
		if kind == eventPathPhysical && !eventStoreVocabularyBinding(rel, name.Name) {
			record(value, "physical event path bound to "+name.Name)
		}
	}
}

func eventStoreVocabularyBinding(rel, name string) bool {
	if rel == "internal/arch/eventstore_guard.go" {
		return true
	}
	if strings.HasPrefix(rel, "internal/home/") {
		lower := strings.ToLower(name)
		return name == "entries" || strings.Contains(lower, "habitat")
	}
	return rel == "internal/authority/os_root_resolver.go" && strings.Contains(strings.ToLower(name), "absorbedos")
}

func eventStoreReturnVocabulary(rel string, expr ast.Expr) bool {
	if rel == "internal/arch/eventstore_guard.go" {
		return true
	}
	if (rel == "internal/eventsource/eventsource.go" || rel == "internal/reviewartifact/resolution.go") && eventStoreContainsDiagnosticPath(expr) {
		return true
	}
	ident, ok := expr.(*ast.Ident)
	return ok && eventStoreVocabularyBinding(rel, ident.Name)
}

func eventStoreContainsDiagnosticPath(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "DiagnosticPath" {
			found = true
			return false
		}
		return !found
	})
	return found
}

func eventStoreExprKind(expr ast.Expr, env map[string]eventPathKind, stringValues map[string]string, imports map[string]string) eventPathKind {
	if value, ok := eventStoreStaticString(expr, stringValues); ok {
		return eventStoreStringKind(value)
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return eventPathNone
		}
		value, err := stdstrconv.Unquote(e.Value)
		if err != nil {
			return eventPathNone
		}
		return eventStoreStringKind(value)
	case *ast.Ident:
		return env[e.Name]
	case *ast.SelectorExpr:
		if e.Sel.Name == "DiagnosticPath" {
			return eventPathPhysical
		}
		return eventPathNone
	case *ast.ParenExpr:
		return eventStoreExprKind(e.X, env, stringValues, imports)
	case *ast.BinaryExpr:
		left := eventStoreExprKind(e.X, env, stringValues, imports)
		right := eventStoreExprKind(e.Y, env, stringValues, imports)
		kind := eventStoreMaxKind(left, right)
		if e.Op == token.ADD && kind == eventPathComponent {
			return eventPathPhysical
		}
		return kind
	case *ast.CompositeLit:
		kind := eventPathNone
		for _, elt := range e.Elts {
			kind = eventStoreMaxKind(kind, eventStoreExprKind(elt, env, stringValues, imports))
		}
		return kind
	case *ast.KeyValueExpr:
		return eventStoreExprKind(e.Value, env, stringValues, imports)
	case *ast.CallExpr:
		kind := eventStoreArgsKind(e.Args, env, stringValues, imports)
		if eventStoreImportedCall(e, imports, "path/filepath", "Join", "FromSlash") || eventStoreImportedCall(e, imports, "path", "Join") {
			if kind != eventPathNone {
				return eventPathPhysical
			}
		}
		if eventStorePathFormattingCall(e, imports) && kind != eventPathNone {
			return eventPathPhysical
		}
		if eventStoreImportedCall(e, imports, "path/filepath", "Clean", "Dir", "ToSlash") ||
			eventStoreImportedCall(e, imports, "path", "Clean", "Dir") {
			return kind
		}
		return eventPathNone
	}
	return eventPathNone
}

func eventStoreStaticString(expr ast.Expr, values map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		value, err := stdstrconv.Unquote(e.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := values[e.Name]
		return value, ok
	case *ast.ParenExpr:
		return eventStoreStaticString(e.X, values)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, leftOK := eventStoreStaticString(e.X, values)
		right, rightOK := eventStoreStaticString(e.Y, values)
		if !leftOK || !rightOK {
			return "", false
		}
		return left + right, true
	default:
		return "", false
	}
}

func eventStoreArgsKind(args []ast.Expr, env map[string]eventPathKind, stringValues map[string]string, imports map[string]string) eventPathKind {
	kind := eventPathNone
	for _, arg := range args {
		kind = eventStoreMaxKind(kind, eventStoreExprKind(arg, env, stringValues, imports))
	}
	return kind
}

func eventStoreMaxKind(a, b eventPathKind) eventPathKind {
	if a > b {
		return a
	}
	return b
}

func eventStoreStringKind(value string) eventPathKind {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	if strings.HasPrefix(normalized, "compound://events/") {
		return eventPathNone
	}
	if normalized == "events" || normalized == "events/" || normalized == "work.jsonl" || normalized == "hook-events.ndjson" || normalized == "traces.ndjson" || normalized == "retrieval-traffic.ndjson" {
		return eventPathComponent
	}
	if strings.HasPrefix(normalized, "events/") || strings.Contains(normalized, "/events/") || strings.HasSuffix(normalized, "/events") {
		return eventPathPhysical
	}
	return eventPathNone
}

func eventStoreLiteralKind(lit *ast.BasicLit) eventPathKind {
	if lit == nil || lit.Kind != token.STRING {
		return eventPathNone
	}
	value, err := stdstrconv.Unquote(lit.Value)
	if err != nil {
		return eventPathNone
	}
	return eventStoreStringKind(value)
}

// These literals describe another owner's vocabulary; they are not paths used by
// production filesystem code. The exception is literal-specific, so any Join or
// filesystem sink in the same file is still rejected by the checks above.
func eventStoreLiteralVocabulary(rel string, lit *ast.BasicLit) bool {
	if rel == "internal/arch/eventstore_guard.go" {
		return true
	}
	if !strings.HasPrefix(rel, "internal/home/") || lit == nil || lit.Kind != token.STRING {
		return false
	}
	value, err := stdstrconv.Unquote(lit.Value)
	return err == nil && value == "events/logs"
}

func eventStoreImports(file *ast.File) map[string]string {
	imports := map[string]string{}
	for _, spec := range file.Imports {
		pathValue, err := stdstrconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(pathValue)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = pathValue
	}
	return imports
}

func eventStoreImportedCall(call *ast.CallExpr, imports map[string]string, importPath string, names ...string) bool {
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
		ident, ok := selector.X.(*ast.Ident)
		return ok && imports[ident.Name] == importPath && allowed[selector.Sel.Name]
	}
	if ident, ok := call.Fun.(*ast.Ident); ok && imports["."] == importPath {
		return allowed[ident.Name]
	}
	return false
}

func eventStoreFilesystemCall(call *ast.CallExpr, imports map[string]string) bool {
	if eventStoreImportedCall(call, imports, "path/filepath", "Glob", "Walk", "WalkDir", "EvalSymlinks") {
		return true
	}
	if eventStoreImportedCall(call, imports, "io/fs", "ReadFile", "ReadDir", "Stat", "Sub", "Glob", "WalkDir") {
		return true
	}
	return eventStoreImportedCall(call, imports, "os",
		"Open", "OpenFile", "ReadFile", "WriteFile", "ReadDir", "Stat", "Lstat", "Create", "CreateTemp",
		"Mkdir", "MkdirAll", "Remove", "RemoveAll", "Rename", "Truncate", "Chmod", "Chown", "Chtimes",
		"Readlink", "Symlink", "Link")
}

func eventStorePathFormattingCall(call *ast.CallExpr, imports map[string]string) bool {
	return eventStoreImportedCall(call, imports, "fmt", "Sprintf", "Sprint", "Sprintln") ||
		eventStoreImportedCall(call, imports, "strings", "Join")
}

func checkEventStorePublicContract(repoRoot string) []Finding {
	required := []struct {
		rel     string
		phrases []string
	}{
		{"docs/architecture/contracts/README.md", []string{"Event-reference contract", "bare IDs", "`compound://events/<id>`", "historical `events/<id>.json`", "segment/index paths"}},
		{"docs/architecture/runtime.md", []string{"`internal/eventstore` is the sole physical-layout owner", "`compound://events/<id>`", "`events/<id>.json`", "`events/work.jsonl`", "`events/hook-events.ndjson`", "`events/traces.ndjson`"}},
		{"docs/architecture/status.md", []string{"| `eventstore` | leaf |", "sole physical-layout owner"}},
	}
	var findings []Finding
	for _, contract := range required {
		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(contract.rel)))
		if err != nil {
			findings = append(findings, Finding{"eventstore-public-contract", Fail, "cannot read " + contract.rel + ": " + err.Error()})
			continue
		}
		text := string(body)
		for _, phrase := range contract.phrases {
			if !strings.Contains(text, phrase) {
				findings = append(findings, Finding{"eventstore-public-contract", Fail, contract.rel + " is missing " + stdstrconv.Quote(phrase)})
			}
		}
	}
	if len(findings) == 0 {
		findings = append(findings, Finding{"eventstore-public-contract", Info, "public event-store ownership and reference compatibility contract is present"})
	}
	return findings
}
