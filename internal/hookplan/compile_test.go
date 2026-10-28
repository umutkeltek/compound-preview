package hookplan

import (
	"slices"
	"testing"

	"compound/internal/harness"
)

func TestDefaultInteractivePosturesObserveActionsWithoutNativeMutationGuard(t *testing.T) {
	t.Parallel()

	for _, harnessName := range []string{"codex", "claude-code"} {
		harnessName := harnessName
		t.Run(harnessName, func(t *testing.T) {
			t.Parallel()

			compiled, err := CompileDefaultPosture(DefaultPostureRequest{
				Harness:         harnessName,
				Registry:        harness.DefaultRegistry(),
				HandlerRef:      "compound",
				HandlerRevision: "sha256:handler-fixture",
			})
			if err != nil {
				t.Fatal(err)
			}
			if compiled.MutationReady {
				t.Fatalf("default %s posture unexpectedly mutation-ready: %+v", harnessName, compiled)
			}
			for _, intention := range compiled.Intentions {
				if intention.Intention == IntentionGuardMutation {
					t.Fatalf("default %s posture installed a blocking mutation guard: %+v", harnessName, intention)
				}
			}
			assertCompiledIntention(t, compiled, IntentionObserveAction, DispositionNative, []string{"PreToolUse"})
			for _, binding := range compiled.HookPlan.Bindings {
				if binding.Event != "PreToolUse" {
					continue
				}
				if binding.Blocking || binding.EnforcementStrength != "observe" {
					t.Fatalf("default %s PreToolUse binding is not observation-only: %+v", harnessName, binding)
				}
				return
			}
			t.Fatalf("default %s posture omitted PreToolUse action observation", harnessName)
		})
	}
}

func TestDefaultIsolatedClaudePostureRetainsExplicitMutationGuard(t *testing.T) {
	t.Parallel()

	compiled, err := CompileDefaultPosture(DefaultPostureRequest{
		Harness:         "claude-zai",
		Registry:        harness.DefaultRegistry(),
		HandlerRef:      "compound",
		HandlerRevision: "sha256:handler-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !compiled.MutationReady {
		t.Fatalf("isolated Claude posture is not mutation-ready: %+v", compiled)
	}
	assertCompiledIntention(t, compiled, IntentionGuardMutation, DispositionNative, []string{"PreToolUse"})
	for _, binding := range compiled.HookPlan.Bindings {
		if binding.Event == "PreToolUse" {
			if !binding.Blocking || binding.EnforcementStrength != "blocking" {
				t.Fatalf("isolated Claude PreToolUse binding is not blocking: %+v", binding)
			}
			return
		}
	}
	t.Fatal("isolated Claude posture omitted PreToolUse mutation guard")
}

func TestCompileSemanticPostureThroughClaudeAdapter(t *testing.T) {
	t.Parallel()

	compiled, err := CompileSemanticPosture(testSemanticPosture("claude-code"), testClaudeAttestation())
	if err != nil {
		t.Fatal(err)
	}

	if !compiled.MutationReady {
		t.Fatalf("Claude native blocking plan must be mutation-ready: %+v", compiled)
	}
	assertCompiledIntention(t, compiled, IntentionPrepareContext, DispositionNative, []string{
		"SessionStart",
	})
	assertCompiledIntention(t, compiled, IntentionGuardMutation, DispositionNative, []string{
		"PreToolUse",
	})
	assertCompiledIntention(t, compiled, IntentionObserveOutcome, DispositionNative, []string{
		"PostToolUse",
	})
}

func TestClaudeContextIntentionsCompileToOneEventEach(t *testing.T) {
	t.Parallel()

	posture := testSemanticPosture("claude-code")
	posture.Intentions = append(posture.Intentions, IntentionPrepareTurnContext)
	compiled, err := CompileSemanticPosture(posture, testClaudeAttestation())
	if err != nil {
		t.Fatal(err)
	}
	assertCompiledIntention(t, compiled, IntentionPrepareContext, DispositionNative, []string{"SessionStart"})
	assertCompiledIntention(t, compiled, IntentionPrepareTurnContext, DispositionNative, []string{"UserPromptSubmit"})

	// A harness that cannot inject per turn must lose only the per-turn intention:
	// folding both events into one intention used to degrade session orientation too.
	attestation := testClaudeAttestation()
	attestation.Features["user_prompt_context"] = false
	compiled, err = CompileSemanticPosture(posture, attestation)
	if err != nil {
		t.Fatal(err)
	}
	assertCompiledIntention(t, compiled, IntentionPrepareContext, DispositionNative, []string{"SessionStart"})
	assertCompiledIntention(t, compiled, IntentionPrepareTurnContext, DispositionUnsupported, nil)
	for _, binding := range compiled.HookPlan.Bindings {
		if binding.Event == "UserPromptSubmit" {
			t.Fatalf("unattested per-turn injection leaked into desired bindings: %+v", binding)
		}
	}
}

// TestDefaultClaudeCodePostureBindsSessionStartWithoutPerTurnInjection is the falsifier for
// dropping claude_code's per-turn context injection: claude_code must compile to a
// SessionStart binding and no UserPromptSubmit binding at all, while the isolated Claude
// lanes that share the same family adapter must still compile both. Before the intention
// split, one prepare_context intention emitted both events for every Claude harness, so the
// claude_code half of this test could not have passed.
func TestDefaultClaudeCodePostureBindsSessionStartWithoutPerTurnInjection(t *testing.T) {
	t.Parallel()

	claudeCode, err := CompileDefaultPosture(DefaultPostureRequest{
		Harness:         "claude-code",
		Registry:        harness.DefaultRegistry(),
		HandlerRef:      "compound",
		HandlerRevision: "sha256:handler-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCompiledIntention(t, claudeCode, IntentionPrepareContext, DispositionNative, []string{"SessionStart"})
	for _, intention := range claudeCode.Intentions {
		if intention.Intention == IntentionPrepareTurnContext {
			t.Fatalf("claude_code default posture requested per-turn context injection: %+v", intention)
		}
	}
	for _, binding := range claudeCode.HookPlan.Bindings {
		if binding.Event == "UserPromptSubmit" {
			t.Fatalf("claude_code default posture bound UserPromptSubmit: %+v", binding)
		}
	}

	for _, harnessName := range []string{"claude-zai", "claude-minimax", "claude-qwen", "codex"} {
		harnessName := harnessName
		t.Run(harnessName, func(t *testing.T) {
			t.Parallel()
			compiled, err := CompileDefaultPosture(DefaultPostureRequest{
				Harness:         harnessName,
				Registry:        harness.DefaultRegistry(),
				HandlerRef:      "compound",
				HandlerRevision: "sha256:handler-fixture",
			})
			if err != nil {
				t.Fatal(err)
			}
			assertCompiledIntention(t, compiled, IntentionPrepareTurnContext, DispositionNative, []string{"UserPromptSubmit"})
			for _, binding := range compiled.HookPlan.Bindings {
				if binding.Event == "UserPromptSubmit" {
					return
				}
			}
			t.Fatalf("%s lost its per-turn context binding: %+v", harnessName, compiled.HookPlan.Bindings)
		})
	}
}

func TestClaudeReceiptIntentionCompilesOnlyWhenSessionEndIsAttested(t *testing.T) {
	t.Parallel()

	receiptIntention := HarnessIntention("record_receipt")
	posture := testSemanticPosture("claude-code")
	posture.Intentions = append(posture.Intentions, receiptIntention)
	attestation := testClaudeAttestation()
	attestation.Features["session_end_receipt"] = true
	compiled, err := CompileSemanticPosture(posture, attestation)
	if err != nil {
		t.Fatal(err)
	}
	assertCompiledIntention(t, compiled, receiptIntention, DispositionNative, []string{"SessionEnd"})

	attestation.Features["session_end_receipt"] = false
	compiled, err = CompileSemanticPosture(posture, attestation)
	if err != nil {
		t.Fatal(err)
	}
	assertCompiledIntention(t, compiled, receiptIntention, DispositionUnsupported, nil)
	for _, binding := range compiled.HookPlan.Bindings {
		if binding.Event == "SessionEnd" {
			t.Fatalf("unattested SessionEnd leaked into desired bindings: %+v", binding)
		}
	}
}

func TestDefaultClaudePosturesRecordSessionEndAndCodexAlsoBindsSessionEnd(t *testing.T) {
	// Codex 0.149.x exposes a native SessionEnd event with transcript_path (binary evidence in
	// the work item's premise verification). The previous "codex does NOT bind SessionEnd"
	// invariant is stale; the new rule_delta (codex.SessionEndReceipt=true, see harness.go:458
	// and adapter_codex.go IntentionRecordReceipt) puts codex on the same receipt path as
	// Claude, routed through the generic session-end verb (NOT session-end-adapter, which is
	// Claude-only). Hermes is intentionally absent: its SessionEnd lives behind a plugin the
	// operator must install, not a native event the binary always fires.
	t.Parallel()

	for _, harnessName := range []string{"claude-code", "claude-zai", "claude-minimax", "claude-qwen", "codex"} {
		harnessName := harnessName
		t.Run(harnessName, func(t *testing.T) {
			t.Parallel()
			compiled, err := CompileDefaultPosture(DefaultPostureRequest{
				Harness:         harnessName,
				Registry:        harness.DefaultRegistry(),
				HandlerRef:      "compound",
				HandlerRevision: "sha256:handler-fixture",
			})
			if err != nil {
				t.Fatal(err)
			}
			assertCompiledIntention(t, compiled, HarnessIntention("record_receipt"), DispositionNative, []string{"SessionEnd"})
		})
	}
}

// TestCodexAdapterCompilesRecordReceiptToSessionEnd (F1) — the headlined codex falsifier.
// Before the rule_delta, codex had no case for IntentionRecordReceipt and returned
// unsupportedIntention. After it, the adapter maps the intention to SessionEnd (Native,
// "receipt" enforcement), producing one nativeBinding{event: "SessionEnd"}. The test also
// proves the attestation gate: turning session_end_receipt off drops the intention back to
// unsupported so the adapter never silently binds a receipt it can't deliver.
func TestCodexAdapterCompilesRecordReceiptToSessionEnd(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		attest  bool
		want    Disposition
		wantEvt []string
	}{
		{name: "attested session_end_receipt binds SessionEnd", attest: true, want: DispositionNative, wantEvt: []string{"SessionEnd"}},
		{name: "unattested session_end_receipt is unsupported", attest: false, want: DispositionUnsupported, wantEvt: nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			att := HarnessAttestation{
				Harness:          "codex",
				Family:           "codex",
				Surface:          "hooks.json",
				Version:          "fixture",
				FeatureState:     "trusted",
				ConfigScope:      "user",
				Revision:         "sha256:codex-fixture",
				FallbackRevision: "none",
				Features: map[string]bool{
					"session_start_context": true,
					"user_prompt_context":   true,
					"pre_tool_use_block":    true,
					"post_tool_use_event":   true,
					"session_end_receipt":   c.attest,
				},
			}
			adapter := codexSemanticAdapter{}
			compiled, bindings := adapter.Compile(IntentionRecordReceipt, att)
			if compiled.Disposition != c.want {
				t.Fatalf("Compile(RecordReceipt) disposition = %q, want %q", compiled.Disposition, c.want)
			}
			if !slices.Equal(compiled.NativeEvents, c.wantEvt) {
				t.Fatalf("NativeEvents = %v, want %v", compiled.NativeEvents, c.wantEvt)
			}
			if c.want == DispositionNative {
				if len(bindings) != 1 || bindings[0].event != "SessionEnd" {
					t.Fatalf("bindings = %+v, want exactly one SessionEnd", bindings)
				}
				if compiled.EnforcementStrength != "receipt" {
					t.Fatalf("EnforcementStrength = %q, want receipt", compiled.EnforcementStrength)
				}
			}
		})
	}
}

func TestCompileSemanticPostureThroughCodexAdapter(t *testing.T) {
	t.Parallel()

	compiled, err := CompileSemanticPosture(testSemanticPosture("codex"), HarnessAttestation{
		Harness:          "codex",
		Family:           "codex",
		Surface:          "hooks.json",
		Version:          "fixture",
		FeatureState:     "trusted",
		ConfigScope:      "user",
		Revision:         "sha256:codex-fixture",
		FallbackRevision: "none",
		Features: map[string]bool{
			"session_start_context": true,
			"user_prompt_context":   true,
			"pre_tool_use_block":    true,
			"post_tool_use_event":   true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !compiled.MutationReady {
		t.Fatalf("Codex native blocking plan must be mutation-ready: %+v", compiled)
	}
	assertCompiledIntention(t, compiled, IntentionPrepareContext, DispositionNative, []string{
		"SessionStart",
	})
	assertCompiledIntention(t, compiled, IntentionGuardMutation, DispositionNative, []string{
		"PreToolUse",
	})
	assertCompiledIntention(t, compiled, IntentionObserveOutcome, DispositionNative, []string{
		"PostToolUse",
	})
}

func TestCompileSemanticPostureThroughOpenCodeAdapterKeepsBlockingGapExplicit(t *testing.T) {
	t.Parallel()

	compiled, err := CompileSemanticPosture(testSemanticPosture("opencode"), HarnessAttestation{
		Harness:          "opencode",
		Family:           "opencode",
		Surface:          "opencode.jsonc+plugin",
		Version:          "fixture",
		FeatureState:     "trusted",
		ConfigScope:      "user",
		Revision:         "sha256:opencode-fixture",
		FallbackRevision: "none",
		Features: map[string]bool{
			"chat_message_context":   true,
			"before_tool_can_deny":   false,
			"after_tool_observation": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if compiled.MutationReady {
		t.Fatalf("OpenCode cannot deny before tool execution and must not become mutation-ready: %+v", compiled)
	}
	assertCompiledIntention(t, compiled, IntentionPrepareContext, DispositionNative, []string{"chat.message"})
	assertCompiledIntention(t, compiled, IntentionGuardMutation, DispositionUnsupported, nil)
	assertCompiledIntention(t, compiled, IntentionObserveOutcome, DispositionNative, []string{"tool.execute.after"})

	for _, binding := range compiled.HookPlan.Bindings {
		if binding.Event == "tool.execute.before" {
			t.Fatalf("unsupported OpenCode pre-tool callback leaked into desired bindings: %+v", binding)
		}
	}
}

func TestCompileSemanticPostureFailsClosedForUnknownHarnessFamily(t *testing.T) {
	t.Parallel()

	_, err := CompileSemanticPosture(testSemanticPosture("mystery"), HarnessAttestation{
		Harness:      "mystery",
		Family:       "mystery",
		Surface:      "unknown",
		Version:      "fixture",
		FeatureState: "unknown",
		ConfigScope:  "unknown",
		Revision:     "sha256:mystery-fixture",
	})
	if err == nil {
		t.Fatal("unknown harness family unexpectedly compiled")
	}
}

func TestCompileSemanticPostureCannotInferFeatureSupportFromFamilyName(t *testing.T) {
	t.Parallel()

	attestation := testClaudeAttestation()
	attestation.Features["pre_tool_use_block"] = false
	compiled, err := CompileSemanticPosture(testSemanticPosture("claude-code"), attestation)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.MutationReady {
		t.Fatalf("family name manufactured native blocking support: %+v", compiled)
	}
	guard := compiledIntention(t, compiled, IntentionGuardMutation)
	if guard.Disposition != DispositionUnsupported {
		t.Fatalf("unattested mutation guard disposition = %q, want unsupported", guard.Disposition)
	}
}

func testSemanticPosture(harnessName string) SemanticPosture {
	return SemanticPosture{
		ID:              "hpost_fixture",
		Revision:        "sha256:posture-fixture",
		AuthorityRef:    "compound://harness-posture/fixture",
		Harness:         harnessName,
		HandlerRef:      "compound",
		HandlerRevision: "sha256:handler-fixture",
		Intentions: []HarnessIntention{
			IntentionPrepareContext,
			IntentionGuardMutation,
			IntentionObserveOutcome,
		},
	}
}

func assertCompiledIntention(
	t *testing.T,
	compiled CompiledPosture,
	intention HarnessIntention,
	disposition Disposition,
	wantEvents []string,
) {
	t.Helper()
	for _, got := range compiled.Intentions {
		if got.Intention != intention {
			continue
		}
		if got.Disposition != disposition {
			t.Fatalf("%s disposition = %q, want %q", intention, got.Disposition, disposition)
		}
		if !slices.Equal(got.NativeEvents, wantEvents) {
			t.Fatalf("%s native events = %v, want %v", intention, got.NativeEvents, wantEvents)
		}
		return
	}
	t.Fatalf("compiled plan missing intention %q: %+v", intention, compiled.Intentions)
}

func compiledIntention(
	t *testing.T,
	compiled CompiledPosture,
	intention HarnessIntention,
) CompiledIntention {
	t.Helper()
	for _, got := range compiled.Intentions {
		if got.Intention == intention {
			return got
		}
	}
	t.Fatalf("compiled plan missing intention %q", intention)
	return CompiledIntention{}
}

func testClaudeAttestation() HarnessAttestation {
	return HarnessAttestation{
		Harness:          "claude-code",
		Family:           "claude",
		Surface:          "settings.json",
		Version:          "fixture",
		FeatureState:     "trusted",
		ConfigScope:      "project",
		Revision:         "sha256:claude-fixture",
		FallbackRevision: "none",
		Features: map[string]bool{
			"session_start_context": true,
			"user_prompt_context":   true,
			"pre_tool_use_block":    true,
			"post_tool_use_event":   true,
		},
	}
}

// TestCompileDefaultPostureAttestationUsesObservedVersion pins the contract
// that CompileDefaultPosture stamps the OBSERVED installed version into
// HarnessAttestation.Version when ObservedVersion returns a non-empty
// string. Before this change the attestation carried "registry:<name>" — an
// empty socket that lost the version signal entirely. This test pins the
// falsifier: an ObservedVersion return of "0.7.4" must surface verbatim.
//
// We exercise through codex because CompileDefaultPosture requires a known
// semantic adapter (claude/codex/opencode) — prime_agent itself has no
// semantic adapter yet, but the contract under test is the attestation
// stamping path which is identical for every registry name. The observedFn
// seam lets us simulate the prime_agent path: the harness arg drives the
// default registry lookup, the observedFn returns whatever the binary probe
// would have surfaced.
func TestCompileDefaultPostureAttestationUsesObservedVersion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		harnessName string
		observed    string
		wantVersion string
	}{
		{
			name:        "observed version surfaces verbatim",
			harnessName: "codex",
			observed:    "0.7.4",
			wantVersion: "0.7.4",
		},
		{
			name:        "unobservable falls back to registry socket",
			harnessName: "codex",
			observed:    "",
			wantVersion: "registry:codex",
		},
		{
			name:        "different observed still surfaces verbatim",
			harnessName: "codex",
			observed:    "0.7.1",
			wantVersion: "0.7.1",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			observedVersionFn := func(name string) string {
				if name == c.harnessName {
					return c.observed
				}
				return ""
			}

			compiled, err := compileDefaultPosture(DefaultPostureRequest{
				Harness:         c.harnessName,
				Registry:        harness.DefaultRegistry(),
				HandlerRef:      "compound",
				HandlerRevision: "sha256:handler-fixture",
			}, observedVersionFn)
			if err != nil {
				t.Fatal(err)
			}
			if got := compiled.CompiledFrom.HarnessVersion; got != c.wantVersion {
				t.Fatalf("HarnessVersion = %q, want %q", got, c.wantVersion)
			}
		})
	}
}
