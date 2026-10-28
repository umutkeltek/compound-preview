package hookplan

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"compound/internal/harness"
)

func TestReconcileObservedExtraNeverBecomesDesired(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Revision:      "sha256:plan",
		Bindings: []Binding{
			{
				ID:         "hook://claude_code/SessionStart",
				Harness:    "claude_code",
				Event:      "SessionStart",
				HandlerRef: "$HOME/.claude/hooks/RecallContext.hook.ts",
			},
		},
	}
	observed := InstallationSnapshot{
		SchemaVersion: InstallationSnapshotSchemaVersion,
		Ref:           "harness-installation://live",
		Revision:      "sha256:observed",
		Bindings: []ObservedBinding{
			{
				Harness:    "claude_code",
				Event:      "SessionStart",
				HandlerRef: "$HOME/.claude/hooks/RecallContext.hook.ts",
				Active:     true,
			},
			{
				Harness:    "claude_code",
				Event:      "SessionEnd",
				HandlerRef: "$HOME/.claude/hooks/SessionReceipt.hook.ts",
				Active:     true,
			},
		},
	}

	got, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(plan.Bindings) != 1 || plan.Bindings[0].Event != "SessionStart" {
		t.Fatalf("observed bindings changed desired plan: %+v", plan.Bindings)
	}
	if len(got.Deltas) != 2 {
		t.Fatalf("Reconcile() deltas = %+v, want registered + extra_unowned", got.Deltas)
	}
	if got.Deltas[0].State != StateRegistered || got.Deltas[0].Event != "SessionStart" {
		t.Fatalf("first delta = %+v, want SessionStart registered", got.Deltas[0])
	}
	if got.Deltas[1].State != StateExtraUnowned || got.Deltas[1].Event != "SessionEnd" {
		t.Fatalf("second delta = %+v, want SessionEnd extra_unowned", got.Deltas[1])
	}
}

func TestReconcilePreservedExternalCoexistsWithDesiredBindingInSameSlot(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Bindings: []Binding{
			{
				ID:                 "recall",
				Harness:            "claude_code",
				Event:              "SessionStart",
				HandlerRef:         "$HOME/.claude/hooks/RecallContext.hook.ts",
				InvocationRevision: "sha256:recall-invocation",
				HandlerRevision:    "sha256:recall-content",
			},
		},
		PreservedBindings: []Binding{
			{
				ID:                 "kitty-env",
				Harness:            "claude_code",
				Event:              "SessionStart",
				HandlerRef:         "$HOME/.claude/hooks/KittyEnvPersist.hook.ts",
				InvocationRevision: "sha256:kitty-invocation",
				HandlerRevision:    "sha256:kitty-content",
			},
		},
	}
	observed := InstallationSnapshot{
		SchemaVersion: InstallationSnapshotSchemaVersion,
		Bindings: []ObservedBinding{
			{
				Harness:            "claude_code",
				Event:              "SessionStart",
				HandlerRef:         "$HOME/.claude/hooks/KittyEnvPersist.hook.ts",
				InvocationRevision: "sha256:kitty-invocation",
				HandlerRevision:    "sha256:kitty-content",
				Active:             true,
			},
			{
				Harness:            "claude_code",
				Event:              "SessionStart",
				HandlerRef:         "$HOME/.claude/hooks/RecallContext.hook.ts",
				InvocationRevision: "sha256:recall-invocation",
				HandlerRevision:    "sha256:recall-content",
				Active:             true,
			},
			{
				Harness:    "claude_code",
				Event:      "SessionEnd",
				HandlerRef: "$HOME/.claude/hooks/UnknownExtra.hook.ts",
				Active:     true,
			},
		},
	}

	got, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if delta := reconciliationDeltaByID(got, "recall"); delta == nil || delta.State != StateReady {
		t.Fatalf("desired RecallContext delta = %+v, want ready", delta)
	}
	if delta := reconciliationDeltaByID(got, "kitty-env"); delta == nil || delta.State != StateExternalPreserved {
		t.Fatalf("preserved KittyEnvPersist delta = %+v, want external_preserved", delta)
	}
	if len(got.Deltas) != 3 || got.Deltas[2].State != StateExtraUnowned || got.Deltas[2].Event != "SessionEnd" {
		t.Fatalf("reconciliation deltas = %+v, want desired + preserved + unknown extra", got.Deltas)
	}
}

func TestReconcileReservesExactPreservedObservationWhenManagedSameSlotIsMissing(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Bindings: []Binding{
			{
				ID:                 "recall",
				Harness:            "claude_code",
				Event:              "SessionStart",
				HandlerRef:         "$HOME/.claude/hooks/RecallContext.hook.ts",
				InvocationRevision: "sha256:recall-invocation",
			},
		},
		PreservedBindings: []Binding{
			{
				ID:                 "kitty-env",
				Harness:            "claude_code",
				Event:              "SessionStart",
				HandlerRef:         "$HOME/.claude/hooks/KittyEnvPersist.hook.ts",
				InvocationRevision: "sha256:kitty-invocation",
				HandlerRevision:    "sha256:kitty-content",
			},
		},
	}
	observed := InstallationSnapshot{Bindings: []ObservedBinding{
		{
			Harness:            "claude_code",
			Event:              "SessionStart",
			HandlerRef:         "$HOME/.claude/hooks/KittyEnvPersist.hook.ts",
			InvocationRevision: "sha256:kitty-invocation",
			HandlerRevision:    "sha256:kitty-content",
			Active:             true,
		},
	}}

	got, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if managed := reconciliationDeltaByID(got, "recall"); managed == nil || managed.State != StateMissing || managed.ObservedHandlerRef != "" {
		t.Fatalf("missing managed same-slot delta = %+v, want missing without stolen preserved observation", managed)
	}
	if preserved := reconciliationDeltaByID(got, "kitty-env"); preserved == nil || preserved.State != StateExternalPreserved {
		t.Fatalf("exact preserved same-slot delta = %+v, want external_preserved", preserved)
	}
	if len(got.Deltas) != 2 {
		t.Fatalf("Reconcile() deltas = %+v, want managed missing + external preserved", got.Deltas)
	}
}

func TestReconcileDesiredSameSlotFallbackStillReportsUnownedMismatchAsDrifted(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Bindings: []Binding{
			{
				ID:         "recall",
				Harness:    "claude_code",
				Event:      "SessionStart",
				HandlerRef: "$HOME/.claude/hooks/RecallContext.hook.ts",
			},
		},
	}
	observed := InstallationSnapshot{Bindings: []ObservedBinding{
		{
			Harness:    "claude_code",
			Event:      "SessionStart",
			HandlerRef: "$HOME/.claude/hooks/UnknownSameSlot.hook.ts",
			Active:     true,
		},
	}}

	got, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(got.Deltas) != 1 || got.Deltas[0].State != StateDrifted || got.Deltas[0].ObservedHandlerRef != "$HOME/.claude/hooks/UnknownSameSlot.hook.ts" {
		t.Fatalf("Reconcile() deltas = %+v, want legacy single-slot fallback drift", got.Deltas)
	}
}

func TestReconcileDoesNotPreserveExternalHookWithAttestationDrift(t *testing.T) {
	tests := []struct {
		name     string
		observed ObservedBinding
	}{
		{
			name: "invocation",
			observed: ObservedBinding{
				Harness:            "claude_code",
				Event:              "SessionStart",
				HandlerRef:         "$HOME/.claude/hooks/KittyEnvPersist.hook.ts",
				InvocationRevision: "sha256:changed-invocation",
				HandlerRevision:    "sha256:kitty-content",
				Active:             true,
			},
		},
		{
			name: "content",
			observed: ObservedBinding{
				Harness:            "claude_code",
				Event:              "SessionStart",
				HandlerRef:         "$HOME/.claude/hooks/KittyEnvPersist.hook.ts",
				InvocationRevision: "sha256:kitty-invocation",
				HandlerRevision:    "sha256:changed-content",
				Active:             true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := HookPlan{
				SchemaVersion: HookPlanSchemaVersion,
				ID:            "hook-plan",
				PreservedBindings: []Binding{
					{
						ID:                 "kitty-env",
						Harness:            "claude_code",
						Event:              "SessionStart",
						HandlerRef:         "$HOME/.claude/hooks/KittyEnvPersist.hook.ts",
						InvocationRevision: "sha256:kitty-invocation",
						HandlerRevision:    "sha256:kitty-content",
					},
				},
			}
			got, err := Reconcile(plan, InstallationSnapshot{Bindings: []ObservedBinding{test.observed}}, harness.DefaultRegistry())
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if len(got.Deltas) != 1 || got.Deltas[0].State != StateExtraUnowned {
				t.Fatalf("Reconcile() deltas = %+v, want attestation drift to remain extra_unowned", got.Deltas)
			}
			if got.Deltas[0].ObservedHandlerRevision != test.observed.HandlerRevision {
				t.Fatalf("extra_unowned delta lost observed handler attestation: %+v", got.Deltas[0])
			}
		})
	}
}

func TestReconcileKeepsMissingDriftActivationAndCapabilityFailuresDistinct(t *testing.T) {
	registry := harness.DefaultRegistry()
	registry.Harnesses["observe_only"] = harness.Harness{
		Name: "observe_only",
		Capabilities: harness.Capabilities{
			PreToolUseBlock: false,
		},
	}
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Revision:      "sha256:plan",
		Bindings: []Binding{
			{
				ID:         "missing",
				Harness:    "claude_code",
				Event:      "SessionEnd",
				HandlerRef: "handler://receipt",
				Activation: ActivationManaged,
			},
			{
				ID:         "activate",
				Harness:    "claude_code",
				Event:      "PostToolUse",
				HandlerRef: "handler://artifact-capture",
				Activation: ActivationOperator,
			},
			{
				ID:         "drift",
				Harness:    "claude_code",
				Event:      "UserPromptSubmit",
				HandlerRef: "handler://desired-context",
			},
			{
				ID:         "unsupported",
				Harness:    "observe_only",
				Event:      "PreToolUse",
				HandlerRef: "handler://policy-gate",
				Blocking:   true,
			},
		},
	}
	observed := InstallationSnapshot{
		SchemaVersion: InstallationSnapshotSchemaVersion,
		Ref:           "harness-installation://fixture",
		Revision:      "sha256:observed",
		Bindings: []ObservedBinding{
			{
				Harness:    "claude_code",
				Event:      "UserPromptSubmit",
				HandlerRef: "handler://old-context",
				Active:     true,
			},
			{
				Harness:    "observe_only",
				Event:      "PreToolUse",
				HandlerRef: "handler://policy-gate",
				Active:     true,
			},
		},
	}

	got, err := Reconcile(plan, observed, registry)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wants := map[string]string{
		"missing":     StateMissing,
		"activate":    StateActivationRequired,
		"drift":       StateDrifted,
		"unsupported": StateUnsupported,
	}
	for id, want := range wants {
		delta := reconciliationDeltaByID(got, id)
		if delta == nil || delta.State != want {
			t.Errorf("delta %q = %+v, want state %q", id, delta, want)
		}
	}
	if len(got.Deltas) != len(wants) {
		t.Fatalf("Reconcile() deltas = %+v, want one result per desired binding", got.Deltas)
	}
}

func reconciliationDeltaByID(reconciliation Reconciliation, id string) *Delta {
	for i := range reconciliation.Deltas {
		if reconciliation.Deltas[i].BindingID == id {
			return &reconciliation.Deltas[i]
		}
	}
	return nil
}

func TestReconcileRejectsDuplicateDesiredSlots(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Bindings: []Binding{
			{ID: "one", Harness: "claude_code", Event: "SessionStart", HandlerRef: "handler://one"},
			{ID: "two", Harness: "claude_code", Event: "SessionStart", HandlerRef: "handler://two"},
		},
	}

	_, err := Reconcile(plan, InstallationSnapshot{}, harness.DefaultRegistry())
	if err == nil || !strings.Contains(err.Error(), "duplicate desired hook slot") {
		t.Fatalf("Reconcile() error = %v, want duplicate desired hook slot", err)
	}
}

func TestReconcileRejectsAmbiguousLegacyObservation(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Bindings: []Binding{
			{ID: "wanted", Harness: "claude_code", Event: "SessionStart", HandlerRef: "handler://wanted"},
		},
	}
	observed := InstallationSnapshot{
		SchemaVersion: InstallationSnapshotSchemaVersion,
		Bindings: []ObservedBinding{
			{Harness: "claude_code", Event: "SessionStart", HandlerRef: "handler://unknown-one", Active: true},
			{Harness: "claude_code", Event: "SessionStart", HandlerRef: "handler://unknown-two", Active: true},
		},
	}

	_, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err == nil || !strings.Contains(err.Error(), "ambiguous observed registrations") {
		t.Fatalf("Reconcile() error = %v, want ambiguous observed registrations", err)
	}
}

func TestReconcileCanonicalizesEquivalentInputOrder(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Revision:      "sha256:plan",
		Bindings: []Binding{
			{ID: "prompt", Harness: "claude_code", Event: "UserPromptSubmit", HandlerRef: "handler://prompt"},
			{ID: "start", Harness: "claude_code", Event: "SessionStart", HandlerRef: "handler://start"},
		},
	}
	observed := InstallationSnapshot{
		SchemaVersion: InstallationSnapshotSchemaVersion,
		Ref:           "harness-installation://fixture",
		Revision:      "sha256:observed",
		Bindings: []ObservedBinding{
			{Harness: "claude_code", Event: "SessionStart", HandlerRef: "handler://start", Active: true},
			{Harness: "claude_code", Event: "SessionEnd", HandlerRef: "handler://extra", Active: true},
			{Harness: "claude_code", Event: "UserPromptSubmit", HandlerRef: "handler://prompt", Active: true},
		},
	}

	first, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	reverseBindings(plan.Bindings)
	reverseObservedBindings(observed.Bindings)
	second, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first reconciliation: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second reconciliation: %v", err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("equivalent inputs produced different reconciliation JSON:\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
}

func TestReconcileReportsDriftWhenExpectedHandlerContentCannotBeObserved(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Bindings: []Binding{
			{
				ID:              "start",
				Harness:         "claude_code",
				Event:           "SessionStart",
				HandlerRef:      "handler://start",
				HandlerRevision: "sha256:handler-content",
			},
		},
	}
	observed := InstallationSnapshot{
		SchemaVersion: InstallationSnapshotSchemaVersion,
		Bindings: []ObservedBinding{
			{
				Harness:    "claude_code",
				Event:      "SessionStart",
				HandlerRef: "handler://start",
				Active:     true,
			},
		},
	}

	got, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(got.Deltas) != 1 || got.Deltas[0].State != StateDrifted {
		t.Fatalf("Reconcile() deltas = %+v, want drifted when expected handler content is unverified", got.Deltas)
	}
	encoded, err := json.Marshal(got.Deltas[0])
	if err != nil {
		t.Fatalf("marshal delta: %v", err)
	}
	if !strings.Contains(string(encoded), `"desired_handler_revision":"sha256:handler-content"`) {
		t.Fatalf("reconciliation delta omitted desired handler attestation: %s", encoded)
	}
}

func TestReconcileDetectsInvocationArgumentDrift(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Bindings: []Binding{
			{
				ID:                 "prompt",
				Harness:            "claude_code",
				Event:              "UserPromptSubmit",
				HandlerRef:         "$HOME/.claude/hooks/IntentRecall.hook.ts",
				InvocationRevision: "sha256:desired-argv",
			},
		},
	}
	observed := InstallationSnapshot{
		SchemaVersion: InstallationSnapshotSchemaVersion,
		Bindings: []ObservedBinding{
			{
				Harness:            "claude_code",
				Event:              "UserPromptSubmit",
				HandlerRef:         "$HOME/.claude/hooks/IntentRecall.hook.ts",
				InvocationRevision: "sha256:changed-argv",
				Active:             true,
			},
		},
	}

	got, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(got.Deltas) != 1 || got.Deltas[0].State != StateDrifted {
		t.Fatalf("Reconcile() deltas = %+v, want invocation drift", got.Deltas)
	}
}

func TestReconcileReportsReadyOnlyForVerifiedHandlerContent(t *testing.T) {
	plan := HookPlan{
		SchemaVersion: HookPlanSchemaVersion,
		ID:            "hook-plan",
		Bindings: []Binding{
			{
				ID:              "start",
				Harness:         "claude_code",
				Event:           "SessionStart",
				HandlerRef:      "handler://start",
				HandlerRevision: "sha256:verified-content",
			},
		},
	}
	observed := InstallationSnapshot{
		SchemaVersion: InstallationSnapshotSchemaVersion,
		Bindings: []ObservedBinding{
			{
				Harness:         "claude_code",
				Event:           "SessionStart",
				HandlerRef:      "handler://start",
				HandlerRevision: "sha256:verified-content",
				Active:          true,
			},
		},
	}

	got, err := Reconcile(plan, observed, harness.DefaultRegistry())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(got.Deltas) != 1 || got.Deltas[0].State != StateReady {
		t.Fatalf("Reconcile() deltas = %+v, want content-verified ready", got.Deltas)
	}
	encoded, err := json.Marshal(got.Deltas[0])
	if err != nil {
		t.Fatalf("marshal delta: %v", err)
	}
	for _, want := range []string{
		`"desired_handler_revision":"sha256:verified-content"`,
		`"observed_handler_revision":"sha256:verified-content"`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("ready delta omitted content attestation %s: %s", want, encoded)
		}
	}
}

func reverseBindings(bindings []Binding) {
	for left, right := 0, len(bindings)-1; left < right; left, right = left+1, right-1 {
		bindings[left], bindings[right] = bindings[right], bindings[left]
	}
}

func reverseObservedBindings(bindings []ObservedBinding) {
	for left, right := 0, len(bindings)-1; left < right; left, right = left+1, right-1 {
		bindings[left], bindings[right] = bindings[right], bindings[left]
	}
}
