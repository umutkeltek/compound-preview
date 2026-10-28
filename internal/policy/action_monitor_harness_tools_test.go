package policy

import "testing"

func TestClassifyToolActionKnowsHarnessNativeTools(t *testing.T) {
	tests := []struct {
		name         string
		tool         string
		wantDomain   string
		wantMutation string
	}{
		{"agent fan-out", "Agent", ActionDomainControl, "local_reversible"},
		{"flattened codex agent fan-out", "collaborationspawn_agent", ActionDomainControl, "local_reversible"},
		{"flattened codex agent list", "collaborationlist_agents", ActionDomainControl, "read"},
		{"flattened codex agent wait", "collaborationwait_agent", ActionDomainControl, "read"},
		{"flattened codex agent message", "collaborationsend_message", ActionDomainControl, "local_reversible"},
		{"deferred tool search", "ToolSearch", ActionDomainControl, "read"},
		{"task create", "TaskCreate", ActionDomainControl, "local_reversible"},
		{"task update", "TaskUpdate", ActionDomainControl, "local_reversible"},
		{"task list", "TaskList", ActionDomainControl, "read"},
		{"task get", "TaskGet", ActionDomainControl, "read"},
		{"task output", "TaskOutput", ActionDomainControl, "read"},
		{"task stop", "TaskStop", ActionDomainControl, "local_reversible"},
		{"message another agent", "SendMessage", ActionDomainControl, "local_reversible"},
		{"monitor", "Monitor", ActionDomainControl, "read"},
		{"lsp lookup", "LSP", ActionDomainControl, "read"},
		{"enter plan mode", "EnterPlanMode", ActionDomainControl, "local_reversible"},
		{"exit plan mode", "ExitPlanMode", ActionDomainControl, "local_reversible"},
		{"enter worktree", "EnterWorktree", ActionDomainControl, "local_reversible"},
		{"exit worktree", "ExitWorktree", ActionDomainControl, "local_reversible"},
		{"end conversation", "EndConversation", ActionDomainControl, "local_reversible"},
		{"ask operator", "AskUserQuestion", ActionDomainHarness, "read"},
		{"snake ask operator", "ask_user_question", ActionDomainHarness, "read"},
		{"session todo write", "TodoWrite", ActionDomainHarness, "read"},
		{"session todo read", "TodoRead", ActionDomainHarness, "read"},
		{"notebook edit", "NotebookEdit", ActionDomainFilesystem, "local_reversible"},
		{"web fetch", "WebFetch", ActionDomainNetwork, "read"},
		{"web search", "WebSearch", ActionDomainNetwork, "read"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyToolAction(ToolActionRequest{Tool: tt.tool})
			if !got.Known {
				t.Fatalf("%q classified unknown: %+v", tt.tool, got)
			}
			if got.Domain != tt.wantDomain {
				t.Fatalf("%q domain = %q, want %q (%+v)", tt.tool, got.Domain, tt.wantDomain, got)
			}
			if got.MutationClass != tt.wantMutation {
				t.Fatalf("%q mutation = %q, want %q (%+v)", tt.tool, got.MutationClass, tt.wantMutation, got)
			}
		})
	}
}

func TestClassifyToolActionKeepsSideEffectHarnessToolsFailClosed(t *testing.T) {
	// Scheduling, notification, and remote-trigger tools carry host or outbound
	// side effects; they must stay unknown (fail closed) until each gets an
	// explicit domain classification.
	for _, tool := range []string{
		"CronCreate",
		"CronDelete",
		"PushNotification",
		"RemoteTrigger",
		"DesignSync",
		"collaborationremote_trigger",
	} {
		got := ClassifyToolAction(ToolActionRequest{Tool: tool})
		if got.Known {
			t.Fatalf("%q unexpectedly known: %+v", tool, got)
		}
	}
}
