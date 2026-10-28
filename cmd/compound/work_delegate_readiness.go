package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"compound/internal/contract"
	"compound/internal/harness"
	"compound/internal/hookplan"
	"compound/internal/work"
)

func init() {
	work.DelegateHarnessReadiness = evaluateDelegateHarnessReadiness
}

var (
	delegateLoadPermissionAuthority = harness.LoadPermissionAuthority
	delegatePermissionDrift         = harness.PermissionDrift
)

// evaluateDelegateHarnessReadiness is the read-only activation check for the
// governed delegated-session path. It shares adapter-owned launch prerequisites
// with execution and checks current permission, hook, and instruction authority.
// A ready snapshot is not launch authority; execution revalidates independently.
func evaluateDelegateHarnessReadiness(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessResult, error) {
	harnessName := normalizeDelegateReadinessHarness(req.Harness)
	if harnessName == "" {
		return work.DelegateHarnessReadinessResult{}, fmt.Errorf("harness is required")
	}

	details := harness.ReadOnlyLaunchReadiness(req.HomePath, harness.RunInput{
		LaunchProfileID: req.LaunchProfileID,
		WorkID:          req.WorkID, Harness: harnessName, Workspace: req.Workspace,
		Command: req.Command, Args: append([]string(nil), req.Args...), PermissionMode: "plan",
		LaneID: req.LaneID, RequestedModelID: req.RequestedModelID,
		RequestedProviderRoute: req.RequestedProviderRoute, Timeout: req.Timeout, Now: req.Now,
	})
	var evidence []work.DelegateHarnessReadinessEvidence
	if req.CommandOverridden {
		details = append(details, "executable: raw command override is not attested by the resolved lane tuple")
	} else if strings.TrimSpace(req.Command) == "" {
		details = append(details, "executable: resolved command is empty")
	} else {
		evidence = append(evidence, work.DelegateHarnessReadinessEvidence{
			Kind: "executable",
			Ref:  "lane-tuple://" + firstDelegateReadinessValue(req.RequestedModelID, req.LaneID, harnessName),
		})
	}
	argvEvidence, argvDetail := delegateActivationArgvReadiness(req)
	if argvDetail != "" {
		details = append(details, argvDetail)
	} else {
		evidence = append(evidence, argvEvidence)
	}

	permissionPolicy, permissionRevision, err := delegateLoadPermissionAuthority(req.HomePath, harnessName)
	if err != nil {
		details = append(details, "permission_authority: "+err.Error())
	} else {
		var postureEvidence *work.DelegateHarnessReadinessEvidence
		drift, err := delegatePermissionDrift(req.HomePath, harnessName)
		switch {
		case err != nil:
			details = append(details, "permission_posture: "+err.Error())
		case drift.Drifted:
			detail := strings.Join(drift.Details, ", ")
			if detail == "" {
				detail = "managed settings differ from authority"
			}
			details = append(details, "permission_posture: "+detail)
		case filepath.Clean(drift.TargetPath) != filepath.Clean(permissionPolicy.TargetPath):
			details = append(details, "permission_posture: validated target does not match permission authority")
		case strings.TrimSpace(drift.TargetRevision) == "":
			details = append(details, "permission_posture: owner did not attest the validated target revision")
		default:
			// PermissionDrift validates and hashes one captured target body.
			// Attest that owner-produced revision directly: a second ReadFile
			// here could hash replacement bytes that were never validated.
			postureEvidence = &work.DelegateHarnessReadinessEvidence{
				Kind:     "permission_posture",
				Ref:      "harness-posture://" + harnessName,
				Revision: drift.TargetRevision,
			}
		}
		_, stableRevision, stableErr := delegateLoadPermissionAuthority(req.HomePath, harnessName)
		switch {
		case stableErr != nil:
			details = append(details, "permission_authority: revalidate after posture: "+stableErr.Error())
		case stableRevision != permissionRevision:
			details = append(details, "permission_authority: revision changed during readiness")
		default:
			evidence = append(evidence, work.DelegateHarnessReadinessEvidence{
				Kind:     "permission_authority",
				Ref:      harness.PermissionAuthorityRef(harnessName),
				Revision: permissionRevision,
			})
			if postureEvidence != nil {
				evidence = append(evidence, *postureEvidence)
			}
		}
	}

	registry := harness.DefaultRegistry()
	manifest, err := contract.LoadInstalledHooksManifest(req.HomePath)
	if err != nil {
		details = append(details, "blocking_hook: "+err.Error())
	} else {
		status := "not_installed"
		for _, row := range contract.InstalledHookGateReadiness(manifest, nativePreToolUseBlockMap(registry)) {
			if row.Harness == harnessName && row.Hook == "PreToolUse" {
				status = row.Status
				break
			}
		}
		if status != "native_blocking" {
			details = append(details, "blocking_hook: status="+status)
		}
	}

	harnessHome := strings.TrimSpace(req.HarnessHome)
	if harnessHome == "" {
		harnessHome, err = os.UserHomeDir()
		if err != nil {
			details = append(details, "blocking_hook_live: resolve harness home: "+err.Error())
		}
	}
	if harnessHome != "" {
		reconciled, reconcileErr := contract.ReconcileInstalledHooks(req.HomePath, harnessHome, registry)
		if reconcileErr != nil {
			details = append(details, "blocking_hook_live: "+reconcileErr.Error())
		} else {
			binding, bindingDetail := delegateDeclaredMutationBlockingBinding(reconciled.Plan, harnessName)
			delta := delegateBindingReconciliation(reconciled.Reconciliation, binding)
			switch {
			case bindingDetail != "":
				details = append(details, "blocking_hook_live: "+bindingDetail)
			case delta == nil:
				details = append(details, "blocking_hook_live: mutation-covering binding has no exact reconciliation")
			case delta.State != hookplan.StateReady:
				details = append(details, "blocking_hook_live: mutation-covering binding status="+delta.State)
			case delta.BindingID != binding.ID ||
				delta.Matcher != binding.Matcher ||
				delta.DesiredHandlerRef != binding.HandlerRef ||
				delta.DesiredInvocationRevision != binding.InvocationRevision ||
				delta.DesiredHandlerRevision != binding.HandlerRevision:
				details = append(details, "blocking_hook_live: mutation-covering reconciliation identity does not match declared binding")
			default:
				evidence = append(evidence,
					work.DelegateHarnessReadinessEvidence{
						Kind:     "blocking_hook_plan",
						Ref:      binding.ID,
						Revision: reconciled.Reconciliation.PlanRevision,
					},
					work.DelegateHarnessReadinessEvidence{
						Kind:     "blocking_hook_installation",
						Ref:      reconciled.Reconciliation.ObservationRef,
						Revision: reconciled.Reconciliation.ObservationRevision,
					},
				)
			}
		}
	}

	packDrift, err := harness.InstructionPackDrift(req.HomePath, harnessName, "", req.Now)
	blockingPackDrift := delegateBlockingInstructionPackDrift(packDrift.Details)
	switch {
	case err != nil:
		details = append(details, "instruction_pack: "+err.Error())
	case packDrift.Drifted:
		if len(blockingPackDrift) > 0 {
			details = append(details, "instruction_pack: "+strings.Join(blockingPackDrift, ", "))
		}
	}
	if err == nil && len(blockingPackDrift) == 0 {
		pack, err := loadDelegateInstructionPack(req.HomePath, harnessName)
		switch {
		case err != nil:
			details = append(details, "instruction_pack: "+err.Error())
		case pack.SchemaVersion != harness.InstructionPackSchemaVersion:
			details = append(details, "instruction_pack: schema_version is not current")
		case normalizeDelegateReadinessHarness(pack.Harness) != harnessName:
			details = append(details, "instruction_pack: harness identity does not match")
		case pack.ContentSHA256 != delegateInstructionPackContentSHA256(pack):
			details = append(details, "instruction_pack: managed pack body content_sha256 is invalid")
		default:
			if pack.PolicyCore.Absent {
				details = append(details, "instruction_pack: policy_core is absent")
			}
			if pack.Posture.Absent {
				details = append(details, "instruction_pack: posture is absent")
			}
			if pack.Roster.InstalledHooks.Absent {
				details = append(details, "instruction_pack: installed_hooks roster is absent")
			}
			if pack.Roster.EffectiveContract.Absent {
				details = append(details, "instruction_pack: effective_contract roster is absent")
			}
			if strings.TrimSpace(pack.Roster.RegistryRevision) == "" {
				details = append(details, "instruction_pack: registry_revision is absent")
			}
			if !pack.PolicyCore.Absent && !pack.Posture.Absent &&
				!pack.Roster.InstalledHooks.Absent && !pack.Roster.EffectiveContract.Absent {
				contextEvidence, contextDetails := validateDelegateInstructionContext(req, pack)
				evidence = append(evidence, contextEvidence...)
				details = append(details, contextDetails...)
			}
			if len(details) == 0 {
				evidence = append(evidence, work.DelegateHarnessReadinessEvidence{
					Kind:     "instruction_pack",
					Ref:      "instruction-pack://" + harnessName,
					Revision: pack.ContentSHA256,
				})
			}
		}
	}

	return work.DelegateHarnessReadinessResult{
		Ready:    len(details) == 0,
		Details:  details,
		Evidence: evidence,
	}, nil
}

func delegateBlockingInstructionPackDrift(driftDetails []string) []string {
	// Scoped contracts no longer rotate because unrelated contexts materialize.
	// Every remaining difference is real drift of this installed consumer.
	return driftDetails
}

func delegateActivationArgvReadiness(req work.DelegateHarnessReadinessRequest) (work.DelegateHarnessReadinessEvidence, string) {
	const policy = "compound.delegate.argv.positional-prompt-only.v0.1"
	evidence := work.DelegateHarnessReadinessEvidence{
		Kind:     "activation_argv",
		Ref:      "lane-tuple://" + firstDelegateReadinessValue(req.RequestedModelID, req.LaneID, normalizeDelegateReadinessHarness(req.Harness)) + "/argv-policy",
		Revision: delegateReadinessBytesRevision([]byte(policy)),
	}
	switch {
	case len(req.Args) == 0:
		return evidence, ""
	case len(req.Args) != 1:
		return work.DelegateHarnessReadinessEvidence{}, "activation_argv: lane tuple does not attest multi-token argv"
	case strings.TrimSpace(req.Args[0]) == "":
		return work.DelegateHarnessReadinessEvidence{}, "activation_argv: empty positional argv is not attested"
	case strings.HasPrefix(strings.TrimSpace(req.Args[0]), "-"):
		return work.DelegateHarnessReadinessEvidence{}, "activation_argv: flag-shaped argv is not attested"
	case strings.ContainsRune(req.Args[0], '\x00'):
		return work.DelegateHarnessReadinessEvidence{}, "activation_argv: argv contains an invalid NUL byte"
	default:
		return evidence, ""
	}
}

func delegateDeclaredMutationBlockingBinding(plan hookplan.HookPlan, harnessName string) (*hookplan.Binding, string) {
	if len(plan.Intentions) > 0 {
		var guard *hookplan.CompiledIntention
		for i := range plan.Intentions {
			if plan.Intentions[i].Intention != hookplan.IntentionGuardMutation {
				continue
			}
			if guard != nil {
				return nil, "mutation guard intention is ambiguous"
			}
			guard = &plan.Intentions[i]
		}
		if guard == nil {
			return nil, "mutation guard intention is absent"
		}
		if guard.Disposition != hookplan.DispositionNative &&
			guard.Disposition != hookplan.DispositionFallback {
			detail := "mutation guard disposition=" + string(guard.Disposition)
			if strings.TrimSpace(guard.Reason) != "" {
				detail += ": " + strings.TrimSpace(guard.Reason)
			}
			return nil, detail
		}
		if guard.EnforcementStrength != "blocking" {
			return nil, "mutation guard enforcement=" + guard.EnforcementStrength
		}

		var match *hookplan.Binding
		for i := range plan.Bindings {
			binding := &plan.Bindings[i]
			if normalizeDelegateReadinessHarness(binding.Harness) != harnessName ||
				binding.Intention != hookplan.IntentionGuardMutation ||
				!binding.Blocking ||
				binding.EnforcementStrength != "blocking" {
				continue
			}
			if match != nil {
				return nil, "mutation-covering semantic binding is ambiguous"
			}
			match = binding
		}
		if match == nil {
			return nil, "mutation-covering semantic binding is absent"
		}
		return match, ""
	}

	// Legacy imported plans carry only adapter-native fields. Keep this
	// compatibility reader until installed-hook authority is regenerated from
	// semantic posture; new compiled plans never reach this branch.
	var match *hookplan.Binding
	for i := range plan.Bindings {
		binding := &plan.Bindings[i]
		if normalizeDelegateReadinessHarness(binding.Harness) != harnessName ||
			binding.Event != "PreToolUse" ||
			!binding.Blocking ||
			binding.Matcher != hookplan.HarnessMatcher(harnessName) {
			continue
		}
		if match != nil {
			return nil, "mutation-covering blocking PreToolUse declaration is ambiguous"
		}
		match = binding
	}
	if match == nil {
		return nil, "mutation-covering blocking PreToolUse declaration is absent"
	}
	return match, ""
}

func delegateBindingReconciliation(reconciliation hookplan.Reconciliation, binding *hookplan.Binding) *hookplan.Delta {
	if binding == nil {
		return nil
	}
	for i := range reconciliation.Deltas {
		delta := &reconciliation.Deltas[i]
		if delta.BindingID == binding.ID {
			return delta
		}
	}
	return nil
}

func delegateInstructionPackContentSHA256(pack harness.InstructionPack) string {
	body := map[string]any{
		"schema_version": pack.SchemaVersion,
		"harness":        pack.Harness,
		"policy_core": map[string]any{
			"authority_ref":   pack.PolicyCore.AuthorityRef,
			"source_revision": pack.PolicyCore.SourceRevision,
			"content_sha256":  pack.PolicyCore.ContentSHA256,
			"absent":          pack.PolicyCore.Absent,
		},
		"posture": map[string]any{
			"authority_ref":   pack.Posture.AuthorityRef,
			"source_revision": pack.Posture.SourceRevision,
			"content_sha256":  pack.Posture.ContentSHA256,
			"absent":          pack.Posture.Absent,
		},
		"roster": map[string]any{
			"registry_revision": pack.Roster.RegistryRevision,
			"installed_hooks": map[string]any{
				"ref":      pack.Roster.InstalledHooks.Ref,
				"revision": pack.Roster.InstalledHooks.Revision,
				"absent":   pack.Roster.InstalledHooks.Absent,
			},
			"effective_contract": map[string]any{
				"ref":      pack.Roster.EffectiveContract.Ref,
				"revision": pack.Roster.EffectiveContract.Revision,
				"absent":   pack.Roster.EffectiveContract.Absent,
			},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	return delegateReadinessBytesRevision(encoded)
}

func validateDelegateInstructionContext(req work.DelegateHarnessReadinessRequest, pack harness.InstructionPack) ([]work.DelegateHarnessReadinessEvidence, []string) {
	var evidence []work.DelegateHarnessReadinessEvidence
	var details []string
	contextPackID := strings.TrimSpace(req.ContextPackID)
	if contextPackID == "" {
		return evidence, []string{"instruction_pack: work has no context_pack_id"}
	}
	if !strings.HasPrefix(contextPackID, "ctx_") || strings.ContainsAny(contextPackID, `/\`) {
		return evidence, []string{"instruction_pack: work context_pack_id is invalid"}
	}

	artifacts, err := contract.ReadContextPackContract(req.HomePath, contextPackID)
	if err != nil {
		return evidence, []string{"instruction_pack: context/contract artifacts: " + err.Error()}
	}
	contextPack := artifacts.ContextPack
	effective := artifacts.EffectiveContract
	if workID := strings.TrimSpace(contextPack.IDs.WorkID); workID != "" && workID != strings.TrimSpace(req.WorkID) {
		details = append(details, "instruction_pack: context pack work identity does not match")
	}

	bound, err := harness.ReadInstructionPackContract(req.HomePath, normalizeDelegateReadinessHarness(req.Harness), pack.Roster.EffectiveContract.Ref)
	if err != nil || bound.Revision != pack.Roster.EffectiveContract.Revision {
		details = append(details, "instruction_pack: effective_contract scoped binding is unresolved or changed")
		return evidence, details
	}
	if normalizeDelegateReadinessHarness(effective.Harness) != normalizeDelegateReadinessHarness(req.Harness) {
		details = append(details, "instruction_pack: effective contract harness identity does not match")
	}
	if strings.TrimSpace(contextPack.IDs.ContractID) == "" || contextPack.IDs.ContractID != effective.ID {
		details = append(details, "instruction_pack: work context pack does not bind the effective contract")
	}
	if workID := strings.TrimSpace(effective.IDs.WorkID); workID != "" && workID != strings.TrimSpace(req.WorkID) {
		details = append(details, "instruction_pack: effective contract work identity does not match")
	}
	if !delegateReadinessSurfaceMatches(contextPack.Scope.Surface, req.Workspace) ||
		!delegateReadinessSurfaceMatches(effective.Scope.Surface, req.Workspace) {
		details = append(details, "instruction_pack: contract/context workspace scope does not match delegated workspace")
	}
	if len(details) == 0 {
		evidence = append(evidence,
			work.DelegateHarnessReadinessEvidence{
				Kind:     "context_pack",
				Ref:      "context-pack://" + contextPack.ID,
				Revision: artifacts.ContextPackRevision,
			},
			work.DelegateHarnessReadinessEvidence{
				Kind:     "effective_contract",
				Ref:      "contract://" + effective.ID,
				Revision: artifacts.EffectiveContractRevision,
			},
		)
	}
	return evidence, details
}

func readDelegateReadinessJSON(path string, out any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

func delegateReadinessSurfaceMatches(surface, workspace string) bool {
	surface = strings.TrimSpace(surface)
	workspace = strings.TrimSpace(workspace)
	if surface == "" || workspace == "" {
		return false
	}
	surfaceAbs, surfaceErr := filepath.Abs(surface)
	workspaceAbs, workspaceErr := filepath.Abs(workspace)
	if surfaceErr != nil || workspaceErr != nil {
		return false
	}
	return filepath.Clean(surfaceAbs) == filepath.Clean(workspaceAbs)
}

func firstDelegateReadinessValue(values ...string) string {
	for _, value := range values {
		if normalized := strings.TrimSpace(value); normalized != "" {
			return normalized
		}
	}
	return "unknown"
}

func delegateReadinessBytesRevision(body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%x", sum)
}

func loadDelegateInstructionPack(homePath, harnessName string) (harness.InstructionPack, error) {
	body, err := os.ReadFile(harness.InstructionPackPath(homePath, harnessName, ""))
	if err != nil {
		return harness.InstructionPack{}, err
	}
	var pack harness.InstructionPack
	if err := json.Unmarshal(body, &pack); err != nil {
		return harness.InstructionPack{}, fmt.Errorf("decode latest pack: %w", err)
	}
	return pack, nil
}

func normalizeDelegateReadinessHarness(value string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), "-", "_")
	if normalized == "claude" {
		return "claude_code"
	}
	return normalized
}
