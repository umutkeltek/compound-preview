package main

import (
	"strings"
	"testing"

	"compound/internal/work"
)

func TestWorkAdvanceExecuteForwardsExplicitLaunchProfile(t *testing.T) {
	// An explicit owner profile always resolves a model (owner authority refuses
	// an empty one). os-harness-entrypoints runs the caller's command verbatim
	// and cannot bind it, so the profile is refused before launch with the typed
	// reason instead of being forwarded and silently ignored. The positive
	// forwarding path on a harness that binds the model is pinned by
	// TestWorkAdvanceExecuteDetachedTupleReachesActualAdapter/saved=false.
	f := newClaimedExecutionFixture(t, "codex", "explicit owner profile reaches the governed attempt")
	t.Cleanup(work.SwapExecutionSelectionResolver(func(req work.ExecutionSelectionResolveRequest) (work.ExecutionSelectionResolution, error) {
		return work.ExecutionSelectionResolution{
			LaunchProfileID: req.LaunchProfileID, Harness: "os-harness-entrypoints",
			ProviderRoute: "synthetic/route", RequestedModelID: "synthetic-model",
		}, nil
	}))
	type exitSignal struct{ code int }
	originalExit := workAdvanceExecuteExit
	workAdvanceExecuteExit = func(code int) { panic(exitSignal{code: code}) }
	t.Cleanup(func() { workAdvanceExecuteExit = originalExit })
	exitCode := 0
	stderr := captureStderr(t, func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				signal, ok := recovered.(exitSignal)
				if !ok {
					panic(recovered)
				}
				exitCode = signal.code
			}
		}()
		cmdWork([]string{"advance", f.workID, "--to", "running", "--by", "codex",
			"--execute", "--harness", "os-harness-entrypoints", "--launch-profile", "fixture-owner",
			"--", "echo", "profile-forwarded"})
	})
	if exitCode != 1 || !strings.Contains(stderr, "selection_unsupported:os-harness-entrypoints:model") {
		t.Fatalf("exit=%d stderr=%q, want exit 1 with selection_unsupported:os-harness-entrypoints:model", exitCode, stderr)
	}
	final, err := f.store.Get(f.workID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != "claimed" || len(final.RunAttemptIDs) != 1 {
		t.Fatalf("refusal changed the envelope: state=%s attempts=%v", final.State, final.RunAttemptIDs)
	}
}
