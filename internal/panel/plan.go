package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/envelope"
)

type PlanInput struct {
	WorkID                string   `json:"work_id,omitempty"`
	TargetKind            string   `json:"target_kind"`
	TargetRefs            []string `json:"target_refs"`
	States                []string `json:"states"`
	Domain                string   `json:"domain,omitempty"`
	Task                  string   `json:"task,omitempty"`
	Limit                 int      `json:"limit"`
	PrivacySensitive      bool     `json:"privacy_sensitive"`
	PrivacyOverrideReason string   `json:"privacy_override_reason,omitempty"`
	Supervised            bool     `json:"supervised"`
	Unsupervised          bool     `json:"-"`
	Budget                Budget   `json:"budget"`
}

type ErrBlockedNoLenses struct {
	TargetKind string
}

func (e ErrBlockedNoLenses) Error() string {
	targetKind := strings.TrimSpace(e.TargetKind)
	if targetKind == "" {
		targetKind = "unknown"
	}
	return fmt.Sprintf("blocked_no_lenses:%s", targetKind)
}

func BuildPlan(homePath string, in PlanInput, nowFn func() time.Time) (Run, error) {
	if err := in.Budget.Validate(); err != nil {
		return Run{}, err
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	binding, err := resolvePanelWorkBinding(homePath, in.WorkID)
	if err != nil {
		return Run{}, err
	}
	targetKind := strings.TrimSpace(in.TargetKind)
	if targetKind == "" {
		return Run{}, fmt.Errorf("target kind required")
	}
	targets, countsBefore, err := resolvePlanTargets(homePath, targetKind, in.TargetRefs, in.States, in.Limit)
	if err != nil {
		return Run{}, err
	}
	taskText := strings.TrimSpace(in.Task)
	if binding.bound {
		boundTaskText := firstPanelTaskText(binding.task)
		if taskText != "" && taskText != boundTaskText {
			return Run{}, fmt.Errorf("panel --task conflicts with task contract bound to work_id %s", binding.workID)
		}
		taskText = boundTaskText
	}
	selection, err := SelectLensesWithContext(homePath, targetKind, LensContext{Domain: in.Domain, Task: taskText})
	if err != nil {
		return Run{}, err
	}
	supervised := in.Supervised
	if !supervised && !in.Unsupervised {
		supervised = true
	}
	run := Run{
		SchemaVersion: RunSchemaVersion,
		ID:            newRunID(now, targetKind, targets, binding.workID, binding.task),
		CreatedAt:     now.Format(time.RFC3339),
		Status:        "planned",
		WorkID:        binding.workID,
		WorkBound:     binding.bound,
		TaskContract:  binding.task,
		TargetKind:    targetKind,
		Supervised:    supervised,
		Targets:       targets,
		LensSet:       LensSet{Included: selection.IncludedRefs, Excluded: selection.Excluded},
		Budget:        in.Budget,
		Findings:      []TargetFinding{},
		CountsBefore:  countsBefore,
		CountsAfter:   copyCounts(countsBefore),
		EvidenceRefs:  []string{},
	}
	if !binding.bound {
		run.TaskContract = envelope.TaskContract{Intent: taskText, Mode: "review"}
	}
	if gapID, err := recordMissingDomainLensCandidate(homePath, targetKind, in, selection); err != nil {
		return Run{}, err
	} else if gapID != "" {
		run.EvidenceRefs = append(run.EvidenceRefs, "candidate:"+gapID)
	}
	if supervised {
		run.Supervision = "operator"
	}

	privacySensitive := in.PrivacySensitive
	privacyReason := strings.TrimSpace(in.PrivacyOverrideReason)
	if targetKind == "memory_candidate" && !in.PrivacySensitive {
		if privacyReason == "" {
			publicKnowledge, err := memoryTargetsDefaultNotSensitive(homePath, targets)
			if err != nil {
				return Run{}, err
			}
			if publicKnowledge {
				run.EvidenceRefs = append(run.EvidenceRefs, "privacy_default:not_sensitive:public_arxiv_memory_candidate")
			} else {
				privacySensitive = true
			}
		} else {
			run.EvidenceRefs = append(run.EvidenceRefs, "override:privacy_not_sensitive:"+privacyReason)
		}
	} else if targetKind != "memory_candidate" && !in.PrivacySensitive {
		if privacyReason != "" {
			run.EvidenceRefs = append(run.EvidenceRefs, "override:privacy_not_sensitive:"+privacyReason)
		} else {
			run.EvidenceRefs = append(run.EvidenceRefs, "privacy_default:not_sensitive:"+targetKind)
		}
	}

	if len(selection.IncludedRefs) == 0 {
		err := ErrBlockedNoLenses{TargetKind: targetKind}
		run.Status = "blocked"
		run.StoppedReason = err.Error()
		store, openErr := Open(homePath)
		if openErr != nil {
			return Run{}, openErr
		}
		if saveErr := store.SaveRun(run); saveErr != nil {
			return Run{}, saveErr
		}
		return run, err
	}

	lanes, err := SelectLanes(homePath, privacySensitive, supervised, now)
	if err != nil {
		var blocked ErrBlockedNoLane
		if errors.As(err, &blocked) {
			run.Route = RoutePlan(lanes)
			run.Status = "blocked"
			run.StoppedReason = "blocked_no_lane:" + blocked.Class
			store, openErr := Open(homePath)
			if openErr != nil {
				return Run{}, openErr
			}
			if saveErr := store.SaveRun(run); saveErr != nil {
				return Run{}, saveErr
			}
			return run, err
		}
		return Run{}, err
	}
	run.Route = RoutePlan(lanes)
	store, err := Open(homePath)
	if err != nil {
		return Run{}, err
	}
	if err := store.SaveRun(run); err != nil {
		return Run{}, err
	}
	return run, nil
}

func resolvePlanTargets(homePath, targetKind string, refs, states []string, limit int) ([]Target, map[string]int, error) {
	inputs, err := loadPlanTargetInputs(homePath, targetKind, states)
	if err != nil {
		return nil, nil, err
	}
	wanted := stringSet(refs)
	stateFilter := stringSet(states)
	counts := map[string]int{}
	targets := []Target{}
	seen := map[string]bool{}
	for _, input := range inputs {
		if len(wanted) > 0 && !wanted[input.Ref] {
			continue
		}
		state := targetState(input.Payload)
		if len(stateFilter) > 0 && !stateFilter[state] {
			continue
		}
		counts[state]++
		seen[input.Ref] = true
		if limit > 0 && len(targets) >= limit {
			continue
		}
		targets = append(targets, Target{Ref: input.Ref, Privacy: input.Visibility, StateBefore: state})
	}
	for ref := range wanted {
		if !seen[ref] {
			return nil, nil, fmt.Errorf("target %q not found kind %s states %s", ref, targetKind, strings.Join(cleanStrings(states), ","))
		}
	}
	return targets, counts, nil
}

func memoryTargetsDefaultNotSensitive(homePath string, targets []Target) (bool, error) {
	if len(targets) == 0 {
		return false, nil
	}
	for _, target := range targets {
		input, err := LoadTargetInput(homePath, "memory_candidate", target.Ref)
		if err != nil {
			return false, err
		}
		if !publicArxivKnowledgeMemoryPayload(input.Payload) {
			return false, nil
		}
	}
	return true, nil
}

func publicArxivKnowledgeMemoryPayload(payload json.RawMessage) bool {
	var candidate struct {
		Entity         string            `json:"entity"`
		Qualifiers     map[string]string `json:"qualifiers"`
		SourceTraceRef string            `json:"source_trace_ref"`
	}
	if err := json.Unmarshal(payload, &candidate); err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(candidate.Entity), "knowledge:") &&
		strings.TrimSpace(candidate.Qualifiers["source"]) == "knowledge_extraction" &&
		strings.HasPrefix(strings.TrimSpace(candidate.Qualifiers["item_ref"]), "arxiv:") &&
		strings.HasPrefix(strings.TrimSpace(candidate.SourceTraceRef), "knowledge_extraction:")
}

func loadPlanTargetInputs(homePath, targetKind string, states []string) ([]TargetInput, error) {
	switch targetKind {
	case "memory_candidate":
		return LoadMemoryCandidateTargets(homePath, states, 0)
	case "skill_candidate":
		return LoadSkillCandidateTargets(homePath, 0)
	case "capability_observation":
		return LoadCapabilityObservationTargets(homePath, 0)
	case "decision_candidate":
		return LoadDecisionCandidateTargets(homePath, 0)
	case "comparison":
		return LoadComparisonTargets(homePath, 0)
	default:
		return nil, fmt.Errorf("unsupported target kind %q", targetKind)
	}
}

func targetState(payload json.RawMessage) string {
	var obj struct {
		Kind    string `json:"kind"`
		State   string `json:"state"`
		Status  string `json:"status"`
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "unknown"
	}
	for _, value := range []string{obj.State, obj.Status, obj.Verdict} {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if obj.Kind == "capability_observation" {
		return "awaiting_verdict"
	}
	return "unknown"
}

func recordMissingDomainLensCandidate(homePath, targetKind string, in PlanInput, selection LensSelection) (string, error) {
	if len(selection.MissingDomainLens) == 0 {
		return "", nil
	}
	domain := strings.TrimSpace(in.Domain)
	task := strings.TrimSpace(in.Task)
	id := "cobs_missing_domain_lens_" + targetDigest(targetKind+"|"+domain+"|"+task)
	detailParts := []string{"target_kind=" + targetKind, "domain=" + domain}
	evidenceRefs := []string{"panel:lens_selection", "target_kind:" + targetKind, "domain:" + domain, "missing_domain_lens:" + strings.Join(selection.MissingDomainLens, ",")}
	if task != "" {
		detailParts = append(detailParts, "task="+task)
		evidenceRefs = append(evidenceRefs, "task:"+task)
	}
	payload := capabilityObservationTarget{
		ID:           id,
		Kind:         string(candidates.CapabilityObs),
		Subject:      "panel_lens_selection",
		Statement:    "No reviewed panel lens matched the requested domain context.",
		Detail:       strings.Join(detailParts, " "),
		Confidence:   1.0,
		State:        "needs_review",
		EvidenceRefs: evidenceRefs,
	}
	if err := candidates.WriteCandidate(homePath, candidates.CapabilityObs, id, payload); err != nil {
		return "", err
	}
	return id, nil
}

func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out[value] = true
		}
	}
	return out
}

func copyCounts(in map[string]int) map[string]int {
	out := map[string]int{}
	for key, value := range in {
		out[key] = value
	}
	return out
}

func newRunID(now time.Time, targetKind string, targets []Target, workID string, task envelope.TaskContract) string {
	parts := []string{targetKind, now.UTC().Format(time.RFC3339Nano)}
	for _, target := range targets {
		parts = append(parts, target.Ref)
	}
	if workID = strings.TrimSpace(workID); workID != "" {
		parts = append(parts, "work:"+workID)
		if data, err := json.Marshal(task); err == nil {
			parts = append(parts, "task_contract:"+string(data))
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "prun_" + now.UTC().Format("20060102T150405Z") + "_" + hex.EncodeToString(sum[:])[:12]
}
