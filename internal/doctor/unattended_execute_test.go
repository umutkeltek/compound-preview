package doctor

// R3A slice 2 deliverable 6: `compound doctor` prints one plain line per
// lane named by the route plan for work.continue.unattended. Tests pin:
//   - the line shape (`unattended execute: <lane> capacity <N>% (<age>),
//     route <eligible|avoid>, grant <present|absent>`);
//   - the eligibility verdict (gate_role eligible vs avoid from the seeded
//     manifest);
//   - the grant presence (with the operator's standing-grants file vs
//     without);
//   - the no-route-plan fallback (single explicit line, not silence).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/routeevidence"
)

// seedRoutePlanForDoctorTest writes a minimal model-capabilities manifest
// to the test home's registries dir. claimLevel is "capable" (gate_role
// eligible) or "avoid" (gate_role avoid).
func seedRoutePlanForDoctorTest(t *testing.T, homePath, claimLevel string) {
	t.Helper()
	body := "version: 1\n" +
		"purpose: doctor unattended-execute fixture\n" +
		"models:\n" +
		"  claude-minimax (token plan):\n" +
		"    lane: claude-minimax (token plan)\n" +
		"    harness: claude_minimax\n" +
		"    tier: cheap\n" +
		"    packet_ceiling: multi-file\n" +
		"    capabilities:\n" +
		"      work.continue.unattended:\n" +
		"        level: " + claimLevel + "\n" +
		"        evidence: doctor fixture\n" +
		"routing_defaults:\n" +
		"  work.continue.unattended:\n" +
		"    - claude-minimax (token plan)\n"
	dir := filepath.Join(homePath, "registries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model-capabilities.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedUnattendedGrantForDoctorTest writes the operator's standing-grants
// file with grant_unattended_work_continue_v1 present. Caller passes
// present=true to install the grant; false leaves the file absent
// (fail-closed default).
func seedUnattendedGrantForDoctorTest(t *testing.T, homePath string, present bool) {
	t.Helper()
	if !present {
		return
	}
	dir := filepath.Join(homePath, "registries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema_version":"compound.standing_grants.v0.1","grants":[{` +
		`"id":"grant_unattended_work_continue_v1","capability":"work.continue.unattended",` +
		`"mutation_class":"local_reversible","reversibility_ceiling":"reversible",` +
		`"blast_radius":"local","target_path":"` + homePath + `/**","expires_at":"2099-01-01T00:00:00Z"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "standing-grants.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeCapacityReader returns a stub reader that reports 30% used and fresh
// for the named lane. Tests inject this through CapacityReader so the doctor
// reads deterministic values without a real CodexBar feed.
func fakeCapacityReader(used float64, fresh bool) CapacityReader {
	return func(homePath, lane string, now time.Time) (float64, bool, error) {
		return used, fresh, nil
	}
}

// TestDoctorUnattendedExecutePrintsEligibleLaneLine: a route plan with an
// eligible lane and the grant installed prints the canonical line shape.
func TestDoctorUnattendedExecutePrintsEligibleLaneLine(t *testing.T) {
	homePath := t.TempDir()
	now := time.Now().UTC()
	seedRoutePlanForDoctorTest(t, homePath, "capable")
	seedUnattendedGrantForDoctorTest(t, homePath, true)

	rows := BuildUnattendedExecuteRows(homePath, now, fakeCapacityReader(30, true))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1; rows=%+v", len(rows), rows)
	}
	if rows[0].Lane != "claude_minimax" {
		t.Fatalf("lane = %q, want claude_minimax", rows[0].Lane)
	}
	if rows[0].RouteGate != "eligible" {
		t.Fatalf("route_gate = %q, want eligible", rows[0].RouteGate)
	}
	if !rows[0].GrantPresent {
		t.Fatal("grant must read present when the operator's file is installed")
	}
	if !rows[0].CapacityFresh {
		t.Fatalf("capacity must read fresh (fake reader); row=%+v", rows[0])
	}

	var buf bytes.Buffer
	PrintUnattendedExecute(&buf, rows)
	out := buf.String()
	if !strings.Contains(out, "unattended execute: claude_minimax capacity 30% (") {
		t.Fatalf("doctor line must match the canonical shape; got %q", out)
	}
	if !strings.Contains(out, "route eligible") {
		t.Fatalf("doctor line must report route eligible; got %q", out)
	}
	if !strings.Contains(out, "grant present") {
		t.Fatalf("doctor line must report grant present; got %q", out)
	}
	_ = routeevidence.DefaultLaneCapacityConfig // satisfy the routeevidence import if a future row needs it
	_ = fmt.Sprint
}

// TestDoctorUnattendedExecuteMarksAvoidAndAbsentGrant: an avoid-gate-role
// lane plus no grant both surface explicit states; the row is not dropped.
func TestDoctorUnattendedExecuteMarksAvoidAndAbsentGrant(t *testing.T) {
	homePath := t.TempDir()
	now := time.Now().UTC()
	seedRoutePlanForDoctorTest(t, homePath, "avoid")
	seedUnattendedGrantForDoctorTest(t, homePath, false)

	rows := BuildUnattendedExecuteRows(homePath, now, fakeCapacityReader(30, true))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].RouteGate != "avoid" {
		t.Fatalf("route_gate = %q, want avoid", rows[0].RouteGate)
	}
	if rows[0].GrantPresent {
		t.Fatal("grant must read absent when the operator's file is not installed")
	}
	if len(rows[0].Notes) == 0 {
		t.Fatal("avoid + absent grant must surface at least one explanatory note")
	}
}

// TestDoctorUnattendedExecuteEmptyStateIsExplicit: when the manifest names
// no lane for work.continue.unattended the section still renders one
// explicit line — silence would let an operator think the seam was
// simply unconfigured.
func TestDoctorUnattendedExecuteEmptyStateIsExplicit(t *testing.T) {
	homePath := t.TempDir()
	rows := BuildUnattendedExecuteRows(homePath, time.Now().UTC(), fakeCapacityReader(0, false))
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 on an empty manifest", len(rows))
	}
	var buf bytes.Buffer
	PrintUnattendedExecute(&buf, rows)
	if !strings.Contains(buf.String(), "no route plan names a lane") {
		t.Fatalf("empty doctor section must be explicit; got %q", buf.String())
	}
}

// TestDoctorUnattendedExecuteJSONShapeMatchesText: the JSON shape mirrors
// the plain-text section (a future operator dashboard reads the same
// schema) — pin the keys so a future rename of the route_gate field
// shows up here rather than as a silent dashboard regression.
func TestDoctorUnattendedExecuteJSONShapeMatchesText(t *testing.T) {
	homePath := t.TempDir()
	now := time.Now().UTC()
	seedRoutePlanForDoctorTest(t, homePath, "capable")
	seedUnattendedGrantForDoctorTest(t, homePath, true)

	rows := BuildUnattendedExecuteRows(homePath, now, fakeCapacityReader(30, true))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	encoded, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lane", "route_gate", "capacity_percent", "capacity_fresh", "grant_present"} {
		if !strings.Contains(string(encoded), key) {
			t.Fatalf("doctor JSON shape must keep %q; got %s", key, string(encoded))
		}
	}
}
