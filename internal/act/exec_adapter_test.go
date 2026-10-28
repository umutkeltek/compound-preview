package act

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
)

const execTransportCapability = "outbound.content_post.publish"

func execTransportIntent(t *testing.T, workID, transport string) envelope.ActionIntent {
	t.Helper()
	now := fixedNow()
	targetPath := filepath.ToSlash(filepath.Join("actuator", "transports", transport+".json"))
	ai := mkIntent(t, workID, execTransportCapability, targetPath, transport, now)
	ai.Action = "post_content"
	ai.Target = envelope.ActionTarget{Kind: "transport", ID: transport, Path: targetPath}
	ai.Reversibility = "manual"
	ai.Preview = "post content through transport " + transport
	ai.RollbackPlan = "delete the posted message via the transport's delete verb (manual)"
	if err := ai.Validate(); err != nil {
		t.Fatalf("test transport intent invalid: %v", err)
	}
	return ai
}

func writeSpyTransport(t *testing.T, dir string) (script, marker string) {
	t.Helper()
	marker = filepath.Join(dir, "transport-ran")
	script = filepath.Join(dir, "spy-transport.sh")
	body := "#!/bin/sh\ncat > \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write spy transport: %v", err)
	}
	return script, marker
}

func execTransportGrantConfig(ai envelope.ActionIntent) policy.Config {
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID:            "grant_fixture_transport",
		Capability:    ai.Capability,
		MutationClass: ai.MutationClass,
		Reversibility: ai.Reversibility,
		BlastRadius:   ai.BlastRadius,
		TargetPath:    ai.Target.Path,
	})
	return cfg
}

func receiptCount(t *testing.T, homePath string) int {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"rec_"}, RegularOnly: true})
	if err != nil {
		t.Fatalf("scan receipts: %v", err)
	}
	return len(entries)
}

func writeTransportConfig(t *testing.T, homePath, name string, argv []string, timeoutS int) {
	t.Helper()
	dir := filepath.Join(homePath, "actuator", "transports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create transport config dir: %v", err)
	}
	body, err := json.Marshal(map[string]any{"argv": argv, "timeout_s": timeoutS})
	if err != nil {
		t.Fatalf("marshal transport config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o644); err != nil {
		t.Fatalf("write transport config: %v", err)
	}
}

func runExecTransport(t *testing.T, homePath string, argv []string) error {
	t.Helper()
	writeTransportConfig(t, homePath, "fixture", argv, 30)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	ai := execTransportIntent(t, "work_argv_validation", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
	if !policy.Allowed(pd) {
		t.Fatalf("precondition: fixture grant did not allow: %s (%v)", pd.Outcome, pd.Reasons)
	}
	_, err = Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("body"), fixedNow())
	return err
}

// F1: Execute must reject a held post before the adapter can run its transport.
func TestExecTransportExecuteHoldNeverRuns(t *testing.T) {
	homePath := t.TempDir()
	_, marker := writeSpyTransport(t, homePath)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	ai := execTransportIntent(t, "work_exec_hold", "fixture")
	pd := policy.Evaluate(ai, policy.DefaultConfig(), fixedNow())
	if pd.Outcome != "hold" {
		t.Fatalf("precondition: ungranted transport post outcome = %q, want hold", pd.Outcome)
	}

	_, err = Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("held body"), fixedNow())
	if err == nil || !strings.Contains(err.Error(), "refusing to write") {
		t.Fatalf("held post must be refused at the act gate, got %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("held post ran the transport; marker stat error = %v", err)
	}
}

// F4: a missing or malformed named config must fail closed before exec and receipt minting.
func TestExecTransportInvalidConfigFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		configBody string
		wantDetail string
	}{
		{name: "missing", wantDetail: "read"},
		{name: "malformed", configBody: "{\"argv\":[", wantDetail: "parse"},
		{name: "empty argv", configBody: "{\"argv\":[]}", wantDetail: "argv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			_, marker := writeSpyTransport(t, homePath)
			if tt.configBody != "" {
				configDir := filepath.Join(homePath, "actuator", "transports")
				if err := os.MkdirAll(configDir, 0o755); err != nil {
					t.Fatalf("create transport config dir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(configDir, "fixture.json"), []byte(tt.configBody), 0o644); err != nil {
					t.Fatalf("write transport config: %v", err)
				}
			}
			store, err := NewArtifactStore(homePath)
			if err != nil {
				t.Fatalf("new artifact store: %v", err)
			}
			ai := execTransportIntent(t, "work_invalid_config_"+tt.name, "fixture")
			pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
			if !policy.Allowed(pd) {
				t.Fatalf("precondition: fixture grant did not allow: %s (%v)", pd.Outcome, pd.Reasons)
			}

			_, err = Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("body"), fixedNow())
			if err == nil || !strings.Contains(err.Error(), `transport "fixture" config`) || !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("invalid config error = %v, want named transport config %s error", err, tt.wantDetail)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("invalid config ran the transport; marker stat error = %v", err)
			}
			if got := receiptCount(t, homePath); got != 0 {
				t.Fatalf("invalid config minted %d receipts, want 0", got)
			}
		})
	}
}

// F7: a transport config cannot name an arbitrary executable, a shell interpreter,
// a relative/traversal path, or contain shell metacharacters in arguments. The load
// fails closed before the process is spawned.
func TestExecTransportArgvValidationFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		wantDetail string
	}{
		{name: "unallowed basename", argv: []string{"rm", "-rf", "/"}, wantDetail: "not an allowed transport executable"},
		{name: "shell interpreter basename", argv: []string{"sh", "-c", "echo pwned"}, wantDetail: "not an allowed transport executable"},
		{name: "absolute shell path", argv: []string{"/bin/sh", "-c", "echo pwned"}, wantDetail: "shell/interpreter"},
		{name: "versioned interpreter path", argv: []string{"/usr/local/bin/python3.9", "-c", "print()"}, wantDetail: "shell/interpreter"},
		{name: "relative path", argv: []string{"../spy-transport.sh"}, wantDetail: "not an allowed transport executable"},
		{name: "traversal in absolute path", argv: []string{"/tmp/../etc/passwd"}, wantDetail: "clean absolute path"},
		{name: "unsafe argument", argv: []string{"compound", "foo; rm -rf /"}, wantDetail: "unsafe characters"},
		{name: "command substitution arg", argv: []string{"compound", "$(id)"}, wantDetail: "unsafe characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homePath := t.TempDir()
			_, marker := writeSpyTransport(t, homePath)
			err := runExecTransport(t, homePath, tt.argv)
			if err == nil {
				t.Fatalf("argv %v must be rejected", tt.argv)
			}
			errStr := err.Error()
			if !strings.Contains(errStr, `transport "fixture" config`) || !strings.Contains(errStr, tt.wantDetail) {
				t.Fatalf("argv %v error = %v, want config-validation error containing %q", tt.argv, err, tt.wantDetail)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("argv %v ran the transport; marker stat error = %v", tt.argv, statErr)
			}
			if got := receiptCount(t, homePath); got != 0 {
				t.Fatalf("argv %v minted %d receipts, want 0", tt.argv, got)
			}
		})
	}
}

// F8: error messages surface a bounded, redacted summary of transport output; the
// full combined stdout/stderr is not echoed to the caller.
func TestExecTransportFailureRedactsLongOutput(t *testing.T) {
	homePath := t.TempDir()
	script := filepath.Join(homePath, "leaky-transport.sh")
	// Emit a long, distinctive failure message. The old code would echo the entire
	// string in the error; the new code must cap it and mark the remainder redacted.
	prefix := "fixture transport exploded "
	tail := strings.Repeat("A", 2000)
	scriptBody := "#!/bin/sh\ncat >/dev/null\nprintf '%s' '" + prefix + tail + "' >&2\nexit 7\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write leaky transport: %v", err)
	}
	writeTransportConfig(t, homePath, "fixture", []string{script}, 30)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	ai := execTransportIntent(t, "work_transport_leak", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
	if !policy.Allowed(pd) {
		t.Fatalf("precondition: fixture grant did not allow: %s (%v)", pd.Outcome, pd.Reasons)
	}

	result, err := Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("body"), fixedNow())
	if err == nil {
		t.Fatalf("leaky transport must error")
	}
	if result.Receipt.ID != "" || result.PostImageRef != "" {
		t.Fatalf("failed transport returned receipt/post image: receipt=%q post=%q", result.Receipt.ID, result.PostImageRef)
	}
	errStr := err.Error()
	if !strings.Contains(errStr, prefix) {
		t.Fatalf("error must include a short prefix of the output, got: %s", errStr)
	}
	if strings.Contains(errStr, tail) {
		t.Fatalf("error leaked the full transport output")
	}
	if !strings.Contains(errStr, "output redacted") {
		t.Fatalf("error must indicate redaction, got: %s", errStr)
	}
	if got := receiptCount(t, homePath); got != 0 {
		t.Fatalf("failed transport minted %d receipts, want 0", got)
	}
}

func TestExecTransportCapturesSuccessfulOutputAsPostImage(t *testing.T) {
	homePath := t.TempDir()
	script := filepath.Join(homePath, "verified-transport.sh")
	scriptBody := "#!/bin/sh\ncat >/dev/null\nprintf '{\"remote_id\":\"post-synth\",\"verified\":true}'\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTransportConfig(t, homePath, "fixture", []string{script}, 30)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ai := execTransportIntent(t, "work_transport_output", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())

	result, err := Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("body"), fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	postImage, err := store.Get(result.PostImageRef)
	if err != nil {
		t.Fatal(err)
	}
	if string(postImage) != `{"remote_id":"post-synth","verified":true}` {
		t.Fatalf("post image = %q", postImage)
	}
	if !containsRef(result.Receipt.ProofRefs, result.PostImageRef) {
		t.Fatalf("receipt does not carry verified transport post-image: %+v", result.Receipt.ProofRefs)
	}
}

// F6: transport failures surface bounded combined output and mint no Receipt or post image.
func TestExecTransportExitFailureIncludesOutputWithoutReceipt(t *testing.T) {
	homePath := t.TempDir()
	script := filepath.Join(homePath, "failing-transport.sh")
	scriptBody := "#!/bin/sh\ncat >/dev/null\nprintf 'fixture transport exploded\\n' >&2\nexit 7\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write failing transport: %v", err)
	}
	// Generous budget: the spy exits immediately, so the deadline exists only as a
	// safety net — a tight budget races shell startup under full-battery load.
	writeTransportConfig(t, homePath, "fixture", []string{script}, 30)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	ai := execTransportIntent(t, "work_transport_failure", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
	if !policy.Allowed(pd) {
		t.Fatalf("precondition: fixture grant did not allow: %s (%v)", pd.Outcome, pd.Reasons)
	}

	result, err := Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("body"), fixedNow())
	if err == nil || !strings.Contains(err.Error(), "fixture transport exploded") {
		t.Fatalf("transport error = %v, want captured process output", err)
	}
	if result.Receipt.ID != "" || result.PostImageRef != "" {
		t.Fatalf("failed transport returned receipt/post image: receipt=%q post=%q", result.Receipt.ID, result.PostImageRef)
	}
	if got := receiptCount(t, homePath); got != 0 {
		t.Fatalf("failed transport minted %d receipts, want 0", got)
	}
	artifacts, err := os.ReadDir(filepath.Join(homePath, "artifacts", "sha256"))
	if err != nil {
		t.Fatalf("read artifact store: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("failed transport stored %d artifacts, want only the pre-image sentinel", len(artifacts))
	}
}

func TestExecTransportTimeoutIncludesPartialOutputWithoutReceipt(t *testing.T) {
	homePath := t.TempDir()
	script := filepath.Join(homePath, "slow-transport.sh")
	// 5s budget for shell startup + printf vs a 60s hang: the deadline deterministically
	// wins AND the partial output deterministically lands, even under full-battery load.
	scriptBody := "#!/bin/sh\nprintf 'transport started\\n' >&2\nexec sleep 60\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write slow transport: %v", err)
	}
	writeTransportConfig(t, homePath, "fixture", []string{script}, 5)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	ai := execTransportIntent(t, "work_transport_timeout", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
	result, err := Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("body"), fixedNow())
	if err == nil || !strings.Contains(err.Error(), "timed out after 5s") || !strings.Contains(err.Error(), "transport started") {
		t.Fatalf("timeout error = %v, want timeout plus partial output", err)
	}
	if result.Receipt.ID != "" || result.PostImageRef != "" || receiptCount(t, homePath) != 0 {
		t.Fatalf("timed-out transport minted governed output: %+v", result)
	}
}
