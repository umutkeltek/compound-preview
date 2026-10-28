package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

type harnessPermissionApplyReport struct {
	SchemaVersion    string                  `json:"schema_version"`
	Mode             string                  `json:"mode"`
	Harness          string                  `json:"harness"`
	TargetPath       string                  `json:"target_path"`
	Changed          bool                    `json:"changed"`
	TargetChanged    bool                    `json:"target_changed"`
	ManifestChanged  bool                    `json:"manifest_changed"`
	WorkID           string                  `json:"work_id"`
	ActionIntentID   string                  `json:"action_intent_id"`
	PolicyDecisionID string                  `json:"policy_decision_id"`
	PolicyOutcome    string                  `json:"policy_outcome"`
	ReceiptID        string                  `json:"receipt_id"`
	TransitionID     string                  `json:"transition_id"`
	PreImageRef      string                  `json:"pre_image_ref"`
	PostImageRef     string                  `json:"post_image_ref"`
	Artifact         harness.HarnessArtifact `json:"artifact"`
	Details          []string                `json:"details,omitempty"`
}

var applyHarnessPermissions = harness.ApplyPermissions
var readPermissionTargetForObservation = os.ReadFile
var recordHarnessPermissionOutcome = func(store *work.Store, workID string, input work.ReceiptedActionInput, now time.Time) (work.ReceiptedActionResult, error) {
	return store.RecordReceiptedAction(workID, input, now)
}
var recordReservedHarnessPermissionOutcome = func(store *work.Store, workID string, input work.ReceiptedActionInput, now time.Time) (work.ReceiptedActionResult, error) {
	return store.RecordReservedReceiptedAction(workID, input, now)
}

func runHarnessGenerateApply(args []string, stdout, stderr io.Writer, now time.Time) int {
	harnessName := strings.TrimSpace(flagValue(args, "--harness"))
	workID := strings.TrimSpace(flagValue(args, "--work"))
	actor := strings.TrimSpace(flagValue(args, "--actor"))
	if harnessName == "" || workID == "" || actor == "" {
		fmt.Fprintln(stderr, "usage: compound harness generate --harness <name> --apply --work <work_id> --actor <operator> [--json]")
		return 2
	}
	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err, "(set COMPOUND_HOME)")
		return 1
	}
	store, err := work.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	w, err := store.Get(workID)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if w.State != "running" {
		fmt.Fprintf(stderr, "refused: work %s is %s; permission apply requires running work\n", workID, w.State)
		return 1
	}
	if !policy.HasOperatorProvenance(actor) {
		fmt.Fprintf(stderr, "refused: --actor %q lacks operator provenance\n", actor)
		return 1
	}
	leases, err := store.ListLeases()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	leaseHolder := ""
	for _, lease := range leases {
		if lease.WorkID == workID {
			leaseHolder = lease.By
			break
		}
	}
	if leaseHolder == "" || leaseHolder != actor {
		fmt.Fprintf(stderr, "refused: --actor %q does not match work lease holder %q\n", actor, leaseHolder)
		return 1
	}
	render, err := harness.RenderPermissions(homePath, harnessName)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	permissionRecoveryPending := false
	if _, permissionRecoveryPending, err = harness.ProbePermissionApplyRecovery(homePath, harnessName); err != nil {
		fmt.Fprintln(stderr, "error: inspect pending permission apply transaction:", err)
		return 1
	}
	preImageRef := "sha256:" + permissionApplyDigest(render.Preimage)
	approvedPostImageRef := render.Artifact.ContentSHA256
	createdAt := now.UTC().Format(time.RFC3339Nano)
	action := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actIDFor("act_", workID, "harness.permissions.apply", harnessName, render.Artifact.SourceRevision, createdAt),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Owner: "compound", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{render.Artifact.SourceAuthorityRef, render.Artifact.SourceRevision, preImageRef, approvedPostImageRef},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         actor,
		Capability:    "harness.permissions.apply",
		Action:        "reconcile_harness_permissions",
		Target:        envelope.ActionTarget{Kind: "harness_settings", ID: harnessName, Path: render.TargetPath},
		MutationClass: "security",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("reconcile Compound-managed permissions for %s at %s", harnessName, render.TargetPath),
		RollbackPlan:  "restore the settings pre-image; ApplyPermissions automatically restores it if artifact provenance cannot be persisted",
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	decision := policy.Evaluate(action, cfg, now)
	if !policy.Allowed(decision) && !permissionRecoveryPending {
		fmt.Fprintf(stderr, "refused: policy %s (%s)\n", decision.Outcome, strings.Join(decision.Reasons, "; "))
		return 1
	}
	authorize := permissionApplyAuthorization(store, homePath, workID)
	reserve := func(outcome harness.PermissionApplyOutcome, authorize harness.PermissionApplyAuthorization) error {
		input, authorizedAt, err := harnessPermissionReceiptedActionInput(outcome)
		if err != nil {
			return err
		}
		return store.ReserveReceiptedAction(outcome.Action.IDs.WorkID, input, authorizedAt, func() error {
			return authorize(outcome.Action, outcome.Decision, authorizedAt)
		})
	}
	abort := func(outcome harness.PermissionApplyOutcome) error {
		return store.AbortReceiptedActionReservation(outcome.Action.IDs.WorkID, outcome.Action.ID)
	}
	var finalized harness.PermissionApplyOutcome
	var receipted work.ReceiptedActionResult
	finalize := func(outcome harness.PermissionApplyOutcome) error {
		input, authorizedAt, err := harnessPermissionReceiptedActionInput(outcome)
		if err != nil {
			return err
		}
		result, err := recordReservedHarnessPermissionOutcome(store, outcome.Action.IDs.WorkID, input, authorizedAt)
		if err != nil {
			return err
		}
		finalized = outcome
		receipted = result
		return nil
	}
	_, err = applyHarnessPermissions(homePath, render, action, decision, authorize, reserve, abort, finalize)
	if err != nil {
		if harness.IsPermissionApplyFinalizationPending(err) {
			fmt.Fprintln(stderr, "error: permission mutation is pending finalization and will recover its exact outcome:", err)
			return 1
		}
		postImageRef, observationErr := observedPermissionTargetRef(render.TargetPath)
		changed := true
		mechanism := "governed permission activation failed after policy allowance"
		if observationErr == nil {
			changed = postImageRef != preImageRef
		} else {
			mechanism += "; post-mutation observation failed: " + observationErr.Error()
		}
		filesChanged := []string{}
		if changed {
			filesChanged = []string{render.TargetPath}
		}
		outcome, recordErr := recordHarnessPermissionOutcome(store, workID, work.ReceiptedActionInput{
			Action: action, PreEvaluatedDecision: &decision, ExpectedWorkState: "running",
			Outcome: "failed", Changed: changed, Summary: "permission activation failed: " + err.Error(), MoveType: "control_plane_repair",
			ProofRefs: []string{render.Artifact.SourceAuthorityRef, render.Artifact.SourceRevision}, BeforeRefs: []string{preImageRef}, AfterRefs: []string{postImageRef},
			FilesChanged: filesChanged, FailureCause: &envelope.FailureCause{SurfaceOutcome: err.Error(), Mechanism: mechanism},
		}, now)
		if recordErr != nil {
			fmt.Fprintf(stderr, "error: permission apply failed: %v; record failed outcome: %v\n", err, recordErr)
			return 1
		}
		fmt.Fprintf(stderr, "error: permission apply failed and was receipted as %s/%s: %v\n", outcome.Receipt.ID, outcome.Transition.ID, err)
		return 1
	}
	if finalized.Action.ID == "" || receipted.Receipt.ID == "" {
		fmt.Fprintln(stderr, "error: permission activation returned without a finalized outcome")
		return 1
	}
	preImageRef = "sha256:" + permissionApplyDigest(finalized.Render.Preimage)
	postImageRef := finalized.Render.Artifact.ContentSHA256
	report := harnessPermissionApplyReport{
		SchemaVersion:    "compound.harness_permission_apply.v0.1",
		Mode:             "apply",
		Harness:          finalized.Render.Artifact.Harness,
		TargetPath:       finalized.Render.TargetPath,
		Changed:          finalized.Changed,
		TargetChanged:    finalized.TargetChanged,
		ManifestChanged:  finalized.ManifestChanged,
		WorkID:           finalized.Action.IDs.WorkID,
		ActionIntentID:   finalized.Action.ID,
		PolicyDecisionID: finalized.Decision.ID,
		PolicyOutcome:    finalized.Decision.Outcome,
		ReceiptID:        receipted.Receipt.ID,
		TransitionID:     receipted.Transition.ID,
		PreImageRef:      preImageRef,
		PostImageRef:     postImageRef,
		Artifact:         finalized.Render.Artifact,
		Details:          finalized.Render.Details,
	}
	if hasFlag(args, "--json") {
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# harness generate (applied)")
	fmt.Fprintf(stdout, "- harness: %s\n", report.Harness)
	fmt.Fprintf(stdout, "- target: %s\n", report.TargetPath)
	fmt.Fprintf(stdout, "- changed: %t\n", report.Changed)
	fmt.Fprintf(stdout, "- work: %s\n", report.WorkID)
	fmt.Fprintf(stdout, "- action_intent: %s\n", report.ActionIntentID)
	fmt.Fprintf(stdout, "- policy: %s (%s)\n", report.PolicyOutcome, report.PolicyDecisionID)
	fmt.Fprintf(stdout, "- receipt: %s\n", report.ReceiptID)
	fmt.Fprintf(stdout, "- transition: %s\n", report.TransitionID)
	fmt.Fprintf(stdout, "- content_sha256: %s\n", report.Artifact.ContentSHA256)
	return 0
}

func harnessPermissionReceiptedActionInput(outcome harness.PermissionApplyOutcome) (work.ReceiptedActionInput, time.Time, error) {
	authorizedAt, err := time.Parse(time.RFC3339Nano, outcome.AuthorizedAt)
	if err != nil {
		return work.ReceiptedActionInput{}, time.Time{}, fmt.Errorf("parse permission authorization timestamp: %w", err)
	}
	receiptOutcome := "no_op"
	if outcome.Changed {
		receiptOutcome = "completed"
	}
	preRef := "sha256:" + permissionApplyDigest(outcome.Render.Preimage)
	return work.ReceiptedActionInput{
		Action: outcome.Action, PreEvaluatedDecision: &outcome.Decision, ExpectedWorkState: "running",
		Outcome: receiptOutcome, Changed: outcome.Changed, Summary: "permission activation " + receiptOutcome, MoveType: "control_plane_repair",
		ProofRefs:  []string{outcome.Render.Artifact.SourceAuthorityRef, outcome.Render.Artifact.SourceRevision},
		BeforeRefs: []string{preRef}, AfterRefs: []string{outcome.Render.Artifact.ContentSHA256},
		FilesChanged: append([]string(nil), outcome.FilesChanged...),
	}, authorizedAt, nil
}

func permissionApplyAuthorization(store *work.Store, homePath, workID string) harness.PermissionApplyAuthorization {
	return func(action envelope.ActionIntent, approved envelope.PolicyDecision, now time.Time) error {
		currentWork, err := store.Get(workID)
		if err != nil {
			return fmt.Errorf("reload work %s: %w", workID, err)
		}
		if currentWork.State != "running" {
			return fmt.Errorf("work %s is %s; permission apply requires running work", workID, currentWork.State)
		}
		leases, err := store.ListLeases()
		if err != nil {
			return fmt.Errorf("reload work lease: %w", err)
		}
		leaseHolder := ""
		for _, lease := range leases {
			if lease.WorkID == workID {
				leaseHolder = lease.By
				break
			}
		}
		if leaseHolder == "" || leaseHolder != action.Actor {
			return fmt.Errorf("action actor %q does not match current work lease holder %q", action.Actor, leaseHolder)
		}
		if expiresAt := strings.TrimSpace(approved.ExpiresAt); expiresAt != "" {
			expires, err := time.Parse(time.RFC3339, expiresAt)
			if err != nil {
				return fmt.Errorf("approved permission policy expires_at invalid: %w", err)
			}
			if !now.Before(expires) {
				return fmt.Errorf("approved permission policy expired at %s", expiresAt)
			}
		}
		cfg, _, err := policy.LoadConfig(homePath)
		if err != nil {
			return fmt.Errorf("reload permission policy: %w", err)
		}
		current := policy.Evaluate(action, cfg, now)
		if !policy.Allowed(current) {
			return fmt.Errorf("current permission policy %s (%s)", current.Outcome, strings.Join(current.Reasons, "; "))
		}
		if current.StandingGrantID != approved.StandingGrantID {
			return fmt.Errorf("current standing grant %q does not match approved standing grant %q", current.StandingGrantID, approved.StandingGrantID)
		}
		if current.Outcome != approved.Outcome || current.ExpiresAt != approved.ExpiresAt || !reflect.DeepEqual(current.Obligations, approved.Obligations) {
			return fmt.Errorf("current permission policy outcome or obligations no longer match the approved decision")
		}
		return nil
	}
}

func permissionApplyDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("%x", digest[:])
}

func observedPermissionTargetRef(path string) (string, error) {
	body, err := readPermissionTargetForObservation(path)
	if err != nil {
		marker := "observation_error:permission-target-read:sha256:" + permissionApplyDigest([]byte(err.Error()))
		return marker, fmt.Errorf("read permission target %s: %w", path, err)
	}
	return "sha256:" + permissionApplyDigest(body), nil
}
