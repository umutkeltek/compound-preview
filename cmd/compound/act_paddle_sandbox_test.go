package main

import (
	"bytes"
	"testing"
)

func TestRunActDispatchesPaddleSandboxNotification(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", "")
	var stdout, stderr bytes.Buffer
	code := runAct([]string{
		"paddle-sandbox-notification",
		"--setting", "ntfset_01234567890123456789012345",
		"--to-url", "https://sandbox.shop.invalid/webhooks/paddle",
		"--work", "work_dispatch_fixture",
		"--actor", "codex:fixture",
	}, &stdout, &stderr)
	if code != 1 || stderr.String() != "error: PADDLE_SANDBOX_API_KEY is required\n" {
		t.Fatalf("dispatch exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
