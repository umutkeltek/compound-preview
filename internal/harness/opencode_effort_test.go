package harness

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"compound/internal/runattempt"
)

// openCodeStampEffort attaches the governed saved selection exactly as the
// work wire supplies it: the selection snapshot is the ONLY effort authority
// this adapter reads, and there is no argv/env/config/permission door beside
// it. The evidence leg starts at the honest "nothing observed yet" value, so a
// test that finds it replaced knows the adapter wrote it.
func openCodeStampEffort(in *RunInput, requested string) {
	in.LaunchProfileID = "fixture/control"
	in.ExecutionSelectionSnapshot = &runattempt.ExecutionSelectionSnapshot{
		Source: "saved", SelectionRevision: 1, Harness: "opencode",
		LaunchProfileID: in.LaunchProfileID, LaunchProfileRevision: in.ExpectedModelRouterRevision,
		RequestedModelID: in.RequestedModelID, ProviderRoute: in.RequestedProviderRoute,
		RequestedEffort: requested, EffortEvidence: "unobservable:pending config application",
	}
}

func openCodeEffortInput(t *testing.T, requested string) (*OpenCodeAdapter, RunInput) {
	t.Helper()
	a, in := openCodeFixture(t, "https://provider.invalid/v1")
	openCodeStampEffort(&in, requested)
	return a, in
}

// openCodeConfigModel decodes the one model entry of the immutable settings
// file the subprocess will read. An absent options key decodes to a nil map,
// which is what makes "no effort requested" distinguishable on the wire.
type openCodeConfigModel struct {
	Limit   map[string]int    `json:"limit"`
	Options map[string]string `json:"options"`
}

func openCodeConfigEntry(t *testing.T, l *openCodeLaunch) openCodeConfigModel {
	t.Helper()
	var cfg struct {
		Provider map[string]struct {
			Models map[string]openCodeConfigModel `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(l.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	entry, ok := cfg.Provider[l.Provider].Models[strings.SplitN(l.Model, "/", 2)[1]]
	if !ok {
		t.Fatalf("config carries no %s model entry", l.Model)
	}
	return entry
}

// Explicit low reaches the immutable config, all three legs are recorded, and
// the caller's own RunInput is left exactly as it was handed over.
func TestOpenCodeLowEffortBindsLaunchAndLeavesCallerSnapshotUntouched(t *testing.T) {
	a, in := openCodeEffortInput(t, "low")
	before := *in.ExecutionSelectionSnapshot

	prepared, err := a.Preflight(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, *in.ExecutionSelectionSnapshot) {
		t.Fatalf("preflight wrote through the caller-owned snapshot: %+v", *in.ExecutionSelectionSnapshot)
	}
	if prepared.ExecutionSelectionSnapshot == in.ExecutionSelectionSnapshot {
		t.Fatal("prepared input still aliases the caller-owned snapshot pointer")
	}
	got := prepared.ExecutionSelectionSnapshot
	if got.RequestedEffort != "low" || got.AppliedEffort != "low" || got.EffortEvidence != "settings:opencode.json" {
		t.Fatalf("effort legs not recorded durably: %+v", got)
	}
	if reflect.DeepEqual(*got, before) {
		t.Fatal("recorded effort equals the untouched caller snapshot; nothing was bound")
	}
	if prepared.opencodeLaunch.Effort != "low" || prepared.opencodeLaunch.EffortBinding != (openCodeEffortBinding{Requested: "low", Applied: "low", Evidence: "settings:opencode.json", Bound: true}) {
		t.Fatalf("launch did not freeze the binding: %+v", prepared.opencodeLaunch.EffortBinding)
	}
	if err := a.validatePrepared(prepared); err != nil {
		t.Fatalf("freshly bound launch refused its own validation: %v", err)
	}
	entry := openCodeConfigEntry(t, prepared.opencodeLaunch)
	if entry.Options["reasoningEffort"] != "low" {
		t.Fatalf("immutable config does not carry reasoningEffort=low: %+v", entry.Options)
	}
	if entry.Limit["output"] != 800 || entry.Limit["context"] != 8192 {
		t.Fatalf("low-effort config changed the output/context bound: %+v", entry.Limit)
	}
	if !strings.Contains(string(prepared.opencodeLaunch.Config), `"reasoningEffort":"low"`) {
		t.Fatal("config bytes do not carry the explicit low effort")
	}
}

// The empty default stays compatible: no options key is emitted at all, so the
// request the provider sees is byte-for-byte the pre-bounded one. Effort legs
// the caller supplied honestly are carried through, never fabricated.
func TestOpenCodeAbsentEffortLeavesConfigAndWireFree(t *testing.T) {
	a, in := openCodeEffortInput(t, "")
	before := *in.ExecutionSelectionSnapshot

	prepared, err := a.Preflight(context.Background(), in)
	if err != nil {
		t.Fatalf("the empty effort default was refused: %v", err)
	}
	if !reflect.DeepEqual(before, *in.ExecutionSelectionSnapshot) {
		t.Fatalf("no-effort preflight mutated the caller snapshot: %+v", *in.ExecutionSelectionSnapshot)
	}
	got := prepared.ExecutionSelectionSnapshot
	if got.RequestedEffort != "" || got.AppliedEffort != "" || got.EffortEvidence != before.EffortEvidence {
		t.Fatalf("no-effort default was rewritten rather than preserved: %+v", got)
	}
	if prepared.opencodeLaunch.Effort != "" || prepared.opencodeLaunch.EffortBinding.Requested != "" || !prepared.opencodeLaunch.EffortBinding.Bound {
		t.Fatalf("no-effort launch still froze an effort: %+v", prepared.opencodeLaunch.EffortBinding)
	}
	entry := openCodeConfigEntry(t, prepared.opencodeLaunch)
	if entry.Options != nil {
		t.Fatalf("no-effort config invented an options key: %+v", entry.Options)
	}
	if strings.Contains(string(prepared.opencodeLaunch.Config), "reasoningEffort") {
		t.Fatal("no-effort config mentions reasoning effort at all")
	}
	if err := a.validatePrepared(prepared); err != nil {
		t.Fatalf("no-effort launch refused its own validation: %v", err)
	}
	// A snapshot that claims an applied effort for a launch carrying none is an
	// over-claim, not an upgrade, and is refused before mint like any other.
	overclaimer, overclaimedIn := openCodeEffortInput(t, "")
	overclaimedIn.ExecutionSelectionSnapshot.AppliedEffort = "low"
	if _, err := overclaimer.Preflight(context.Background(), overclaimedIn); err == nil || err.Error() != "OPENCODE_EFFORT_APPLIED_MISMATCH" {
		t.Fatalf("over-claimed applied effort accepted or misreported: %v", err)
	}
}

// Only the exact value "low" is supported. Everything else — including values
// that would normalize to "low" — refuses before mint, without touching the
// caller snapshot and without leaving a prepared launch behind.
func TestOpenCodeUnsupportedEffortRefusedBeforeMint(t *testing.T) {
	for _, requested := range []string{"high", "medium", "xhigh", "max", "none", "minimal", "low ", " low", "Low", "LOW", "unknown"} {
		t.Run(requested, func(t *testing.T) {
			a, in := openCodeEffortInput(t, requested)
			before := *in.ExecutionSelectionSnapshot
			got, err := a.Preflight(context.Background(), in)
			if err == nil {
				t.Fatalf("unsupported effort %q accepted", requested)
			}
			if !strings.HasPrefix(err.Error(), "OPENCODE_EFFORT_") {
				t.Fatalf("unsupported effort %q refused for the wrong reason: %v", requested, err)
			}
			if !reflect.DeepEqual(before, *in.ExecutionSelectionSnapshot) {
				t.Fatalf("refused preflight still mutated the caller snapshot: %+v", *in.ExecutionSelectionSnapshot)
			}
			if got.opencodeLaunch != nil || got.Command != "" || got.ExpectedModelRouterRevision != "" {
				t.Fatalf("refused preflight returned a partially prepared launch: %+v", got)
			}
		})
	}
	// The effort door does not soften the pre-existing caller-override bans.
	for name, change := range map[string]func(*RunInput){
		"argv": func(in *RunInput) { in.Args = []string{"--effort", "low"} },
		"env":  func(in *RunInput) { in.Env = []string{"OPENCODE_EFFORT=low"} },
	} {
		t.Run("caller-"+name, func(t *testing.T) {
			a, in := openCodeEffortInput(t, "low")
			change(&in)
			if _, err := a.Preflight(context.Background(), in); err == nil || err.Error() != "OPENCODE_CALLER_OVERRIDE_FORBIDDEN" {
				t.Fatalf("caller %s override was not refused for an in-scope effort run: %v", name, err)
			}
		})
	}
}

// Every way of moving the effort legs after preflight is drift: raising or
// lowering the request, rewriting the applied value, re-sourcing the evidence,
// and dropping or replacing the snapshot outright.
func TestOpenCodeEffortDriftRefusedAtPreflightReuse(t *testing.T) {
	for name, drift := range map[string]func(*RunInput){
		"requested-raised":   func(in *RunInput) { in.ExecutionSelectionSnapshot.RequestedEffort = "high" },
		"requested-dropped":  func(in *RunInput) { in.ExecutionSelectionSnapshot.RequestedEffort = "" },
		"applied-raised":     func(in *RunInput) { in.ExecutionSelectionSnapshot.AppliedEffort = "high" },
		"applied-dropped":    func(in *RunInput) { in.ExecutionSelectionSnapshot.AppliedEffort = "" },
		"evidence-resourced": func(in *RunInput) { in.ExecutionSelectionSnapshot.EffortEvidence = "receipt:synthetic-fabricated" },
		"evidence-erased":    func(in *RunInput) { in.ExecutionSelectionSnapshot.EffortEvidence = "" },
		"snapshot-dropped":   func(in *RunInput) { in.ExecutionSelectionSnapshot = nil },
		"snapshot-replaced": func(in *RunInput) {
			openCodeStampEffort(in, "low")
			in.ExecutionSelectionSnapshot.EffortEvidence = "unobservable:pending config application"
		},
		"launch-legs-replaced": func(in *RunInput) { openCodeStampEffort(in, "high") },
	} {
		t.Run(name, func(t *testing.T) {
			a, in := openCodeEffortInput(t, "low")
			prepared, err := a.Preflight(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			drift(&prepared)
			// Reuse of a prepared launch is refused...
			if err := a.validatePrepared(prepared); err == nil || err.Error() != "OPENCODE_LAUNCH_BINDING_DRIFT" {
				t.Fatalf("effort drift %q accepted or misreported at reuse: %v", name, err)
			}
			// ...and so is a preflight handed that same drifted input.
			if _, err := a.Preflight(context.Background(), prepared); err == nil || err.Error() != "OPENCODE_LAUNCH_BINDING_DRIFT" {
				t.Fatalf("effort drift %q accepted or misreported at preflight: %v", name, err)
			}
		})
	}
	// A nil launch is refused, not dereferenced.
	a, _ := openCodeEffortInput(t, "low")
	if err := a.validatePrepared(RunInput{}); err == nil || err.Error() != "OPENCODE_PREPARED_INPUT_INVALID" {
		t.Fatalf("nil launch was not refused cleanly: %v", err)
	}
}

// The binding is re-checked inside the start guard, immediately before the
// native subprocess starts, so drift injected at that exact moment still
// refuses and the subprocess never runs.
func TestOpenCodeEffortDriftAroundStartGuardRefusesToLaunch(t *testing.T) {
	for _, mode := range []string{"drifted", "clean"} {
		t.Run(mode, func(t *testing.T) {
			a, in := openCodeEffortInput(t, "low")
			prepared, err := a.Preflight(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			// A stand-in for sandbox-exec that records the fact it was launched.
			marker := filepath.Join(t.TempDir(), "launched")
			sandbox := filepath.Join(t.TempDir(), "synthetic-sandbox")
			if err := os.WriteFile(sandbox, []byte("#!/bin/sh\n: > "+marker+"\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			a.sandboxCommand = sandbox
			prepared.StartGuard = func(_ context.Context, start func() error) error {
				if mode == "drifted" {
					prepared.ExecutionSelectionSnapshot.RequestedEffort = "high"
				}
				return start()
			}
			out, runErr := a.Run(context.Background(), prepared)
			_, statErr := os.Stat(marker)
			launched := statErr == nil
			if out.RunAttempt.ID == "" || out.RunAttempt.Status != runattempt.StatusFailed {
				t.Fatalf("%s: run did not terminalize as failed: %s %v", mode, out.RunAttempt.Status, runErr)
			}
			if mode == "clean" {
				if !launched || runErr == nil {
					t.Fatalf("clean control run did not reach launch, so the drift case proves nothing: launched=%v err=%v", launched, runErr)
				}
				return
			}
			if launched {
				t.Fatal("effort drift immediately around start guard still launched the subprocess")
			}
			// The adapter's stableStartError deliberately replaces a guard's own
			// error with a content-free string, so the observable fact here is
			// "refused before start". That this exact drift is what the guard
			// refused is asserted directly on the same drifted input.
			if runErr == nil || !strings.Contains(runErr.Error(), "OPENCODE_START_FAILED") {
				t.Fatalf("start-guard drift did not surface the stable start refusal: %v", runErr)
			}
			if err := a.validatePrepared(prepared); err == nil || err.Error() != "OPENCODE_LAUNCH_BINDING_DRIFT" {
				t.Fatalf("the guarded input is not the drift the guard refused: %v", err)
			}
			if _, err := os.Stat(prepared.opencodeLaunch.Root); !os.IsNotExist(err) {
				t.Fatal("refused launch retained the isolated credential root")
			}
		})
	}
}

// Opt-in wire proof: the real installed executable is driven against a
// temporary synthetic TLS endpoint with a temporary synthetic key, so the
// reasoning_effort leg is observed on the actual provider request. No real
// provider is contacted, no real credential is read, nothing is spent.
func TestOpenCodeInstalledNativeSyntheticEffortOnWire(t *testing.T) {
	command := os.Getenv("COMPOUND_OPENCODE_NATIVE_TEST_BIN")
	if command == "" || runtime.GOOS != "darwin" {
		t.Skip("set COMPOUND_OPENCODE_NATIVE_TEST_BIN for installed-native synthetic effort proof")
	}
	for _, tc := range []struct {
		name, requested string
		wantEffort      bool
	}{
		{"absent", "", false},
		{"low", "low", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies []map[string]any
			server := newOpenCodeSyntheticWireServer(t, &mu, &bodies)
			defer server.Close()
			a, in := openCodeFixtureCommand(t, server.URL+"/v1", command)
			a.testNetworkPort = server.port
			a.testCAPath = server.caPath
			openCodeStampEffort(&in, tc.requested)
			in.Prompt = "Return exactly CONTROL_OK. Do not request tools."

			out, err := a.Run(context.Background(), in)
			if err != nil || out.RunAttempt.Status != runattempt.StatusSucceeded || out.TimedOut {
				t.Fatalf("native %s-effort run did not finish normally: status=%s err=%v", tc.name, out.RunAttempt.Status, err)
			}
			mu.Lock()
			captured := append([]map[string]any(nil), bodies...)
			mu.Unlock()
			if len(captured) != 1 {
				t.Fatalf("native %s-effort run made %d provider requests, expected exactly 1", tc.name, len(captured))
			}
			request := captured[0]
			if request["model"] != "control" || request["max_tokens"] != float64(800) {
				t.Fatalf("native %s-effort request drifted on model/output cap: model=%v max_tokens=%v", tc.name, request["model"], request["max_tokens"])
			}
			if tools, ok := request["tools"].([]any); ok && len(tools) != 0 {
				t.Fatalf("native %s-effort request exposed tools", tc.name)
			}
			got, present := request["reasoning_effort"]
			if present != tc.wantEffort || (tc.wantEffort && got != "low") {
				t.Fatalf("native %s-effort wire reasoning_effort: present=%v value=%v want_present=%v want=low", tc.name, present, got, tc.wantEffort)
			}
			transcript, readErr := os.ReadFile(out.TranscriptPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(transcript), "CONTROL_OK") {
				t.Fatalf("native %s-effort run produced no normal text: %s", tc.name, transcript)
			}
			t.Logf("native_wire effort=%q reasoning_effort_present=%v value=%v max_tokens=%v requests=%d attempt=%s status=%s",
				tc.requested, present, got, request["max_tokens"], len(captured), out.RunAttempt.ID, out.RunAttempt.Status)
		})
	}
}

// openCodeSyntheticWireServer is the one successful streaming shape the
// adapter accepts, plus a recording of every request body for the wire
// assertions. It listens on loopback with a self-signed certificate the test
// pins explicitly; it never reaches a real provider.
type openCodeSyntheticWireServer struct {
	*httptest.Server
	caPath string
	port   int
}

func newOpenCodeSyntheticWireServer(t *testing.T, mu *sync.Mutex, bodies *[]map[string]any) *openCodeSyntheticWireServer {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
			http.Error(w, "synthetic provider request body was not JSON", http.StatusBadRequest)
			return
		}
		mu.Lock()
		*bodies = append(*bodies, request)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer synthetic-selected-credential" {
			http.Error(w, "synthetic selected credential missing", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"control\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"CONTROL_OK\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"control\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	caPath := filepath.Join(t.TempDir(), "synthetic-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return &openCodeSyntheticWireServer{Server: server, caPath: caPath, port: port}
}
