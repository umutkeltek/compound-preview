package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

type nativeLaunchBindingFixture struct {
	adapter        *OMPNativeVerifierAdapter
	projection     OMPOwnerProjection
	projectionPath string
	marker         string
	prepared       RunInput
}

func newNativeLaunchBindingFixture(t *testing.T) nativeLaunchBindingFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "child.started")
	write := func(name, body string) ompNativeBinding {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		return ompNativeBinding{Path: path, SHA256: hex.EncodeToString(sum[:])}
	}
	wrapper := write("wrapper", "synthetic wrapper\n")
	executable := write("omp", "synthetic executable\n")
	models := write("agent/models.yml", "synthetic: model\n")
	interpreter := write("interpreter", "#!/bin/sh\n: > '"+strings.ReplaceAll(marker, "'", "'\\''")+"'\nexit 0\n")
	p := validOMPOwnerProjection(t)
	p.PromptOnly.Wrapper.Path, p.PromptOnly.Wrapper.SHA256 = wrapper.Path, wrapper.SHA256
	p.PromptOnly.Wrapper.ExpectedSHA256 = wrapper.SHA256
	p.PromptOnly.Executable.Path, p.PromptOnly.Executable.SHA256 = executable.Path, executable.SHA256
	p.PromptOnly.ModelsConfig.Path, p.PromptOnly.ModelsConfig.SHA256 = models.Path, models.SHA256
	p.PromptOnly.ModelsConfig.ExpectedSHA256 = models.SHA256
	if err := p.Validate(p.Profile.ID); err != nil {
		t.Fatal(err)
	}
	projectionRoot := filepath.Join(root, "projections")
	if err := os.MkdirAll(projectionRoot, 0700); err != nil {
		t.Fatal(err)
	}
	projectionPath := filepath.Join(projectionRoot, p.Profile.ID+".json")
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectionPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	ownedHome := filepath.Join(root, "home")
	if err := os.MkdirAll(ownedHome, 0700); err != nil {
		t.Fatal(err)
	}
	a := NewOMPNativeVerifierAdapter(runattempt.Open(ownedHome))
	a.projectionRoot, a.profileID, a.interpreterPath = projectionRoot, p.Profile.ID, interpreter.Path
	prepared, err := a.Preflight(context.Background(), RunInput{
		WorkID: "work_fixture", ActionIntentID: "act_fixture",
		Workspace: t.TempDir(), Prompt: "synthetic launch boundary", Now: time.Now().UTC(),
		StartGuard: func(_ context.Context, start func() error) error { return start() },
	})
	if err != nil {
		t.Fatal(err)
	}
	return nativeLaunchBindingFixture{a, p, projectionPath, marker, prepared}
}

func TestOMPNativePreparedInputRejectsCallerDrift(t *testing.T) {
	for _, field := range []string{"command", "args", "env", "permission", "harness"} {
		t.Run(field, func(t *testing.T) {
			f := newNativeLaunchBindingFixture(t)
			in := f.prepared
			switch field {
			case "command":
				in.Command = "caller-command"
			case "args":
				in.Args = []string{"--caller-flag"}
			case "env":
				in.Env = []string{"CALLER_FLAG=1"}
			case "permission":
				in.PermissionMode = "bypass"
			case "harness":
				in.Harness = "caller-harness"
			}
			resolved, err := f.adapter.Preflight(context.Background(), in)
			if err == nil {
				t.Fatal("prepared input drift was normalized instead of rejected")
			}
			if err.Error() != "OMP_NATIVE_PREPARED_INPUT_INVALID" {
				t.Fatalf("caller drift must fail content-free: %v", err)
			}
			if resolved.ompNativeVerifier != nil || resolved.Command != "" {
				t.Fatal("rejected caller input retained prepared launch authority")
			}
		})
	}
}

func TestOMPNativeRevalidatesBindingsInsideStartGuard(t *testing.T) {
	for _, binding := range []string{"wrapper", "executable", "models", "interpreter", "owner_revision"} {
		t.Run(binding, func(t *testing.T) {
			f := newNativeLaunchBindingFixture(t)
			guardCalled := false
			f.prepared.StartGuard = func(_ context.Context, start func() error) error {
				guardCalled = true
				var path string
				switch binding {
				case "wrapper":
					path = f.projection.PromptOnly.Wrapper.Path
				case "executable":
					path = f.projection.PromptOnly.Executable.Path
				case "models":
					path = f.projection.PromptOnly.ModelsConfig.Path
				case "interpreter":
					path = f.prepared.ompNativeVerifier.Interpreter.Path
				case "owner_revision":
					p := f.projection
					p.ConformanceRevision = "sha256:" + strings.Repeat("b", 64)
					body, err := json.Marshal(p)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(f.projectionPath, body, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if path != "" {
					// Keep the interpreter executable and able to mark a launch.
					body, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, append(body, []byte("\n# changed binding\n")...), 0700); err != nil {
						t.Fatal(err)
					}
				}
				return start()
			}
			out, runErr := f.adapter.Run(context.Background(), f.prepared)
			if !guardCalled {
				t.Fatalf("test did not reach the real start guard: status=%s err=%v", out.RunAttempt.Status, runErr)
			}
			if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
				t.Fatalf("child started after bound launch drift: %v", err)
			}
			if runErr == nil || out.RunAttempt.ID == "" || out.RunAttempt.Status != "failed" {
				t.Fatalf("drift must terminalize a minted failed attempt: status=%s err=%v", out.RunAttempt.Status, runErr)
			}
			stored, err := f.adapter.store.Get(out.RunAttempt.ID)
			if err != nil || stored.Status != "failed" || len(stored.ProofRefs) != 0 {
				t.Fatalf("failed launch must be durable with no success proofs: status=%s err=%v", stored.Status, err)
			}
		})
	}
}

func TestOMPNativeUnchangedBindingControlReachesChild(t *testing.T) {
	f := newNativeLaunchBindingFixture(t)
	out, runErr := f.adapter.Run(context.Background(), f.prepared)
	// This stub only marks execution; it deliberately cannot emit a native proof.
	if runErr == nil || out.RunAttempt.Status != "failed" {
		t.Fatal("stub must not be mistaken for a successful native proof")
	}
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatalf("unchanged fixture did not reach the child: %v; run error=%v", err, runErr)
	}
}
