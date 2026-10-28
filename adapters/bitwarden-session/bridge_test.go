package bitwardensession

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestSharedVaultBridgeAndInstaller(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS Keychain adapter")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "test_bridge.py")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native bridge regression failed: %v\n%s", err, output)
	}
}
