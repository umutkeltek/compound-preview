package cmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/terminal"
)

// stubRunner is a controllable Runner for tests. Each test sets prefixes to
// program responses; the production shell-out is never invoked. recorder
// (optional) captures every invocation's args for assertion.
//
// Responses are matched by PREFIX in priority order:
//   - exact join of args
//   - prefix join of args
//   - default (empty key)
//
// This lets tests respond to "rpc feed.push --json <payload>" without
// needing to know the payload's exact bytes.
type stubRunner struct {
	prefixes map[string]response
	exact    map[string]response
	recorder *[][]string
}

type response struct {
	out []byte
	err error
}

func (s *stubRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	if s.recorder != nil {
		*s.recorder = append(*s.recorder, append([]string(nil), args...))
	}
	key := strings.Join(args, " ")
	if r, ok := s.exact[key]; ok {
		return r.out, r.err
	}
	// Prefix match — longest first.
	var bestKey string
	var best response
	for p, r := range s.prefixes {
		if strings.HasPrefix(key, p) && len(p) > len(bestKey) {
			bestKey = p
			best = r
		}
	}
	if bestKey != "" {
		return best.out, best.err
	}
	return nil, fmt.Errorf("stubRunner: unexpected args %v", args)
}

func TestDoctor_NotInstalled(t *testing.T) {
	r := &stubRunner{
		exact: map[string]response{
			"version": {err: &exec.Error{Name: "cmux", Err: exec.ErrNotFound}},
		},
	}
	t1 := newWithRunner(r)
	rep, err := t1.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.Installed {
		t.Fatal("Installed=true; want false when cmux not on PATH")
	}
	if rep.IPCReachable {
		t.Fatal("IPCReachable=true; want false")
	}
	if len(rep.Issues) == 0 || !strings.Contains(rep.Issues[0], "PATH") {
		t.Fatalf("Issues[0] = %v; want one mentioning PATH", rep.Issues)
	}
}

func TestDoctor_InstalledAndIPCReachable(t *testing.T) {
	identifyJSON := []byte(`{"workspace_id":"w1","surface_id":"s1","type":"terminal"}`)
	r := &stubRunner{
		exact: map[string]response{
			"version":         {out: []byte("cmux 0.64.20")},
			"identify --json": {out: identifyJSON},
		},
	}
	t1 := newWithRunner(r)
	rep, err := t1.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !rep.Installed {
		t.Fatal("Installed=false; want true")
	}
	if !rep.IPCReachable {
		t.Fatal("IPCReachable=false; want true (cmux returned valid JSON)")
	}
}

func TestDoctor_IPCUnreachable(t *testing.T) {
	r := &stubRunner{
		exact: map[string]response{
			"version":         {out: []byte("cmux 0.64.20")},
			"identify --json": {out: []byte("not-json"), err: nil},
		},
	}
	t1 := newWithRunner(r)
	rep, err := t1.Doctor(context.Background())
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !rep.Installed {
		t.Fatal("Installed=false; want true")
	}
	if rep.IPCReachable {
		t.Fatal("IPCReachable=true; want false (non-JSON response)")
	}
}

func TestNameAndBundleID(t *testing.T) {
	t1 := New()
	if t1.Name() != Name {
		t.Fatalf("Name() = %q; want %q", t1.Name(), Name)
	}
	if t1.BundleID() != BundleID {
		t.Fatalf("BundleID() = %q; want %q", t1.BundleID(), BundleID)
	}
}

func TestHooks_AreDocumented(t *testing.T) {
	t1 := New()
	hs := t1.Hooks()
	if hs.Install == "" {
		t.Fatal("Hooks.Install empty; want documented snippet")
	}
	if !strings.Contains(hs.Install, "idempotent") && !strings.Contains(hs.Install, "Idempotent") {
		t.Fatalf("Install snippet should mention idempotence; got %q", hs.Install)
	}
	if len(hs.Paths) != 3 {
		t.Fatalf("Hooks.Paths len = %d; want 3 (on-notification, on-pane-finish, on-compound-event)", len(hs.Paths))
	}
	for _, p := range hs.Paths {
		if !filepath.IsAbs(p) {
			t.Fatalf("Hooks.Paths contains non-absolute path: %s", p)
		}
	}
}

func TestFeedPush_RPCPath(t *testing.T) {
	captured := [][]string{}
	r := &stubRunner{
		prefixes: map[string]response{
			"rpc feed.push --json ": {out: []byte("ok")},
		},
		recorder: &captured,
	}
	t1 := newWithRunner(r)
	api := t1.Feed()
	if err := api.Push(context.Background(), "hello world", terminal.FeedPushOpts{Source: "test", Kind: "event"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 RPC call, got %d", len(captured))
	}
	got := strings.Join(captured[0], " ")
	if !strings.HasPrefix(got, "rpc feed.push --json ") {
		t.Fatalf("unexpected args: %v", captured[0])
	}
	// Validate payload JSON shape.
	js := strings.TrimPrefix(got, "rpc feed.push --json ")
	var payload map[string]any
	if err := json.Unmarshal([]byte(js), &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if payload["body"] != "hello world" {
		t.Fatalf("payload.body = %v", payload["body"])
	}
	if payload["source"] != "test" || payload["kind"] != "event" {
		t.Fatalf("payload.source/kind = %v, %v", payload["source"], payload["kind"])
	}
}

func TestFeedPush_RPCFails_SetStatusFallback(t *testing.T) {
	captured := [][]string{}
	r := &stubRunner{
		prefixes: map[string]response{
			"rpc feed.push --json ": {err: errors.New("rpc broken")},
		},
		exact: map[string]response{
			"set-status compound-event fallback body": {out: []byte("ok")},
		},
		recorder: &captured,
	}
	t1 := newWithRunner(r)
	if err := t1.Feed().Push(context.Background(), "fallback body", terminal.FeedPushOpts{}); err != nil {
		t.Fatalf("Push: %v (want nil — fallback must succeed)", err)
	}
	// Two calls: rpc (failed) then set-status (succeeded).
	if len(captured) != 2 {
		t.Fatalf("expected 2 calls (rpc + fallback), got %d: %v", len(captured), captured)
	}
	if !strings.HasPrefix(strings.Join(captured[1], " "), "set-status ") {
		t.Fatalf("fallback call was not set-status: %v", captured[1])
	}
}

func TestWorkspace_SetIcon_Unsupported(t *testing.T) {
	t1 := New()
	if err := t1.Workspace().SetIcon(context.Background(), terminal.WorkspaceRef{Title: "x"}, "●"); !errors.Is(err, terminal.ErrNotSupported) {
		t.Fatalf("SetIcon err = %v; want ErrNotSupported", err)
	}
}

func TestWorkspace_Select_NoRef(t *testing.T) {
	t1 := New()
	if err := t1.Workspace().Select(context.Background(), terminal.WorkspaceRef{}); err == nil {
		t.Fatal("Select with empty ref returned nil err")
	}
}
