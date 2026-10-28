package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/hookplan"
)

func TestHarnessPosturePlanCompilesAdapterNativeConfigurationWithoutVendorFlags(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		harness               string
		wantFamily            string
		wantReady             bool
		wantGuard             bool
		wantGuardDisposition  hookplan.Disposition
		wantActionObservation bool
	}{
		{harness: "claude-zai", wantFamily: "claude", wantReady: true, wantGuard: true, wantGuardDisposition: hookplan.DispositionNative},
		{harness: "claude-code", wantFamily: "claude", wantReady: false, wantActionObservation: true},
		{harness: "codex", wantFamily: "codex", wantReady: false, wantActionObservation: true},
		{harness: "opencode", wantFamily: "opencode", wantReady: false, wantGuard: true, wantGuardDisposition: hookplan.DispositionUnsupported},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runHarnessPosturePlan(
				[]string{"--harness", tc.harness, "--json"},
				&stdout,
				&stderr,
			)
			if code != 0 {
				t.Fatalf("posture-plan exit=%d stderr=%s", code, stderr.String())
			}
			var got hookplan.CompiledPosture
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("decode posture plan: %v\n%s", err, stdout.String())
			}
			if got.CompiledFrom.HarnessFamily != tc.wantFamily || got.MutationReady != tc.wantReady {
				t.Fatalf("compiled posture = %+v", got)
			}
			guard, hasGuard := compiledIntentionForTest(got, hookplan.IntentionGuardMutation)
			if hasGuard != tc.wantGuard {
				t.Fatalf("guard present=%t, want %t: %+v", hasGuard, tc.wantGuard, got.Intentions)
			}
			if hasGuard && guard.Disposition != tc.wantGuardDisposition {
				t.Fatalf("guard disposition = %q, want %q", guard.Disposition, tc.wantGuardDisposition)
			}
			observedAction, hasActionObservation := compiledIntentionForTest(got, hookplan.IntentionObserveAction)
			if hasActionObservation != tc.wantActionObservation {
				t.Fatalf("action observation present=%t, want %t: %+v", hasActionObservation, tc.wantActionObservation, got.Intentions)
			}
			if hasActionObservation &&
				(observedAction.Disposition != hookplan.DispositionNative ||
					observedAction.EnforcementStrength != "observe") {
				t.Fatalf("action observation is not native observe-only: %+v", observedAction)
			}
		})
	}
}

func TestHarnessPosturePlanRejectsUnknownFlags(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := runHarnessPosturePlan(
		[]string{"--harness", "codex", "--jsno"},
		&stdout,
		&stderr,
	)
	if code != 2 {
		t.Fatalf("posture-plan exit=%d, want 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown flag") {
		t.Fatalf("posture-plan did not explain unknown flag: %s", stderr.String())
	}
}

func compiledIntentionForTest(
	compiled hookplan.CompiledPosture,
	want hookplan.HarnessIntention,
) (hookplan.CompiledIntention, bool) {
	for _, intention := range compiled.Intentions {
		if intention.Intention == want {
			return intention, true
		}
	}
	return hookplan.CompiledIntention{}, false
}
