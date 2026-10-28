package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/runattempt"
)

func TestClaudeLaneProfileCannotCrossHarness(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "registries", "model-capabilities.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	body := `version: 1
capabilities: [build-implement]
models:
  fixture-other:
    lane: fixture-other
    harness: claude_minimax
    provider_route: fixture-provider
    tier: cheap
    packet_ceiling: multi-file
    aliases: [fixture-other]
    capabilities:
      build-implement: {level: capable, evidence: fixture}
routing_defaults:
  build-implement: [fixture-other]
`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	adapter := newClaudeLaneAdapter(runattempt.Open(home), "claude_qwen", time.Second)
	_, err := adapter.Preflight(context.Background(), RunInput{WorkID: "work_profile_harness", LaunchProfileID: "fixture-other", Workspace: home, Now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "belongs to harness") {
		t.Fatalf("cross-harness profile was not refused: %v", err)
	}
	attempts, err := runattempt.Open(home).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("cross-harness preflight minted attempts: %+v", attempts)
	}
}
