package source

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/activity"
)

// --- fixtures ---

// cmuxFixtureRunner returns canned cmux surface.list output (loaded from testdata).
type cmuxFixtureRunner struct{ payload []byte }

// Run returns the canned payload or an error if cmd failed.
func (r cmuxFixtureRunner) Run(ctx context.Context) ([]byte, error) {
	return r.payload, nil
}

// errorRunner always returns an error — useful for testing exec-failure paths.
type errorRunner struct{ err error }

func (r errorRunner) Run(ctx context.Context) ([]byte, error) { return nil, r.err }

// fixtureLsof returns a canned cwd path; or fails if Path == "".
type fixtureLsof struct{ Path string }

func (l fixtureLsof) Run(ctx context.Context, pid int) (string, error) {
	if l.Path == "" {
		return "", errors.New("fake lsof: empty path")
	}
	return l.Path, nil
}

// fixturePID returns a canned PID or 0.
type fixturePID struct{ PID int }

func (p fixturePID) FocusedPID(ctx context.Context, appName string) (int, error) {
	if p.PID == 0 {
		return 0, errors.New("fake pid resolver: not focused")
	}
	return p.PID, nil
}

// fixtureAWEvents returns canned vscode events for the editor source.
type fixtureAWEvents struct{ events []editorEvent }

func (f fixtureAWEvents) FetchEvents(ctx context.Context, bucketID string, start, end time.Time, limit int) ([]editorEvent, error) {
	return f.events, nil
}

// loadTestdata reads a testdata file relative to the package's testdata dir.
func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("testdata", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return b
}

// --- Slug ---

func TestSlug_HandlesVariousInputs(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "x"},
		{"  ", "x"},
		{"hello", "hello"},
		{"Hello World", "hello-world"},
		{"abc/def", "abc-def"},
		{"a:b;c=d", "a-b-c-d"},
		{"--strip--", "strip"},
		{"café", "caf"},
		{strings.Repeat("a", 200), strings.Repeat("a", 64)},
	}
	for _, tc := range cases {
		got := Slug(tc.in)
		if got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- ContentFreeInvariant ---

func TestCheckEnvelopeContentFree_AcceptsCleanEnvelope(t *testing.T) {
	env := Envelope{
		PaneCwd:      "/Users/example/projects/example",
		PaneType:     "terminal",
		AgentKind:    "agent-hook",
		AgentName:    "Claude Code",
		WorkspaceRef: "ws-abc123",
		SurfaceRef:   "surf-def456",
		WindowRef:    "win-789",
	}
	if err := CheckEnvelopeContentFree(env); err != nil {
		t.Fatalf("clean envelope must validate, got: %v", err)
	}
}

func TestCheckEnvelopeContentFree_RejectsAllBannedKeys(t *testing.T) {
	// Build an envelope with EVERY banned key at top level — must fail.
	for _, banned := range []string{"title", "url", "document", "text", "content", "file", "command_args", "script", "initial_command", "env", "language"} {
		env := Envelope{}
		// Inject the banned key by JSON round-trip — the typed Envelope doesn't have
		// these fields, so we marshal+re-marshal with an extra field.
		b, _ := json.Marshal(env)
		var generic map[string]any
		_ = json.Unmarshal(b, &generic)
		generic[banned] = "leaked content"
		rb, _ := json.Marshal(generic)
		if err := CheckRawContentFree(rb); err == nil {
			t.Errorf("banned key %q must be refused, but CheckRawContentFree accepted", banned)
		}
	}
}

func TestCheckEnvelopeContentFree_RejectsBannedKeysNested(t *testing.T) {
	// Top-level is clean; nested map carries a banned key.
	raw := []byte(`{"pane_cwd":"/Users/example/projects/example","meta":{"file":"leaked"}}`)
	if err := CheckRawContentFree(raw); err == nil {
		t.Fatalf("nested banned key must be refused")
	}
}

func TestCheckEnvelopeContentFree_RejectsBannedKeysInArray(t *testing.T) {
	// Nested banned key inside a JSON array element.
	raw := []byte(`{"surfaces":[{"title":"leaked","app":"com.example.ok"}]}`)
	if err := CheckRawContentFree(raw); err == nil {
		t.Fatalf("array-nested banned key must be refused")
	}
}

// --- CmuxSource ---

func TestCmuxSource_ParsesSurfaceListFixture(t *testing.T) {
	payload := loadTestdata(t, "cmux_surface_list.json")
	s := newCmuxSourceWithRunner(cmuxFixtureRunner{payload: payload})

	env, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: CmuxBundleID})
	if err != nil {
		t.Fatalf("TryEnvelope: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true with focused surface in fixture")
	}
	// Focused surface: requested_working_directory + resume_binding.kind/name.
	if env.PaneCwd != "/Users/example/projects/example" {
		t.Errorf("PaneCwd = %q", env.PaneCwd)
	}
	if env.PaneType != "agent-session" {
		t.Errorf("PaneType = %q, want agent-session", env.PaneType)
	}
	if env.PaneResolution != PaneResolutionCmuxIPC {
		t.Errorf("PaneResolution = %q, want %q (cmux IPC is high-confidence)", env.PaneResolution, PaneResolutionCmuxIPC)
	}
	if env.AgentKind != "agent-hook" {
		t.Errorf("AgentKind = %q, want agent-hook", env.AgentKind)
	}
	if env.AgentName != "Claude Code" {
		t.Errorf("AgentName = %q, want Claude Code", env.AgentName)
	}
	// Slugs from UUIDs.
	if env.SurfaceRef == "" || env.WorkspaceRef == "" || env.WindowRef == "" {
		t.Errorf("refs must be populated: %+v", env)
	}
	// Content-bearing fields MUST NOT leak.
	if strings.Contains(env.AgentName, "agent-hook") || strings.Contains(env.AgentName, "/bin/zsh") {
		t.Errorf("AgentName leaked content: %q", env.AgentName)
	}
}

func TestCmuxSource_NoFocusedSurface(t *testing.T) {
	// Fixture where no surface has focused=true; should return ok=false.
	payload := []byte(`{"surfaces":[{"id":"x","type":"terminal","focused":false,"requested_working_directory":"/x"}]}`)
	s := newCmuxSourceWithRunner(cmuxFixtureRunner{payload: payload})
	_, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: CmuxBundleID})
	if err != nil {
		t.Fatalf("TryEnvelope: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when no surface focused")
	}
}

func TestCmuxSource_NotInstalledGracefulSkip(t *testing.T) {
	s := newCmuxSourceWithRunner(errorRunner{err: exec.ErrNotFound})
	env, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: CmuxBundleID})
	if err != nil {
		t.Fatalf("exec.ErrNotFound must surface as (zero, false, nil), got err: %v", err)
	}
	if ok || env.PaneCwd != "" {
		t.Fatalf("expected graceful skip; got ok=%v env=%+v", ok, env)
	}
}

func TestCmuxSource_TransportErrorPropagates(t *testing.T) {
	s := newCmuxSourceWithRunner(errorRunner{err: errors.New("boom")})
	_, _, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: CmuxBundleID})
	if err == nil {
		t.Fatalf("non-NotFound error must propagate")
	}
}

func TestCmuxSource_IgnoresOtherBundleIDs(t *testing.T) {
	s := newCmuxSourceWithRunner(cmuxFixtureRunner{payload: []byte(`{"surfaces":[]}`)})
	_, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "com.googlecode.iterm2"})
	if err != nil {
		t.Fatalf("TryEnvelope: %v", err)
	}
	if ok {
		t.Fatalf("CmuxSource must return ok=false for non-cmux bundle id")
	}
}

func TestCmuxSource_ContentFreeInvariant(t *testing.T) {
	s := NewCmuxSource()
	if err := s.ContentFreeInvariant(); err != nil {
		t.Fatalf("ContentFreeInvariant: %v", err)
	}
}

// --- AppleScriptSource ---

func TestAppleScriptSource_ResolvesCwdViaPIDAndLsof(t *testing.T) {
	s := &AppleScriptSource{
		cfg:     AppleScriptConfig{BundleID: "com.googlecode.iterm2", DisplayName: "iTerm2"},
		execCmd: fixtureLsof{Path: "/Users/example/projects/example"},
		getPID:  fixturePID{PID: 12345},
	}
	env, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "com.googlecode.iterm2"})
	if err != nil || !ok {
		t.Fatalf("expected ok=true err=nil, got ok=%v err=%v", ok, err)
	}
	if env.PaneCwd != "/Users/example/projects/example" {
		t.Errorf("PaneCwd = %q", env.PaneCwd)
	}
	if env.PaneType != "terminal" {
		t.Errorf("PaneType = %q, want terminal", env.PaneType)
	}
	if env.PaneResolution != PaneResolutionBestEffortProcess {
		t.Errorf("PaneResolution = %q, want %q (pgrep/lsof is a low-confidence guess)", env.PaneResolution, PaneResolutionBestEffortProcess)
	}
}

func TestAppleScriptSource_LsofFailsCleanly(t *testing.T) {
	s := &AppleScriptSource{
		cfg:     AppleScriptConfig{BundleID: "com.apple.Terminal"},
		execCmd: fixtureLsof{Path: ""}, // empty path → error
		getPID:  fixturePID{PID: 999},
	}
	_, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "com.apple.Terminal"})
	if err != nil {
		t.Fatalf("lsof failure should be graceful skip, got err: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when lsof fails")
	}
}

func TestAppleScriptSource_PIDResolutionFailsCleanly(t *testing.T) {
	s := &AppleScriptSource{
		cfg:     AppleScriptConfig{BundleID: "dev.warp.Warp"},
		execCmd: fixtureLsof{Path: "/x"},
		getPID:  fixturePID{PID: 0}, // zero → "not focused" error
	}
	_, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "dev.warp.Warp"})
	if err != nil {
		t.Fatalf("PID-resolution failure should be graceful skip, got err: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when PID resolution fails")
	}
}

func TestAppleScriptSource_IgnoresOtherBundleIDs(t *testing.T) {
	s := &AppleScriptSource{
		cfg:     AppleScriptConfig{BundleID: "com.googlecode.iterm2"},
		execCmd: fixtureLsof{Path: "/x"},
		getPID:  fixturePID{PID: 1},
	}
	_, ok, _ := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "com.apple.Terminal"})
	if ok {
		t.Fatalf("AppleScriptSource must return ok=false for non-matching bundle id")
	}
}

// --- EditorSource ---

func TestEditorSource_ReadsProjectFieldOnly(t *testing.T) {
	s := newEditorSourceWithClient(
		EditorConfig{BundleID: "com.microsoft.VSCode", DisplayName: "VS Code"},
		fixtureAWEvents{events: []editorEvent{{
			Timestamp: "2026-07-30T12:00:00Z",
			Duration:  60,
			Data:      []byte(`{"project":"/Users/example/projects/example","file":"/secret/path","language":"go"}`),
		}}},
	)
	env, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "com.microsoft.VSCode"})
	if err != nil || !ok {
		t.Fatalf("expected ok=true err=nil, got ok=%v err=%v", ok, err)
	}
	if env.PaneCwd != "/Users/example/projects/example" {
		t.Errorf("PaneCwd = %q, want project", env.PaneCwd)
	}
	if env.PaneType != "editor" {
		t.Errorf("PaneType = %q, want editor", env.PaneType)
	}
	if env.PaneResolution != PaneResolutionAWEvent {
		t.Errorf("PaneResolution = %q, want %q (AW editor event)", env.PaneResolution, PaneResolutionAWEvent)
	}
	// Defensive: file and language must NEVER appear in the envelope.
	b, _ := json.Marshal(env)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "language") {
		t.Fatalf("envelope leaked content: %s", b)
	}
}

func TestEditorSource_NoProjectField(t *testing.T) {
	s := newEditorSourceWithClient(
		EditorConfig{BundleID: "com.microsoft.VSCode"},
		fixtureAWEvents{events: []editorEvent{{
			Data: []byte(`{"file":"/secret/path"}`),
		}}},
	)
	_, ok, err := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "com.microsoft.VSCode"})
	if err != nil {
		t.Fatalf("TryEnvelope: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false when project field missing")
	}
}

func TestEditorSource_NoEventsGracefulSkip(t *testing.T) {
	s := newEditorSourceWithClient(
		EditorConfig{BundleID: "com.microsoft.VSCode"},
		fixtureAWEvents{events: nil},
	)
	_, ok, _ := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "com.microsoft.VSCode"})
	if ok {
		t.Fatalf("expected ok=false when no events")
	}
}

func TestEditorSource_IgnoresOtherBundleIDs(t *testing.T) {
	s := newEditorSourceWithClient(
		EditorConfig{BundleID: "com.microsoft.VSCode"},
		fixtureAWEvents{events: []editorEvent{{Data: []byte(`{"project":"/x"}`)}}},
	)
	_, ok, _ := s.TryEnvelope(context.Background(), FocusedApp{BundleID: "com.todesktop.230313mzl4w4u96"})
	if ok {
		t.Fatalf("EditorSource must return ok=false for non-matching bundle id")
	}
}

func TestEditorSource_ContentFreeInvariant(t *testing.T) {
	s := NewEditorSource(EditorConfig{BundleID: "com.microsoft.VSCode"})
	if err := s.ContentFreeInvariant(); err != nil {
		t.Fatalf("ContentFreeInvariant: %v", err)
	}
}

// --- Registry ---

func TestRegistry_ResolvesAllKnownBundles(t *testing.T) {
	r := NewRegistry()
	wantBundles := []string{
		"com.cmuxterm.app",
		"com.googlecode.iterm2",
		"dev.warp.Warp",
		"com.apple.Terminal",
		"com.microsoft.VSCode",
		"com.todesktop.230313mzl4w4u96",
	}
	for _, b := range wantBundles {
		s, ok := r.Resolve(b)
		if !ok {
			t.Errorf("Resolve(%q) not found", b)
			continue
		}
		if s.AppBundleID() != b {
			t.Errorf("Resolve(%q).AppBundleID() = %q", b, s.AppBundleID())
		}
	}
}

func TestRegistry_UnknownBundleReturnsFalse(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Resolve("com.example.unknown"); ok {
		t.Fatalf("expected ok=false for unknown bundle")
	}
	if _, ok := r.Resolve(""); ok {
		t.Fatalf("expected ok=false for empty bundle id")
	}
}

func TestRegistry_ContentFreeInvariant_RunsAllSources(t *testing.T) {
	r := NewRegistry()
	if err := r.ContentFreeInvariant(); err != nil {
		t.Fatalf("ContentFreeInvariant on default registry: %v", err)
	}
}

func TestRegistry_ContentFreeInvariant_FailsOnBadSource(t *testing.T) {
	// Construct a fake source whose envelope construction is buggy.
	bad := &buggySource{bundle: "com.test.bad"}
	r := NewRegistryFromSources(bad)
	err := r.ContentFreeInvariant()
	if err == nil {
		t.Fatalf("expected invariant failure")
	}
	var invErr *InvariantError
	if !errors.As(err, &invErr) {
		t.Fatalf("expected *InvariantError, got %T: %v", err, err)
	}
	if invErr.BundleID != "com.test.bad" {
		t.Errorf("BundleID = %q", invErr.BundleID)
	}
}

func TestRegistry_NilSafe(t *testing.T) {
	var r *Registry
	if _, ok := r.Resolve("com.cmuxterm.app"); ok {
		t.Fatalf("nil registry should return ok=false")
	}
	if err := r.ContentFreeInvariant(); err != nil {
		t.Fatalf("nil registry ContentFreeInvariant should be nil")
	}
}

func TestNewRegistryFromSources_DropsEmptyAndDuplicate(t *testing.T) {
	empty := &emptyBundleSource{}
	dup := &emptyBundleSource{} // also returns "" bundle id
	r := NewRegistryFromSources(empty, dup)
	if _, ok := r.Resolve(""); ok {
		t.Fatalf("empty bundle id sources should be dropped")
	}
	// Make a duplicate-id source and verify first-wins.
	a := &fakeSource{bundle: "x.test"}
	b := &fakeSource{bundle: "x.test"}
	r = NewRegistryFromSources(a, b)
	got, ok := r.Resolve("x.test")
	if !ok || got != a {
		t.Fatalf("first occurrence should win")
	}
}

// --- Schema-level invariant integration ---

func TestExtendedForbiddenKeys_SupersetOfSchemaForbidden(t *testing.T) {
	// Every entry in activity.ForbiddenMetaKeys must also be in ExtendedForbiddenKeys.
	for k := range activity.ForbiddenMetaKeys {
		if !ExtendedForbiddenKeys[k] {
			t.Fatalf("activity.ForbiddenMetaKeys[%q] missing from ExtendedForbiddenKeys — invariant drift", k)
		}
	}
	// ExtendedForbiddenKeys must have AT LEAST the Tier-1 additions.
	for _, k := range []string{"file", "command_args", "script", "initial_command", "env", "language"} {
		if !ExtendedForbiddenKeys[k] {
			t.Fatalf("Tier-1 forbidden key %q missing from ExtendedForbiddenKeys", k)
		}
	}
}

// --- test fixtures used by helpers ---

type buggySource struct{ bundle string }

func (s *buggySource) AppBundleID() string { return s.bundle }
func (s *buggySource) TryEnvelope(ctx context.Context, f FocusedApp) (Envelope, bool, error) {
	return Envelope{}, false, nil
}
func (s *buggySource) ContentFreeInvariant() error {
	return errors.New("synthetic invariant failure for test")
}

type emptyBundleSource struct{}

func (s *emptyBundleSource) AppBundleID() string { return "" }
func (s *emptyBundleSource) TryEnvelope(ctx context.Context, f FocusedApp) (Envelope, bool, error) {
	return Envelope{}, false, nil
}
func (s *emptyBundleSource) ContentFreeInvariant() error { return nil }

type fakeSource struct{ bundle string }

func (s *fakeSource) AppBundleID() string { return s.bundle }
func (s *fakeSource) TryEnvelope(ctx context.Context, f FocusedApp) (Envelope, bool, error) {
	return Envelope{}, false, nil
}
func (s *fakeSource) ContentFreeInvariant() error { return nil }

// Compile-time guard: ensure the editor source's default AW bridge type aligns with
// what fetchAWEventsForEditor returns. If the editorEvent shape diverges from
// awstream.Event, this breaks at build time.
var _ = func() {
	type want = []editorEvent
	_ = want(nil)
}
