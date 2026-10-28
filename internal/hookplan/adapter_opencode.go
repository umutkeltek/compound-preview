package hookplan

import "compound/internal/harness"

type openCodeSemanticAdapter struct{}

func (openCodeSemanticAdapter) Compile(
	intention HarnessIntention,
	attestation HarnessAttestation,
) (CompiledIntention, []nativeBinding) {
	switch intention {
	case IntentionPrepareContext:
		if !attestation.Features["chat_message_context"] {
			return unsupportedIntention(intention, "chat.message context injection is not attested"), nil
		}
		return CompiledIntention{
			Intention:           intention,
			Disposition:         DispositionNative,
			EnforcementStrength: "context",
			NativeEvents:        []string{"chat.message"},
		}, []nativeBinding{{event: "chat.message"}}
	case IntentionGuardMutation:
		if attestation.Features["before_tool_can_deny"] {
			return CompiledIntention{
				Intention:           intention,
				Disposition:         DispositionNative,
				EnforcementStrength: "blocking",
				NativeEvents:        []string{"tool.execute.before"},
			}, []nativeBinding{{event: "tool.execute.before", matcher: "*"}}
		}
		return unsupportedIntention(
			intention,
			"tool.execute.before cannot deny execution; no blocking fallback is attested",
		), nil
	case IntentionObserveOutcome:
		if !attestation.Features["after_tool_observation"] {
			return unsupportedIntention(intention, "tool.execute.after observation is not attested"), nil
		}
		return CompiledIntention{
			Intention:           intention,
			Disposition:         DispositionNative,
			EnforcementStrength: "observe",
			NativeEvents:        []string{"tool.execute.after"},
		}, []nativeBinding{{event: "tool.execute.after", matcher: "*"}}
	default:
		return unsupportedIntention(intention, "OpenCode adapter does not implement this lifecycle intention"), nil
	}
}

func (openCodeSemanticAdapter) Attest(capabilities harness.Capabilities) map[string]bool {
	return map[string]bool{
		"chat_message_context":   capabilities.SessionStartContext || capabilities.UserPromptContext,
		"before_tool_can_deny":   capabilities.PreToolUseBlock,
		"after_tool_observation": capabilities.PostToolUseEvent,
	}
}
