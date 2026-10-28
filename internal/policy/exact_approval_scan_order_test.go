package policy

import (
	"strconv"
	"testing"
)

// Pick a deterministic fixture whose first grant sorts before its successor.
// Binding-schema additions change hashes; both callers still assert actual
// stored grant order, proving Scan must encounter the stale grant first.
func orderedExactApprovalShadowRequest(t *testing.T, cfg Config, label string) ToolActionRequest {
	t.Helper()
	for n := 0; n < 100; n++ {
		request := monitorRequest("browser.submit", map[string]any{"target": "publish", "form": label + "-" + strconv.Itoa(n)})
		digest := ActionBindingDigest(MonitorToolAction(request, cfg, fixedNow).Binding)
		firstPlan := "approval_plan_" + shortActionHash(digest, "1")
		nextPlan := "approval_plan_" + shortActionHash(digest, "2")
		firstGrant := "approval_grant_" + shortActionHash(firstPlan, digest)
		nextGrant := "approval_grant_" + shortActionHash(nextPlan, digest)
		if firstGrant < nextGrant {
			return request
		}
	}
	t.Fatal("cannot construct stale-before-ready scan fixture")
	return ToolActionRequest{}
}
