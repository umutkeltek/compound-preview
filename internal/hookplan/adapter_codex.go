package hookplan

import "compound/internal/harness"

type codexSemanticAdapter struct{}

func (codexSemanticAdapter) Compile(
	intention HarnessIntention,
	attestation HarnessAttestation,
) (CompiledIntention, []nativeBinding) {
	switch intention {
	case IntentionPrepareContext:
		if !attestation.Features["session_start_context"] {
			return unsupportedIntention(intention, "session start context injection is not attested"), nil
		}
		return CompiledIntention{
			Intention:           intention,
			Disposition:         DispositionNative,
			EnforcementStrength: "context",
			NativeEvents:        []string{"SessionStart"},
		}, []nativeBinding{{event: "SessionStart"}}
	case IntentionPrepareTurnContext:
		if !attestation.Features["user_prompt_context"] {
			return unsupportedIntention(intention, "per-turn context injection is not attested"), nil
		}
		return CompiledIntention{
			Intention:           intention,
			Disposition:         DispositionNative,
			EnforcementStrength: "context",
			NativeEvents:        []string{"UserPromptSubmit"},
		}, []nativeBinding{{event: "UserPromptSubmit"}}
	case IntentionGuardMutation:
		if !attestation.Features["pre_tool_use_block"] {
			return unsupportedIntention(intention, "native blocking pre-tool execution is not attested"), nil
		}
		return CompiledIntention{
			Intention:           intention,
			Disposition:         DispositionNative,
			EnforcementStrength: "blocking",
			NativeEvents:        []string{"PreToolUse"},
		}, []nativeBinding{{event: "PreToolUse", matcher: "*"}}
	case IntentionObserveAction:
		if !attestation.Features["pre_tool_use_block"] {
			return unsupportedIntention(intention, "pre-tool action observation is not attested"), nil
		}
		return CompiledIntention{
			Intention:           intention,
			Disposition:         DispositionNative,
			EnforcementStrength: "observe",
			NativeEvents:        []string{"PreToolUse"},
		}, []nativeBinding{{event: "PreToolUse", matcher: "*"}}
	case IntentionObserveOutcome:
		if !attestation.Features["post_tool_use_event"] {
			return unsupportedIntention(intention, "post-tool outcome observation is not attested"), nil
		}
		return CompiledIntention{
			Intention:           intention,
			Disposition:         DispositionNative,
			EnforcementStrength: "observe",
			NativeEvents:        []string{"PostToolUse"},
		}, []nativeBinding{{event: "PostToolUse", matcher: "*"}}
	case IntentionRecordReceipt:
		// Codex 0.149.x exposes a native SessionEnd hook with a transcript_path in the wire
		// payload (binary evidence: `strings /opt/homebrew/Caskroom/codex/0.149.1/bin/codex |
		// grep` shows session_end in the hook event vocabulary and session_id, transcript_path,
		// hook_event_name, stop_hook_active in the wire payload). The handler is the same
		// generic `compound hook session-end --live --harness codex` verb used by Stop (hook.go
		// routes "SessionEnd","Stop" to sessionEnd()); only codex's registry capability was
		// previously false because the receipt semantic was unproven on the binary. The
		// handler distinguishes SessionEnd from Stop via the Stop short-circuit (fx.StopHookActive
		// || fx.StopTriageHandled) at hook.go:2103, so registering SessionEnd for codex here
		// does NOT touch the Stop feedback path or its pinning tests.
		if !attestation.Features["session_end_receipt"] {
			return unsupportedIntention(intention, "session-end receipt is not attested"), nil
		}
		return CompiledIntention{
			Intention:           intention,
			Disposition:         DispositionNative,
			EnforcementStrength: "receipt",
			NativeEvents:        []string{"SessionEnd"},
		}, []nativeBinding{{event: "SessionEnd"}}
	default:
		return unsupportedIntention(intention, "Codex adapter does not implement this lifecycle intention"), nil
	}
}

func (codexSemanticAdapter) Attest(capabilities harness.Capabilities) map[string]bool {
	return map[string]bool{
		"session_start_context": capabilities.SessionStartContext,
		"user_prompt_context":   capabilities.UserPromptContext,
		"pre_tool_use_block":    capabilities.PreToolUseBlock,
		"post_tool_use_event":   capabilities.PostToolUseEvent,
		"session_end_receipt":   capabilities.SessionEndReceipt,
	}
}
