// Package hookplan owns desired hook bindings and their reconciliation against
// read-only harness installation evidence. Runtime hook handling stays in
// internal/hook; observed registrations never become desired authority here.
package hookplan

import (
	"fmt"
	"sort"

	"compound/internal/harness"
)

const (
	HookPlanSchemaVersion             = "compound.hook_plan.v0.1"
	InstallationSnapshotSchemaVersion = "compound.hook_installation_snapshot.v0.1"
	ReconciliationSchemaVersion       = "compound.hook_reconciliation.v0.1"
)

const (
	ActivationManaged  = "managed"
	ActivationOperator = "operator"
)

const (
	StateRegistered         = "registered"
	StateReady              = "ready"
	StateMissing            = "missing"
	StateExtraUnowned       = "extra_unowned"
	StateExternalPreserved  = "external_preserved"
	StateDrifted            = "drifted"
	StateActivationRequired = "activation_required"
	StateUnsupported        = "unsupported"
)

// HookPlan is desired authority. It is intentionally a different type from
// InstallationSnapshot so observations cannot be promoted by assignment.
type HookPlan struct {
	SchemaVersion     string              `json:"schema_version" yaml:"schema_version"`
	ID                string              `json:"id" yaml:"id"`
	AuthorityRef      string              `json:"authority_ref,omitempty" yaml:"authority_ref,omitempty"`
	Revision          string              `json:"revision" yaml:"revision"`
	CompiledFrom      *CompilationContext `json:"compiled_from,omitempty" yaml:"compiled_from,omitempty"`
	Intentions        []CompiledIntention `json:"intentions,omitempty" yaml:"intentions,omitempty"`
	Bindings          []Binding           `json:"bindings" yaml:"bindings"`
	PreservedBindings []Binding           `json:"preserved_bindings,omitempty" yaml:"preserved_bindings,omitempty"`
}

// Binding is one desired harness event binding.
type Binding struct {
	ID                  string           `json:"id" yaml:"id"`
	Harness             string           `json:"harness" yaml:"harness"`
	Event               string           `json:"event" yaml:"event"`
	Matcher             string           `json:"matcher,omitempty" yaml:"matcher,omitempty"`
	HandlerRef          string           `json:"handler_ref" yaml:"handler_ref"`
	InvocationRevision  string           `json:"invocation_revision,omitempty" yaml:"invocation_revision,omitempty"`
	HandlerRevision     string           `json:"handler_revision,omitempty" yaml:"handler_revision,omitempty"`
	Intention           HarnessIntention `json:"intention,omitempty" yaml:"intention,omitempty"`
	Disposition         Disposition      `json:"disposition,omitempty" yaml:"disposition,omitempty"`
	EnforcementStrength string           `json:"enforcement_strength,omitempty" yaml:"enforcement_strength,omitempty"`
	Activation          string           `json:"activation,omitempty" yaml:"activation,omitempty"`
	Blocking            bool             `json:"blocking,omitempty" yaml:"blocking,omitempty"`
}

// InstallationSnapshot is observed evidence from harness-owned config files.
type InstallationSnapshot struct {
	SchemaVersion string            `json:"schema_version"`
	Ref           string            `json:"ref"`
	Revision      string            `json:"revision"`
	Bindings      []ObservedBinding `json:"bindings"`
}

// ObservedBinding describes what a harness config currently registers. It is
// evidence only and carries no desired-state fields.
type ObservedBinding struct {
	BindingID          string `json:"binding_id,omitempty"`
	Harness            string `json:"harness"`
	Event              string `json:"event"`
	Matcher            string `json:"matcher,omitempty"`
	HandlerRef         string `json:"handler_ref"`
	InvocationRevision string `json:"invocation_revision,omitempty"`
	HandlerRevision    string `json:"handler_revision,omitempty"`
	Active             bool   `json:"active"`
	SourceRef          string `json:"source_ref,omitempty"`
}

type Delta struct {
	BindingID                  string `json:"binding_id,omitempty"`
	Harness                    string `json:"harness"`
	Event                      string `json:"event"`
	Matcher                    string `json:"matcher,omitempty"`
	State                      string `json:"state"`
	DesiredHandlerRef          string `json:"desired_handler_ref,omitempty"`
	ObservedHandlerRef         string `json:"observed_handler_ref,omitempty"`
	DesiredInvocationRevision  string `json:"desired_invocation_revision,omitempty"`
	ObservedInvocationRevision string `json:"observed_invocation_revision,omitempty"`
	DesiredHandlerRevision     string `json:"desired_handler_revision,omitempty"`
	ObservedHandlerRevision    string `json:"observed_handler_revision,omitempty"`
	ObservedSourceRef          string `json:"observed_source_ref,omitempty"`
	Reason                     string `json:"reason,omitempty"`
}

type Reconciliation struct {
	SchemaVersion       string  `json:"schema_version"`
	PlanID              string  `json:"plan_id"`
	PlanRevision        string  `json:"plan_revision"`
	ObservationRef      string  `json:"observation_ref"`
	ObservationRevision string  `json:"observation_revision"`
	Deltas              []Delta `json:"deltas"`
}

// Reconcile compares desired authority to observed evidence without mutating
// either input.
func Reconcile(plan HookPlan, observed InstallationSnapshot, registry harness.Registry) (Reconciliation, error) {
	out := Reconciliation{
		SchemaVersion:       ReconciliationSchemaVersion,
		PlanID:              plan.ID,
		PlanRevision:        plan.Revision,
		ObservationRef:      observed.Ref,
		ObservationRevision: observed.Revision,
	}
	desiredBindings := append([]Binding(nil), plan.Bindings...)
	preservedBindings := append([]Binding(nil), plan.PreservedBindings...)
	observedBindings := append([]ObservedBinding(nil), observed.Bindings...)
	sort.SliceStable(desiredBindings, func(i, j int) bool {
		return bindingSortKey(desiredBindings[i]) < bindingSortKey(desiredBindings[j])
	})
	sort.SliceStable(observedBindings, func(i, j int) bool {
		return observedBindingSortKey(observedBindings[i]) < observedBindingSortKey(observedBindings[j])
	})
	sort.SliceStable(preservedBindings, func(i, j int) bool {
		return bindingSortKey(preservedBindings[i]) < bindingSortKey(preservedBindings[j])
	})
	if err := validateDesiredBindings(desiredBindings); err != nil {
		return Reconciliation{}, err
	}

	reservedForPreserved := preservedObservationReservations(desiredBindings, preservedBindings, observedBindings)
	consumed := make([]bool, len(observedBindings))
	for _, desired := range desiredBindings {
		matched := -1
		var slotCandidates []int
		var identityMatches []int
		for i, actual := range observedBindings {
			if consumed[i] || reservedForPreserved[i] || !sameSlot(desired, actual) {
				continue
			}
			if actual.BindingID != "" && actual.BindingID != desired.ID {
				continue
			}
			slotCandidates = append(slotCandidates, i)
			if actual.BindingID == desired.ID || handlerInvocationMatches(desired, actual) {
				identityMatches = append(identityMatches, i)
			}
		}
		if len(identityMatches) == 1 {
			matched = identityMatches[0]
		}
		if len(identityMatches) > 1 {
			return Reconciliation{}, fmt.Errorf("hook plan binding %q has %d ambiguous identity matches for %s/%s matcher %q", desired.ID, len(identityMatches), desired.Harness, desired.Event, desired.Matcher)
		}
		if matched < 0 && len(slotCandidates) == 1 {
			matched = slotCandidates[0]
		}
		if matched < 0 && len(slotCandidates) > 1 {
			return Reconciliation{}, fmt.Errorf("hook plan binding %q has %d ambiguous observed registrations for %s/%s matcher %q", desired.ID, len(slotCandidates), desired.Harness, desired.Event, desired.Matcher)
		}
		delta := Delta{
			BindingID:                 desired.ID,
			Harness:                   desired.Harness,
			Event:                     desired.Event,
			Matcher:                   desired.Matcher,
			DesiredHandlerRef:         desired.HandlerRef,
			DesiredInvocationRevision: desired.InvocationRevision,
			DesiredHandlerRevision:    desired.HandlerRevision,
			State:                     StateMissing,
		}
		if !bindingSupported(desired, registry) {
			delta.State = StateUnsupported
			delta.Reason = "harness registry does not support the desired event capability"
		}
		if matched >= 0 {
			actual := observedBindings[matched]
			consumed[matched] = true
			delta.ObservedHandlerRef = actual.HandlerRef
			delta.ObservedInvocationRevision = actual.InvocationRevision
			delta.ObservedHandlerRevision = actual.HandlerRevision
			delta.ObservedSourceRef = actual.SourceRef
			switch {
			case delta.State == StateUnsupported:
			case !actual.Active:
				delta.State = StateActivationRequired
				delta.Reason = "desired binding is installed but inactive"
			case desired.HandlerRef != actual.HandlerRef || invocationRevisionDrifted(desired, actual) || handlerRevisionDrifted(desired, actual):
				delta.State = StateDrifted
				delta.Reason = "observed handler or invocation does not match desired binding"
			case handlerRevisionVerified(desired, actual):
				delta.State = StateReady
			default:
				delta.State = StateRegistered
				delta.Reason = "handler invocation matches; content revision is unverified"
			}
		} else if delta.State != StateUnsupported && desired.Activation == ActivationOperator {
			delta.State = StateActivationRequired
			delta.Reason = "desired binding requires operator activation"
		}
		out.Deltas = append(out.Deltas, delta)
	}
	for _, preserved := range preservedBindings {
		matched := -1
		for i, actual := range observedBindings {
			if consumed[i] || !preservedBindingMatches(preserved, actual) {
				continue
			}
			matched = i
			break
		}
		if matched < 0 {
			continue
		}
		actual := observedBindings[matched]
		consumed[matched] = true
		out.Deltas = append(out.Deltas, Delta{
			BindingID:                  preserved.ID,
			Harness:                    actual.Harness,
			Event:                      actual.Event,
			Matcher:                    actual.Matcher,
			State:                      StateExternalPreserved,
			ObservedHandlerRef:         actual.HandlerRef,
			ObservedInvocationRevision: actual.InvocationRevision,
			ObservedHandlerRevision:    actual.HandlerRevision,
			ObservedSourceRef:          actual.SourceRef,
			Reason:                     "observed external hook exactly matches preserved declaration",
		})
	}
	for i, actual := range observedBindings {
		if consumed[i] {
			continue
		}
		out.Deltas = append(out.Deltas, Delta{
			BindingID:                  actual.BindingID,
			Harness:                    actual.Harness,
			Event:                      actual.Event,
			Matcher:                    actual.Matcher,
			State:                      StateExtraUnowned,
			ObservedHandlerRef:         actual.HandlerRef,
			ObservedInvocationRevision: actual.InvocationRevision,
			ObservedHandlerRevision:    actual.HandlerRevision,
			ObservedSourceRef:          actual.SourceRef,
		})
	}
	return out, nil
}

func validateDesiredBindings(bindings []Binding) error {
	seenSlots := map[string]string{}
	for _, binding := range bindings {
		key := bindingSlotKey(binding.Harness, binding.Event, binding.Matcher)
		if priorID, exists := seenSlots[key]; exists {
			return fmt.Errorf("duplicate desired hook slot %s/%s matcher %q in bindings %q and %q", binding.Harness, binding.Event, binding.Matcher, priorID, binding.ID)
		}
		seenSlots[key] = binding.ID
	}
	return nil
}

func bindingSortKey(binding Binding) string {
	return bindingSlotKey(binding.Harness, binding.Event, binding.Matcher) + "\x00" + binding.ID + "\x00" + binding.HandlerRef + "\x00" + binding.InvocationRevision
}

func observedBindingSortKey(binding ObservedBinding) string {
	return bindingSlotKey(binding.Harness, binding.Event, binding.Matcher) + "\x00" + binding.BindingID + "\x00" + binding.HandlerRef + "\x00" + binding.InvocationRevision + "\x00" + binding.SourceRef
}

func bindingSlotKey(harnessName, event, matcher string) string {
	return harnessName + "\x00" + event + "\x00" + matcher
}

func sameSlot(desired Binding, actual ObservedBinding) bool {
	return desired.Harness == actual.Harness && desired.Event == actual.Event && desired.Matcher == actual.Matcher
}

func preservedBindingMatches(preserved Binding, actual ObservedBinding) bool {
	if !sameSlot(preserved, actual) || preserved.HandlerRef != actual.HandlerRef || preserved.InvocationRevision != actual.InvocationRevision {
		return false
	}
	return preserved.HandlerRevision == "" || preserved.HandlerRevision == actual.HandlerRevision
}

func preservedObservationReservations(desired, preserved []Binding, observed []ObservedBinding) []bool {
	reserved := make([]bool, len(observed))
	for i, actual := range observed {
		managedIdentity := false
		for _, binding := range desired {
			if desiredBindingIdentityMatches(binding, actual) {
				managedIdentity = true
				break
			}
		}
		if managedIdentity {
			continue
		}
		for _, binding := range preserved {
			if preservedBindingMatches(binding, actual) {
				reserved[i] = true
				break
			}
		}
	}
	return reserved
}

func desiredBindingIdentityMatches(desired Binding, actual ObservedBinding) bool {
	if !sameSlot(desired, actual) || (actual.BindingID != "" && actual.BindingID != desired.ID) {
		return false
	}
	return actual.BindingID == desired.ID || handlerInvocationMatches(desired, actual)
}

func bindingSupported(binding Binding, registry harness.Registry) bool {
	if _, found := registry.Get(binding.Harness); !found {
		return false
	}
	if binding.Blocking && binding.Event != "PreToolUse" {
		return false
	}
	return registry.Supports(binding.Harness, binding.Event)
}

func handlerRevisionDrifted(desired Binding, actual ObservedBinding) bool {
	return desired.HandlerRevision != "" && desired.HandlerRevision != actual.HandlerRevision
}

func handlerRevisionVerified(desired Binding, actual ObservedBinding) bool {
	return desired.HandlerRevision != "" && desired.HandlerRevision == actual.HandlerRevision
}

func invocationRevisionDrifted(desired Binding, actual ObservedBinding) bool {
	return desired.InvocationRevision != "" && actual.InvocationRevision != "" && desired.InvocationRevision != actual.InvocationRevision
}

func handlerInvocationMatches(desired Binding, actual ObservedBinding) bool {
	if desired.HandlerRef != actual.HandlerRef {
		return false
	}
	if desired.InvocationRevision == "" {
		return true
	}
	return desired.InvocationRevision == actual.InvocationRevision
}
