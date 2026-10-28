package harness

// skill_adapter_test.go — falsifiers F1, F2, F3.
//
// F1: the canonical body is harness-neutral — no Claude-specific strings,
//     contains the profile registry, the agent-browser door and the
//     isolation ladder.
// F2: render is deterministic per harness — same body+adapter+time ⇒
//     byte-identical; footer carries body sha256.
// F3: install detects drift; replaces symlinked parent with a real dir and
//     reports it.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallSkillAdaptersRejectsUnsupportedSkillBeforeMutation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	path := filepath.Join(root, ".codex", "skills", "ExampleSkill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("---\nname: ExampleSkill\n---\nOwned canonical procedure.\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		_, err := InstallSkillAdapters("ExampleSkill", DefaultSkillAdapters(), apply, time.Now(), fixtureBody(t))
		if err == nil || !strings.Contains(err.Error(), "supports only Browser") {
			t.Fatalf("apply=%t: expected unsupported skill refusal, got %v", apply, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatalf("unsupported request changed existing procedure: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("unsupported request created another adapter: %v", err)
	}
}

func fixtureBody(t *testing.T) []byte {
	t.Helper()
	// A harness-neutral body. Mirrors the canonical rewrite: no PAI path,
	// no voice-notify curl, no execution.jsonl, no Claude built-ins, no
	// mcp__ tool names. References browser-profiles.json + agent-browser.
	return []byte(`---
name: Browser
description: harness-neutral body (test fixture)
---

# Browser (test fixture body)

One door: ` + "`agent-browser --session <task> --profile <dir> <verb>`" + `.

Profile registry: ` + "`~/.compound/registries/browser-profiles.json`" + `.

Backend vocabulary: navigate, snapshot, click, type, screenshot, extract,
login-check, close.

Safety rules:
- Never log in with an unenrolled profile.
- Never persist credentials in transcripts.

Recipes / Stories / Workflows continue to ship with the body.
`)
}

func fixedNow() time.Time {
	return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
}

// F1: body is harness-neutral.
func TestBrowserSkillBodyIsHarnessNeutral(t *testing.T) {
	home := os.Getenv("HOME")
	if strings.TrimSpace(home) == "" {
		t.Skip("HOME not set — no canonical body available in this test environment")
	}
	path := filepath.Join(home, ".agents", "skills", "Browser", "SKILL.md")
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("canonical body absent at %s — public clone, skipping (the test is for the live body)", path)
		}
		t.Fatalf("read canonical body: %v", err)
	}
	text := string(body)
	banned := []string{
		"~/.claude/",
		"PAI/",
		"localhost:31337",
		"mcp__",
		"Claude Code built-in",
	}
	for _, needle := range banned {
		if strings.Contains(text, needle) {
			t.Errorf("F1 FAIL: canonical body still contains Claude-coupled string %q — move it to the claude_code adapter", needle)
		}
	}
	required := []string{
		"browser-profiles.json",
		"agent-browser --session",
		"--profile",
		"compound browser goal",
		// The isolation ladder: every level is named so a lane can say
		// which one it is on.
		"isolated_headless",
		"isolated_visible",
		"operator_browser",
		"foreground_gui",
		// App Store Connect is an API door, never a browser door.
		"App Store Connect",
	}
	for _, needle := range required {
		if !strings.Contains(text, needle) {
			t.Errorf("F1 FAIL: canonical body missing required neutral reference %q", needle)
		}
	}
}

// F1 test on a fixture body — proves the renderer-side assertion shape so
// F1 is reproducible in any environment.
func TestFixtureBodyIsHarnessNeutral(t *testing.T) {
	body := string(fixtureBody(t))
	banned := []string{"~/.claude/", "PAI/", "localhost:31337", "mcp__", "Claude Code built-in"}
	for _, needle := range banned {
		if strings.Contains(body, needle) {
			t.Fatalf("fixture body contains %q", needle)
		}
	}
}

// F2: render is deterministic per harness, footer carries body sha256.
func TestRenderSkillAdapterPerHarnessIsDeterministic(t *testing.T) {
	body := fixtureBody(t)
	now := fixedNow()
	all := DefaultSkillAdapters()
	if len(all) < 3 {
		t.Fatalf("default adapters want ≥3 (claude_code, codex, hermes); got %d", len(all))
	}

	for _, h := range all {
		t.Run(h.ID, func(t *testing.T) {
			a, err := RenderSkillAdapter(body, h, now)
			if err != nil {
				t.Fatalf("render %s: %v", h.ID, err)
			}
			b, err := RenderSkillAdapter(body, h, now)
			if err != nil {
				t.Fatalf("re-render %s: %v", h.ID, err)
			}
			if string(a) != string(b) {
				t.Errorf("F2 FAIL: two renders of %s are NOT byte-identical (len=%d vs %d)", h.ID, len(a), len(b))
			}
			// Body must be present verbatim.
			if !strings.Contains(string(a), string(body)) {
				t.Errorf("F2 FAIL: %s adapter does not contain the body verbatim", h.ID)
			}
			// Footer must carry the body sha256.
			want := sha256.Sum256(body)
			wantHex := hex.EncodeToString(want[:])
			if !strings.Contains(string(a), "body_sha256="+wantHex) {
				t.Errorf("F2 FAIL: %s adapter footer missing body sha256 %s", h.ID, wantHex)
			}
			// Each adapter must carry a harness-specific header. Check by
			// name (the slot may be substituted with the private overlay in
			// production, so we don't byte-compare against h.Header verbatim
			// for claude_code). The harness name + "Adapter (rendered)"
			// marker survives substitution.
			wantMarker := strings.Split(h.Header, "Adapter")[0]
			if !strings.Contains(string(a), wantMarker) {
				t.Errorf("F2 FAIL: %s adapter header missing (marker %q not found)", h.ID, wantMarker)
			}
		})
	}

	// Distinct harnesses produce distinct bytes (the header differs).
	a, _ := RenderSkillAdapter(body, all[0], now)
	c, _ := RenderSkillAdapter(body, all[1], now)
	if string(a) == string(c) {
		t.Errorf("F2 FAIL: claude_code and codex adapters are byte-identical (header should differ)")
	}
}

// F2 edge: empty body is a refusal, not a panic.
func TestRenderSkillAdapterRefusesEmptyBody(t *testing.T) {
	if _, err := RenderSkillAdapter(nil, DefaultSkillAdapters()[0], fixedNow()); err == nil {
		t.Fatal("expected refusal on empty body")
	}
}

// F10: the tracked claude_code template is free of private-tool references.
// Private content lives in the overlay file at
// .compound-private/skills/Browser/adapters/claude_code.extras.md (gitignored).
// Render with overlay present → includes private content. Render without →
// omits it. F2 determinism still holds.
func TestClaudeExtrasComeFromPrivateOverlayNotTemplate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// The compiled template (the header string) MUST NOT carry any
	// private-tool reference. Only the generic {{claude_extras}} slot.
	for _, a := range DefaultSkillAdapters() {
		if a.ID != "claude_code" {
			continue
		}
		banned := []string{"localhost", "PAI", "~/.claude"}
		for _, needle := range banned {
			if strings.Contains(a.Header, needle) {
				t.Errorf("F10 FAIL: compiled claude_code template still carries %q — must live in the private overlay only", needle)
			}
		}
		// The slot itself MUST be present so the door knows where to substitute.
		if !strings.Contains(a.Header, "{{claude_extras}}") {
			t.Errorf("F10 FAIL: claude_code template is missing the {{claude_extras}} slot")
		}
	}

	claude := HarnessAdapter{
		ID:        "claude_code",
		SkillDir:  func(s string) string { return filepath.Join(home, ".claude", "skills", s) },
		SkillPath: func(s string) string { return filepath.Join(home, ".claude", "skills", s, "SKILL.md") },
		Header:    DefaultSkillAdapters()[0].Header, // same tracked template
	}
	overlay := []byte("OVERLAY_PRIVATE_CONTENT\n")

	overlayDir := filepath.Join(home, ".agents", "skills", "Browser", "adapters")
	if err := os.MkdirAll(overlayDir, 0o755); err != nil {
		t.Fatalf("mkdir overlay dir: %v", err)
	}
	overlayPath := filepath.Join(overlayDir, "claude_code.extras.md")
	if err := os.WriteFile(overlayPath, overlay, 0o644); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	now := fixedNow()
	body := fixtureBody(t)

	// Render WITH overlay — private content must appear.
	with, err := RenderSkillAdapter(body, claude, now)
	if err != nil {
		t.Fatalf("render with overlay: %v", err)
	}
	if !strings.Contains(string(with), string(overlay)) {
		t.Errorf("F10 FAIL: render with overlay present does not include private content")
	}

	// Remove the overlay — render must omit it (no error, no leftover slot).
	if err := os.Remove(overlayPath); err != nil {
		t.Fatalf("remove overlay: %v", err)
	}
	without, err := RenderSkillAdapter(body, claude, now)
	if err != nil {
		t.Fatalf("render without overlay: %v", err)
	}
	if strings.Contains(string(without), string(overlay)) {
		t.Errorf("F10 FAIL: render without overlay still includes private content")
	}
	if strings.Contains(string(without), "{{claude_extras}}") {
		t.Errorf("F10 FAIL: render without overlay still has an unfilled slot — template must substitute empty")
	}

	// F2 determinism: two renders with the same overlay state are byte-identical.
	if err := os.WriteFile(overlayPath, overlay, 0o644); err != nil {
		t.Fatalf("rewrite overlay: %v", err)
	}
	a, _ := RenderSkillAdapter(body, claude, now)
	b, _ := RenderSkillAdapter(body, claude, now)
	if string(a) != string(b) {
		t.Errorf("F10 determinism: two renders with overlay are NOT byte-identical")
	}

	// And without the overlay — same property.
	if err := os.Remove(overlayPath); err != nil {
		t.Fatalf("remove overlay (2): %v", err)
	}
	c, _ := RenderSkillAdapter(body, claude, now)
	d, _ := RenderSkillAdapter(body, claude, now)
	if string(c) != string(d) {
		t.Errorf("F10 determinism: two renders without overlay are NOT byte-identical")
	}
}

// F3: install (apply) writes a real file; second run reports unchanged;
// hand-edit makes the next dry-run report drift; a symlinked parent gets
// replaced with a real dir and reported.
func TestInstallSkillAdaptersDetectsDrift(t *testing.T) {
	home := t.TempDir()
	// Each adapter reads $HOME; set HOME per-test.
	t.Setenv("HOME", home)

	body := fixtureBody(t)
	now := fixedNow()

	// Build a synthetic claude_code adapter whose SkillDir/SkillPath root at
	// the temp HOME — exercises the same code path as production but with
	// isolated dirs.
	claude := HarnessAdapter{
		ID: "claude_code",
		SkillDir: func(s string) string {
			return filepath.Join(home, ".claude", "skills", s)
		},
		SkillPath: func(s string) string {
			return filepath.Join(home, ".claude", "skills", s, "SKILL.md")
		},
		Header: "CLAUDE_HEADER\n",
	}
	codex := HarnessAdapter{
		ID: "codex",
		SkillDir: func(s string) string {
			return filepath.Join(home, ".codex", "skills", s)
		},
		SkillPath: func(s string) string {
			return filepath.Join(home, ".codex", "skills", s, "SKILL.md")
		},
		Header: "CODEX_HEADER\n",
	}
	hermes := HarnessAdapter{
		ID: "hermes",
		SkillDir: func(s string) string {
			return filepath.Join(home, ".hermes", "skills", s)
		},
		SkillPath: func(s string) string {
			return filepath.Join(home, ".hermes", "skills", s, "SKILL.md")
		},
		Header: "HERMES_HEADER\n",
	}
	adapters := []HarnessAdapter{claude, codex, hermes}

	// 1. dry-run: nothing exists yet → all three plan to write.
	dry, err := InstallSkillAdapters("Browser", adapters, false, now, body)
	if err != nil {
		t.Fatalf("dry-run install: %v", err)
	}
	if len(dry.Adapters) != 3 {
		t.Fatalf("dry report wants 3 adapters, got %d", len(dry.Adapters))
	}
	for _, a := range dry.Adapters {
		if a.Action != ActionWrite {
			t.Errorf("F3 dry: %s action=%s want write", a.Harness, a.Action)
		}
	}

	// 2. apply: writes the files; second run is clean.
	applied, err := InstallSkillAdapters("Browser", adapters, true, now, body)
	if err != nil {
		t.Fatalf("apply install: %v", err)
	}
	for _, a := range applied.Adapters {
		if a.Action != ActionWrite {
			t.Errorf("F3 apply: %s action=%s want write", a.Harness, a.Action)
		}
		got, err := os.ReadFile(a.Path)
		if err != nil {
			t.Fatalf("read installed %s: %v", a.Harness, err)
		}
		// Each installed file is a real file in a real dir (not a symlink).
		fi, err := os.Lstat(a.Path)
		if err != nil {
			t.Fatalf("lstat %s: %v", a.Harness, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("F3: %s is a symlink — should be a real file", a.Harness)
		}
		// Footer sha256 matches.
		want := sha256.Sum256(body)
		wantHex := hex.EncodeToString(want[:])
		if !strings.Contains(string(got), "body_sha256="+wantHex) {
			t.Errorf("F3: %s missing body sha256 footer", a.Harness)
		}
	}

	clean, err := InstallSkillAdapters("Browser", adapters, true, now, body)
	if err != nil {
		t.Fatalf("clean re-run: %v", err)
	}
	for _, a := range clean.Adapters {
		if a.Action != ActionUnchanged {
			t.Errorf("F3 second run: %s action=%s want unchanged", a.Harness, a.Action)
		}
	}

	// 3. drift: hand-edit one file, dry-run reports drift for that file.
	claudePath := claude.SkillPath("Browser")
	if err := os.WriteFile(claudePath, []byte("HAND EDIT\n"), 0o644); err != nil {
		t.Fatalf("hand edit: %v", err)
	}
	drift, err := InstallSkillAdapters("Browser", adapters, false, now, body)
	if err != nil {
		t.Fatalf("drift dry-run: %v", err)
	}
	var sawClaudeDrift bool
	for _, a := range drift.Adapters {
		if a.Harness != "claude_code" {
			if a.Action != ActionUnchanged {
				t.Errorf("F3 drift: non-edited %s action=%s want unchanged", a.Harness, a.Action)
			}
			continue
		}
		if a.Action != ActionUpdate {
			t.Errorf("F3 drift: edited %s action=%s want update", a.Harness, a.Action)
		}
		sawClaudeDrift = true
	}
	if !sawClaudeDrift {
		t.Errorf("F3 FAIL: drift dry-run did not report claude_code as drifted")
	}

	// 4. symlink-into-shared-canon is materialized. Stage a symlink at
	// $HOME/.hermes/skills/Browser that points outside the test home, then
	// re-run apply — the door should remove the symlink, mkdir a real dir,
	// and write the adapter inside. First remove the real dir the prior
	// apply created, so the symlink is the only state.
	if err := os.RemoveAll(filepath.Dir(hermes.SkillPath("Browser"))); err != nil {
		t.Fatalf("remove prior hermes skills dir: %v", err)
	}
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "skills")
	if err := os.Symlink(shimPath, filepath.Dir(hermes.SkillPath("Browser"))); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}
	matReport, err := InstallSkillAdapters("Browser", adapters, true, now, body)
	if err != nil {
		t.Fatalf("materialize run: %v", err)
	}
	for _, a := range matReport.Adapters {
		if a.Harness != "hermes" {
			continue
		}
		if !strings.Contains(a.Detail, "replaced symlink") {
			t.Errorf("F3 symlink: expected 'replaced symlink' detail on hermes; got %q", a.Detail)
		}
	}
	// The path that was a symlink should now be a real dir.
	fi, err := os.Lstat(filepath.Dir(hermes.SkillPath("Browser")))
	if err != nil {
		t.Fatalf("lstat post-materialize: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("F3 FAIL: hermes parent is still a symlink after materialization")
	}
}
