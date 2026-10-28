package lensmodulator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/act"
	"compound/internal/behaviorallens"
	"compound/internal/eventstore"
	"compound/internal/finding"
	"compound/internal/policy"
)

// These tests drive the GOVERNED MODULATOR — the first organ that ACTS on a recorded
// behavioral-lens signal rather than only observing it. Every proof runs against a
// mktemp COMPOUND_HOME (never a real home) and makes NO provider/network call: signals
// are recorded as synthetic fixtures, and the only mutation is a reversible route/role
// repoint through the governed act seam. The three falsifiers are written to FAIL if the
// modulator's depth is wrong — an ungated write, an uncalibrated arm, or a leaked
// operator_private modulation.

func fixedNow() time.Time { return time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC) }

// mkSignal builds a valid recorded InferredSignal fixture for one lens+subject.
func mkSignal(t *testing.T, lensID, subject string, value float64, vis behaviorallens.Visibility, now time.Time) behaviorallens.InferredSignal {
	t.Helper()
	sig := behaviorallens.InferredSignal{
		SchemaVersion: behaviorallens.SignalSchemaVersion,
		ID:            "blsig_" + lensID + "_" + subject,
		LensID:        lensID,
		Subject:       subject,
		Value:         value,
		Confidence:    0.9,
		Visibility:    vis,
		Method:        "fixture",
		CreatedAt:     now.UTC().Format(time.RFC3339),
	}
	if err := sig.Validate(); err != nil {
		t.Fatalf("test signal invalid: %v", err)
	}
	return sig
}

func recordSignal(t *testing.T, home string, sig behaviorallens.InferredSignal) {
	t.Helper()
	if err := behaviorallens.RecordSignal(home, sig); err != nil {
		t.Fatalf("record signal: %v", err)
	}
}

// grantConfig returns the built-in policy plus ONE StandingGrant scoped to the demote
// capability, external_side_effect class, easy reversibility ceiling, local blast radius,
// and the exact route-store path — the D3 "set the envelope" shape. This is the operator's
// explicit pre-authorization: WITHOUT it the demote holds and act refuses.
func grantConfig(home string) policy.Config {
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID:                   "grant_lens_modulate",
		Capability:           CapabilityDemote,
		MutationClass:        "external_side_effect",
		ReversibilityCeiling: "easy",
		BlastRadius:          "local",
		TargetPath:           RouteStorePath(home),
	})
	return cfg
}

func armSycophancy(t *testing.T, home string, now time.Time) {
	t.Helper()
	th := Threshold{LensID: "sycophancy", MinValue: 0.5, DemoteTo: "demoted", Baseline: "active"}
	if err := Arm(home, th, now); err != nil {
		t.Fatalf("arm threshold: %v", err)
	}
}

// FALSIFIER 3 (calibration guard): arming a threshold with an EMPTY signal ledger for
// that lens is refused; arming succeeds ONLY after >=1 real measured signal is recorded.
// This is what stops a threshold being armed on an assumed/marketing number.
func TestArmRefusesWithoutMeasuredSignal(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	th := Threshold{LensID: "sycophancy", MinValue: 0.5, DemoteTo: "demoted", Baseline: "active"}

	if err := Arm(home, th, now); err == nil {
		t.Fatal("expected Arm to refuse on an EMPTY signal ledger (calibration guard)")
	}
	if cfg, _ := LoadThresholds(home); len(cfg.Thresholds) != 0 {
		t.Fatalf("threshold config must stay empty after a refused arm, got %d", len(cfg.Thresholds))
	}

	// A signal for a DIFFERENT lens must not satisfy the guard for this lens.
	recordSignal(t, home, mkSignal(t, "hedging", "lane-x", 0.9, behaviorallens.VisibilityPublicSafe, now))
	if err := Arm(home, th, now); err == nil {
		t.Fatal("expected Arm to refuse when the only signal is for a different lens")
	}

	// Now a real measured signal for THIS lens exists — arming is allowed.
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
	if err := Arm(home, th, now); err != nil {
		t.Fatalf("Arm should succeed once a measured signal exists for the lens: %v", err)
	}
	cfg, err := LoadThresholds(home)
	if err != nil {
		t.Fatalf("load thresholds: %v", err)
	}
	if len(cfg.Thresholds) != 1 || cfg.Thresholds[0].LensID != "sycophancy" {
		t.Fatalf("armed threshold not persisted: %+v", cfg.Thresholds)
	}
}

// FALSIFIER 1 (default OFF): with NO threshold armed the modulator does nothing — even
// when a crossing signal and an allowing grant both exist. Absent config = fail-closed.
func TestModulateOffWhenNoThresholdArmed(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))

	res, err := Modulate(home, grantConfig(home), "work_off", now)
	if err != nil {
		t.Fatalf("modulate: %v", err)
	}
	if len(res.Modulations) != 0 {
		t.Fatalf("modulator must be OFF with no threshold armed, got %d modulations", len(res.Modulations))
	}
	if _, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); ok {
		t.Fatal("no route-store entry may exist when the modulator is OFF")
	}
}

// FALSIFIER 1 (no allowing PolicyDecision): a crossing signal with an ARMED threshold but
// NO standing grant holds at policy, so act refuses and NOTHING is written. Same input +
// an allowing grant DOES write — proving the PolicyDecision gate is load-bearing, not that
// the modulator simply never writes.
func TestModulateRefusesWithoutGrantWritesWithGrant(t *testing.T) {
	now := fixedNow()

	// (a) no grant -> refused, route store untouched.
	{
		home := t.TempDir()
		recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
		armSycophancy(t, home, now)

		res, err := Modulate(home, policy.DefaultConfig(), "work_nogrant", now)
		if err != nil {
			t.Fatalf("modulate: %v", err)
		}
		if len(res.Modulations) != 1 || res.Modulations[0].Applied || !res.Modulations[0].Refused {
			t.Fatalf("without a grant the demote must be REFUSED and not applied, got %+v", res.Modulations)
		}
		if v, _, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); v == "demoted" {
			t.Fatal("route store was mutated despite a refused (holding) PolicyDecision")
		}
	}

	// (b) allowing grant -> the SAME crossing signal is demoted through act, with a real
	// outward_delta receipt/transition and captured pre/post images.
	{
		home := t.TempDir()
		recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
		armSycophancy(t, home, now)

		res, err := Modulate(home, grantConfig(home), "work_grant", now)
		if err != nil {
			t.Fatalf("modulate: %v", err)
		}
		if len(res.Modulations) != 1 {
			t.Fatalf("want exactly one modulation, got %d", len(res.Modulations))
		}
		m := res.Modulations[0]
		if !m.Applied || m.Refused {
			t.Fatalf("with a grant the demote must apply, got %+v", m)
		}
		if m.MoveType != "outward_delta" {
			t.Fatalf("receipt move_type must be outward_delta, got %q", m.MoveType)
		}
		if m.PreImageRef == "" || m.PostImageRef == "" || m.ReceiptID == "" || m.TransitionID == "" {
			t.Fatalf("applied modulation must carry pre/post refs + receipt/transition ids: %+v", m)
		}
		if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); !ok || v != "demoted" {
			t.Fatalf("route store must show lane-a demoted, got value=%q ok=%v", v, ok)
		}
	}
}

// FALSIFIER 2 (crossing vs below + revert round-trip): a lane ABOVE threshold gets a
// reversible demote; a lane BELOW threshold is untouched; and the persisted act.Result
// lets Revert restore the exact pre-image (value equal after apply -> revert).
func TestModulateDemotesCrossingLeavesBelowAndRevertRestores(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-hi", 0.90, behaviorallens.VisibilityPublicSafe, now)) // crosses 0.5
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-lo", 0.10, behaviorallens.VisibilityPublicSafe, now)) // below 0.5
	armSycophancy(t, home, now)

	res, err := Modulate(home, grantConfig(home), "work_rt", now)
	if err != nil {
		t.Fatalf("modulate: %v", err)
	}
	// exactly the crossing lane is modulated; the below-threshold lane never appears.
	if len(res.Modulations) != 1 || res.Modulations[0].Lane != "lane-hi" || !res.Modulations[0].Applied {
		t.Fatalf("only the crossing lane should be demoted, got %+v", res.Modulations)
	}
	if _, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-lo"); ok {
		t.Fatal("a lane BELOW threshold must be untouched (no route-store entry)")
	}
	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-hi"); !ok || v != "demoted" {
		t.Fatalf("crossing lane must be demoted, got value=%q ok=%v", v, ok)
	}

	// The apply captured a pre-image artifact holding the baseline ("active").
	store, err := act.NewArtifactStore(home)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	pre, err := store.Get(res.Modulations[0].PreImageRef)
	if err != nil {
		t.Fatalf("load pre-image artifact: %v", err)
	}
	if string(pre) != "active" {
		t.Fatalf("captured pre-image must be the baseline %q, got %q", "active", string(pre))
	}

	// Revert consumes the persisted act.Result and restores the exact pre-image.
	later := now.Add(time.Second)
	rev, err := Revert(home, grantConfig(home), "lane-hi", "work_rt_revert", later)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if !rev.Reverted {
		t.Fatalf("revert must report reverted: %+v", rev)
	}
	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-hi"); !ok || v != "active" {
		t.Fatalf("after revert the lane must be restored to baseline %q, got value=%q ok=%v", "active", v, ok)
	}
}

// FENCE 4 (operator_private stays operator-local): an operator_private signal's modulation
// is written under a quarantined operator-local key root and is EXCLUDED from the public
// route projection. A public_safe modulation is retained. This fails if a private signal's
// modulation ever leaks into the shared route surface / public projection.
func TestOperatorPrivateModulationStaysOperatorLocal(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	recordSignal(t, home, mkSignal(t, "sycophancy", "pub-lane", 0.9, behaviorallens.VisibilityPublicSafe, now))
	recordSignal(t, home, mkSignal(t, "sycophancy", "priv-lane", 0.9, behaviorallens.VisibilityOperatorPrivate, now))
	armSycophancy(t, home, now)

	res, err := Modulate(home, grantConfig(home), "work_vis", now)
	if err != nil {
		t.Fatalf("modulate: %v", err)
	}
	if len(res.Modulations) != 2 {
		t.Fatalf("want both lanes modulated operator-locally, got %d", len(res.Modulations))
	}

	// The operator_private modulation is written to the quarantined root, NOT the shared one.
	if _, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "priv-lane"); ok {
		t.Fatal("operator_private modulation leaked into the shared route surface")
	}
	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityOperatorPrivate, "priv-lane"); !ok || v != "demoted" {
		t.Fatalf("operator_private modulation must live under the operator-local root, got value=%q ok=%v", v, ok)
	}

	// The public projection contains ONLY the public_safe modulation.
	pub := res.PublicProjection()
	if len(pub.Modulations) != 1 || pub.Modulations[0].Lane != "pub-lane" {
		t.Fatalf("public projection must expose only the public_safe modulation, got %+v", pub.Modulations)
	}
	for _, m := range pub.Modulations {
		if m.Visibility == behaviorallens.VisibilityOperatorPrivate {
			t.Fatal("public projection must never carry an operator_private modulation")
		}
	}
}

// FALSIFIER 1b (byte-identity on refusal): a refused demote must leave the route store
// EXACTLY as it was — including ABSENT when it never existed. The weaker "value != demoted"
// check (case (a) of TestModulateRefusesWithoutGrantWritesWithGrant) misses a baseline key
// materialized BEFORE policy was consulted. This asserts no route-store write happened at
// all on refusal: it fails if any store mutation precedes an allowing PolicyDecision.
func TestModulateRefusalLeavesRouteStoreByteIdentical(t *testing.T) {
	now := fixedNow()

	// (a) no prior store: a refused demote must NOT create the route store file.
	{
		home := t.TempDir()
		recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
		armSycophancy(t, home, now)

		res, err := Modulate(home, policy.DefaultConfig(), "work_nogrant", now)
		if err != nil {
			t.Fatalf("modulate: %v", err)
		}
		if len(res.Modulations) != 1 || !res.Modulations[0].Refused {
			t.Fatalf("want exactly one refused modulation, got %+v", res.Modulations)
		}
		if _, err := os.Stat(RouteStorePath(home)); !os.IsNotExist(err) {
			t.Fatalf("a refused demote must not create the route store (no write before an allowing PolicyDecision); stat err=%v", err)
		}
	}

	// (b) pre-existing (unrelated) store: a refused demote must leave its bytes IDENTICAL —
	// it may not silently materialize a baseline entry for the refused lane.
	{
		home := t.TempDir()
		recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
		armSycophancy(t, home, now)
		if _, err := ensureLaneEntry(RouteStorePath(home), publicLaneRoot, "other-lane", "active"); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		before, err := os.ReadFile(RouteStorePath(home))
		if err != nil {
			t.Fatalf("read seeded store: %v", err)
		}

		if _, err := Modulate(home, policy.DefaultConfig(), "work_nogrant2", now); err != nil {
			t.Fatalf("modulate: %v", err)
		}
		after, err := os.ReadFile(RouteStorePath(home))
		if err != nil {
			t.Fatalf("read store after refusal: %v", err)
		}
		if string(before) != string(after) {
			t.Fatalf("refused demote mutated the route store:\n before=%q\n after =%q", string(before), string(after))
		}
	}
}

// FALSIFIER (governed action is observable end-to-end): an APPLIED demote's receipt/transition
// ids in the action ledger must resolve to real envelope files in the home's canonical event
// store (home/events/<id>.json) — not dangling references. This is what makes the modulator's
// own governed action visible to the event readers (executorlens/eventsource/capgraph). It
// fails if the act seam only returns receipts in memory instead of recording them.
func TestAppliedDemoteReceiptResolvesInEventStore(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
	armSycophancy(t, home, now)

	res, err := Modulate(home, grantConfig(home), "work_obs", now)
	if err != nil {
		t.Fatalf("modulate: %v", err)
	}
	if len(res.Modulations) != 1 || !res.Modulations[0].Applied {
		t.Fatalf("want exactly one applied modulation, got %+v", res.Modulations)
	}
	m := res.Modulations[0]
	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{m.ReceiptID, m.TransitionID} {
		if id == "" {
			t.Fatalf("applied modulation must carry receipt+transition ids: %+v", m)
		}
		exists, err := events.Exists(id)
		if err != nil || !exists {
			t.Fatalf("ledger reference %s does not resolve in the canonical event store: exists=%v err=%v", id, exists, err)
		}
	}
}

// F4 (partial-failure compensation): the modulator materializes a baseline lane entry BEFORE the
// governed act. If that act then FAILS, a freshly-created entry must be compensated (deleted) so a
// failed modulation leaves the route store as it found it — the lane key ABSENT. A pre-existing
// entry is never touched. The act is forced to fail by wedging the canonical event store (a file
// where home/events must be a dir), so act.Execute aborts after the baseline is materialized.
func TestModulateCompensatesCreatedLaneEntryWhenActFails(t *testing.T) {
	now := fixedNow()

	// (a) lane ABSENT -> materialized -> act fails -> the created key is compensated to ABSENT.
	{
		home := t.TempDir()
		recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
		armSycophancy(t, home, now)
		if err := os.WriteFile(filepath.Join(home, "events"), []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("wedge event store: %v", err)
		}
		if _, err := Modulate(home, grantConfig(home), "work_partial", now); err == nil {
			t.Fatal("expected Modulate to surface the failed act")
		}
		if _, present, err := currentValue(RouteStorePath(home), publicLaneRoot, "lane-a"); err != nil {
			t.Fatalf("read route store: %v", err)
		} else if present {
			t.Fatal("a failed act must compensate the lane entry it created — the key must be ABSENT")
		}
	}

	// (b) lane PRE-EXISTING at baseline -> act fails -> the pre-existing entry is left UNTOUCHED.
	{
		home := t.TempDir()
		recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
		armSycophancy(t, home, now)
		// Seed a pre-existing entry at baseline (not already-demoted, so the demote still proceeds).
		if err := saveStore(RouteStorePath(home), map[string]any{publicLaneRoot: map[string]any{"lane-a": "active"}}); err != nil {
			t.Fatalf("seed lane entry: %v", err)
		}
		if err := os.WriteFile(filepath.Join(home, "events"), []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("wedge event store: %v", err)
		}
		if _, err := Modulate(home, grantConfig(home), "work_partial2", now); err == nil {
			t.Fatal("expected Modulate to surface the failed act")
		}
		val, present, err := currentValue(RouteStorePath(home), publicLaneRoot, "lane-a")
		if err != nil {
			t.Fatalf("read route store: %v", err)
		}
		if !present || val != "active" {
			t.Fatalf("a pre-existing lane entry must be untouched by a failed act: present=%v val=%q", present, val)
		}
	}
}

// FALSIFIER (ledger preflight aborts BEFORE any route-store mutation): a wedge at the action-ledger
// PATH ITSELF (a directory sitting where the jsonl file must be) must make Modulate abort with the
// route store left byte-identical (absent) and ZERO new receipts/transitions. Before the preflight
// this SAME wedge still produced a non-nil error from appendActionRecord's OpenFile — but only AFTER
// act.Execute had already mutated the route store and recorded a receipt/transition. This test is
// written to FAIL against that shape: it demands the store stay ABSENT and the event store stay
// EMPTY, not merely that Modulate returns an error.
func TestModulateLedgerPreflightAbortsBeforeAnyRouteStoreMutation(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
	armSycophancy(t, home, now)

	// Wedge the ledger LEAF PATH itself as a directory — MkdirAll(parent) trivially succeeds since the
	// parent IS this directory's own parent, but OpenFile on the leaf then fails because it is a dir.
	if err := os.MkdirAll(actionLedgerPath(home), 0o755); err != nil {
		t.Fatalf("wedge action ledger path: %v", err)
	}

	if _, err := Modulate(home, grantConfig(home), "work_ledger_wedge", now); err == nil {
		t.Fatal("expected Modulate to abort when the action ledger path is wedged")
	}

	if _, err := os.Stat(RouteStorePath(home)); !os.IsNotExist(err) {
		t.Fatalf("route store must stay ABSENT (byte-identical) when the ledger preflight aborts; stat err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "events"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read events dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("want ZERO new receipts/transitions when the ledger preflight aborts, got %d event files: %v", len(entries), names)
	}
}

// FALSIFIER (post-act ledger-append backstop, compensating revert): a ledger-append failure AFTER a
// successful act.Execute must not strand the demote. This forces the append to fail via the
// package-private seam (appendActionRecordFn, mirroring act's own recordEnvelopeFn test seam) and
// asserts the compensating revert restores the route store to its pre-image, no recovery finding was
// needed, a later Revert() correctly finds nothing open (there is genuinely nothing left to revert),
// and the returned error NAMES the recovery path taken.
func TestModulateCompensatesLedgerAppendFailureWithRevert(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
	armSycophancy(t, home, now)

	orig := appendActionRecordFn
	appendActionRecordFn = func(string, actionRecord) error {
		return fmt.Errorf("injected ledger append failure")
	}
	defer func() { appendActionRecordFn = orig }()

	_, err := Modulate(home, grantConfig(home), "work_ledger_fail_revert", now)
	if err == nil {
		t.Fatal("expected Modulate to surface the ledger-append failure")
	}
	if !strings.Contains(err.Error(), "compensating revert succeeded") {
		t.Fatalf("error must name the compensating-revert recovery path, got: %v", err)
	}

	// The route store is restored to its pre-image (the baseline this pass materialized) — the
	// demote is NOT stranded even though its OWN ledger row never landed.
	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); !ok || v != "active" {
		t.Fatalf("compensating revert must restore the route store to baseline, got value=%q ok=%v", v, ok)
	}

	// Neither the failed demote row nor the (also-failing, best-effort, swallowed) revert-log
	// attempt ever landed in OUR ledger — but that is fine: the store itself is back at baseline, so
	// "no ledger row for this lane" matches reality exactly.
	if records, lerr := loadActionRecords(home); lerr != nil {
		t.Fatalf("load action records: %v", lerr)
	} else if len(records) != 0 {
		t.Fatalf("want zero action-ledger records for the lane (both appends were injected to fail), got %+v", records)
	}

	// A subsequent Revert() call correctly finds nothing open — never a stale attempt to revert an
	// already-restored lane.
	rev, rerr := Revert(home, grantConfig(home), "lane-a", "work_ledger_fail_revert_check", now.Add(time.Second))
	if rerr != nil {
		t.Fatalf("revert: %v", rerr)
	}
	if rev.Reverted {
		t.Fatalf("Revert must find nothing open (compensation already restored baseline), got %+v", rev)
	}

	// No recovery finding was needed — the compensating revert alone resolved it.
	if findings, ferr := finding.List(home, "lensmodulator-recovery"); ferr != nil {
		t.Fatalf("list findings: %v", ferr)
	} else if len(findings) != 0 {
		t.Fatalf("no recovery finding should be recorded when the compensating revert succeeds, got %+v", findings)
	}
}

// FALSIFIER (post-act ledger-append backstop, durable recovery finding): when the ledger append AND
// the compensating revert BOTH fail, Modulate must land a durable, operator-visible recovery finding
// (lane, receipt/transition ids, pre/post image refs) and return an error naming it — never a bare
// swallowed error that leaves the lane demoted with no trace. The compensating revert is forced to
// fail by stripping the owner write bit from the route store file INSIDE the injected append seam —
// which only runs after the forward act.Execute has already completed, so only the later
// compensating-revert write is blocked, not the original demote.
func TestModulateRecordsRecoveryFindingWhenCompensatingRevertAlsoFails(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
	armSycophancy(t, home, now)

	orig := appendActionRecordFn
	appendActionRecordFn = func(string, actionRecord) error {
		if err := os.Chmod(RouteStorePath(home), 0o444); err != nil {
			t.Fatalf("chmod route store read-only: %v", err)
		}
		return fmt.Errorf("injected ledger append failure")
	}
	defer func() {
		appendActionRecordFn = orig
		_ = os.Chmod(RouteStorePath(home), 0o644)
	}()

	_, err := Modulate(home, grantConfig(home), "work_ledger_fail_recovery", now)
	if err == nil {
		t.Fatal("expected Modulate to surface the failure")
	}
	if !strings.Contains(err.Error(), "MANUAL RECOVERY REQUIRED") {
		t.Fatalf("error must flag manual recovery, got: %v", err)
	}

	findings, ferr := finding.List(home, "lensmodulator-recovery")
	if ferr != nil {
		t.Fatalf("list findings: %v", ferr)
	}
	if len(findings) != 1 {
		t.Fatalf("want exactly one recovery finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.ProducedBy != "internal/lensmodulator" {
		t.Fatalf("recovery finding ProducedBy = %q, want internal/lensmodulator", f.ProducedBy)
	}
	if !strings.HasPrefix(f.Topic, "lensmodulator-recovery/lane-a") {
		t.Fatalf("recovery finding topic must name the lane, got %q", f.Topic)
	}
	if len(f.EvidenceRefs) != 2 {
		t.Fatalf("recovery finding must carry pre+post image refs, got %v", f.EvidenceRefs)
	}
	if !strings.Contains(err.Error(), f.ID) {
		t.Fatalf("error must name the recovery finding id %q, got: %v", f.ID, err)
	}

	// The lane is left DEMOTED (not silently rolled back, not silently un-flagged) — the recovery
	// finding exists precisely because the demote could NOT be safely auto-reverted.
	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); !ok || v != "demoted" {
		t.Fatalf("lane must be left DEMOTED pending manual recovery, got value=%q ok=%v", v, ok)
	}
}

// Plan is the dry-run default: it names the lanes that WOULD be demoted without touching
// the route store or evaluating policy.
func TestPlanIsDryRunAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
	armSycophancy(t, home, now)

	plan, err := Plan(home, now)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Modulations) != 1 || plan.Modulations[0].Applied {
		t.Fatalf("plan must show a would-be (not applied) modulation, got %+v", plan.Modulations)
	}
	if _, err := os.Stat(RouteStorePath(home)); !os.IsNotExist(err) {
		t.Fatal("Plan must not create the route store (dry-run writes nothing)")
	}
}

// --- Revert ledger discipline (packet B2: the same discipline Modulate got in lane B) ---

// demoteLaneA arms sycophancy, records a crossing signal for lane-a, and applies the demote through
// Modulate — the real machinery, not a hand-rolled ledger row, so the resulting PreImageRef/
// PostImageRef point at real artifacts a subsequent Revert can consume.
func demoteLaneA(t *testing.T, home string, now time.Time, workID string) Modulation {
	t.Helper()
	recordSignal(t, home, mkSignal(t, "sycophancy", "lane-a", 0.9, behaviorallens.VisibilityPublicSafe, now))
	armSycophancy(t, home, now)
	res, err := Modulate(home, grantConfig(home), workID, now)
	if err != nil {
		t.Fatalf("modulate: %v", err)
	}
	if len(res.Modulations) != 1 || !res.Modulations[0].Applied {
		t.Fatalf("want exactly one applied demote, got %+v", res.Modulations)
	}
	return res.Modulations[0]
}

func countEventFiles(t *testing.T, home string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "events"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read events dir: %v", err)
	}
	return len(entries)
}

// FALSIFIER B2-1 (ledger preflight aborts BEFORE any route-store mutation): an unwritable action
// ledger at Revert entry must abort BEFORE act.Execute — route store left exactly as Revert found it,
// zero new receipts/transitions. Revert must first READ the ledger to find what to revert (unlike
// Modulate, which never reads the action ledger before writing it), so Modulate's own falsifier
// mechanism — a directory sitting at the leaf path — breaks loadActionRecords itself before Revert
// can even find an open record to act on (verified: os.ReadFile on a directory always errors), so it
// cannot reproduce "mutates then fails on append" for Revert specifically. A read-only leaf (chmod
// 0o444) is the faithful equivalent for Revert's read-then-mutate-then-append shape: reads still
// succeed (an open record is found) but writes fail, so the SAME preflight probe Modulate uses
// (open-for-append) catches it up front, before act.Execute.
func TestRevertLedgerPreflightAbortsBeforeAnyRouteStoreMutation(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	demoteLaneA(t, home, now, "work_wedge_setup")

	ledgerPath := actionLedgerPath(home)
	if err := os.Chmod(ledgerPath, 0o444); err != nil {
		t.Fatalf("wedge ledger read-only: %v", err)
	}
	defer os.Chmod(ledgerPath, 0o644)

	before, err := os.ReadFile(RouteStorePath(home))
	if err != nil {
		t.Fatalf("read route store before revert: %v", err)
	}
	beforeEvents := countEventFiles(t, home)

	rev, err := Revert(home, grantConfig(home), "lane-a", "work_wedge_revert", now.Add(time.Second))
	if err == nil {
		t.Fatalf("expected Revert to error when the ledger is unwritable, got %+v", rev)
	}

	after, rerr := os.ReadFile(RouteStorePath(home))
	if rerr != nil {
		t.Fatalf("read route store after revert: %v", rerr)
	}
	if string(before) != string(after) {
		t.Fatalf("route store must stay byte-identical when the ledger preflight aborts:\n before=%q\n after =%q", before, after)
	}
	afterEvents := countEventFiles(t, home)
	if afterEvents != beforeEvents {
		t.Fatalf("want ZERO new receipts/transitions when the ledger preflight aborts, got %d -> %d event files", beforeEvents, afterEvents)
	}
}

// FALSIFIER B2-2 (post-restore ledger-append backstop, recovery finding): a ledger-append failure
// AFTER a successful revert act.Execute must NOT counter-mutate (re-demoting the just-restored lane
// would be wrong) — it must land a durable recovery finding naming the lane, receipt/transition ids,
// and the restored value, and return an error naming it. Forces the append to fail via the SAME
// package-private seam Modulate's own backstop tests use (appendActionRecordFn) — no second seam.
func TestRevertRecordsRecoveryFindingWhenLedgerAppendFails(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	demoteLaneA(t, home, now, "work_backstop_setup")

	orig := appendActionRecordFn
	appendActionRecordFn = func(string, actionRecord) error {
		return fmt.Errorf("injected ledger append failure")
	}
	defer func() { appendActionRecordFn = orig }()

	rev, err := Revert(home, grantConfig(home), "lane-a", "work_backstop_revert", now.Add(time.Second))
	if err == nil {
		t.Fatalf("expected Revert to surface the ledger-append failure, got %+v", rev)
	}
	if !strings.Contains(err.Error(), "recorded recovery finding") {
		t.Fatalf("error must name the recovery-finding recovery path, got: %v", err)
	}

	// act.Execute already succeeded (BEFORE the injected append failure), so the store IS restored.
	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); !ok || v != "active" {
		t.Fatalf("route store must be restored to baseline despite the ledger-append failure, got value=%q ok=%v", v, ok)
	}

	findings, ferr := finding.List(home, "lensmodulator-recovery")
	if ferr != nil {
		t.Fatalf("list findings: %v", ferr)
	}
	if len(findings) != 1 {
		t.Fatalf("want exactly one recovery finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.ProducedBy != "internal/lensmodulator" {
		t.Fatalf("recovery finding ProducedBy = %q, want internal/lensmodulator", f.ProducedBy)
	}
	if !strings.HasPrefix(f.Topic, "lensmodulator-recovery/lane-a") {
		t.Fatalf("recovery finding topic must name the lane, got %q", f.Topic)
	}
	if len(f.EvidenceRefs) != 2 {
		t.Fatalf("recovery finding must carry pre+post image refs, got %v", f.EvidenceRefs)
	}
	if !strings.Contains(err.Error(), f.ID) {
		t.Fatalf("error must name the recovery finding id %q, got: %v", f.ID, err)
	}

	// The open-demote ledger row for lane-a is now STALE (this call's own append never landed) —
	// exactly the disagreement the next falsifier proves a second Revert refuses to re-mutate from.
	records, lerr := loadActionRecords(home)
	if lerr != nil {
		t.Fatalf("load action records: %v", lerr)
	}
	if _, ok := latestOpenDemote(records, "lane-a"); !ok {
		t.Fatal("the ledger row for lane-a must still read as an open demote (that staleness is exactly what the next falsifier's safety check must catch)")
	}
}

// FALSIFIER B2-3 (stale-open double-revert): the store is already restored (e.g. left behind by
// falsifier 2's end-state, or seeded directly here) while the ledger still shows an open demote. A
// SECOND Revert() must NOT call act/mutate — it must detect the store no longer holds the demoted
// value and return Reverted:false naming the stale reason. Before the fix this re-mutates from the
// stale pre-image (rec.PreImageRef == "active"), producing a spurious no-op-valued write with its own
// new receipt/transition — exactly the double-revert-from-stale-pre-image class the packet describes.
func TestRevertRefusesStaleOpenDoubleRevert(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	demoteLaneA(t, home, now, "work_stale_setup")

	// Seed the disagreement directly: the open "demote" ledger record stays as-is, but the route store
	// is restored to baseline OUTSIDE of Revert — mirroring exactly what falsifier 2 leaves behind
	// (act.Execute succeeded, its own ledger append failed).
	if err := saveStore(RouteStorePath(home), map[string]any{publicLaneRoot: map[string]any{"lane-a": "active"}}); err != nil {
		t.Fatalf("seed restored store: %v", err)
	}

	beforeEvents := countEventFiles(t, home)

	rev, err := Revert(home, grantConfig(home), "lane-a", "work_stale_revert", now.Add(time.Second))
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if rev.Reverted {
		t.Fatalf("a stale-open double-revert must NOT report Reverted:true, got %+v", rev)
	}
	if !strings.Contains(rev.Reason, "stale") {
		t.Fatalf("reason must name the stale-record condition, got %q", rev.Reason)
	}

	afterEvents := countEventFiles(t, home)
	if afterEvents != beforeEvents {
		t.Fatalf("stale-open Revert must not call act (zero new receipts/transitions), got %d -> %d event files", beforeEvents, afterEvents)
	}
	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); !ok || v != "active" {
		t.Fatalf("store must stay at its already-restored value, got value=%q ok=%v", v, ok)
	}

	// The ledger heals: a subsequent load finds nothing open for the lane.
	records, lerr := loadActionRecords(home)
	if lerr != nil {
		t.Fatalf("load action records: %v", lerr)
	}
	if _, ok := latestOpenDemote(records, "lane-a"); ok {
		t.Fatal("the stale-open Revert must heal the ledger — a later load must find nothing open for the lane")
	}
}

// FALSIFIER B2-4 (happy path unaffected): demote -> Revert -> store restored, ledger record appended,
// latestOpenDemote finds nothing open. Must keep working exactly as before the ledger-discipline
// changes above (this is what "all existing modulator tests stay green" is proving package-wide).
func TestRevertHappyPathClosesLedger(t *testing.T) {
	home := t.TempDir()
	now := fixedNow()
	demoteLaneA(t, home, now, "work_happy_setup")

	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); !ok || v != "demoted" {
		t.Fatalf("setup: lane-a must be demoted before revert, got value=%q ok=%v", v, ok)
	}

	rev, err := Revert(home, grantConfig(home), "lane-a", "work_happy_revert", now.Add(time.Second))
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if !rev.Reverted {
		t.Fatalf("happy-path revert must report Reverted:true, got %+v", rev)
	}
	if rev.RestoredValue != "active" || rev.ReceiptID == "" || rev.TransitionID == "" {
		t.Fatalf("happy-path revert must carry restored value + receipt/transition ids: %+v", rev)
	}
	if v, ok, _ := CurrentAdjustment(home, behaviorallens.VisibilityPublicSafe, "lane-a"); !ok || v != "active" {
		t.Fatalf("route store must be restored to baseline, got value=%q ok=%v", v, ok)
	}

	records, lerr := loadActionRecords(home)
	if lerr != nil {
		t.Fatalf("load action records: %v", lerr)
	}
	if len(records) != 2 {
		t.Fatalf("want exactly demote+revert rows in the ledger, got %d: %+v", len(records), records)
	}
	if _, ok := latestOpenDemote(records, "lane-a"); ok {
		t.Fatal("after a successful revert, latestOpenDemote must find nothing open for the lane")
	}
}
