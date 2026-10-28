package work

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"compound/internal/envelope"
)

// ExecutionSelectionResolver is the read-only seam the launch wire and the
// save seam both consult to turn a launch profile id into its harness, route,
// and model identity. The production resolver lives in routeevidence
// (owning the Compound model-router authority); tests inject a fixture to
// avoid loading a manifest.
//
// A failure here means the operator named a launch profile id that the
// owner authority does not know. The save seam surfaces this refusal
// verbatim (the contract) and the launch seam falls back to the project
// default — exactly the same shape work.DelegateHarnessResolver follows.
//
// work_r7a_execution_selection_20260911.
type ExecutionSelectionResolver func(req ExecutionSelectionResolveRequest) (ExecutionSelectionResolution, error)

// ExecutionSelectionResolveRequest is the read-only input the resolver
// accepts. CommandOverride lets a CLI pass --effort overrides the wire
// resolved from the profile's requested native effort.
type ExecutionSelectionResolveRequest struct {
	HomePath        string
	LaunchProfileID string
	CommandOverride string
	EffortOverride  string
	Now             time.Time
}

// ExecutionSelectionResolution is the bounded identity the resolver returns.
// A zero value means the resolver could not produce a non-trivial identity
// (the launch profile id was unknown, or the resolver was not installed);
// the save seam refuses in that case and the launch seam falls through to
// the project default.
type ExecutionSelectionResolution struct {
	LaunchProfileID       string
	LaunchProfileRevision string
	Harness               string
	ProviderRoute         string
	RequestedModelID      string
	RequestedEffort       string
}

// DefaultExecutionSelectionResolver is the production resolver the work
// package exposes for launch profile id -> {harness, route, model, effort}
// resolution. It is set by routeevidence (the Compound model-router
// authority owner) at Install time; tests inject a fixture via
// SwapExecutionSelectionResolver. The seam here intentionally does NOT
// import routeevidence to keep the work package free of the
// routeevidence dependency cycle the executewire package exists to break.
var DefaultExecutionSelectionResolver ExecutionSelectionResolver

// SwapExecutionSelectionResolver is the test seam: it returns a restore
// func the caller MUST defer so subsequent tests see the production
// resolver. Production code MUST NOT call this.
func SwapExecutionSelectionResolver(fn ExecutionSelectionResolver) func() {
	prev := DefaultExecutionSelectionResolver
	DefaultExecutionSelectionResolver = fn
	return func() { DefaultExecutionSelectionResolver = prev }
}

// ResolveSelection walks the seam: it asks the installed resolver to map a
// launch profile id to the identity tuple the envelope stores. The
// returned envelope.ExecutionSelection is fully populated (Source, Revision,
// SelectedBy, SelectedAt) so a caller can persist it directly. An error
// from the resolver is returned verbatim — the contract says the owner
// authority's refusal is the truth the work envelope records.
//
// The optional command / effort overrides let CLI flags (`--effort`) layer
// on top of the profile's own resolved effort without rewriting the
// profile; the saved selection records the override verbatim so the
// reviewer sees what was actually asked for.
//
// work_r7a_execution_selection_20260911.
func ResolveSelection(req ExecutionSelectionResolveRequest, actor string, now time.Time) (envelope.ExecutionSelection, error) {
	if strings.TrimSpace(req.LaunchProfileID) == "" {
		return envelope.ExecutionSelection{}, errors.New("resolve execution selection: launch_profile_id is required")
	}
	resolver := DefaultExecutionSelectionResolver
	if resolver == nil {
		return envelope.ExecutionSelection{}, errors.New("resolve execution selection: no execution selection resolver installed (routeevidence not wired)")
	}
	resolved, err := resolver(req)
	if err != nil {
		return envelope.ExecutionSelection{}, fmt.Errorf("resolve execution selection %q: %w", req.LaunchProfileID, err)
	}
	if strings.TrimSpace(resolved.Harness) == "" {
		return envelope.ExecutionSelection{}, fmt.Errorf("resolve execution selection %q: owner authority returned empty harness", req.LaunchProfileID)
	}
	if strings.TrimSpace(resolved.RequestedModelID) == "" {
		return envelope.ExecutionSelection{}, fmt.Errorf("resolve execution selection %q: owner authority returned empty requested_model_id", req.LaunchProfileID)
	}
	effort := strings.TrimSpace(resolved.RequestedEffort)
	if override := strings.TrimSpace(req.EffortOverride); override != "" {
		effort = override
	}
	out := envelope.ExecutionSelection{
		LaunchProfileID:       strings.TrimSpace(req.LaunchProfileID),
		LaunchProfileRevision: strings.TrimSpace(resolved.LaunchProfileRevision),
		Harness:               strings.TrimSpace(resolved.Harness),
		ProviderRoute:         strings.TrimSpace(resolved.ProviderRoute),
		RequestedModelID:      strings.TrimSpace(resolved.RequestedModelID),
		RequestedEffort:       effort,
		Source:                "explicit",
		Revision:              1,
		SelectedBy:            strings.TrimSpace(actor),
		SelectedAt:            now.UTC().Format(time.RFC3339),
	}
	if err := out.Validate(); err != nil {
		return envelope.ExecutionSelection{}, fmt.Errorf("resolve execution selection %q: %w", req.LaunchProfileID, err)
	}
	return out, nil
}

// SaveExecutionSelection records an EXPLICIT save of the work-level
// execution selection. source MUST be "explicit" — implicit copies of the
// project / system default land through BuildWork, never here. The new
// selection is validated, the revision is bumped from any prior saved
// revision by exactly +1, and a same-state lifecycle event is appended
// whose Note follows the prescribed format:
//
//	"execution_selection rev=<n> launch_profile=<id> harness=<h> route=<r> model=<m> effort=<e> source=explicit (was rev=<n-1> <prior-summary>)"
//
// expectedPrevRevision is the CAS guard: 0 means "no prior expected" (the
// first save), any other value MUST match the envelope's current revision
// exactly. A stale writer is refused with a typed error the CLI surfaces
// verbatim — the contract the packet pins.
//
// The persisted envelope validates through envelope.WorkEnvelope.Validate
// (the validator delegates to validateExecutionSelection), so an invalid
// selection is refused BEFORE the file is rewritten — the historical
// envelope stays byte-identical on the failure path.
//
// work_r7a_execution_selection_20260911.
func (s *Store) SaveExecutionSelection(workID string, expectedPrevRevision int, sel envelope.ExecutionSelection, actor string, now time.Time) (envelope.WorkEnvelope, error) {
	var out envelope.WorkEnvelope
	var prev envelope.WorkEnvelope
	var prevSel envelope.ExecutionSelection
	err := s.withLifecycleMutation(func() error {
		cur, getErr := s.Get(workID)
		if getErr != nil {
			return getErr
		}
		if cur.State == "completed" || cur.State == "failed" || cur.State == "cancelled" {
			return fmt.Errorf("save execution selection %q: work is %s (terminal); cannot replace saved selection", workID, cur.State)
		}
		if sel.Source != "explicit" {
			return fmt.Errorf("save execution selection %q: source must be explicit, got %q", workID, sel.Source)
		}
		if err := sel.Validate(); err != nil {
			return fmt.Errorf("save execution selection %q: %w", workID, err)
		}
		if cur.ExecutionSelection != nil {
			prevSel = *cur.ExecutionSelection
			if expectedPrevRevision > 0 && cur.ExecutionSelection.Revision != expectedPrevRevision {
				return fmt.Errorf("save execution selection %q: execution_selection revision changed: expected %d, found %d", workID, expectedPrevRevision, cur.ExecutionSelection.Revision)
			}
			if sel.Revision != cur.ExecutionSelection.Revision+1 {
				sel.Revision = cur.ExecutionSelection.Revision + 1
			}
		} else {
			if expectedPrevRevision > 0 {
				return fmt.Errorf("save execution selection %q: execution_selection revision changed: expected %d, found 0", workID, expectedPrevRevision)
			}
			if sel.Revision != 1 {
				sel.Revision = 1
			}
		}
		selectedBy := strings.TrimSpace(actor)
		if selectedBy == "" {
			selectedBy = sel.SelectedBy
		}
		if selectedBy == "" {
			selectedBy = "compound"
		}
		selectedAt := sel.SelectedAt
		if selectedAt == "" {
			selectedAt = now.UTC().Format(time.RFC3339)
		}
		copySel := sel
		copySel.SelectedBy = selectedBy
		copySel.SelectedAt = selectedAt
		if err := copySel.Validate(); err != nil {
			return fmt.Errorf("save execution selection %q: %w", workID, err)
		}
		cur.ExecutionSelection = &copySel
		// envelope.WorkEnvelope.Validate is the contract gate — it routes
		// through validateExecutionSelection so the same field rules the
		// post-write load uses.
		if err := cur.Validate(); err != nil {
			return fmt.Errorf("save execution selection %q: persisted envelope invalid: %w", workID, err)
		}
		prev = cur
		// out captures the freshly-built envelope for the same-state event.
		out = cur
		note := formatSelectionSaveNote(prevSel, copySel)
		event := Event{
			TS:     ts(now),
			WorkID: workID,
			From:   cur.State,
			To:     cur.State,
			By:     selectedBy,
			Note:   note,
		}
		if err := s.appendEventUnlocked(event); err != nil {
			return fmt.Errorf("save execution selection %q: append lifecycle event: %w", workID, err)
		}
		if err := s.writeJSON(s.workPath(workID), &cur); err != nil {
			return fmt.Errorf("save execution selection %q: persist envelope: %w", workID, err)
		}
		return nil
	})
	if err != nil {
		return prev, err
	}
	return out, nil
}

// formatSelectionSaveNote renders the prescribed same-state lifecycle note
// for an explicit save. The "(was rev=<n-1> ...)" suffix is present only
// when there WAS a prior selection (revision > 1 save).
func formatSelectionSaveNote(prev envelope.ExecutionSelection, cur envelope.ExecutionSelection) string {
	parts := []string{
		"execution_selection",
		fmt.Sprintf("rev=%d", cur.Revision),
		fmt.Sprintf("launch_profile=%s", cur.LaunchProfileID),
		fmt.Sprintf("harness=%s", cur.Harness),
		fmt.Sprintf("route=%s", cur.ProviderRoute),
		fmt.Sprintf("model=%s", cur.RequestedModelID),
		fmt.Sprintf("effort=%s", cur.RequestedEffort),
		"source=" + cur.Source,
	}
	if prev.Revision > 0 {
		parts = append(parts, fmt.Sprintf("(was rev=%d launch_profile=%s harness=%s route=%s model=%s effort=%s source=%s)",
			prev.Revision, prev.LaunchProfileID, prev.Harness, prev.ProviderRoute, prev.RequestedModelID, prev.RequestedEffort, prev.Source))
	}
	return strings.Join(parts, " ")
}

// SelectedExecutionSelection returns the saved selection for workID, or a
// zero value + false when the work has no saved selection. Read-only; the
// launch wire consults this before falling through to the project default.
func (s *Store) SelectedExecutionSelection(workID string) (envelope.ExecutionSelection, bool, error) {
	w, err := s.Get(workID)
	if err != nil {
		return envelope.ExecutionSelection{}, false, err
	}
	if w.ExecutionSelection == nil {
		return envelope.ExecutionSelection{}, false, nil
	}
	return *w.ExecutionSelection, true, nil
}
