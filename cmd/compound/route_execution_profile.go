package main

import (
	routeevidencecli "compound/internal/routeevidence/cli"
)

// tryRouteExecutionProfileCommand is the thin dispatch onto the route
// evidence CLI organ (internal/routeevidence/cli). The body lives in the
// organ — mirroring the promotedriver pattern — so cmd/compound stays a thin
// dispatch layer under its LOC budget ratchet
// (internal/arch.checkCmdCompoundBudget).
func tryRouteExecutionProfileCommand(args []string) bool {
	return routeevidencecli.TryExecutionProfileCommand(args)
}
