package home

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveHealthPreflightDoesNotReachFullDoctorDiagnostics(t *testing.T) {
	functions := productionFunctionCalls(t)
	reachable := reachableFunctions(functions, "requireHealthyHome")
	if !reachable["inspectHomeHealth"] {
		t.Error("requireHealthyHome does not reach the shared structural inspectHomeHealth helper")
	}
	for _, forbidden := range []string{
		"Doctor", "DoctorWithOptions", "DiagnosePrivateCustody", "underGitRepo",
		"hygieneChecks", "backupFreshnessWarnings", "DoctorSetup",
	} {
		if reachable[forbidden] {
			t.Errorf("requireHealthyHome transitively reaches full-doctor diagnostic %s", forbidden)
		}
	}
}

func TestArchiveHealthPreflightMatchesDoctorHealthVerdict(t *testing.T) {
	t.Run("private artifact corruption is diagnostic only", func(t *testing.T) {
		homePath := initializedArchiveHealthTestHome(t)
		objectPath := filepath.Join(homePath, "private", "artifacts", "sha256", strings.Repeat("a", 64))
		if err := os.WriteFile(objectPath, []byte("body does not match its digest name"), 0o600); err != nil {
			t.Fatal(err)
		}
		doctor := Doctor(homePath)
		if !doctor.OK || doctor.PrivateCustody.OK {
			t.Fatalf("doctor verdict lost diagnostic/health separation: OK=%t private_custody.OK=%t", doctor.OK, doctor.PrivateCustody.OK)
		}
		foundCorruptBody := false
		for _, finding := range doctor.PrivateCustody.Findings {
			if strings.Contains(finding.Detail, "content hash does not match digest filename") {
				foundCorruptBody = true
				break
			}
		}
		if !foundCorruptBody {
			t.Fatalf("full Doctor omitted corrupt private-body finding: %+v", doctor.PrivateCustody)
		}
		if _, err := ArchiveOperationalHistory(homePath, OperationalArchiveOptions{Kinds: []string{"memory-candidate"}}, archiveTestNow); err != nil {
			t.Fatalf("archive rejected structurally healthy home with private-custody finding: %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "invalid manifest", mutate: func(t *testing.T, homePath string) {
			if err := os.WriteFile(filepath.Join(homePath, ManifestName), []byte("not-json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing scaffold", mutate: func(t *testing.T, homePath string) {
			if err := os.RemoveAll(filepath.Join(homePath, HabitatDirs()[0])); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unknown habitat root", mutate: func(t *testing.T, homePath string) {
			if err := os.Mkdir(filepath.Join(homePath, "unknown-synthetic-root"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := initializedArchiveHealthTestHome(t)
			tt.mutate(t, homePath)
			if doctor := Doctor(homePath); doctor.OK {
				t.Fatalf("Doctor accepted structurally unhealthy home: %+v", doctor)
			}
			if _, err := ArchiveOperationalHistory(homePath, OperationalArchiveOptions{Kinds: []string{"memory-candidate"}}, archiveTestNow); err == nil {
				t.Fatal("archive accepted structurally unhealthy home")
			}
		})
	}
}

func initializedArchiveHealthTestHome(t *testing.T) string {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "global", archiveTestNow); err != nil {
		t.Fatal(err)
	}
	return homePath
}

func productionFunctionCalls(t *testing.T) map[string]map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	functions := map[string]map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			calls := map[string]bool{}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok {
					calls[ident.Name] = true
				}
				return true
			})
			functions[fn.Name.Name] = calls
		}
	}
	return functions
}

func reachableFunctions(functions map[string]map[string]bool, root string) map[string]bool {
	reachable := map[string]bool{}
	queue := []string{root}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if reachable[name] {
			continue
		}
		reachable[name] = true
		for called := range functions[name] {
			if !reachable[called] {
				queue = append(queue, called)
			}
		}
	}
	return reachable
}
