package hook

import (
	"strings"
	"testing"
	"time"

	"compound/internal/tooltopology"
)

func doorMapHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if _, err := tooltopology.Refresh(home, tooltopology.Inputs{
		DoorsJSON:       []byte(`[{"door":"web","status":"PASS","detail":"ok","use":"search-cli \"<q>\""},{"door":"gui","status":"HUMAN","detail":"needs a grant"}]`),
		DoorsObservedAt: fixedNow.Add(-time.Hour),
	}, fixedNow); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLiveSessionStartServesDoorMapBlock(t *testing.T) {
	home := doorMapHome(t)
	deps := baseDeps()
	deps.HomePath = home
	res, err := Run(Fixture{Hook: "session-start", Harness: "codex", Cwd: "/repo/productX", RepoKind: "product", Live: true}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.ContextOutput, "## Door map") || !strings.Contains(res.ContextOutput, "PASS web: search-cli") || !strings.Contains(res.ContextOutput, "HUMAN gui") {
		t.Fatalf("live session-start must carry the door map block, got %q", res.ContextOutput)
	}
	if len(res.ContextOutput) > tooltopology.MaxServedBytes {
		t.Fatalf("block is %d bytes, cap %d", len(res.ContextOutput), tooltopology.MaxServedBytes)
	}
}

func TestLiveSessionStartWithoutSnapshotSaysSo(t *testing.T) {
	deps := baseDeps()
	deps.HomePath = t.TempDir()
	res, err := Run(Fixture{Hook: "session-start", Harness: "codex", Cwd: "/repo/productX", RepoKind: "product", Live: true}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.ContextOutput, "Door map: none produced yet") {
		t.Fatalf("a missing snapshot must be visible, got %q", res.ContextOutput)
	}
}

func TestFixtureReplaySessionStartStaysFreeOfDoorMap(t *testing.T) {
	deps := baseDeps()
	deps.HomePath = doorMapHome(t)
	res, err := Run(Fixture{Hook: "session-start", Harness: "codex", Cwd: "/repo/productX", RepoKind: "product"}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.ContextOutput != "" {
		t.Fatalf("fixture replay must not depend on machine state, got %q", res.ContextOutput)
	}
}
