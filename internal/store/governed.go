package store

import (
	"fmt"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/persist"
	"compound/internal/policy"
)

// storeGovernedCapability scopes the ActionIntent every `compound store` write
// verb mints; an operator's standing grant covers exactly this capability +
// external_side_effect class + the target path, mirroring
// eventstore_rebaseline.go's eventStoreRebaselineCapability.
const storeGovernedCapability = "store.postgres_export.write"

// storeGovernedIntent builds the external_side_effect / easy / local
// ActionIntent for one governed store write (migrate --apply, sync --confirm,
// embed --confirm), mirroring rebaselineIntent in eventstore_rebaseline.go.
// The validated config derives the opaque `postgres-profile:<label>` target.
// Callers cannot supply an arbitrary path: raw and masked DSNs both expose
// connection topology and can mishandle keyword/value forms, so neither may
// land in a persisted envelope, report, receipt, or transition.
func storeGovernedIntent(workID, action string, cfg persist.Config, preview string, now time.Time) (envelope.ActionIntent, error) {
	if err := cfg.ValidatePostgresExperiment(); err != nil {
		return envelope.ActionIntent{}, fmt.Errorf("validate experimental Postgres target: %w", err)
	}
	targetPath := cfg.PostgresTargetRef()
	nowStr := now.UTC().Format(time.RFC3339Nano)
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actIDFor("act_", workID, storeGovernedCapability, targetPath, nowStr),
			CreatedAt:     nowStr,
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{"target:" + targetPath},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "operator",
		Capability:    storeGovernedCapability,
		Action:        action,
		Target:        envelope.ActionTarget{Kind: "store_postgres_export", ID: "postgres-ledger-copy", Path: targetPath},
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       preview,
		RollbackPlan:  "delete the exported rows from the Postgres copy (the file ledger under COMPOUND_HOME is untouched)",
	}, nil
}

func storeGovernedAuthorityContext(targetPath string) policy.EvaluationContext {
	return policy.EvaluationContext{Authority: &policy.AuthorityContext{
		TargetScope:               "private",
		VerifiedRecoverability:    policy.RecoverabilityVerified,
		OperatorControlContinuity: policy.OperatorControlContinuous,
		ExpectedReceipt:           "validated outward_delta Receipt",
		ExpectedProof:             []string{"Receipt + Transition recorded in the home event store"},
		EvidenceRefs:              []string{"falsifier:cmd/compound/store_governed_test.go#gated-write-refuses-without-grant", "target:" + targetPath},
	}}
}

// gateStoreGovernedWrite evaluates policy for one store write and refuses
// unless an allowing PolicyDecision holds — the same by-construction refusal
// `compound act` and `event-store rebaseline` use.
func gateStoreGovernedWrite(homePath string, ai envelope.ActionIntent, now time.Time) (policy.AuthorityEvaluation, error) {
	cfg, policySource, err := policy.LoadConfig(homePath)
	if err != nil {
		return policy.AuthorityEvaluation{}, fmt.Errorf("load policy: %w", err)
	}
	evaluation := policy.EvaluateAuthority(ai, cfg, storeGovernedAuthorityContext(ai.Target.Path), policySource, now)
	if !evaluation.Authority.Current || !evaluation.Authority.Allowed {
		return evaluation, fmt.Errorf("refused: %s", evaluation.Authority.Wording)
	}
	if err := act.Gate(ai, evaluation.Decision); err != nil {
		return evaluation, fmt.Errorf("refused: %w", err)
	}
	return evaluation, nil
}

// recordStoreGovernedProof mints and durably persists the Receipt+Transition
// pair for a completed store write, so the export is reconstructable from the
// home event log alone. This is NOT a transactional atomic guarantee: the
// event store backend currently writes each envelope separately. The function
// therefore uses best-effort compensation: if the Transition write fails, the
// already-written Receipt is removed so we never intentionally leave an orphan
// proof of an incomplete act. If the Remove itself fails, the orphan receipt ID
// is returned and the error names the orphan so the operator can repair by
// deleting that receipt from the home event store.
func recordStoreGovernedProof(store eventstoreRecorder, ai envelope.ActionIntent, summary string, now time.Time) (receiptID, transitionID string, err error) {
	receipt, transition := storeGovernedReceiptAndTransition(ai, summary, now)
	if err := recordEventstoreEnvelope(store, &receipt); err != nil {
		return "", "", fmt.Errorf("record receipt: %w", err)
	}
	if err := recordEventstoreEnvelope(store, &transition); err != nil {
		compErr := store.Remove(receipt.ID)
		if compErr != nil {
			return receipt.ID, "", fmt.Errorf("record transition failed and orphan receipt %s compensation failed; manual repair required: remove the orphan receipt from the home event store: transition: %w; compensation: %v", receipt.ID, err, compErr)
		}
		return "", "", fmt.Errorf("record transition: %w", err)
	}
	return receipt.ID, transition.ID, nil
}

// storeGovernedReceiptAndTransition mirrors rebaselineReceiptAndTransition's
// shape (an outward_delta Receipt + Transition), minus the artifact CAS refs —
// the export's pre-image is the file ledger itself.
func storeGovernedReceiptAndTransition(ai envelope.ActionIntent, summary string, now time.Time) (envelope.Receipt, envelope.Transition) {
	nowStr := now.UTC().Format(time.RFC3339Nano)
	evidenceRefs := []string{"target:" + ai.Target.Path}
	verificationRefs := []string{"falsifier:cmd/compound/store_governed_test.go#gated-write-refuses-without-grant"}
	newHeader := func(schemaVersion, id string) envelope.Header {
		return envelope.Header{
			SchemaVersion: schemaVersion,
			ID:            id,
			CreatedAt:     nowStr,
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: ai.IDs.WorkID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  append([]string{}, evidenceRefs...),
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		}
	}
	receipt := envelope.Receipt{
		Header:   newHeader(envelope.SVReceipt, actIDFor("rec_", ai.ID, summary)),
		Outcome:  "completed",
		MoveType: "outward_delta",
		StateDelta: envelope.StateDelta{
			Changed:      true,
			Summary:      summary,
			FilesChanged: []string{ai.Target.Path},
			Artifacts:    []string{},
			Verification: verificationRefs,
		},
		Cost:      envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:    envelope.ReceiptSkills{Used: []string{ai.Capability}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true},
		Memory:    envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:      envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs: []string{},
	}
	transition := envelope.Transition{
		Header:     newHeader(envelope.SVTransition, actIDFor("tr_", ai.ID, summary)),
		BeforeRefs: []string{"ledger:file"},
		ActionRefs: []string{ai.ID},
		AfterRefs:  []string{"store:postgres"},
		StateDelta: summary,
		Projections: envelope.Projections{
			Memory: []string{}, Skill: []string{ai.Capability}, Product: []string{}, Cost: []string{},
			Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{},
		},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
	}
	return receipt, transition
}
