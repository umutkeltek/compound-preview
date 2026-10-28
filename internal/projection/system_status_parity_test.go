package projection

import (
	"encoding/json"
	"testing"
	"time"
)

// osStatusKeyDisposition records, for one top-level key of os.ts's self-computed
// snapshot, whether the system-status projection folds it, excludes it, or defers
// it — with a one-line reason.
type osStatusKeyDisposition struct {
	Key         string
	Disposition string
	Reason      string
}

const (
	// folded: the key is present in the system-status projection (a section or a
	// top-level field).
	dispFolded = "folded"
	// excludedLegacy: no compound authority backs the key, or it retires with
	// gbrain / the os doctor probes — a documented, intentional drop.
	dispExcludedLegacy = "excluded_legacy"
	// deferredCompoundAuthority: a REAL compound authority os.ts folds, but outside
	// W5 J4's named five-authority scope (and, for capgraph-hosted ones, blocked by
	// the projection→capgraph import cycle). J8 must fold these before os.ts's
	// snapshot can fully retire — this is the checklist.
	deferredCompoundAuthority = "deferred_compound_authority"
)

// osStatusSnapshotKeys is the FIELD-PARITY FIXTURE (W5 J4 falsifier): every
// top-level key os.ts writes to the legacy operator OS's dashboard/os-status-latest.json, as
// live-probed 2026-07-11. Enumerated by reading both the os.ts snapshot assembly
// (probe* functions writing onto a shared record, write site os.ts:4351) and the
// on-disk dashboard file (22 keys, confirmed consistent). If os.ts grows a key,
// re-run the recon and add it here with a disposition — the test guards the
// count, so an unaccounted key is caught.
var osStatusSnapshotKeys = []osStatusKeyDisposition{
	// --- folded (W5 J4's five named authorities + projection bookkeeping) ---
	{"workbench", dispFolded, "internal/work read model -> sections.workbench"},
	{"job_roster", dispFolded, "internal/authority job-roster authority -> sections.job_roster"},
	{"route_evidence", dispFolded, "internal/routeevidence authority -> sections.route_evidence"},
	{"product_authority", dispFolded, "product-feedback ledger counts -> sections.product_authority"},
	{"capability_authority", dispFolded, "capability-graph registry counts -> sections.capability_authority"},
	{"generated_at", dispFolded, "projection.generated_at"},
	{"verdict", dispFolded, "projection.verdict (fold-scoped, not the full os doctor verdict)"},

	// --- excluded_legacy (no compound authority, or retires with gbrain/os-doctor) ---
	{"ledger_24h", dispExcludedLegacy, "os.ts parses the OS provider-ledger jsonl directly; cost/route data is owned by route_evidence"},
	{"product_feedback", dispExcludedLegacy, "os.ts-self-computed subset alias of product_authority; no independent source"},
	{"memory_engine_doctor", dispExcludedLegacy, "gbrain-backed sidecar doctor; retires with gbrain (W5 R4)"},
	{"lines", dispExcludedLegacy, "free-text health-probe sink (daemons/switchback/gbrain/brain-git); no authority equivalent"},
	{"counts", dispExcludedLegacy, "os.ts derives fail/warn tallies from lines; not an authority fold"},
	{"doctor_inherited_verdict", dispExcludedLegacy, "derived from lines/os-doctor snapshot; not an authority fold"},

	// --- deferred_compound_authority (real compound authority, outside J4 scope; J8 folds before os.ts fully retires) ---
	{"event_trace_authority", deferredCompoundAuthority, "compound://events authority; fold in J8/follow-up"},
	{"tool_boundary_doctor", deferredCompoundAuthority, "capgraph doctor (import cycle projection->capgraph); fold via file read in J8"},
	{"external_repo_intake", deferredCompoundAuthority, "capgraph review authority; fold in J8/follow-up"},
	{"skill_source_doctor", deferredCompoundAuthority, "capgraph doctor; fold in J8/follow-up"},
	{"review_queue", deferredCompoundAuthority, "reviews authority; fold in J8/follow-up"},
	{"release_goal_holds", deferredCompoundAuthority, "policy authority; fold in J8/follow-up"},
	{"operator_state", deferredCompoundAuthority, "policy authority; fold in J8/follow-up"},
	{"placement_rules", deferredCompoundAuthority, "policy authority; fold in J8/follow-up"},
	{"standing_grants", deferredCompoundAuthority, "policy authority; fold in J8/follow-up"},
}

// TestSystemStatusOSFieldParity is the J4 falsifier: it proves every os.ts
// snapshot field is accounted for — present in the projection (folded) or
// documented as excluded/deferred with a reason. A folded key missing from the
// projection reds; a new projection section not documented in the fixture reds.
func TestSystemStatusOSFieldParity(t *testing.T) {
	// 1. the fixture is well-formed and accounts for exactly the 22 os.ts keys.
	seen := map[string]bool{}
	for _, k := range osStatusSnapshotKeys {
		if seen[k.Key] {
			t.Fatalf("duplicate os.ts key in parity fixture: %s", k.Key)
		}
		seen[k.Key] = true
		if k.Reason == "" {
			t.Errorf("os.ts key %q has no disposition reason (a silent drop is not honest)", k.Key)
		}
		switch k.Disposition {
		case dispFolded, dispExcludedLegacy, deferredCompoundAuthority:
		default:
			t.Errorf("os.ts key %q has unknown disposition %q", k.Key, k.Disposition)
		}
	}
	const wantOSKeys = 22
	if len(seen) != wantOSKeys {
		t.Fatalf("parity fixture accounts for %d os.ts keys, want %d (re-run the os.ts recon if the snapshot changed)", len(seen), wantOSKeys)
	}

	// 2. build the projection and index its top-level + section keys.
	proj, err := BuildSystemStatus(t.TempDir(), time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	raw, err := json.Marshal(proj)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal projection: %v", err)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(doc["sections"], &sections); err != nil {
		t.Fatalf("unmarshal sections: %v", err)
	}
	present := func(key string) bool {
		if _, ok := doc[key]; ok { // top-level field (generated_at, verdict)
			return true
		}
		_, ok := sections[key] // folded section
		return ok
	}

	// 3. FALSIFIER: every folded os.ts key must be present in the projection.
	folded := map[string]bool{}
	for _, k := range osStatusSnapshotKeys {
		if k.Disposition != dispFolded {
			continue
		}
		folded[k.Key] = true
		if !present(k.Key) {
			t.Errorf("os.ts field %q is marked folded but is MISSING from the system-status projection", k.Key)
		}
	}

	// 4. reverse parity: every projection section must be a documented folded key —
	//    a new section without a fixture entry is undocumented drift.
	for key := range sections {
		if !folded[key] {
			t.Errorf("projection section %q is not documented as a folded os.ts key in the parity fixture", key)
		}
	}

	// 5. honesty guard: exclusions/deferrals are documented, not empty.
	var excluded, deferred int
	for _, k := range osStatusSnapshotKeys {
		switch k.Disposition {
		case dispExcludedLegacy:
			excluded++
		case deferredCompoundAuthority:
			deferred++
		}
	}
	if excluded == 0 || deferred == 0 {
		t.Errorf("parity fixture must document excluded(%d) and deferred(%d) os.ts keys", excluded, deferred)
	}
}
