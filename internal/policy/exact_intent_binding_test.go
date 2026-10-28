package policy

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

// generatedWorkIntent builds the kind of ActionIntent a work-owned action seam
// generates: the typed binding fields ArgumentsHash/Destination/
// SchemaRevision/InputRefs stay empty and the intent identity lives in actor,
// work, target, and security semantics instead.
func generatedWorkIntent(id, workID, actor, targetPath, preview string) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            id,
			CreatedAt:     fixedNow.UTC().Format(time.RFC3339Nano),
			Producer:      envelope.Producer{Kind: "work_seam", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier2",
		},
		Actor:         actor,
		Capability:    "work.act",
		Action:        "act",
		Target:        envelope.ActionTarget{Kind: "work", ID: workID, Path: targetPath},
		MutationClass: "external_side_effect",
		Reversibility: "manual",
		Preview:       preview,
	}
}

func holdDecisionForTest(intent envelope.ActionIntent) envelope.PolicyDecision {
	return envelope.PolicyDecision{
		Header: envelope.Header{
			SchemaVersion: "compound.policy_decision.v0.1",
			ID:            "pol_fixture_" + strings.TrimPrefix(intent.ID, "act_"),
			CreatedAt:     fixedNow.UTC().Format(time.RFC3339Nano),
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier2",
		},
		ActionIntentID: intent.ID,
		Outcome:        "hold",
		Reasons:        []string{"fixture hold decision for exact approval matching"},
	}
}

// TestExactApprovalGrantNeverCoversDistinctGeneratedWorkIntents is the core
// falsifier for full-intent binding: two generated work intents share every
// typed binding field (capability, action, empty hash/destination/schema/
// input refs) yet differ in actor, work, target, and preview. One operator
// approval must never authorize the other intent.
func TestExactApprovalGrantNeverCoversDistinctGeneratedWorkIntents(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	store := NewExactActionApprovalStore(homePath)
	approved := generatedWorkIntent(
		"act_full_bind_approved",
		"work_generated_fixture_a",
		"codex:fixture",
		"srv/fixture-a.example.invalid",
		"fixture preview approved by operator",
	)
	if err := approved.Validate(); err != nil {
		t.Fatalf("approved intent must be valid: %v", err)
	}
	approvePlanBackedIntentForTest(
		t, homePath, store, approved, cfg, 5*time.Minute,
		fixedNow, fixedNow.Add(time.Second), "operator:fixture",
	)

	impostor := generatedWorkIntent(
		"act_full_bind_impostor",
		"work_generated_fixture_b",
		"claude:fixture",
		"srv/fixture-b.example.invalid",
		"impostor preview was never approved",
	)
	if err := impostor.Validate(); err != nil {
		t.Fatalf("impostor intent must be valid: %v", err)
	}
	if ActionBindingDigest(ActionBindingForIntent(approved, cfg)) ==
		ActionBindingDigest(ActionBindingForIntent(impostor, cfg)) {
		t.Fatalf("distinct generated work intents must not share one binding digest")
	}
	decision, matched, err := store.ApplyMatching(impostor, cfg, holdDecisionForTest(impostor), fixedNow.Add(2*time.Second))
	if err != nil {
		t.Fatalf("impostor apply matching: %v", err)
	}
	if matched {
		t.Fatalf("impostor intent matched an approval granted for a different action")
	}
	if Allowed(decision) {
		t.Fatalf("impostor intent was authorized by a foreign grant: %+v", decision)
	}

	// The grant still serves the intent it was minted for.
	retry := approved
	retry.ID = "act_full_bind_retry"
	retry.CreatedAt = fixedNow.Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	persistActionIntentForTest(t, homePath, retry)
	allowed, matched, err := store.ApplyMatching(retry, cfg, holdDecisionForTest(retry), fixedNow.Add(2*time.Second))
	if err != nil || !matched || !Allowed(allowed) {
		t.Fatalf("same-body retry was not authorized: matched=%v err=%v decision=%+v", matched, err, allowed)
	}
}

// TestExactApprovalFailsClosedForLegacyInsufficientBinding: a grant whose
// digest was computed from a pre-full-intent binding (no FullIntentDigest)
// must stop matching current intents rather than be widened to cover them.
func TestExactApprovalFailsClosedForLegacyInsufficientBinding(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	monitored := MonitorToolAction(monitorRequest("gmail.send", map[string]any{
		"recipient": "person@example.invalid",
		"subject":   "fixture",
		"body":      "hello",
	}), cfg, fixedNow)
	legacy := monitored.Binding
	legacy.FullIntentDigest = ""
	approval, err := NewExactActionApproval(
		"approval_grant_legacy_insufficient",
		"operator:legacy_fixture",
		legacy,
		fixedNow.Add(5*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	store := NewExactActionApprovalStore(homePath)
	if _, err := store.Save(approval); err != nil {
		t.Fatal(err)
	}
	decision, matched, err := store.ApplyMatching(
		monitored.Intent, cfg, monitored.Decision, fixedNow.Add(time.Second),
	)
	if err != nil || matched || Allowed(decision) {
		t.Fatalf("legacy insufficient binding was widened: matched=%v err=%v decision=%+v", matched, err, decision)
	}
	status, statusErr := store.Check(approval.ID, fixedNow.Add(2*time.Second))
	if statusErr != nil || status.Status != ExactApprovalStatusReady {
		t.Fatalf("legacy grant status=%+v err=%v, want untouched and ready", status, statusErr)
	}
}

// TestCanonicalFullIntentDigestIsStableForRetryIdentityAndReordering: the
// digest ignores only the retry-volatile id/timestamp and normalizes set-like
// slices, so an approved retry converges on the same grant identity.
func TestCanonicalFullIntentDigestIsStableForRetryIdentityAndReordering(t *testing.T) {
	base := MonitorToolAction(monitorRequest("gmail.send", map[string]any{
		"recipient": "person@example.invalid",
		"subject":   "fixture",
		"body":      "hello",
	}), DefaultConfig(), fixedNow).Intent

	retry := base
	retry.ID = "act_retry_new_identity"
	retry.CreatedAt = fixedNow.Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if CanonicalFullIntentDigest(base) != CanonicalFullIntentDigest(retry) {
		t.Fatalf("retry identity change must not change the full intent digest")
	}

	reordered := base
	if len(reordered.EvidenceRefs) >= 2 {
		reordered.EvidenceRefs = []string{base.EvidenceRefs[1], base.EvidenceRefs[0]}
	}
	if len(reordered.SecurityReasons) >= 2 {
		reordered.SecurityReasons = []string{base.SecurityReasons[1], base.SecurityReasons[0]}
	}
	if CanonicalFullIntentDigest(base) != CanonicalFullIntentDigest(reordered) {
		t.Fatalf("set-like slice reordering must not change the full intent digest")
	}
}

// TestCanonicalFullIntentDigestBindsEveryMutableIntentField: changing any
// security-relevant intent field must change the digest, so a grant minted for
// one body can never be replayed against a mutated one.
func TestCanonicalFullIntentDigestBindsEveryMutableIntentField(t *testing.T) {
	base := generatedWorkIntent(
		"act_digest_mutation_base",
		"work_digest_fixture",
		"codex:fixture",
		"srv/fixture.example.invalid",
		"fixture preview",
	)
	mutations := []struct {
		name   string
		mutate func(*envelope.ActionIntent)
	}{
		{"actor", func(i *envelope.ActionIntent) { i.Actor = "claude:fixture" }},
		{"work id", func(i *envelope.ActionIntent) { i.IDs.WorkID = "work_other_fixture" }},
		{"session id", func(i *envelope.ActionIntent) { i.IDs.SessionID = "session_other_fixture" }},
		{"scope project", func(i *envelope.ActionIntent) { i.Scope.ProjectID = "project_other_fixture" }},
		{"capability", func(i *envelope.ActionIntent) { i.Capability = "work.act.other" }},
		{"action", func(i *envelope.ActionIntent) { i.Action = "act-other" }},
		{"target kind", func(i *envelope.ActionIntent) { i.Target.Kind = "path" }},
		{"target id", func(i *envelope.ActionIntent) { i.Target.ID = "work_other_fixture" }},
		{"target path", func(i *envelope.ActionIntent) { i.Target.Path = "srv/other.example.invalid" }},
		{"destination", func(i *envelope.ActionIntent) { i.Destination = "person@example.invalid" }},
		{"arguments hash", func(i *envelope.ActionIntent) { i.ArgumentsHash = "sha256:" + strings.Repeat("0", 64) }},
		{"schema revision", func(i *envelope.ActionIntent) { i.SchemaRevision = "schema:other-v1" }},
		{"security floor outcome", func(i *envelope.ActionIntent) {
			i.SecurityFloorOutcome = "hold"
			i.SecurityReasons = []string{"fixture floor"}
		}},
		{"security reasons", func(i *envelope.ActionIntent) { i.SecurityReasons = []string{"fixture reason"} }},
		{"wip capture", func(i *envelope.ActionIntent) { i.CapturesWorkInProgress = true }},
		{"mutation class", func(i *envelope.ActionIntent) { i.MutationClass = "destructive" }},
		{"reversibility", func(i *envelope.ActionIntent) { i.Reversibility = "irreversible" }},
		{"blast radius", func(i *envelope.ActionIntent) { i.BlastRadius = "production" }},
		{"estimated cost", func(i *envelope.ActionIntent) { usd := 2.5; i.EstimatedCost.USD = &usd }},
		{"preview", func(i *envelope.ActionIntent) { i.Preview = "mutated preview" }},
		{"rollback plan", func(i *envelope.ActionIntent) { i.RollbackPlan = "mutated rollback" }},
		{"risk", func(i *envelope.ActionIntent) { i.Risk = "tier3" }},
		{"privacy", func(i *envelope.ActionIntent) { i.Privacy = "sensitive" }},
		{"authority source class", func(i *envelope.ActionIntent) { i.Authority.SourceClass = "generated" }},
		{"authority status", func(i *envelope.ActionIntent) { i.Authority.Status = "stale" }},
		{"uncertainty", func(i *envelope.ActionIntent) { i.Uncertainty = envelope.Uncertainty{Level: "suspected"} }},
		{"producer", func(i *envelope.ActionIntent) { i.Producer = envelope.Producer{Kind: "other_seam", Name: "other"} }},
		{"host binding", func(i *envelope.ActionIntent) { i.Host = &envelope.HostBindings{HostID: "host_fixture"} }},
	}
	baseDigest := CanonicalFullIntentDigest(base)
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			mutated := base
			tt.mutate(&mutated)
			if CanonicalFullIntentDigest(mutated) == baseDigest {
				t.Fatalf("mutating %s did not change the full intent digest", tt.name)
			}
		})
	}
}

func TestCanonicalFullIntentDigestBindsInputTrustAndConfidentiality(t *testing.T) {
	base := generatedWorkIntent("act_input_binding", "work_input_fixture", "fixture", "srv/fixture.example.invalid", "fixture")
	base.InputSources = []envelope.SourceBinding{{SourceID: "source_fixture", ContentHash: "sha256:" + strings.Repeat("a", 64), TrustClass: "trusted", Confidentiality: "private"}}
	for _, field := range []string{"trust", "confidentiality"} {
		other := base
		other.InputSources = append([]envelope.SourceBinding{}, base.InputSources...)
		if field == "trust" {
			other.InputSources[0].TrustClass = "untrusted"
		} else {
			other.InputSources[0].Confidentiality = "public"
		}
		if CanonicalFullIntentDigest(other) == CanonicalFullIntentDigest(base) {
			t.Fatalf("%s not bound", field)
		}
	}
}

func TestTypedExactApprovalRejectsInsufficientOrForgedIntentBinding(t *testing.T) {
	for _, scenario := range []string{"legacy", "forged"} {
		t.Run(scenario, func(t *testing.T) {
			m := MonitorToolAction(monitorRequest("browser.submit", map[string]any{"target": "fixture", "form": "publish"}), DefaultConfig(), fixedNow)
			if scenario == "legacy" {
				m.Binding.FullIntentDigest = ""
			}
			a, err := NewExactActionApproval("approval_typed_fixture", "operator:fixture", m.Binding, fixedNow.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "forged" {
				m.Intent.Target.ID = "other@example.invalid"
				m.Intent.Actor = "other"
			}
			consumer := NewApprovalConsumptionStore(t.TempDir())
			pd, err := ApplyExactActionApproval(m, a, consumer, fixedNow.Add(time.Second))
			if err == nil || Allowed(pd) {
				t.Fatalf("invalid binding accepted: %+v %v", pd, err)
			}
			if err := consumer.Consume(a.ID, fixedNow.Add(2*time.Second)); err != nil {
				t.Fatalf("invalid binding consumed authority: %v", err)
			}
		})
	}
}
