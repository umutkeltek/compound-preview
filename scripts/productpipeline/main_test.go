package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
)

func TestPublicBundleFreshCloneRoutes(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	bundle := filepath.Join(repo, "skills/product-pipeline")
	if _, err := os.Stat(filepath.Join(bundle, "SKILL.md")); err != nil {
		t.Fatalf("public lifecycle bundle is unavailable in a fresh checkout: %v", err)
	}
	// Copy only the public bundle. No ignored overlay, registry snapshot or account config.
	relocated := filepath.Join(t.TempDir(), "relocated checkout", "skills", "product-pipeline")
	err := filepath.WalkDir(bundle, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(bundle, p)
		if err != nil {
			return err
		}
		out := filepath.Join(relocated, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0700)
		}
		if !d.Type().IsRegular() {
			return fs.ErrInvalid
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	env := append(filteredEnv(os.Environ(), "COMPOUND_HOME"), "COMPOUND_HOME="+home)
	run := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("public command %v failed: %v\n%s", args, err, out)
		}
		return out
	}
	body := run("run", "./scripts/productpipeline", "--root", relocated)
	var m capgraph.SkillManifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, body)
	}
	if len(m.Skills) != 16 {
		t.Fatalf("core entries = %d, want one router plus fifteen branches", len(m.Skills))
	}
	if !strings.HasPrefix(m.SourceRef.Path, relocated+string(filepath.Separator)) {
		t.Fatalf("source points outside relocated public bundle: %s", m.SourceRef.Path)
	}
	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifestPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	run("run", "./cmd/compound", "home", "init")
	imported := string(run("run", "./cmd/compound", "capability", "import-skills", manifestPath))
	if !strings.Contains(imported, "- status: ok") {
		t.Fatalf("native importer did not accept public bundle:\n%s", imported)
	}
	out := run("run", "./cmd/compound", "capability", "surface", "--harness", "codex", "--json", "Design a product lifecycle pipeline.")
	var surface capgraph.SurfaceReport
	if err := json.Unmarshal(out, &surface); err != nil {
		t.Fatalf("surface is not JSON: %v", err)
	}
	if !hasOffer(surface.Offers, "skill:product-pipeline") {
		t.Fatalf("native public router is not offered:\n%s", out)
	}
	store, err := capgraph.OpenReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	checkRoutingCases(t, store, relocated)
	// Read through the actual ContextPack consumer, not just the graph projection.
	packBody := run("run", "./cmd/compound", "orient", "--harness", "codex", "--cwd", relocated, "--task", "Verify critical journey recovery including offline and permission failures.")
	var pack envelope.ContextPack
	if err := json.Unmarshal(packBody, &pack); err != nil {
		t.Fatalf("ContextPack is not JSON: %v", err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}
	wanted := "capability://instruction_rule:product-lifecycle-qa"
	included := false
	for _, ref := range pack.Included {
		if ref.Ref == wanted {
			included = true
		}
	}
	if !included {
		t.Fatalf("real ContextPack did not include QA context: %s", packBody)
	}
	readBody := false
	for _, offer := range pack.CapabilityOffers {
		if offer.Ref != wanted {
			continue
		}
		for _, required := range offer.Required {
			if required.Kind == "read" && required.Ref == filepath.Join(relocated, "branches/qa.md") {
				readBody = true
			}
		}
	}
	if !readBody {
		t.Fatal("ContextPack did not carry the public QA read target")
	}
}

func filteredEnv(env []string, key string) []string {
	var out []string
	for _, v := range env {
		if !strings.HasPrefix(v, key+"=") {
			out = append(out, v)
		}
	}
	return out
}

func publicBundle(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../skills/product-pipeline"))
}

func hasOffer(offers []capgraph.Surfaced, id string) bool {
	for _, offer := range offers {
		if offer.Node.ID == id {
			return true
		}
	}
	return false
}

func checkRoutingCases(t *testing.T, store *capgraph.Store, bundle string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(bundle, "routing.cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		Cases []struct {
			ID, Task         string
			Expected, Absent []string
		}
		KnownLimitations []struct {
			ID, Task      string
			ObservedOffer string `json:"observed_offer"`
		} `json:"known_limitations"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases.Cases) != 21 || len(cases.KnownLimitations) != 1 {
		t.Fatal("routing corpus unexpectedly changed")
	}
	for _, c := range cases.Cases {
		t.Run(c.ID, func(t *testing.T) {
			offers, err := store.SurfaceForHarness(c.Task, "codex")
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range c.Expected {
				if !hasOffer(offers, id) {
					t.Errorf("missing expected context %s", id)
				}
			}
			for _, id := range c.Absent {
				if hasOffer(offers, id) {
					t.Errorf("near-miss offered %s", id)
				}
			}
			for _, o := range offers {
				if len(o.Node.Read) == 0 {
					t.Errorf("offered context has no local body: %s", o.Node.ID)
				}
				for _, read := range o.Node.Read {
					if !strings.HasPrefix(read, bundle+string(filepath.Separator)) {
						t.Errorf("read escapes relocated bundle: %s", read)
					}
					if _, err := os.ReadFile(read); err != nil {
						t.Errorf("offered read unavailable: %v", err)
					}
				}
			}
		})
	}
	// Deliberately not counted as a silence success: a complete negated lexical
	// match remains an offer in the existing matcher. Consumer intent must gate action.
	for _, c := range cases.KnownLimitations {
		offers, err := store.SurfaceForHarness(c.Task, "codex")
		if err != nil {
			t.Fatal(err)
		}
		if !hasOffer(offers, c.ObservedOffer) {
			t.Fatalf("known negation behavior changed; re-evaluate limitation before changing claim")
		}
	}
}

func copyPublicBundle(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bundle")
	err := filepath.WalkDir(publicBundle(t), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(publicBundle(t), p)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func mutateTemplate(t *testing.T, root string, edit func(*capgraph.SkillManifest)) {
	t.Helper()
	path := filepath.Join(root, "routing.template.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m capgraph.SkillManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	edit(&m)
	b, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratorRejectsUnsafeAndUnbundledTargets(t *testing.T) {
	// These reject paths before any importer/policy/state mutation is possible.
	cases := []struct {
		name string
		edit func(*testing.T, string)
	}{
		{"missing", func(t *testing.T, root string) {
			mutateTemplate(t, root, func(m *capgraph.SkillManifest) { m.Skills[0].Read[0] = "missing.md" })
		}},
		{"escape", func(t *testing.T, root string) {
			mutateTemplate(t, root, func(m *capgraph.SkillManifest) { m.Skills[0].Read[0] = "../outside.md" })
		}},
		{"absolute", func(t *testing.T, root string) {
			mutateTemplate(t, root, func(m *capgraph.SkillManifest) { m.SourceRef.Path = filepath.Join(root, "README.md") })
		}},
		{"symlink_file", func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, "SKILL.md"), filepath.Join(root, "original.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("original.md", filepath.Join(root, "SKILL.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink_directory", func(t *testing.T, root string) {
			if err := os.Rename(filepath.Join(root, "branches"), filepath.Join(root, "original-branches")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("original-branches", filepath.Join(root, "branches")); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink_root", func(t *testing.T, root string) {
			target := root + "-real"
			if err := os.Rename(root, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, root); err != nil {
				t.Fatal(err)
			}
		}},
		{"unbundled_route", func(t *testing.T, root string) {
			mutateTemplate(t, root, func(m *capgraph.SkillManifest) { m.Skills[1].Route[0].Destination = "skill:not-installed" })
		}},
		{"unbundled_relation", func(t *testing.T, root string) {
			mutateTemplate(t, root, func(m *capgraph.SkillManifest) { m.Skills[0].Requires = []string{"skill:not-installed"} })
		}},
		{"invalid_native", func(t *testing.T, root string) {
			mutateTemplate(t, root, func(m *capgraph.SkillManifest) { m.SchemaVersion = "unsupported" })
		}},
		{"unknown_field", func(t *testing.T, root string) {
			p := filepath.Join(root, "routing.template.json")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			b = append([]byte(`{"unknown_field":true,`), b[1:]...)
			if err := os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"extra_json", func(t *testing.T, root string) {
			p := filepath.Join(root, "routing.template.json")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, append(b, []byte("{}")...), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := copyPublicBundle(t)
			c.edit(t, root)
			if _, err := generate(root, time.Now()); err == nil {
				t.Fatal("unsafe/invalid target was accepted")
			}
		})
	}
}

func TestRevisionRelocationAndContent(t *testing.T) {
	a, b := copyPublicBundle(t), copyPublicBundle(t)
	first, err := generate(a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, err := generate(b, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceRef.Revision != second.SourceRef.Revision {
		t.Fatal("location/clock changed content revision")
	}
	if first.SourceRef.Path == second.SourceRef.Path || first.ObservedAt == second.ObservedAt {
		t.Fatal("runtime anchors/time were not resolved freshly")
	}
	p := filepath.Join(b, "branches/ui.md")
	content, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(content, []byte("\nChanged method.\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := generate(b, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceRef.Revision == changed.SourceRef.Revision {
		t.Fatal("content change did not change revision")
	}
}

func TestPublicSourcesAndExplicitOnlyExclusions(t *testing.T) {
	root := publicBundle(t)
	m, err := generate(root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Skills {
		if s.ID == "skill:prototype" || s.ID == "skill:pick-ui-library" {
			t.Fatal("explicit-only external skill auto-bound")
		}
		if len(s.Boundary) == 0 {
			t.Fatalf("missing authority boundary: %s", s.ID)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, "SOURCES.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"ai-that-works/ai-that-works", "humanlayer/skills", "humanlayer/12-factor-agents", "cursor/plugins", "emilkowalski/skills", "mattpocock/skills", "coreyhaines31/marketingskills", "anthropics/skills", "pbakaus/impeccable", "resend/resend-skills"} {
		if !strings.Contains(string(b), "https://github.com/"+source+"/tree/") {
			t.Errorf("missing pinned source %s", source)
		}
	}
	for _, boundary := range []string{"original Compound synthesis", "does not vendor", "License scope", "not established", "no assumed copying rights", "No upstream"} {
		if !strings.Contains(string(b), boundary) {
			t.Errorf("missing attribution/license boundary %q", boundary)
		}
	}
	// Enforce full revisions and license-byte hashes without freezing upstream
	// revisions forever. Changing a source still requires the documented review.
	pins := regexp.MustCompile(`https://github.com/[^/[:space:]]+/[^/[:space:]]+/tree/([a-f0-9]+)`).FindAllSubmatch(b, -1)
	if len(pins) != 10 {
		t.Fatalf("pinned source catalog entries = %d, want 10", len(pins))
	}
	for _, pin := range pins {
		if len(pin[1]) != 40 {
			t.Errorf("non-full revision %s", pin[1])
		}
	}
	hashes := regexp.MustCompile("inspected bytes SHA-256 `([a-f0-9]{64})`").FindAll(b, -1)
	if len(hashes) != 9 {
		t.Fatalf("verified license hash count = %d, want 9 licensed sources", len(hashes))
	}
}

func TestPublicMarkdownLinksResolve(t *testing.T) {
	root := publicBundle(t)
	links := regexp.MustCompile(`\]\(([^)]+)\)`)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".md" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, link := range links.FindAllSubmatch(b, -1) {
			ref := strings.Split(string(link[1]), "#")[0]
			if ref == "" || strings.HasPrefix(ref, "https://") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(p), ref)); err != nil {
				t.Errorf("%s has unavailable link %s: %v", filepath.Base(p), ref, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
