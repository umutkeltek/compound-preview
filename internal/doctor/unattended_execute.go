package doctor

// Doctor section: unattended execute — one plain line per lane the route plan
// names for work.continue.unattended. The line tells the operator at a glance
// (a) whether a lane is even eligible, (b) what its current capacity reading
// is and how stale, and (c) whether the operator's grant is present. Fail
// closed everywhere: missing route plan, missing lane, missing grant, and
// missing capacity row all render explicit states rather than dropping the
// row silently.
//
// Wiring:
//   - routeevidence.LoadModelCapabilitiesSnapshot for the route plan
//   - autonomyproof.GateRole for the eligible/avoid verdict
//   - a capacity snapshot reader (the production wire calls
//     executeStageCapacityFromSnapshot; this module does not couple to that
//     name so the doctor can run without the execute stage package being
//     initialized)
//   - ~/.compound/registries/standing-grants.json for the grant presence
//
// The check is read-only and writes nothing — the operator runs
// `compound policy import-standing-grants` (not the doctor) to flip grant
// status, and `compound capacity freshness <lane>` (also not the doctor) to
// see the freshness window in isolation.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/routeevidence"
	autonomyproof "compound/internal/routeevidence/autonomy"
)

const unattendedExecuteCapability = "work.continue.unattended"

// UnattendedExecuteLane is one row in the doctor "unattended execute"
// section: route eligible/avoid verdict, capacity reading + age, grant
// presence. JSON-tagged so the doctor's `--json` shape stays consistent
// with every other section (a future operator dashboard reads the same
// schema).
type UnattendedExecuteLane struct {
	Lane            string   `json:"lane"`
	RouteGate       string   `json:"route_gate"` // eligible | avoid | undeclared
	CapacityPercent float64  `json:"capacity_percent,omitempty"`
	CapacityFresh   bool     `json:"capacity_fresh"`
	CapacityAge     string   `json:"capacity_age,omitempty"` // human-friendly ("2m0s", "16m0s")
	GrantPresent    bool     `json:"grant_present"`
	Notes           []string `json:"notes,omitempty"`
}

// CapacityReader is the seam this package uses to ask a lane's live capacity.
// Production callers wire it to the execute stage's capacity hook (the
// reader fed by CodexBar's usage row); tests substitute a stub. The
// signature mirrors the seam-level fn so a one-line wrapper keeps the
// production contract and the doctor test seam identical.
type CapacityReader func(homePath, lane string, now time.Time) (usedPercent float64, fresh bool, err error)

// BuildUnattendedExecuteRows walks the route plan, asks the supplied
// capacity reader for a reading, and returns one row per lane the manifest
// names for work.continue.unattended. A missing manifest, an unreadable
// grant registry, or an unreadable capacity feed all surface as explicit
// values on the row (never as a dropped lane) so the operator sees the
// real reason a tick would refuse.
func BuildUnattendedExecuteRows(homePath string, now time.Time, capacity CapacityReader) []UnattendedExecuteLane {
	if capacity == nil {
		return nil
	}
	manifest, _, _, err := routeevidence.LoadModelCapabilitiesSnapshot(homePath)
	if err != nil {
		return nil
	}
	if len(manifest.RoutingDefaults[unattendedExecuteCapability]) == 0 {
		return nil
	}
	grantPresent := HasUnattendedGrant(homePath)
	var rows []UnattendedExecuteLane
	for _, modelID := range manifest.RoutingDefaults[unattendedExecuteCapability] {
		entry, ok := manifest.Models[modelID]
		if !ok {
			continue
		}
		claim := entry.Capabilities[unattendedExecuteCapability]
		gate := autonomyproof.GateRole(unattendedExecuteCapability, entry.Tier, strings.TrimSpace(claim.Level))
		harnessName := strings.TrimSpace(entry.Harness)
		if harnessName == "" {
			continue
		}
		row := UnattendedExecuteLane{
			Lane:         harnessName,
			RouteGate:    gate,
			GrantPresent: grantPresent,
		}
		// Capacity reading — pulled from the supplied reader so the
		// doctor's verdict matches what the seam would observe at the
		// next tick. A read error does not drop the row; the row
		// surfaces fresh=false and a note.
		used, fresh, capErr := capacity(homePath, harnessName, now)
		if capErr != nil {
			row.CapacityFresh = false
			row.Notes = append(row.Notes, "capacity read error: "+capErr.Error())
		} else {
			row.CapacityPercent = used
			row.CapacityFresh = fresh
			// Recompute age off the snapshot's lastProven directly so
			// the doctor surfaces an exact duration (the seam does not
			// surface the age, just the freshness boolean).
			if cap, capErr := routeevidence.ReadLaneCapacityWithConfig(harnessName, now, routeevidence.DefaultLaneCapacityConfig(homePath)); capErr == nil {
				if !cap.LastProven.IsZero() {
					row.CapacityAge = now.Sub(cap.LastProven).Round(time.Second).String()
				}
			}
		}
		if !row.CapacityFresh && row.CapacityAge == "" {
			row.Notes = append(row.Notes, "capacity snapshot missing or stale; seam will refuse with capacity_unknown")
		}
		if gate != "eligible" {
			row.Notes = append(row.Notes, "route gate_role is "+gate+"; seam will refuse with no_route")
		}
		if !grantPresent {
			row.Notes = append(row.Notes, "grant grant_unattended_work_continue_v1 absent; seam will refuse with no_grant")
		}
		rows = append(rows, row)
	}
	return rows
}

// HasUnattendedGrant reads the operator's standing-grants registry and
// reports whether the unattended grant is present and not expired. A
// missing registry file is fail-closed (no grant).
func HasUnattendedGrant(homePath string) bool {
	if strings.TrimSpace(homePath) == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(homePath, "registries", "standing-grants.json"))
	if err != nil {
		return false
	}
	var registry struct {
		Grants []struct {
			ID         string `json:"id"`
			Capability string `json:"capability"`
			ExpiresAt  string `json:"expires_at"`
		} `json:"grants"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return false
	}
	for _, g := range registry.Grants {
		if g.ID != "grant_unattended_work_continue_v1" || g.Capability != unattendedExecuteCapability {
			continue
		}
		if g.ExpiresAt == "" {
			return true
		}
		exp, err := time.Parse(time.RFC3339, g.ExpiresAt)
		if err != nil {
			return false
		}
		return !time.Now().UTC().After(exp)
	}
	return false
}

// PrintUnattendedExecute renders the doctor section. Empty state prints
// one explicit line so the operator knows the manifest had no eligible
// lane (the alternative — silent absence — would let them think the seam
// was simply unconfigured).
func PrintUnattendedExecute(stdout io.Writer, rows []UnattendedExecuteLane) {
	fmt.Fprintln(stdout, "\n## unattended execute")
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "- (no route plan names a lane for work.continue.unattended)")
		return
	}
	for _, r := range rows {
		capacity := "unknown"
		if r.CapacityFresh {
			capacity = fmt.Sprintf("%d%%", int(r.CapacityPercent))
		} else {
			capacity = "stale"
		}
		age := r.CapacityAge
		if age == "" {
			age = "n/a"
		}
		grant := "absent"
		if r.GrantPresent {
			grant = "present"
		}
		fmt.Fprintf(stdout, "- unattended execute: %s capacity %s (%s), route %s, grant %s\n",
			r.Lane, capacity, age, r.RouteGate, grant)
		for _, note := range r.Notes {
			fmt.Fprintf(stdout, "    note: %s\n", note)
		}
	}
}
