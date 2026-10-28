package harness

// skill_adapter.go — one Browser skill body, one adapter per harness.
//
// The canonical Browser body lives at the operator's skills root under a
// "Browser" subdir (harness-neutral: backend vocabulary, profile registry,
// safety rules). Each enrolled harness gets an adapter rendered from that
// body + a per-harness template embedded below. Adapters are derived, not
// hand-edited; the door detects drift by byte-comparing the installed file
// against a fresh render and re-writes on --apply.
//
// Public surface (this is the contract the CLI and tests bind to):
//   - HarnessAdapter: name + skill-dir resolver + header template.
//   - DefaultSkillAdapters(): the three adapters the packet calls for
//     (claude_code, codex, hermes), in install order.
//   - SkillAdaptersFor(names []string): subset by name (case-insensitive).
//   - RenderSkillAdapter(body []byte, h HarnessAdapter) ([]byte, error):
//     deterministic render — same body + same adapter ⇒ byte-identical bytes.
//   - InstallReport: per-adapter target path, action, sha256 of body.
//   - InstallSkillAdapters(home, skill, adapters, apply, now, bodyOverride)
//     (InstallReport, error): writes to the harness skill dir for each
//     adapter; dry-run by default; replaces a symlink-into-shared-canon with
//     a real dir and reports it.
//
// Privacy: tracked templates are GENERIC — they name tool verbs and CLI
// shapes, never product handles, personal paths, or private URLs. Harness-
// specific private content (Claude MCP tool namespace, voice-notify endpoint,
// Claude-only customizations directory + execution-log path) lives in a
// gitignored overlay at <skills-root>/Browser/adapters/<harness>.extras.md;
// the door substitutes its contents into the {{harness_extras}} slot at
// render time when present. TestRedactGateOnSkillAdapterFile is the public
// redact gate: HIGH=0 MEDIUM=0 on every commit.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/home"
)

// SkillAdapterSchemaVersion tags the rendered adapter footer so a reader can
// detect a stale adapter when the body's content sha256 differs.
const SkillAdapterSchemaVersion = "compound.skill_adapter.v0.1"

// ClaudeExtrasSlot is the marker in the tracked claude_code header that the
// door substitutes at render time with the private overlay contents when
// present. The literal MUST stay generic — any private-tool reference in
// the template is a redact-gate fail (HIGH/MEDIUM internal.url_private).
const ClaudeExtrasSlot = "{{claude_extras}}"

// ClaudeExtrasOverlayPath is the location of the private overlay (gitignored
// under .compound-private). The path resolves to the same dir as the
// canonical body: ~/.agents/skills (which is a symlink into
// .compound-private/skills). Production reads $HOME; tests inject via HOME.
func claudeExtrasOverlayPath(skill string) string {
	return filepath.Join(homeEnv(), ".agents", "skills", skill, "adapters", "claude_code.extras.md")
}

// HarnessAdapter is the per-harness render contract: a stable id, the skill
// directory under that harness's HOME, and the harness-specific header that
// is prepended to the canonical body. Templates are kept here as Go string
// constants so they are diff-reviewable in tracked source.
type HarnessAdapter struct {
	// ID is the harness id (matches compound harness list ids). Stable across
	// renders; used in the footer + for path lookups.
	ID string
	// SkillDir returns the absolute path to the per-harness skill dir for the
	// given skill name, rooted at the user's HOME. Implementation may resolve
	// env or hard-coded layouts; tests inject a temp HOME.
	SkillDir func(skill string) string
	// SkillPath is the rendered file path (SkillDir + skill + "/SKILL.md").
	SkillPath func(skill string) string
	// Header is the per-harness invocation block prepended to the body.
	// Generic — names tool verbs, never product handles.
	Header string
}

// DefaultSkillAdapters returns the three adapters the packet specifies.
// Order matters for the install report — it matches the order the CLI prints.
func DefaultSkillAdapters() []HarnessAdapter {
	return []HarnessAdapter{
		claudeCodeAdapter(),
		codexAdapter(),
		hermesAdapter(),
	}
}

// SkillAdaptersFor returns the subset of default adapters whose ids match
// (case-insensitive, trimmed). Empty/unknown list ⇒ nil.
func SkillAdaptersFor(names []string) []HarnessAdapter {
	if len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		if id := strings.ToLower(strings.TrimSpace(n)); id != "" {
			want[id] = true
		}
	}
	var out []HarnessAdapter
	for _, a := range DefaultSkillAdapters() {
		if want[strings.ToLower(a.ID)] {
			out = append(out, a)
		}
	}
	return out
}

// claudeCodeAdapter: the tracked template carries ONLY a generic slot
// ({{claude_extras}}); every harness-specific MCP-tool-name reference AND
// every Claude-only extra (voice-notify endpoint, Claude customizations
// directory, execution-log path) lives in the private overlay at
// .compound-private/skills/Browser/adapters/claude_code.extras.md
// (gitignored). Render reads the overlay at render time and substitutes its
// contents verbatim for the slot when present. The function comment and the
// header MUST stay free of private-tool references; the public redact gate
// (TestRedactGateOnSkillAdapterFile) proves that on every commit.
//
// Byte-identity guarantee: when the overlay carries the SAME content the
// pre-slice-6 template hard-coded, the rendered adapter is byte-for-byte
// identical to the pre-slice-6 output (same description, same intro, same
// body, same footer — only the SOURCE of the Claude-invocation + Claude-
// extras block moves from tracked to overlay). Re-applying via the door on
// an unchanged overlay preserves the on-disk adapter sha256.
func claudeCodeAdapter() HarnessAdapter {
	header := `---
name: Browser
description: Claude browser-automation adapter. One door for every web task (open, read, click, fill, screenshot, logged-in site): the ` + "`agent-browser`" + ` CLI with an isolated profile, headless by default; canonical body below. Harness-specific invocation block comes from a private operator overlay at render time.
---

# Claude Browser Adapter (rendered, do not edit by hand)

This adapter is rendered by ` + "`compound harness skill-adapter`" + ` from the canonical body at
` + "`~/.agents/skills/Browser/SKILL.md`" + `. If the body changes, re-run the door to refresh.

{{claude_extras}}

---

`
	return HarnessAdapter{
		ID: "claude_code",
		SkillDir: func(skill string) string {
			return filepath.Join(homeEnv(), ".claude", "skills", skill)
		},
		SkillPath: func(skill string) string {
			return filepath.Join(homeEnv(), ".claude", "skills", skill, "SKILL.md")
		},
		Header: header,
	}
}

// codexAdapter: shell-driven agent-browser CLI, the one browser door every
// harness reaches the same way. The bundled Codex browser plugin's enabled
// state is operator config; the template names it only to say it is not the
// door for this skill, and never asserts whether it is on or off.
func codexAdapter() HarnessAdapter {
	header := `---
name: Browser
description: Codex browser-automation adapter. One door: the ` + "`agent-browser`" + ` CLI from the shell, isolated profile, headless by default. Canonical body below.
---

# Codex Browser Adapter (rendered, do not edit by hand)

This adapter is rendered by ` + "`compound harness skill-adapter`" + ` from the canonical body at
` + "`~/.agents/skills/Browser/SKILL.md`" + `. If the body changes, re-run the door to refresh.

## Codex invocation (shell)

Run the ` + "`agent-browser`" + ` CLI through the shell. Pass the same ` + "`--session`" + ` and
` + "`--profile`" + ` on every call: a call without ` + "`--profile`" + ` restarts that session's
browser without the profile.

` + "```" + `bash
DIR="$(jq -r '.profiles[] | select(.id=="<profile-id>") | .user_data_dir' ~/.compound/registries/browser-profiles.json)"
agent-browser --session <task> --profile "$DIR" open https://example.com
agent-browser --session <task> --profile "$DIR" snapshot -i
agent-browser --session <task> --profile "$DIR" click @e2
agent-browser --session <task> --profile "$DIR" fill @e3 "text"
agent-browser --session <task> --profile "$DIR" screenshot /tmp/shot.png
agent-browser --session <task> --profile "$DIR" get url
agent-browser --session <task> --profile "$DIR" close
` + "```" + `

A public page with no login drops ` + "`--profile`" + ` and pins the sites instead:

` + "```" + `bash
agent-browser --session <task> --allowed-domains example.com open https://example.com
` + "```" + `

## Codex bundled browser plugin

The bundled in-app browser plugin (` + "`plugins.\"browser@openai-bundled\"`" + ` in
` + "`~/.codex/config.toml`" + `) drives the Codex app's own browser, mainly for local pages.
Whether it is enabled is operator config, not this adapter's claim. It is not wired to the
profile registry and is not a door for logged-in or unattended work; use the commands above.

---

`
	return HarnessAdapter{
		ID: "codex",
		SkillDir: func(skill string) string {
			return filepath.Join(homeEnv(), ".codex", "skills", skill)
		},
		SkillPath: func(skill string) string {
			return filepath.Join(homeEnv(), ".codex", "skills", skill, "SKILL.md")
		},
		Header: header,
	}
}

// hermesAdapter: hermes reaches the one door (agent-browser CLI) through its
// shell like every other harness. Its native browser_* tools are named only
// for public, unprofiled pages: nothing verifies that they read the profile
// registry, so profile-bound work stays on the CLI.
func hermesAdapter() HarnessAdapter {
	header := `---
name: Browser
description: Hermes browser-automation adapter. One door: the ` + "`agent-browser`" + ` CLI from the shell (isolated profile, headless by default). Hermes-native ` + "`browser_*`" + ` tools only for public pages. Canonical body below.
---

# Hermes Browser Adapter (rendered, do not edit by hand)

This adapter is rendered by ` + "`compound harness skill-adapter`" + ` from the canonical body at
` + "`~/.agents/skills/Browser/SKILL.md`" + `. If the body changes, re-run the door to refresh.

## Hermes invocation

**1. ` + "`agent-browser`" + ` (the door)** — the default, and the only path for logged-in or
profile-bound work. Pass the same ` + "`--session`" + ` and ` + "`--profile`" + ` on every call:

` + "```" + `bash
agent-browser --session <task> --profile "$DIR" open https://example.com
agent-browser --session <task> --profile "$DIR" snapshot -i
agent-browser --session <task> --profile "$DIR" click @e2
agent-browser --session <task> --profile "$DIR" screenshot /tmp/shot.png
agent-browser --session <task> --profile "$DIR" close
` + "```" + `

` + "`$DIR`" + ` is the profile's ` + "`user_data_dir`" + ` from ` + "`~/.compound/registries/browser-profiles.json`" + `.

**2. Hermes-native tools** (` + "`browser_navigate`" + `, ` + "`browser_snapshot`" + `, ` + "`browser_click`" + `,
` + "`browser_type`" + `, ` + "`browser_scroll`" + `, ` + "`browser_press`" + `, ` + "`browser_back`" + `, ` + "`browser_console`" + `,
` + "`browser_vision`" + `, ` + "`browser_get_images`" + `) — public, unprofiled pages only. Nothing
verifies that they read the profile registry, so never use them for a logged-in site. Their
argument names are hermes's own; read the tool schema rather than assuming parity with the CLI.

---

`
	return HarnessAdapter{
		ID: "hermes",
		SkillDir: func(skill string) string {
			return filepath.Join(homeEnv(), ".hermes", "skills", skill)
		},
		SkillPath: func(skill string) string {
			return filepath.Join(homeEnv(), ".hermes", "skills", skill, "SKILL.md")
		},
		Header: header,
	}
}

// homeEnv is the indirection point for $HOME — overridable in tests by
// setting the env var HOME to a temp dir. Production reads the real env.
func homeEnv() string {
	return os.Getenv("HOME")
}

// RenderSkillAdapter deterministically combines header + body + footer.
// Same (body, adapter) ⇒ byte-identical output (no timestamp in the footer
// — that would make every run "drift" against its prior self). The footer
// carries the body's sha256 so a reader can verify the adapter matches the
// current body. now is still accepted for callers that want a one-shot
// timestamped render; it does NOT appear in the output.
//
// The {{claude_extras}} slot in the claude_code template is substituted at
// render time with the contents of the private overlay
// (claude_code.extras.md) when present; absent the slot expands to empty.
// No private-tool reference may ever appear in the tracked template —
// overlays carry the private content.
func RenderSkillAdapter(body []byte, h HarnessAdapter, now time.Time) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("render skill adapter %q: body is empty", h.ID)
	}
	if strings.TrimSpace(h.ID) == "" {
		return nil, fmt.Errorf("render skill adapter: id is empty")
	}
	if now.IsZero() {
		now = time.Now()
	}
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])

	header := h.Header
	// Substitute the claude_extras slot with the private overlay when present.
	// The overlay is substituted VERBATIM (no extra prefix) so the rendered
	// output is byte-for-byte identical to the pre-overlay-template version
	// when the same overlay content is present.
	if strings.Contains(header, ClaudeExtrasSlot) {
		overlay, err := os.ReadFile(claudeExtrasOverlayPath("Browser"))
		if err == nil {
			header = strings.Replace(header, ClaudeExtrasSlot, string(overlay), 1)
		} else if os.IsNotExist(err) {
			header = strings.Replace(header, ClaudeExtrasSlot, "", 1)
		} else {
			return nil, fmt.Errorf("read claude_extras overlay: %w", err)
		}
	}

	var b strings.Builder
	b.WriteString(header)
	// Body verbatim — no rewriting, no template expansion. The whole point
	// of the door is that the body is the canonical owner; the adapter is
	// just a thin harness-syntax wrapper.
	b.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		b.WriteByte('\n')
	}
	// Footer: schema version + harness id + body sha256. NO timestamp — a
	// timestamp in the footer would make every re-render "drift" against
	// its prior self, defeating the unchanged-detection contract.
	fmt.Fprintf(&b, "\n<!-- %s harness=%s body_sha256=%s -->\n",
		SkillAdapterSchemaVersion, h.ID, bodyHash)
	return []byte(b.String()), nil
}

// InstallAction is one of: write (fresh install or replace symlink with real
// dir), update (content changed), unchanged (byte-identical), skip (target
// path is outside the user's HOME).
type InstallAction string

const (
	ActionWrite     InstallAction = "write"
	ActionUpdate    InstallAction = "update"
	ActionUnchanged InstallAction = "unchanged"
	ActionSkip      InstallAction = "skip"
)

// AdapterInstall is one adapter's outcome.
type AdapterInstall struct {
	Harness    string        `json:"harness"`
	Path       string        `json:"path"`
	Action     InstallAction `json:"action"`
	BodySHA256 string        `json:"body_sha256"`
	Detail     string        `json:"detail,omitempty"`
}

// InstallReport summarizes one install run.
type InstallReport struct {
	Skill    string           `json:"skill"`
	Apply    bool             `json:"apply"`
	Adapters []AdapterInstall `json:"adapters"`
}

// InstallSkillAdapters renders each adapter and (when apply=true) writes it
// to the harness's skill dir. dry-run by default; the report records what
// WOULD happen. The home argument is accepted for testability but currently
// the path resolution reads $HOME (each adapter's SkillPath is HOME-rooted).
//
// bodyOverride is for tests — when non-empty, the renderer reads from a
// fixture path instead of the canonical ~/.agents/skills/Browser/SKILL.md.
// Production callers pass nil.
//
// Symlink policy: if a target like ~/.codex/skills/Browser/SKILL.md is a
// symlink into a shared-canon dir (a real entry in the spine-owned symlink
// layer), we replace the symlink with a real file in a real dir, record the
// replacement in the report's Detail, and continue. codex has no Browser
// entry today; claude has a symlink-shaped dir under .claude/skills/Browser —
// that is detected and replaced by a real dir here.
func InstallSkillAdapters(skill string, adapters []HarnessAdapter, apply bool, now time.Time, bodyOverride []byte) (InstallReport, error) {
	if strings.TrimSpace(skill) == "" {
		return InstallReport{}, fmt.Errorf("install skill adapters: skill name is empty")
	}
	if skill != "Browser" {
		return InstallReport{}, fmt.Errorf("install skill adapters: unsupported skill %q; this renderer supports only Browser", skill)
	}
	if now.IsZero() {
		now = time.Now()
	}
	body := bodyOverride
	if body == nil {
		body = mustReadBody()
	}
	rep := InstallReport{Skill: skill, Apply: apply}
	for _, h := range adapters {
		entry := AdapterInstall{Harness: h.ID}
		path := h.SkillPath(skill)
		entry.Path = path

		rendered, err := RenderSkillAdapter(body, h, now)
		if err != nil {
			return rep, fmt.Errorf("render %s: %w", h.ID, err)
		}
		sum := sha256.Sum256(rendered)
		entry.BodySHA256 = hex.EncodeToString(sum[:])

		// If the target's parent is a symlink into shared canon, replace with
		// a real dir before writing the file. The packet calls this out: codex
		// has no Browser entry (so writing creates a fresh dir); claude has
		// one that may be a symlink (so we materialize a real dir).
		parent := filepath.Dir(path)
		replaced, repDetail, err := materializeSymlinkedParent(parent, apply)
		if err != nil {
			entry.Action = ActionSkip
			entry.Detail = err.Error()
			rep.Adapters = append(rep.Adapters, entry)
			continue
		}
		if replaced {
			entry.Detail = repDetail
		}

		existing, err := os.ReadFile(path)
		switch {
		case err != nil && !os.IsNotExist(err):
			entry.Action = ActionSkip
			entry.Detail = err.Error()
			rep.Adapters = append(rep.Adapters, entry)
			continue
		case err != nil && os.IsNotExist(err):
			entry.Action = ActionWrite
		case string(existing) == string(rendered):
			entry.Action = ActionUnchanged
		default:
			entry.Action = ActionUpdate
		}

		if apply {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				entry.Action = ActionSkip
				entry.Detail = err.Error()
				rep.Adapters = append(rep.Adapters, entry)
				continue
			}
			if err := os.WriteFile(path, rendered, 0o644); err != nil {
				entry.Action = ActionSkip
				entry.Detail = err.Error()
				rep.Adapters = append(rep.Adapters, entry)
				continue
			}
		}
		rep.Adapters = append(rep.Adapters, entry)
	}
	// Stable order in the report.
	sort.SliceStable(rep.Adapters, func(i, j int) bool {
		return rep.Adapters[i].Harness < rep.Adapters[j].Harness
	})
	return rep, nil
}

// materializeSymlinkedParent ensures the parent dir of the target is a real
// directory under the user's HOME. If any ancestor of the parent (within
// HOME) is a symlink into a shared canon dir (the spine-owned symlink
// layer), removing that symlink and mkdir'ing a real dir at the same path
// is what lets the door OWN the adapter file content. Stops at HOME so we
// never touch system dirs (/var, /tmp) — those can be symlinks too and we
// must not rewrite them. Returns (replaced, detail, err).
func materializeSymlinkedParent(parent string, apply bool) (bool, string, error) {
	home := homeEnv()
	if strings.TrimSpace(home) == "" {
		return false, "", nil
	}
	// Walk up from `parent` to HOME (inclusive). HOME itself is the floor;
	// do not try to rewrite anything at or above it.
	abs, err := filepath.Abs(parent)
	if err != nil {
		return false, "", err
	}
	homeAbs, err := filepath.Abs(home)
	if err != nil {
		return false, "", err
	}
	for cur := abs; ; cur = filepath.Dir(cur) {
		// Stop when we have walked ABOVE HOME (cur shorter than HOME OR cur
		// not a path-prefix match of HOME). HOME itself can be a symlink
		// (e.g. on macOS /var -> /private/var) — do not touch it.
		if cur == homeAbs || !strings.HasPrefix(cur, homeAbs+string(filepath.Separator)) {
			return false, "", nil
		}
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// Missing path is fine — the caller will MkdirAll.
				return false, "", nil
			}
			return false, "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(cur)
			detail := fmt.Sprintf("replaced symlink %s -> %s with real dir", cur, target)
			if apply {
				if err := os.Remove(cur); err != nil {
					return false, "", fmt.Errorf("remove symlink %s: %w", cur, err)
				}
				if err := os.MkdirAll(cur, 0o755); err != nil {
					return false, "", fmt.Errorf("mkdir %s: %w", cur, err)
				}
			}
			return true, detail, nil
		}
		if !fi.IsDir() {
			return false, "", fmt.Errorf("parent %s is not a directory (mode=%v)", cur, fi.Mode())
		}
	}
}

// mustReadBody reads the canonical body from the standard location. The body
// path is HOME-rooted because ~/.agents/skills is a symlink that the spine
// owns; we resolve it once per call (cheap).
func mustReadBody() []byte {
	home := homeEnv()
	body, err := os.ReadFile(filepath.Join(home, ".agents", "skills", "Browser", "SKILL.md"))
	if err != nil {
		// Tests inject a HOME that points at a temp dir; if the body is
		// missing the caller will get a render error from the empty-body
		// check above. Production reads the real body.
		return []byte{}
	}
	return body
}

// FormatInstallReport renders an InstallReport as the compact CLI text
// shape. Used by the CLI dispatcher; --json path uses json.Marshal directly.
func FormatInstallReport(rep InstallReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# harness skill-adapter skill=%s apply=%t\n", rep.Skill, rep.Apply)
	for _, a := range rep.Adapters {
		fmt.Fprintf(&b, "- harness=%s action=%s path=%s\n", a.Harness, a.Action, a.Path)
		if a.Detail != "" {
			fmt.Fprintf(&b, "  detail: %s\n", a.Detail)
		}
	}
	return b.String()
}

// RunSkillAdapterCLI parses flags + dispatches + prints. It exists so the
// cmd/compound dispatcher stays at a tiny delta — the work lives here.
func RunSkillAdapterCLI(args []string, apply, asJSON bool, now time.Time) error {
	skill := strings.TrimSpace(skillAdapterFlag(args, "--skill"))
	if skill == "" {
		return fmt.Errorf("usage: compound harness skill-adapter --skill Browser [--harness H] [--apply] [--json]")
	}
	adapters := SkillAdaptersFor(strings.Split(skillAdapterFlag(args, "--harness"), ","))
	if len(adapters) == 0 {
		adapters = DefaultSkillAdapters()
	}
	rep, err := InstallSkillAdapters(skill, adapters, apply, now, nil)
	if err != nil {
		return err
	}
	if asJSON {
		b, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	fmt.Print(FormatInstallReport(rep))
	return nil
}

// RunBrowserOwnerSubcommand is the SINGLE entry point cmd/compound/main.go
// calls for all browser-owner subcommands. Returning true means the
// dispatcher handled it; false means the caller should keep searching its
// own tables. The name is exported via internal/harness so cmd/compound keeps
// only the if-and-return case line — no flag parsing, no imports, no logic.
//
// Home-flag resolution uses $COMPOUND_HOME (via home.Resolve); passing an
// empty string defers to the door's env-driven default.
func RunBrowserOwnerSubcommand(target string, args []string) bool {
	switch target {
	case "skill-adapter":
		now := time.Now()
		return runBrowserOwnerErr(RunSkillAdapterCLI(args, hasFlag(args, "--apply"), hasFlag(args, "--json"), now))
	case "browser-profiles":
		// 2026-09-15 diagnostic-integrity contract: doctor is READ-ONLY;
		// `seed` is the explicit join/migration writer (preimage preserved,
		// sha256 CAS). No auto-seed anywhere.
		if len(args) == 0 {
			fmt.Fprintln(os.Stderr, "usage: compound harness browser-profiles doctor [--json] [--home P]")
			fmt.Fprintln(os.Stderr, "       compound harness browser-profiles seed [--json] [--home P]   # explicit join of social-accounts into the registry")
			return true
		}
		homePath, err := browserOwnerHome(flagValue(args[1:], "--home"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return true
		}
		switch args[0] {
		case "doctor":
			return runBrowserOwnerErr(RunBrowserProfileDoctorCLI(homePath, hasFlag(args[1:], "--json")))
		case "seed":
			return runBrowserOwnerErr(RunBrowserProfileSeedCLI(homePath, hasFlag(args[1:], "--json"), time.Now()))
		default:
			fmt.Fprintln(os.Stderr, "usage: compound harness browser-profiles doctor [--json] [--home P]")
			return true
		}
	}
	return false
}

// runBrowserOwnerErr prints an error to stderr and returns true (handled)
// uniformly so RunBrowserOwnerSubcommand can be a single boolean expression.
func runBrowserOwnerErr(err error) bool {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return true
}

// browserOwnerHome resolves optional --home through Compound's canonical
// flag → environment → global-default precedence.
func browserOwnerHome(flag string) (string, error) {
	return home.Resolve(flag)
}

// minimal flag-parsing helper mirrors cmd/compound's flagValue without
// pulling main.go's flag layer across the package boundary.
func flagValue(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], name+"=") {
			return strings.TrimPrefix(args[i], name+"=")
		}
	}
	return ""
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// skillAdapterFlag parses --flag value out of an args slice. Tiny copy of
// cmd/compound's flagValue so the package owns its CLI.
func skillAdapterFlag(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(args[i], name+"=") {
			return strings.TrimPrefix(args[i], name+"=")
		}
	}
	return ""
}
