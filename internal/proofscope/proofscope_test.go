package proofscope

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
)

// fixtureFiles is a tiny module: a leaf, a prod chain a <- b <- c, an
// unrelated d, e whose test imports b, f that prod-imports e, a runtime.Caller
// root reader, a fixture reader, a temp-dir traversal test and a test that
// names a package by literal import path.
func fixtureFiles() map[string]string {
	return map[string]string{
		"go.mod":                      "module compound\n\ngo 1.23\n",
		"README.md":                   "# fixture\n",
		"Makefile":                    "all:\n",
		"docs/guide.md":               "guide\n",
		"fixtures/x/data.json":        "{}\n",
		"fixtures/other/y.json":       "{}\n",
		"internal/a/a.go":             "package a\n",
		"internal/a/testdata/in.json": "{}\n",
		"internal/b/b.go":             "package b\nimport _ \"compound/internal/a\"\n",
		"internal/c/c.go":             "package c\nimport _ \"compound/internal/b\"\n",
		"internal/d/d.go":             "package d\n",
		"internal/e/e.go":             "package e\n",
		"internal/e/e_test.go":        "package e\nimport (\n\t\"testing\"\n\t_ \"compound/internal/b\"\n)\nfunc TestE(t *testing.T) {}\n",
		"internal/f/f.go":             "package f\nimport _ \"compound/internal/e\"\n",
		"internal/rr/rr.go":           "package rr\n",
		"internal/rr/rr_test.go":      "package rr\nimport (\n\t\"runtime\"\n\t\"testing\"\n)\nfunc TestRoot(t *testing.T) { _, _, _, _ = runtime.Caller(0) }\n",
		"internal/fx/fx.go":           "package fx\n",
		"internal/fx/fx_test.go":      "package fx\nimport (\n\t\"path/filepath\"\n\t\"testing\"\n)\nfunc TestFixture(t *testing.T) { _ = filepath.Join(\"..\", \"..\", \"fixtures\", \"x\") }\n",
		"internal/esc/esc.go":         "package esc\n",
		"internal/esc/esc_test.go":    "package esc\nimport \"testing\"\nfunc TestEscape(t *testing.T) { _ = \"../escape\" }\n",
		"internal/lit/lit.go":         "package lit\n",
		"internal/lit/lit_test.go":    "package lit\nimport \"testing\"\nfunc TestLit(t *testing.T) { _ = []string{\"compound/internal/c\"} }\n",
	}
}

func snapshot(files map[string]string, withSource bool) Snapshot {
	var snap Snapshot
	if withSource {
		snap.Source = map[string][]byte{}
	}
	for name, body := range files {
		snap.Paths = append(snap.Paths, name)
		if withSource && (name == "go.mod" || len(name) > 3 && name[len(name)-3:] == ".go") {
			snap.Source[name] = []byte(body)
		}
	}
	slices.Sort(snap.Paths)
	return snap
}

func derive(base, head map[string]string, changed ...string) Scope {
	return Derive(snapshot(base, false), snapshot(head, true), changed)
}

func pkgs(names ...string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = "compound/internal/" + name
	}
	return out
}

func with(files map[string]string, edits map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		out[k] = v
	}
	for k, v := range edits {
		if v == "" {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

func TestDeriveAffectedScopes(t *testing.T) {
	files := fixtureFiles()
	for _, tc := range []struct {
		name    string
		changed []string
		want    []string
		seeds   []string
	}{
		{"docs only runs only root readers", []string{"docs/guide.md"}, pkgs("rr"), nil},
		{"root markdown is not a root file", []string{"README.md"}, pkgs("rr"), nil},
		{"leaf change follows prod importers and one test hop", []string{"internal/a/a.go"}, pkgs("a", "b", "c", "e", "lit", "rr"), pkgs("a")},
		{"testdata seeds its ancestor package", []string{"internal/a/testdata/in.json"}, pkgs("a", "b", "c", "e", "lit", "rr"), pkgs("a")},
		{"fixture change triggers its reader only", []string{"fixtures/x/data.json"}, pkgs("fx", "rr"), nil},
		{"other fixture triggers no reader", []string{"fixtures/other/y.json"}, pkgs("rr"), nil},
		{"literal import path is a test edge", []string{"internal/c/c.go"}, pkgs("c", "lit", "rr"), pkgs("c")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := derive(files, files, tc.changed...)
			if got.Mode != Affected || got.Reason != "diff" {
				t.Fatalf("mode=%s reason=%s; want affected diff", got.Mode, got.Reason)
			}
			if !reflect.DeepEqual(got.Packages, tc.want) {
				t.Fatalf("packages=%v; want %v", got.Packages, tc.want)
			}
			if !reflect.DeepEqual(got.Changed, tc.seeds) {
				t.Fatalf("changed=%v; want %v", got.Changed, tc.seeds)
			}
			if !reflect.DeepEqual(got.RootReaders, pkgs("rr")) {
				t.Fatalf("root readers=%v; want rr only", got.RootReaders)
			}
			for _, excluded := range pkgs("d", "f", "esc") {
				if slices.Contains(got.Packages, excluded) {
					t.Fatalf("packages=%v include %s", got.Packages, excluded)
				}
			}
		})
	}
}

func TestDeriveFullScopes(t *testing.T) {
	files := fixtureFiles()
	withoutD := with(files, map[string]string{"internal/d/d.go": ""})
	for _, tc := range []struct {
		name       string
		base, head map[string]string
		changed    []string
		reason     string
	}{
		{"go.mod", files, files, []string{"go.mod"}, "module-files:go.mod"},
		{"go.sum", files, files, []string{"go.sum"}, "module-files:go.sum"},
		{"nested go.work", files, files, []string{"tools/go.work"}, "module-files:tools/go.work"},
		{"unknown root file", files, files, []string{"Makefile"}, "root-file:Makefile"},
		{"testbattery owner", files, files, []string{"scripts/testbattery/x.go"}, "scope-owner:scripts/testbattery/x.go"},
		{"proofscope owner", files, files, []string{"internal/proofscope/y.go"}, "scope-owner:internal/proofscope/y.go"},
		{"package removed", files, withoutD, []string{"internal/d/d.go"}, "package-removed:internal/d"},
		{"empty diff", files, files, nil, "empty-diff"},
		{"parse error", files, with(files, map[string]string{"internal/d/d.go": "packge d\n"}), []string{"internal/d/d.go"}, "parse-error:internal/d/d.go"},
		{"wrong module", files, with(files, map[string]string{"go.mod": "module example.com/other\n"}), []string{"docs/guide.md"}, "module-path"},
		{"missing go.mod", files, with(files, map[string]string{"go.mod": ""}), []string{"docs/guide.md"}, "module-path"},
		{"affected all", map[string]string{"go.mod": "module compound\n", "internal/a/a.go": "package a\n", "internal/b/b.go": "package b\nimport _ \"compound/internal/a\"\n"},
			map[string]string{"go.mod": "module compound\n", "internal/a/a.go": "package a\n", "internal/b/b.go": "package b\nimport _ \"compound/internal/a\"\n"},
			[]string{"internal/a/a.go"}, "affected-all"},
		{"first sorted rule wins", files, files, []string{"internal/a/a.go", "Makefile", "go.sum"}, "root-file:Makefile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := derive(tc.base, tc.head, tc.changed...)
			if got.Mode != Full || got.Reason != tc.reason || got.Packages != nil {
				t.Fatalf("got %+v; want full %s", got, tc.reason)
			}
		})
	}
}

func TestDeriveIsDeterministicAcrossInputOrder(t *testing.T) {
	files := fixtureFiles()
	changed := []string{"internal/a/a.go", "fixtures/x/data.json", "docs/guide.md"}
	want := derive(files, files, changed...)
	rng := rand.New(rand.NewSource(1))
	for range 20 {
		base, head := snapshot(files, false), snapshot(files, true)
		rng.Shuffle(len(base.Paths), func(i, j int) { base.Paths[i], base.Paths[j] = base.Paths[j], base.Paths[i] })
		rng.Shuffle(len(head.Paths), func(i, j int) { head.Paths[i], head.Paths[j] = head.Paths[j], head.Paths[i] })
		order := slices.Clone(changed)
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		if got := Derive(base, head, order); !reflect.DeepEqual(got, want) {
			t.Fatalf("order-dependent scope:\n got %+v\nwant %+v", got, want)
		}
	}
}

func TestPinnedRootReadersRunWithoutMarkers(t *testing.T) {
	files := with(fixtureFiles(), map[string]string{
		"internal/arch/arch.go":         "package arch\n",
		"internal/contract/contract.go": "package contract\n",
	})
	got := derive(files, files, "docs/guide.md")
	if want := pkgs("arch", "contract", "rr"); got.Mode != Affected || !reflect.DeepEqual(got.Packages, want) || !reflect.DeepEqual(got.RootReaders, want) {
		t.Fatalf("got %+v; want packages and root readers %v", got, want)
	}
}

func TestRootReaderMarkers(t *testing.T) {
	for name, body := range map[string]string{
		"os.Getwd":       "package m\nimport (\n\t\"os\"\n\t\"testing\"\n)\nfunc TestM(t *testing.T) { _, _ = os.Getwd() }\n",
		"show-toplevel":  "package m\nimport \"testing\"\nfunc TestM(t *testing.T) { _ = []string{\"rev-parse\", \"--show-toplevel\"} }\n",
		"GOMOD":          "package m\nimport \"testing\"\nfunc TestM(t *testing.T) { _ = \"GOMOD\" }\n",
		"module pattern": "package m\nimport \"testing\"\nfunc TestM(t *testing.T) { _ = \"compound/...\" }\n",
		"walks to root":  "package m\nimport (\n\t\"path/filepath\"\n\t\"testing\"\n)\nfunc TestM(t *testing.T) { _ = filepath.Join(\"..\", \"..\") }\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := with(fixtureFiles(), map[string]string{"internal/m/m.go": "package m\n", "internal/m/m_test.go": body})
			got := derive(files, files, "docs/guide.md")
			if !slices.Contains(got.RootReaders, "compound/internal/m") {
				t.Fatalf("root readers=%v; want internal/m", got.RootReaders)
			}
		})
	}
}

func TestDeletedFixtureStillTriggersReader(t *testing.T) {
	base := fixtureFiles()
	head := with(base, map[string]string{"fixtures/x/data.json": ""})
	got := derive(base, head, "fixtures/x/data.json")
	if got.Mode != Affected || !slices.Contains(got.Packages, "compound/internal/fx") {
		t.Fatalf("got %+v; want fx triggered by a deleted fixture", got)
	}
}

func TestBareParentLiteralReadsParentDirectory(t *testing.T) {
	files := with(fixtureFiles(), map[string]string{
		"internal/m/m.go":      "package m\n",
		"internal/m/m_test.go": "package m\nimport \"testing\"\nfunc TestM(t *testing.T) { _ = []string{\"..\"} }\n",
	})
	if got := derive(files, files, "internal/d/d.go"); !slices.Contains(got.Packages, "compound/internal/m") || slices.Contains(got.RootReaders, "compound/internal/m") {
		t.Fatalf("got %+v; want m triggered by a change under internal/ and not a root reader", got)
	}
	if got := derive(files, files, "docs/guide.md"); slices.Contains(got.Packages, "compound/internal/m") {
		t.Fatalf("got %+v; want m untouched by a docs change", got)
	}
}
