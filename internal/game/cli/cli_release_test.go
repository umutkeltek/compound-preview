package cli

import (
	"strings"
	"testing"
)

func TestCmdGameReleaseRequiresSubcommand(t *testing.T) {
	code, _ := runGameCmd(t, func() int { return cmdGameRelease([]string{}) })
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestCmdGameReleaseRejectsUnknownSubcommand(t *testing.T) {
	code, _ := runGameCmd(t, func() int { return cmdGameRelease([]string{"ship"}) })
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestCmdGameReleasePlanRejectsUnknownFlag(t *testing.T) {
	code, _ := runGameCmd(t, func() int {
		return cmdGameReleasePlan([]string{"--idea-id", "x", "--product-id", "y", "--bogus"})
	})
	if code != 2 {
		t.Errorf("expected exit 2, got %d", code)
	}
}

func TestCmdGameReleasePlanRequiresAllFlags(t *testing.T) {
	cases := []string{
		"--idea-id x --product-id y --change-summary s --rollback r",             // missing --post-release-metric
		"--idea-id x --product-id y --change-summary s --post-release-metric m",  // missing --rollback
		"--idea-id x --product-id y --rollback r --post-release-metric m",        // missing --change-summary
		"--idea-id x --change-summary s --rollback r --post-release-metric m",    // missing --product-id
		"--product-id y --change-summary s --rollback r --post-release-metric m", // missing --idea-id
	}
	for _, args := range cases {
		t.Run(args, func(t *testing.T) {
			code, stderr := runGameCmd(t, func() int {
				return cmdGameReleasePlan(strings.Fields(args))
			})
			if code != 2 {
				t.Errorf("args=%q: expected exit 2, got %d stderr=%s", args, code, stderr)
			}
		})
	}
}
