package work

// Design-sketch gate (alignment depth scales with risk): a WorkEnvelope may declare a
// PlanClass (envelope.PlanClasses) saying how much alignment it owes before it runs. v1 binds
// on exactly ONE class -- `phased` work must carry an attached `design-sketch:` artifact before
// it may enter `running`.
//
// The rule is deliberately HARDCODED here rather than read from a registry. A policy surface
// would be a second place to look for the answer, and the whole point of the gate is that the
// alignment happens BEFORE the work starts; a configurable gate invites the configuration to be
// widened at the moment it would first bite. The escape hatch is not a config key, it is
// declaring a shallower plan_class at create time, which is an auditable envelope field.
//
// The gate mirrors the autonomy-halt / editable-surfaces / freeze deny pattern: it runs at the
// generic Advance seam (advanceLocked) BEFORE any mutation, and a refusal writes durable
// evidence (an ActionIntent + a deny PolicyDecision + a lifecycle event that does not move
// state) so a refused start is auditable rather than an error that dies with the process.

import (
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
)

// PlanClassPhased is the only plan class gated in v1.
const PlanClassPhased = "phased"

// designSketchArtifactPrefix is the artifact-ref kind the gate scans for. Artifact refs are
// free strings on the envelope with no central validator; kind prefixes are a SCAN-TIME
// convention each consumer parses for itself, exactly like `commit:` in land_check.go.
const designSketchArtifactPrefix = "design-sketch:"

// designSketchMissingReason is the machine-readable refusal token. Callers (and tests) key on
// this rather than on the prose, so the teaching text can be improved without breaking them.
const designSketchMissingReason = "design_sketch_missing"

// designSketchArtifactRef returns the most recently attached `design-sketch:<ref>` artifact.
// Scans newest-first so a later sketch supersedes an earlier one, mirroring
// latestCommitArtifact. A bare `design-sketch:` with no ref does not count -- an empty pointer
// is not a sketch, and accepting it would make the gate satisfiable by a no-op attach.
func designSketchArtifactRef(artifactIDs []string) (string, bool) {
	for i := len(artifactIDs) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(artifactIDs[i])
		if !strings.HasPrefix(candidate, designSketchArtifactPrefix) {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(candidate, designSketchArtifactPrefix)) == "" {
			continue
		}
		return candidate, true
	}
	return "", false
}

// designSketchGuard denies a `phased` work entering `running` without an attached design-sketch
// artifact, and records the denial durably. It is a no-op for every other plan class (including
// EMPTY, which means oneshot for every envelope written before the field existed), for every
// transition that is not INTO running, and for work already running -- the gate fires on the
// edge, so a same-state attach on running work is never re-gated.
func (s *Store) designSketchGuard(w *envelope.WorkEnvelope, to, actor string, now time.Time) error {
	if w == nil || to != "running" || w.State == "running" {
		return nil
	}
	if strings.TrimSpace(w.PlanClass) != PlanClassPhased {
		return nil
	}
	if _, ok := designSketchArtifactRef(w.ArtifactIDs); ok {
		return nil
	}
	reason := fmt.Sprintf(
		"%s: phased work requires an attached design-sketch artifact (work advance %s --to %s --artifact %s<ref>) before advancing to running",
		designSketchMissingReason, w.ID, w.State, designSketchArtifactPrefix,
	)
	if err := s.recordDesignSketchDeny(*w, actor, to, reason, now); err != nil {
		return err
	}
	return fmt.Errorf("%s", reason)
}

// recordDesignSketchDeny writes the ActionIntent and a deny PolicyDecision for a phased work
// refused entry into running, plus a lifecycle event, so the denial leaves an auditable trail
// while the work stays in place -- the same shape as recordEditableSurfaceDeny.
//
// The deny PolicyDecision is built here rather than in internal/policy because this gate owns
// no policy registry (see the file header): every internal/policy deny constructor exists to
// attach ITS registry as an authority ref, and there is no such registry to cite. The decision
// is descriptive audit content, not a live gate an adapter evaluates.
func (s *Store) recordDesignSketchDeny(w envelope.WorkEnvelope, actor, to, reason string, now time.Time) error {
	ai := s.advanceActionIntent(w, actor, to, now, AdvancePlanOptions{Actor: actor, To: to})
	if err := ai.Validate(); err != nil {
		return err
	}
	pd := designSketchDenyDecision(ai, reason, now)
	if err := pd.Validate(); err != nil {
		return err
	}
	if err := s.writeEventJSON(ai.ID, &ai); err != nil {
		return err
	}
	if err := s.writeEventJSON(pd.ID, &pd); err != nil {
		return err
	}
	return s.appendEvent(Event{
		TS:       ts(now),
		WorkID:   w.ID,
		From:     w.State,
		To:       w.State,
		By:       actor,
		Note:     "design-sketch deny: " + reason,
		Attached: []string{ai.ID, pd.ID},
	})
}

// designSketchDenyDecision builds the recorded deny PolicyDecision for a refused phased start.
func designSketchDenyDecision(ai envelope.ActionIntent, reason string, now time.Time) envelope.PolicyDecision {
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: envelope.SVPolicyDecision,
			ID:            idFor("pol_", ai.ID, "deny", ts(now)),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: ai.IDs.WorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       orDefault(ai.Privacy, "private"),
			Risk:          orDefault(ai.Risk, "tier1"),
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		ActionIntentID: ai.ID,
		Outcome:        "deny",
		Reasons:        []string{reason},
		Obligations:    []string{},
	}
}
