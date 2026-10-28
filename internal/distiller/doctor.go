package distiller

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/writeback"
)

// DoctorCheck is one health/debt observation from the distiller read model.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// DoctorReport summarizes the distiller's compounding state without promoting anything.
type DoctorReport struct {
	Status                                string         `json:"status"`
	TotalDecisions                        int            `json:"total_decisions"`
	DecisionsByKind                       map[string]int `json:"decisions_by_kind"`
	VerdictCounts                         map[string]int `json:"verdict_counts"`
	CandidateInboxCounts                  map[string]int `json:"candidate_inbox_counts"`
	MemoryPromoted                        int            `json:"memory_promoted"`
	MemoryParked                          int            `json:"memory_parked"`
	MemoryRejected                        int            `json:"memory_rejected"`
	DuplicateMemoryRejections             int            `json:"duplicate_memory_rejections"`
	DuplicateCandidateRejections          int            `json:"duplicate_candidate_rejections"`
	NoPromoteRows                         int            `json:"no_promote_rows"`
	DuplicateMemoryNoPromoteRows          int            `json:"duplicate_memory_no_promote_rows"`
	DuplicateCandidateNoPromoteRows       int            `json:"duplicate_candidate_no_promote_rows"`
	EvalCases                             int            `json:"eval_cases"`
	MethodUpgradeDebt                     int            `json:"method_upgrade_debt"`
	PolicyPatchDebt                       int            `json:"policy_patch_debt"`
	CapabilityObservations                int            `json:"capability_observations"`
	OperationalizedCapabilityObservations int            `json:"operationalized_capability_observations"`
	SelfImprovementDebt                   int            `json:"self_improvement_debt"`
	Checks                                []DoctorCheck  `json:"checks"`
}

// Doctor reads COMPOUND_HOME distiller state: adjudication ledger, typed inboxes,
// and no-promote rows. It is intentionally read-only.
func Doctor(homePath string) (DoctorReport, error) {
	report := DoctorReport{
		Status:               "ok",
		DecisionsByKind:      map[string]int{},
		VerdictCounts:        map[string]int{},
		CandidateInboxCounts: map[string]int{},
	}

	decisions, err := readAdjudicationDecisions(filepath.Join(homePath, "ledger", "adjudications.jsonl"))
	if err != nil {
		return report, err
	}
	report.TotalDecisions = len(decisions)
	for _, d := range decisions {
		report.DecisionsByKind[d.Kind]++
		report.VerdictCounts[d.Verdict]++

		if d.Kind == KindMemory {
			switch d.Verdict {
			case Promote:
				report.MemoryPromoted++
			case Park:
				report.MemoryParked++
			case Reject:
				report.MemoryRejected++
			}
			if d.Verdict == Reject && d.Relation == "duplicate" {
				report.DuplicateMemoryRejections++
			}
		} else if d.Verdict == Reject && d.Relation == "duplicate" {
			report.DuplicateCandidateRejections++
		}
		if d.Kind == KindCapabilityObs && d.Verdict == Operationalize {
			report.OperationalizedCapabilityObservations++
		}
	}

	for _, kind := range []string{
		KindMemory,
		KindSkill,
		KindDecision,
		KindFailure,
		KindTaste,
		KindProduct,
		KindRoute,
		KindCapabilityObs,
		KindMethodUpgrade,
		KindEvalCase,
		KindPolicyPatch,
	} {
		n, err := countCandidateFiles(homePath, kind)
		if err != nil {
			return report, err
		}
		report.CandidateInboxCounts[kind] = n
	}

	report.EvalCases = report.CandidateInboxCounts[KindEvalCase]
	resolvedImprovements, err := terminalImprovementReviews(homePath)
	if err != nil {
		return report, err
	}
	report.MethodUpgradeDebt, err = unresolvedImprovementCandidates(homePath, KindMethodUpgrade, resolvedImprovements)
	if err != nil {
		return report, err
	}
	report.PolicyPatchDebt, err = unresolvedImprovementCandidates(homePath, KindPolicyPatch, resolvedImprovements)
	if err != nil {
		return report, err
	}
	report.CapabilityObservations = report.CandidateInboxCounts[KindCapabilityObs]
	report.SelfImprovementDebt = report.MethodUpgradeDebt + report.PolicyPatchDebt

	noPromotes, err := readNoPromoteDecisions(filepath.Join(homePath, "ledger", "no-promote-decisions.jsonl"))
	if err != nil {
		return report, err
	}
	report.NoPromoteRows = len(noPromotes)
	for _, d := range noPromotes {
		if d.CandidateKind == KindMemory && d.OwnerTarget.Kind == "memory_claim" {
			report.DuplicateMemoryNoPromoteRows++
		} else if d.OwnerTarget.Kind == "candidate" {
			report.DuplicateCandidateNoPromoteRows++
		}
	}

	report.Checks = append(report.Checks,
		DoctorCheck{
			Name:   "eval_corpus",
			Status: statusForPositive(report.EvalCases),
			Detail: fmt.Sprintf("%d eval case candidate(s)", report.EvalCases),
		},
		DoctorCheck{
			Name:   "method_policy_debt",
			Status: statusForDebt(report.SelfImprovementDebt),
			Detail: fmt.Sprintf("%d method upgrade review(s), %d policy patch review(s) unresolved", report.MethodUpgradeDebt, report.PolicyPatchDebt),
		},
		DoctorCheck{
			Name:   "capability_observations",
			Status: statusForPositive(report.CapabilityObservations),
			Detail: fmt.Sprintf("%d candidate(s), %d operationalized", report.CapabilityObservations, report.OperationalizedCapabilityObservations),
		},
		DoctorCheck{
			Name:   "duplicate_churn",
			Status: statusForPositive(report.DuplicateMemoryRejections + report.DuplicateCandidateRejections),
			Detail: fmt.Sprintf("lifetime totals: %d duplicate memory rejection(s), %d duplicate candidate rejection(s), %d memory no-promote row(s), %d candidate no-promote row(s); recent rate unknown", report.DuplicateMemoryRejections, report.DuplicateCandidateRejections, report.DuplicateMemoryNoPromoteRows, report.DuplicateCandidateNoPromoteRows),
		},
	)
	if report.SelfImprovementDebt > 0 {
		report.Status = "attention"
	}
	return report, nil
}

func readAdjudicationDecisions(path string) ([]AdjudicationDecision, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var out []AdjudicationDecision
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var d AdjudicationDecision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			return nil, fmt.Errorf("parse adjudication row %q: %w", line, err)
		}
		out = append(out, d)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readNoPromoteDecisions(path string) ([]writeback.NoPromoteDecision, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var out []writeback.NoPromoteDecision
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var d writeback.NoPromoteDecision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			return nil, fmt.Errorf("parse no-promote row %q: %w", line, err)
		}
		out = append(out, d)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func countCandidateFiles(homePath, kind string) (int, error) {
	rel := candidates.InboxDir(candidates.Kind(kind))
	if rel == "" {
		return 0, fmt.Errorf("candidate kind %q has no inbox mapping", kind)
	}
	files, err := filepath.Glob(filepath.Join(homePath, rel, "*.json"))
	if err != nil {
		return 0, err
	}
	return len(files), nil
}

// The inbox is a durable candidate inventory: a reviewed candidate remains on disk.
// Only a terminal owner review closes the operator decision represented by debt.
func unresolvedImprovementCandidates(homePath, kind string, reviews map[string]improvementReviewState) (int, error) {
	rel := candidates.InboxDir(candidates.Kind(kind))
	files, err := filepath.Glob(filepath.Join(homePath, rel, "*.json"))
	if err != nil {
		return 0, err
	}
	var count int
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		var candidate Candidate
		if err := json.Unmarshal(data, &candidate); err != nil {
			return 0, fmt.Errorf("parse improvement candidate %s: %w", path, err)
		}
		if candidate.ID != strings.TrimSuffix(filepath.Base(path), ".json") || candidate.Kind != kind {
			return 0, fmt.Errorf("improvement candidate %s has mismatched id or kind", path)
		}
		if !reviews[kind+"\x00"+candidate.ID].terminal {
			count++
		}
	}
	return count, nil
}

type improvementReviewState struct {
	at       time.Time
	id       string
	terminal bool
}

func terminalImprovementReviews(homePath string) (map[string]improvementReviewState, error) {
	reviews := map[string]improvementReviewState{}
	dir := filepath.Join(homePath, "reviews", ImprovementReviewArea)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return reviews, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".reviewed.json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var reviewed ReviewedImprovement
		if err := json.Unmarshal(data, &reviewed); err != nil {
			return nil, fmt.Errorf("parse improvement review %s: %w", path, err)
		}
		if reviewed.SchemaVersion != ReviewedImprovementSchemaVersion || reviewed.ReviewID == "" || reviewed.ID != "reviewed_"+reviewed.ReviewID || entry.Name() != reviewed.ReviewID+".reviewed.json" || reviewed.PlanID == "" || reviewed.PlanID != filepath.Base(reviewed.PlanID) || reviewed.CandidateID == "" {
			return nil, fmt.Errorf("invalid improvement review %s", path)
		}
		at, err := time.Parse(time.RFC3339, reviewed.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("invalid improvement review timestamp %s: %w", path, err)
		}
		plan, err := readImprovementPlan(homePath, reviewed.PlanID)
		if err != nil {
			return nil, err
		}
		if plan.SchemaVersion != ImprovementPlanSchemaVersion {
			return nil, fmt.Errorf("improvement review %s references invalid plan schema", path)
		}
		if reviewed.CandidateID != plan.CandidateID || reviewed.CandidateKind != plan.CandidateKind || reviewed.ImprovementID != plan.Draft.ImprovementID || (reviewed.CandidateKind != KindMethodUpgrade && reviewed.CandidateKind != KindPolicyPatch) {
			return nil, fmt.Errorf("improvement review %s does not match its plan", path)
		}
		if reviewed.Status != improvementReviewStatus(reviewed.Decision) || !improvementReviewDecisionOK(reviewed.Decision) {
			return nil, fmt.Errorf("improvement review %s has inconsistent decision or status", path)
		}
		key := reviewed.CandidateKind + "\x00" + reviewed.CandidateID
		prior := reviews[key]
		if prior.id == "" || at.After(prior.at) || (at.Equal(prior.at) && reviewed.ReviewID > prior.id) {
			reviews[key] = improvementReviewState{
				at: at, id: reviewed.ReviewID,
				terminal: reviewed.Decision == ImprovementReviewDecisionAccept || reviewed.Decision == ImprovementReviewDecisionReject,
			}
		}
	}
	return reviews, nil
}

func statusForPositive(n int) string {
	if n > 0 {
		return "observed"
	}
	return "empty"
}

func statusForDebt(n int) string {
	if n > 0 {
		return "attention"
	}
	return "ok"
}

// NeedsWriteback reports whether distiller method/policy debt should enter the
// governed owner writeback path. Duplicate churn is handled by NoPromoteDecision,
// not by rewriting distillation policy.
func NeedsWriteback(report DoctorReport) bool {
	return report.MethodUpgradeDebt > 0 || report.PolicyPatchDebt > 0
}

// EvidenceRefs returns bounded metadata refs for a distiller doctor report.
func EvidenceRefs(report DoctorReport) []string {
	refs := []string{
		fmt.Sprintf("distiller:adjudications:%d", report.TotalDecisions),
		fmt.Sprintf("distiller:method_upgrades:%d", report.MethodUpgradeDebt),
		fmt.Sprintf("distiller:policy_patches:%d", report.PolicyPatchDebt),
		fmt.Sprintf("distiller:eval_cases:%d", report.EvalCases),
		fmt.Sprintf("distiller:capability_observations:%d", report.CapabilityObservations),
		fmt.Sprintf("distiller:duplicate_memory_rejections:%d", report.DuplicateMemoryRejections),
	}
	if report.MethodUpgradeDebt > 0 {
		refs = append(refs, "candidate_kind:"+KindMethodUpgrade)
	}
	if report.PolicyPatchDebt > 0 {
		refs = append(refs, "candidate_kind:"+KindPolicyPatch)
	}
	return refs
}

// WritebackGate converts distillation method/policy debt into the generic owner
// writeback contract. It never applies a new distillation method.
func WritebackGate(report DoctorReport, actionIntentRef, policyDecisionRef string, now time.Time) writeback.OwnerWritebackGate {
	kind := KindMethodUpgrade
	if report.PolicyPatchDebt > 0 {
		kind = KindPolicyPatch
	}
	return writeback.OwnerWritebackGate{
		SchemaVersion:     writeback.GateSchemaVersion,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		CandidateRef:      fmt.Sprintf("distiller:method_policy_debt:%d:%d:%s", report.MethodUpgradeDebt, report.PolicyPatchDebt, methodPolicyDigest(report)),
		CandidateKind:     kind,
		OwnerTarget:       writeback.OwnerTarget{Kind: "distillation_policy", Ref: "distillation_policy:" + DefaultPolicy().Stamp()},
		GateBasis:         fmt.Sprintf("distiller doctor found %d method upgrade(s) and %d policy patch(es); distillation policy may need governed update", report.MethodUpgradeDebt, report.PolicyPatchDebt),
		ActionIntentRef:   actionIntentRef,
		MutationClass:     "local_reversible",
		PolicyDecisionRef: policyDecisionRef,
		EvidenceRefs:      EvidenceRefs(report),
	}
}

func methodPolicyDigest(report DoctorReport) string {
	refs := EvidenceRefs(report)
	sort.Strings(refs)
	sum := sha256.Sum256([]byte(strings.Join(refs, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}
