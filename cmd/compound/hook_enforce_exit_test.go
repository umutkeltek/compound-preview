package main

import (
	"testing"

	"compound/internal/envelope"
	"compound/internal/hook"
)

func TestHookEnforceExitDistinguishesUnavailableContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		hookName   string
		status     string
		failReason string
		want       int
	}{
		{
			name:       "context unavailable has distinct stop code",
			hookName:   "pre-tool-use",
			status:     "fail_open",
			failReason: envelope.FailOpenContextUnavailable,
			want:       4,
		},
		{
			name:       "capability absent still stops enforcement wrapper",
			hookName:   "pre-tool-use",
			status:     "fail_open",
			failReason: envelope.FailOpenCapabilityAbsent,
			want:       1,
		},
		{
			name:     "legacy fail open still stops enforcement wrapper",
			hookName: "pre-tool-use",
			status:   "fail_open",
			want:     1,
		},
		{
			name:     "blocked still stops enforcement wrapper",
			hookName: "pre-tool-use",
			status:   "blocked",
			want:     1,
		},
		{
			name:       "non-enforcement hook never maps context failure to process exit",
			hookName:   "user-prompt-submit",
			status:     "fail_open",
			failReason: envelope.FailOpenContextUnavailable,
			want:       0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hook.Result{Event: envelope.HookEvent{
				Status:         tt.status,
				FailOpenReason: tt.failReason,
			}}
			if got := hookEnforceExit(tt.hookName, result); got != tt.want {
				t.Fatalf("hookEnforceExit(%q, status=%q, reason=%q) = %d, want %d",
					tt.hookName, tt.status, tt.failReason, got, tt.want)
			}
		})
	}
}
