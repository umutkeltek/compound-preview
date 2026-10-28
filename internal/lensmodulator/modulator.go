// Package lensmodulator is the GOVERNED MODULATOR — the first compound organ that ACTS on a
// recorded behavioral-lens signal instead of only observing it. It reads InferredSignals
// (behaviorallens.LoadSignals, operator-local) and, when a signal crosses an operator-armed
// threshold, applies a REVERSIBLE route/role adjustment (demote the flagged lane) THROUGH the
// generic governed write seam (internal/act). It never mutates without an allowing PolicyDecision.
//
// Three load-bearing invariants:
//   - DEFAULT OFF / fail-closed. No armed threshold => the modulator does nothing. A stranger clone
//     never wrote a threshold config, so it can never act.
//   - GOVERNED BY CONSTRUCTION. Every demote is an ActionIntent evaluated by policy and executed by
//     act.Execute, which refuses without a matching allowing PolicyDecision for that exact intent. No
//     standing grant => the demote holds and NOTHING is written. Revert is itself a governed act,
//     never a privileged bypass.
//   - CALIBRATION GUARD. A threshold cannot be armed until >=1 real measured signal for that lens
//     exists — you cannot arm on an assumed/marketing number.
//
// Visibility boundary: an operator_private signal's modulation stays operator-local — it is written
// to a QUARANTINED key root under COMPOUND_HOME and excluded from the public route projection. Only
// public_safe modulations may reach a shared route surface. All state lives under COMPOUND_HOME,
// never the repo, and no path makes a provider/network call.
//
// The organ owns no authority object: envelope/policy/work stay authority, act owns the write seam,
// behaviorallens owns the signal evidence. lensmodulator only COMPOSES them into the one enforcement
// point that turns a recorded signal into a governed, reversible route/role change.
package lensmodulator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/behaviorallens"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/policy"

	"gopkg.in/yaml.v3"
)

// ThresholdSchemaVersion versions the per-home armed-threshold config.
const ThresholdSchemaVersion = "compound.lens_modulator_thresholds.v0.1"

// ActionRecordSchemaVersion versions the append-only demote/revert action ledger.
const ActionRecordSchemaVersion = "compound.lens_modulator_action.v0.1"

// CapabilityDemote is the capability every demote/revert ActionIntent declares, so an operator's
// standing grant can pre-authorize exactly this modulation and nothing else.
const CapabilityDemote = "lensmodulator.route_role.demote"

// thresholdsRelPath — per-home armed-threshold config, relative to COMPOUND_HOME. ABSENT = OFF.
const thresholdsRelPath = "config/lens-modulator-thresholds.json"

// routeStoreRelPath — the operator-local route/role adjustment store the modulator repoints through
// the governed act seam. Under COMPOUND_HOME, never the repo, never a tracked/public surface.
const routeStoreRelPath = "config/lane-route-adjustments.yaml"

// actionLedgerRelPath — append-only record of every governed demote/revert, relative to
// COMPOUND_HOME. It persists the act.Result refs so a later Revert can undo a demote.
const actionLedgerRelPath = "ledger/lens-modulator-actions.jsonl"

const (
	// publicLaneRoot is the SHARED route surface: public_safe-driven demotes land here, and this is
	// the only root a public route projection may read.
	publicLaneRoot = "lanes"
	// privateLaneRoot quarantines operator_private-driven demotes: operator-local only, never shared.
	privateLaneRoot = "operator_private_lanes"
)

// Threshold is one operator-armed rule: for lens LensID (optionally scoped to a single Subject), a
// recorded signal whose Value is at or above MinValue demotes its subject lane to DemoteTo. Baseline
// is the value a lane starts at if it has no entry yet.
type Threshold struct {
	LensID       string  `json:"lens_id"`
	SubjectScope string  `json:"subject_scope,omitempty"`
	MinValue     float64 `json:"min_value"`
	DemoteTo     string  `json:"demote_to"`
	Baseline     string  `json:"baseline,omitempty"`
}

// ThresholdConfig is the per-home set of armed thresholds. Empty/absent = modulator OFF.
type ThresholdConfig struct {
	SchemaVersion string      `json:"schema_version,omitempty"`
	Thresholds    []Threshold `json:"thresholds,omitempty"`
}

// Modulation is one lane the modulator matched. Applied is true iff a governed write happened;
// Refused is true iff policy declined (no allowing PolicyDecision). Visibility carries the driving
// signal's exposure class so the public projection can enforce the boundary on the row itself.
type Modulation struct {
	LensID       string                    `json:"lens_id"`
	Lane         string                    `json:"lane"`
	Root         string                    `json:"root"`
	Visibility   behaviorallens.Visibility `json:"visibility"`
	SignalValue  float64                   `json:"signal_value"`
	Threshold    float64                   `json:"threshold"`
	DemoteTo     string                    `json:"demote_to"`
	Baseline     string                    `json:"baseline"`
	Applied      bool                      `json:"applied"`
	Refused      bool                      `json:"refused,omitempty"`
	Reason       string                    `json:"reason,omitempty"`
	PreImageRef  string                    `json:"pre_image_ref,omitempty"`
	PostImageRef string                    `json:"post_image_ref,omitempty"`
	ReceiptID    string                    `json:"receipt_id,omitempty"`
	TransitionID string                    `json:"transition_id,omitempty"`
	MoveType     string                    `json:"move_type,omitempty"`
}

// Skip records a matched-lens signal the modulator did NOT act on, and why (below threshold, not
// keyable, already demoted) — so the gate is legible, never silent.
type Skip struct {
	LensID     string                    `json:"lens_id"`
	Lane       string                    `json:"lane"`
	Visibility behaviorallens.Visibility `json:"visibility"`
	Reason     string                    `json:"reason"`
}

// ModulationResult is the outcome of one modulate/plan pass.
type ModulationResult struct {
	Modulations []Modulation `json:"modulations"`
	Skipped     []Skip       `json:"skipped,omitempty"`
}

// PublicProjection returns the shareable view: ONLY public_safe modulations/skips. operator_private
// rows are dropped BY CONSTRUCTION here, so their existence never leaves the home through a shared
// route projection.
func (r ModulationResult) PublicProjection() ModulationResult {
	out := ModulationResult{}
	for _, m := range r.Modulations {
		if m.Visibility == behaviorallens.VisibilityOperatorPrivate {
			continue
		}
		out.Modulations = append(out.Modulations, m)
	}
	for _, s := range r.Skipped {
		if s.Visibility == behaviorallens.VisibilityOperatorPrivate {
			continue
		}
		out.Skipped = append(out.Skipped, s)
	}
	return out
}

// RevertOutcome is the result of undoing a lane's latest demote.
type RevertOutcome struct {
	Lane          string `json:"lane"`
	Reverted      bool   `json:"reverted"`
	RestoredValue string `json:"restored_value,omitempty"`
	ReceiptID     string `json:"receipt_id,omitempty"`
	TransitionID  string `json:"transition_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// RouteStorePath is the operator-local route/role store path for a home. Exported so the operator's
// standing grant (and the CLI) can scope to exactly this target.
func RouteStorePath(home string) string {
	return filepath.Join(home, filepath.FromSlash(routeStoreRelPath))
}

func thresholdsPath(home string) string {
	return filepath.Join(home, filepath.FromSlash(thresholdsRelPath))
}

func actionLedgerPath(home string) string {
	return filepath.Join(home, filepath.FromSlash(actionLedgerRelPath))
}

func laneRoot(v behaviorallens.Visibility) string {
	if v == behaviorallens.VisibilityOperatorPrivate {
		return privateLaneRoot
	}
	return publicLaneRoot
}

// LoadThresholds reads a home's armed thresholds. An ABSENT file returns the zero config (OFF) — the
// fail-closed default a stranger clone gets.
func LoadThresholds(home string) (ThresholdConfig, error) {
	if strings.TrimSpace(home) == "" {
		return ThresholdConfig{}, fmt.Errorf("no COMPOUND_HOME")
	}
	data, err := os.ReadFile(thresholdsPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return ThresholdConfig{}, nil
		}
		return ThresholdConfig{}, err
	}
	var cfg ThresholdConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ThresholdConfig{}, fmt.Errorf("parse thresholds %s: %w", thresholdsPath(home), err)
	}
	if v := strings.TrimSpace(cfg.SchemaVersion); v != "" && v != ThresholdSchemaVersion {
		return ThresholdConfig{}, fmt.Errorf("thresholds schema_version=%q, want %q", v, ThresholdSchemaVersion)
	}
	return cfg, nil
}

func (t Threshold) validate() error {
	if strings.TrimSpace(t.LensID) == "" {
		return fmt.Errorf("threshold lens_id required")
	}
	if strings.TrimSpace(t.DemoteTo) == "" {
		return fmt.Errorf("threshold demote_to required")
	}
	return nil
}

// Arm appends a threshold and persists the config — but ONLY after the CALIBRATION GUARD passes: the
// signal ledger must already hold >=1 real measured signal for that lens. Arming on an empty ledger
// (a marketing/assumed number) is refused, and nothing is persisted.
func Arm(home string, th Threshold, now time.Time) error {
	if err := th.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(th.Baseline) == "" {
		th.Baseline = "active"
	}
	measured, err := hasMeasuredSignal(home, th.LensID)
	if err != nil {
		return err
	}
	if !measured {
		return fmt.Errorf("calibration guard: cannot arm a threshold for lens %q with no recorded signal — record at least one real measured signal before arming (never arm on an assumed number)", th.LensID)
	}
	cfg, err := LoadThresholds(home)
	if err != nil {
		return err
	}
	cfg.Thresholds = append(cfg.Thresholds, th)
	return writeThresholds(home, cfg)
}

// hasMeasuredSignal reports whether the operator-local ledger holds at least one recorded signal for
// lensID. It reads every signal (LoadSignals) because calibration is about whether the lens has EVER
// measured — public_safe or operator_private, both count for the operator's own instance.
func hasMeasuredSignal(home, lensID string) (bool, error) {
	signals, err := behaviorallens.LoadSignals(home)
	if err != nil {
		return false, err
	}
	for _, s := range signals {
		if s.LensID == lensID {
			return true, nil
		}
	}
	return false, nil
}

func writeThresholds(home string, cfg ThresholdConfig) error {
	cfg.SchemaVersion = ThresholdSchemaVersion
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := thresholdsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// matchOne returns the first armed threshold that a signal crosses (lens id match, subject-scope
// match if set, value at or above MinValue).
func matchOne(cfg ThresholdConfig, sig behaviorallens.InferredSignal) (Threshold, bool) {
	for _, th := range cfg.Thresholds {
		if th.LensID != sig.LensID {
			continue
		}
		if s := strings.TrimSpace(th.SubjectScope); s != "" && s != sig.Subject {
			continue
		}
		if sig.Value >= th.MinValue {
			return th, true
		}
	}
	return Threshold{}, false
}

// Plan is the DRY-RUN default: it names the lanes that WOULD be demoted without touching the route
// store or evaluating policy. It writes nothing.
func Plan(home string, now time.Time) (ModulationResult, error) {
	cfg, err := LoadThresholds(home)
	if err != nil {
		return ModulationResult{}, err
	}
	var res ModulationResult
	if len(cfg.Thresholds) == 0 {
		return res, nil
	}
	signals, err := behaviorallens.LoadSignals(home)
	if err != nil {
		return ModulationResult{}, err
	}
	for _, sig := range signals {
		th, ok := matchOne(cfg, sig)
		if !ok {
			continue
		}
		if skip, bad := unkeyableSkip(sig); bad {
			res.Skipped = append(res.Skipped, skip)
			continue
		}
		res.Modulations = append(res.Modulations, Modulation{
			LensID: sig.LensID, Lane: sig.Subject, Root: laneRoot(sig.Visibility), Visibility: sig.Visibility,
			SignalValue: sig.Value, Threshold: th.MinValue, DemoteTo: th.DemoteTo, Baseline: th.Baseline,
			Applied: false, Reason: "dry-run: would demote",
		})
	}
	return res, nil
}

// Modulate reads recorded signals and, for each crossing signal, applies a REVERSIBLE demote THROUGH
// the governed act seam. It is fail-closed (no threshold => OFF, no write) and governed by
// construction: policy.Evaluate builds the PolicyDecision and act.Execute refuses to write without an
// allowing decision for that exact intent. A refused demote leaves the route store byte-identical.
// Every applied demote is persisted to the action ledger so Revert can undo it — so BEFORE the
// per-lane mutation loop this proves the action ledger itself is appendable (preflightActionLedger)
// and aborts before any route-store mutation if it is not. If the ledger append for an applied
// demote fails ANYWAY (a failure mode the preflight cannot fully rule out — e.g. disk pressure
// between preflight and append), afterAppendFailure attempts a compensating governed revert of the
// just-applied demote, or lands a durable recovery finding — a demote must never become
// unrevertable by a silently swallowed ledger-append error.
func Modulate(home string, policyCfg policy.Config, workID string, now time.Time) (ModulationResult, error) {
	cfg, err := LoadThresholds(home)
	if err != nil {
		return ModulationResult{}, err
	}
	var res ModulationResult
	if len(cfg.Thresholds) == 0 {
		return res, nil // DEFAULT OFF
	}
	signals, err := behaviorallens.LoadSignals(home)
	if err != nil {
		return ModulationResult{}, err
	}
	store, err := act.NewArtifactStore(home)
	if err != nil {
		return ModulationResult{}, err
	}
	adapter := act.NewConfigKeyAdapter()
	storePath := RouteStorePath(home)

	// Ledger writability PREFLIGHT before the per-lane mutation loop: prove the action ledger path
	// is appendable BEFORE any route-store mutation happens this pass. A wedge at the ledger path
	// itself (e.g. a directory sitting where the jsonl file must be) aborts here, byte-identical
	// route store, zero new receipts/transitions — never a mutated-but-unrecorded demote.
	if err := preflightActionLedger(home); err != nil {
		return ModulationResult{}, err
	}

	for _, sig := range signals {
		th, ok := matchOne(cfg, sig)
		if !ok {
			continue // below threshold / no matching threshold => untouched
		}
		if skip, bad := unkeyableSkip(sig); bad {
			res.Skipped = append(res.Skipped, skip)
			continue
		}
		root := laneRoot(sig.Visibility)
		// Idempotency check WITHOUT mutating the store: read the current value directly (not
		// adapter.ReadCurrent, which errors on an absent key). An absent lane is NOT "already
		// demoted", so it proceeds; a lane already at DemoteTo is skipped. Reading (never writing)
		// here is what lets a refused pass leave the store byte-identical.
		if curVal, present, err := currentValue(storePath, root, sig.Subject); err != nil {
			return ModulationResult{}, err
		} else if present && curVal == th.DemoteTo {
			res.Skipped = append(res.Skipped, Skip{LensID: sig.LensID, Lane: sig.Subject, Visibility: sig.Visibility, Reason: "already demoted (idempotent)"})
			continue
		}

		target := envelope.ActionTarget{Kind: "route_role", ID: root + "." + sig.Subject, Path: storePath}
		ai := buildDemoteIntent(workID, target, th, sig.LensID, sig.Subject, now)
		pd := policy.Evaluate(ai, policyCfg, now)
		// GATE BEFORE ANY WRITE. Confirm the governed write is admissible BEFORE materializing the
		// baseline key the repoint-only adapter needs. A refused (hold/deny) decision therefore leaves
		// the route store byte-identical — absent stays absent. act.Execute re-checks the same gate.
		if gateErr := act.Gate(ai, pd); gateErr != nil {
			res.Modulations = append(res.Modulations, Modulation{
				LensID: sig.LensID, Lane: sig.Subject, Root: root, Visibility: sig.Visibility,
				SignalValue: sig.Value, Threshold: th.MinValue, DemoteTo: th.DemoteTo, Baseline: th.Baseline,
				Applied: false, Refused: true, Reason: gateErr.Error(),
			})
			continue
		}

		// Admitted: NOW materialize the target key at the baseline so the config-key adapter (which
		// repoints, never creates) has something to repoint. This setup write happens ONLY on the
		// allowed path; the pre-image the act captures is exactly this baseline. Track whether WE
		// created it so a failed act can restore absence.
		createdEntry, err := ensureLaneEntry(storePath, root, sig.Subject, th.Baseline)
		if err != nil {
			return ModulationResult{}, err
		}
		result, execErr := act.Execute(store, adapter, ai, pd, []byte(th.DemoteTo), now)
		if execErr != nil {
			// The gate already admitted this intent, so a failure here is a genuine I/O/validation
			// error (not a policy refusal). If WE materialized the baseline entry just now (it did not
			// pre-exist), compensate by deleting it so the failed modulation leaves the route store as
			// it found it — restoring absence — before surfacing the error. A pre-existing entry is
			// never touched.
			if createdEntry {
				if delErr := deleteLaneEntry(storePath, root, sig.Subject); delErr != nil {
					return ModulationResult{}, fmt.Errorf("act execute after gate admitted lane %q failed (%w); compensating deletion of the entry this pass created ALSO failed: %v", sig.Subject, execErr, delErr)
				}
				return ModulationResult{}, fmt.Errorf("act execute after gate admitted lane %q: %w (compensated: deleted the baseline entry this pass created)", sig.Subject, execErr)
			}
			return ModulationResult{}, fmt.Errorf("act execute after gate admitted lane %q: %w", sig.Subject, execErr)
		}

		rec := actionRecord{
			SchemaVersion: ActionRecordSchemaVersion, Kind: "demote", WorkID: workID,
			Lane: sig.Subject, Root: root, LensID: sig.LensID, Visibility: string(sig.Visibility),
			StorePath: storePath, Baseline: th.Baseline, DemoteTo: th.DemoteTo, Threshold: th.MinValue,
			Capability: CapabilityDemote, ActionIntentID: ai.ID,
			PreImageRef: result.PreImageRef, PostImageRef: result.PostImageRef,
			ReceiptID: result.Receipt.ID, TransitionID: result.Transition.ID,
			CreatedAt: now.UTC().Format(time.RFC3339),
		}
		if err := appendActionRecordFn(home, rec); err != nil {
			// The demote itself succeeded (route store mutated, receipt/transition recorded through
			// act's own event store) but OUR ledger row — the only place Revert() looks to find an
			// open demote — never landed. Left alone this is exactly the permanent strand the package
			// header promises never happens. Compensate or land a recovery finding; never a bare
			// return that strands silently.
			return afterAppendFailure(home, policyCfg, store, adapter, ai, result, sig, th, root, err, now)
		}
		res.Modulations = append(res.Modulations, Modulation{
			LensID: sig.LensID, Lane: sig.Subject, Root: root, Visibility: sig.Visibility,
			SignalValue: sig.Value, Threshold: th.MinValue, DemoteTo: th.DemoteTo, Baseline: th.Baseline,
			Applied: true, PreImageRef: result.PreImageRef, PostImageRef: result.PostImageRef,
			ReceiptID: result.Receipt.ID, TransitionID: result.Transition.ID, MoveType: result.Receipt.MoveType,
		})
	}
	return res, nil
}

// Revert undoes a lane's latest still-open demote by consuming the persisted act.Result: it restores
// the captured pre-image THROUGH the governed act seam (act.RevertIntent + act.Execute), so a
// rollback is itself a governed, receipted, re-gated outbound write — never a privileged bypass.
//
// It gets the SAME ledger discipline Modulate has: a writability PREFLIGHT before any route-store
// mutation this call (preflightActionLedger, identical seam Modulate uses), and a STALE-OPEN safety
// check that verifies the store's CURRENT value still matches what the open demote record expects
// before mutating — so a second Revert against a ledger record left open by an earlier append failure
// can never re-mutate from a stale pre-image. If the ledger append for a successful restore fails
// anyway, Revert does NOT counter-mutate (the restored store IS the safe state — re-demoting would be
// wrong); it lands a durable recovery finding instead (afterRevertAppendFailure).
func Revert(home string, policyCfg policy.Config, lane, workID string, now time.Time) (RevertOutcome, error) {
	records, err := loadActionRecords(home)
	if err != nil {
		return RevertOutcome{}, err
	}
	rec, ok := latestOpenDemote(records, lane)
	if !ok {
		return RevertOutcome{Lane: lane, Reverted: false, Reason: "no open demote record for lane"}, nil
	}

	// Ledger writability PREFLIGHT before ANY route-store mutation this call — same probe Modulate
	// runs, same failure mode it exists to catch (a directory/unwritable wedge at the ledger leaf). A
	// failure here errors out with the route store untouched and zero new receipts/transitions, never
	// a restore that then fails to record itself.
	if err := preflightActionLedger(home); err != nil {
		return RevertOutcome{}, err
	}

	storePath := RouteStorePath(home)

	// STALE-OPEN SAFETY: verify the store's CURRENT value still matches what this open demote record
	// expects BEFORE mutating. A prior Revert (or Modulate's own compensating revert) may have already
	// restored the store while its OWN ledger row failed to land — leaving this record open despite
	// the store already being safe. Re-running act.Execute from rec's stale pre-image here would be a
	// SECOND mutation, not a revert. Instead, do not mutate: report Reverted:false and heal the ledger
	// with a closing record so a later Revert() stops finding this record open (best-effort — the
	// store is already at the safe value regardless of whether this append succeeds).
	if curVal, present, err := currentValue(storePath, rec.Root, lane); err != nil {
		return RevertOutcome{}, err
	} else if !present || curVal != rec.DemoteTo {
		_ = appendActionRecordFn(home, actionRecord{
			SchemaVersion: ActionRecordSchemaVersion, Kind: "revert", WorkID: workID,
			Lane: lane, Root: rec.Root, LensID: rec.LensID, Visibility: rec.Visibility,
			StorePath: storePath, Baseline: rec.Baseline, DemoteTo: rec.DemoteTo, Threshold: rec.Threshold,
			Capability: CapabilityDemote, CreatedAt: now.UTC().Format(time.RFC3339),
		})
		return RevertOutcome{Lane: lane, Reverted: false, Reason: "store already restored; open ledger record is stale"}, nil
	}

	store, err := act.NewArtifactStore(home)
	if err != nil {
		return RevertOutcome{}, err
	}
	adapter := act.NewConfigKeyAdapter()
	target := envelope.ActionTarget{Kind: "route_role", ID: rec.Root + "." + lane, Path: storePath}

	// Reconstruct the original intent shape so the revert is governed identically (same capability,
	// class, reversibility, blast, target). RevertIntent only reads res.PreImageRef/PostImageRef.
	orig := buildDemoteIntent(rec.WorkID, target, Threshold{
		LensID: rec.LensID, MinValue: rec.Threshold, DemoteTo: rec.DemoteTo, Baseline: rec.Baseline,
	}, rec.LensID, lane, now)
	revertAI := act.RevertIntent(orig, act.Result{PreImageRef: rec.PreImageRef, PostImageRef: rec.PostImageRef}, now)

	preBytes, err := store.Get(rec.PreImageRef)
	if err != nil {
		return RevertOutcome{}, fmt.Errorf("load pre-image artifact for revert: %w", err)
	}
	pd := policy.Evaluate(revertAI, policyCfg, now)
	revRes, execErr := act.ExecuteRevert(store, adapter, revertAI, pd, preBytes, now)
	if execErr != nil {
		return RevertOutcome{Lane: lane, Reverted: false, Reason: execErr.Error()}, nil
	}
	revRec := actionRecord{
		SchemaVersion: ActionRecordSchemaVersion, Kind: "revert", WorkID: workID,
		Lane: lane, Root: rec.Root, LensID: rec.LensID, Visibility: rec.Visibility,
		StorePath: storePath, Baseline: rec.Baseline, DemoteTo: rec.DemoteTo, Threshold: rec.Threshold,
		Capability: CapabilityDemote, ActionIntentID: revertAI.ID,
		PreImageRef: revRes.PreImageRef, PostImageRef: revRes.PostImageRef,
		ReceiptID: revRes.Receipt.ID, TransitionID: revRes.Transition.ID,
		CreatedAt: now.UTC().Format(time.RFC3339),
	}
	if err := appendActionRecordFn(home, revRec); err != nil {
		// The revert itself SUCCEEDED — route store restored, receipt/transition recorded through
		// act's own event store — but OUR ledger row never landed. Unlike Modulate's own backstop,
		// counter-mutating here would be WRONG (re-demoting the lane this call just restored), so land
		// a durable recovery finding instead and return an error naming it.
		return afterRevertAppendFailure(home, lane, workID, revRes, string(preBytes), err, now)
	}
	return RevertOutcome{
		Lane: lane, Reverted: true, RestoredValue: string(preBytes),
		ReceiptID: revRes.Receipt.ID, TransitionID: revRes.Transition.ID,
	}, nil
}

// afterRevertAppendFailure handles a ledger-append failure that occurs AFTER a successful revert
// act.Execute: the route store is already restored to its pre-image (the safe state). Unlike
// Modulate's afterAppendFailure, there is no compensating counter-mutation to attempt — the restored
// value IS correct, so re-demoting the lane to "fix" this would be wrong. Instead, land a durable,
// operator-visible recovery finding naming the lane, receipt/transition ids, and the restored value,
// and note that the open-demote ledger record this revert intended to close is now STALE (a later
// Revert() call will still see it as open via latestOpenDemote). The stale-open safety check above is
// what makes that staleness survivable rather than a landmine: it refuses to re-mutate from it.
func afterRevertAppendFailure(home, lane, workID string, revRes act.Result, restoredValue string, appendErr error, now time.Time) (RevertOutcome, error) {
	rec, ferr := finding.Record(home, finding.Finding{
		Topic: "lensmodulator-recovery/" + lane,
		Body: fmt.Sprintf(
			"lensmodulator revert SUCCEEDED (route store restored) but its action-ledger record could not be appended — the open-demote ledger record for this lane is now STALE.\n"+
				"lane: %s\nrestored_value: %s\nreceipt_id: %s\ntransition_id: %s\nledger_append_error: %v\n",
			lane, restoredValue, revRes.Receipt.ID, revRes.Transition.ID, appendErr),
		ProducedBy:   "internal/lensmodulator",
		WorkID:       workID,
		EvidenceRefs: []string{revRes.PreImageRef, revRes.PostImageRef},
	}, now)
	if ferr != nil {
		return RevertOutcome{}, fmt.Errorf(
			"lensmodulator: revert succeeded (route store restored to %q) but ledger append failed (%v) AND recovery finding could not be recorded (%v) — lane %q left with a STALE open ledger record; MANUAL RECOVERY REQUIRED (receipt %s, transition %s)",
			restoredValue, appendErr, ferr, lane, revRes.Receipt.ID, revRes.Transition.ID)
	}
	return RevertOutcome{}, fmt.Errorf(
		"lensmodulator: revert succeeded (route store restored to %q) but ledger append failed (%v) — recorded recovery finding %s for lane %q; the open-demote ledger record is now STALE (a later Revert will refuse to re-mutate from it)",
		restoredValue, appendErr, rec.ID, lane)
}

// CurrentAdjustment reads the current route/role value for a lane under the visibility-appropriate
// key root. ok=false means no entry exists.
func CurrentAdjustment(home string, v behaviorallens.Visibility, lane string) (string, bool, error) {
	return currentValue(RouteStorePath(home), laneRoot(v), lane)
}

// buildDemoteIntent constructs the governed reversible route/role demote intent for one lane. It is
// external_side_effect / easy reversibility / local blast, and carries a rollback_plan (act's gate
// refuses a non-read write without one).
func buildDemoteIntent(workID string, target envelope.ActionTarget, th Threshold, lensID, lane string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            idFor("act_", workID, CapabilityDemote, target.Path, target.ID, th.DemoteTo, lensID),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "lensmodulator",
		Capability:    CapabilityDemote,
		Action:        "demote_lane_route_role",
		Target:        target,
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("demote lane %q to %q on %s signal >= %.3f", lane, th.DemoteTo, lensID, th.MinValue),
		RollbackPlan:  fmt.Sprintf("restore captured pre-image route/role for lane %q via act.RevertIntent", lane),
	}
}

func unkeyableSkip(sig behaviorallens.InferredSignal) (Skip, bool) {
	if strings.Contains(sig.Subject, ".") {
		return Skip{LensID: sig.LensID, Lane: sig.Subject, Visibility: sig.Visibility, Reason: "subject not route-store keyable (contains '.')"}, true
	}
	return Skip{}, false
}

// --- action ledger (persist act.Result refs for revert) ---

type actionRecord struct {
	SchemaVersion  string  `json:"schema_version"`
	Kind           string  `json:"kind"` // "demote" | "revert"
	WorkID         string  `json:"work_id"`
	Lane           string  `json:"lane"`
	Root           string  `json:"root"`
	LensID         string  `json:"lens_id"`
	Visibility     string  `json:"visibility"`
	StorePath      string  `json:"store_path"`
	Baseline       string  `json:"baseline"`
	DemoteTo       string  `json:"demote_to"`
	Threshold      float64 `json:"threshold"`
	Capability     string  `json:"capability"`
	ActionIntentID string  `json:"action_intent_id"`
	PreImageRef    string  `json:"pre_image_ref"`
	PostImageRef   string  `json:"post_image_ref"`
	ReceiptID      string  `json:"receipt_id"`
	TransitionID   string  `json:"transition_id"`
	CreatedAt      string  `json:"created_at"`
}

// preflightActionLedger proves the action-ledger path is appendable BEFORE any route-store
// mutation, by attempting the exact open the real append later performs — MkdirAll the parent, then
// open the ledger PATH ITSELF in create/append mode — and closing WITHOUT writing a byte. The real
// failure mode this exists to catch is the ledger path itself being wedged (e.g. a directory sitting
// where the jsonl file must be): a probe file written elsewhere in the same parent directory would
// NOT catch that, because the parent stays writable while only the leaf name is wedged. O_APPEND
// never truncates, so a pre-existing ledger's bytes are left untouched; if the file was absent,
// closing without a Write leaves it either still absent or a zero-byte file that parses identically
// to absent (loadActionRecords treats both as zero records) — never junk content either way.
func preflightActionLedger(home string) error {
	path := actionLedgerPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("lensmodulator: action ledger directory not writable: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("lensmodulator: action ledger not writable: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("lensmodulator: action ledger not writable: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("lensmodulator: action ledger not writable: %w", err)
	}
	return nil
}

// afterAppendFailure handles a ledger-append failure that occurs AFTER a successful act.Execute: the
// demote is otherwise complete (route store mutated, receipt/transition recorded in the canonical
// event store) but this organ's OWN action-ledger row — the only place Revert() looks to find an
// open demote (latestOpenDemote) — never landed. Left alone, Revert() would report "no open demote
// for lane" FOREVER: a permanent strand, exactly what the package header's "REVERSIBLE by
// construction" invariant promises never happens.
//
// So: attempt a compensating governed revert THROUGH THE SAME act seam, restoring the pre-image the
// original act.Result already captured (result.PreImage — no re-read needed). If that succeeds the
// route store is back at baseline and there is nothing left to strand: a ledger with no row for a
// lane that is NOT demoted matches reality exactly, as if the demote had never been attempted. If the
// compensating revert ALSO fails, land a durable, operator-visible recovery finding naming the lane,
// receipt/transition ids, and pre/post image refs — manual recovery, never a silently swallowed
// error.
func afterAppendFailure(home string, policyCfg policy.Config, store *act.ArtifactStore, adapter act.Adapter, ai envelope.ActionIntent, result act.Result, sig behaviorallens.InferredSignal, th Threshold, root string, appendErr error, now time.Time) (ModulationResult, error) {
	revertAI := act.RevertIntent(ai, result, now)
	revertPD := policy.Evaluate(revertAI, policyCfg, now)
	revRes, revErr := act.ExecuteRevert(store, adapter, revertAI, revertPD, result.PreImage, now)
	if revErr == nil {
		// Best-effort: log the compensating revert to our own ledger too. Its failure is NOT fatal —
		// the route store is already back at baseline, so a ledger with no row for this lane matches
		// reality (not demoted). Swallowed on purpose: retrying through the same failing append would
		// only recurse into the same problem this function exists to route around.
		_ = appendActionRecordFn(home, actionRecord{
			SchemaVersion: ActionRecordSchemaVersion, Kind: "revert", WorkID: ai.IDs.WorkID,
			Lane: sig.Subject, Root: root, LensID: sig.LensID, Visibility: string(sig.Visibility),
			StorePath: ai.Target.Path, Baseline: th.Baseline, DemoteTo: th.DemoteTo, Threshold: th.MinValue,
			Capability: CapabilityDemote, ActionIntentID: revertAI.ID,
			PreImageRef: revRes.PreImageRef, PostImageRef: revRes.PostImageRef,
			ReceiptID: revRes.Receipt.ID, TransitionID: revRes.Transition.ID,
			CreatedAt: now.UTC().Format(time.RFC3339),
		})
		return ModulationResult{}, fmt.Errorf(
			"lensmodulator: action ledger append failed for lane %q demote (%v); compensating revert succeeded (revert receipt %s) — route store restored to pre-image, demote NOT stranded",
			sig.Subject, appendErr, revRes.Receipt.ID)
	}

	rec, ferr := finding.Record(home, finding.Finding{
		Topic: "lensmodulator-recovery/" + sig.Subject,
		Body: fmt.Sprintf(
			"lensmodulator demote left UNRECORDED in the action ledger and the compensating revert FAILED — manual recovery required.\n"+
				"lane: %s\naction_intent: %s\nreceipt_id: %s\ntransition_id: %s\n"+
				"pre_image_ref: %s\npost_image_ref: %s\nledger_append_error: %v\nrevert_error: %v\n",
			sig.Subject, ai.ID, result.Receipt.ID, result.Transition.ID, result.PreImageRef, result.PostImageRef, appendErr, revErr),
		ProducedBy:   "internal/lensmodulator",
		WorkID:       ai.IDs.WorkID,
		EvidenceRefs: []string{result.PreImageRef, result.PostImageRef},
	}, now)
	if ferr != nil {
		return ModulationResult{}, fmt.Errorf(
			"lensmodulator: action ledger append FAILED (%v), compensating revert FAILED (%v), AND recovery finding could not be recorded (%v) — lane %q left DEMOTED with NO ledger record; MANUAL RECOVERY REQUIRED (receipt %s, transition %s, pre_image_ref %s, post_image_ref %s)",
			appendErr, revErr, ferr, sig.Subject, result.Receipt.ID, result.Transition.ID, result.PreImageRef, result.PostImageRef)
	}
	return ModulationResult{}, fmt.Errorf(
		"lensmodulator: action ledger append failed (%v) and compensating revert failed (%v) — recorded recovery finding %s for lane %q; MANUAL RECOVERY REQUIRED (receipt %s, transition %s)",
		appendErr, revErr, rec.ID, sig.Subject, result.Receipt.ID, result.Transition.ID)
}

// appendActionRecordFn indirects appendActionRecord so a test can force the post-act ledger-append
// failure (the compensating-revert / recovery-finding paths in afterAppendFailure) that the real
// filesystem will not produce once preflightActionLedger has proven the ledger writable. It is
// unexported and swapped ONLY in tests, mirroring internal/act's recordEnvelopeFn seam; the
// production path always calls the real appendActionRecord.
var appendActionRecordFn = appendActionRecord

func appendActionRecord(home string, rec actionRecord) error {
	path := actionLedgerPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(&rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func loadActionRecords(home string) ([]actionRecord, error) {
	data, err := os.ReadFile(actionLedgerPath(home))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []actionRecord
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var rec actionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("parse action record line %d: %w", i+1, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// latestOpenDemote returns the most recent demote for a lane that has not since been reverted. The
// ledger is chronological and append-only, so a later "revert" row for the lane closes the demote.
func latestOpenDemote(records []actionRecord, lane string) (actionRecord, bool) {
	var open actionRecord
	found := false
	for _, rec := range records {
		if rec.Lane != lane {
			continue
		}
		switch rec.Kind {
		case "demote":
			open = rec
			found = true
		case "revert":
			found = false
		}
	}
	return open, found
}

// --- route-store helpers ---

func loadStore(path string) (map[string]any, error) {
	m := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse route store %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func saveStore(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ensureLaneEntry guarantees root.lane exists in the store, initialized to baseline if absent, so the
// governed ConfigKeyAdapter (which repoints, never creates) has a key to repoint. It reports whether
// it CREATED the entry this call (created=true) versus finding a pre-existing one (created=false), so
// a caller can compensate a materialized-then-orphaned baseline if the subsequent governed act fails.
func ensureLaneEntry(path, root, lane, baseline string) (created bool, err error) {
	m, err := loadStore(path)
	if err != nil {
		return false, err
	}
	sub, _ := m[root].(map[string]any)
	if sub == nil {
		sub = map[string]any{}
		m[root] = sub
	}
	if _, ok := sub[lane]; ok {
		return false, nil
	}
	sub[lane] = baseline
	if err := saveStore(path, m); err != nil {
		return false, err
	}
	return true, nil
}

// deleteLaneEntry removes root.lane from the store, restoring its absence. It is the compensation for
// a baseline entry ensureLaneEntry materialized THIS pass when the subsequent governed act failed —
// so a failed modulation leaves the route store's lane keys as it found them. Only the one key is
// removed; sibling lanes and a now-empty root map are left in place (an empty root is not lane state).
func deleteLaneEntry(path, root, lane string) error {
	m, err := loadStore(path)
	if err != nil {
		return err
	}
	sub, _ := m[root].(map[string]any)
	if sub == nil {
		return nil
	}
	delete(sub, lane)
	return saveStore(path, m)
}

func currentValue(path, root, lane string) (string, bool, error) {
	m, err := loadStore(path)
	if err != nil {
		return "", false, err
	}
	sub, _ := m[root].(map[string]any)
	if sub == nil {
		return "", false, nil
	}
	v, ok := sub[lane]
	if !ok {
		return "", false, nil
	}
	return fmt.Sprintf("%v", v), true, nil
}

func idFor(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}
