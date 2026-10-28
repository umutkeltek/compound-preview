package sessionmining

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/distiller"
	"compound/internal/mem"
	"compound/internal/reviewartifact"
)

const ApplyReportSchemaVersion = "compound.sessions_mine_apply_report.v0.1"

type Finding struct {
	Kind         string   `json:"kind"`
	SemanticKey  string   `json:"semantic_key,omitempty"`
	Summary      string   `json:"summary"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	Confidence   float64  `json:"confidence,omitempty"`
}

type ApplyInput struct {
	Preferences   []Finding
	RulesToRetire []Finding
	SkillUpdates  []Finding
}

type ApplyReport struct {
	SchemaVersion          string           `json:"schema_version"`
	Status                 string           `json:"status"`
	CreatedAt              string           `json:"created_at"`
	DryRun                 bool             `json:"dry_run"`
	PreferenceCandidateIDs []string         `json:"preference_candidate_ids,omitempty"`
	RetireProposalIDs      []string         `json:"retire_proposal_ids,omitempty"`
	SkillUpdateProposalIDs []string         `json:"skill_update_proposal_ids,omitempty"`
	RoutedArtifacts        []RoutedArtifact `json:"routed_artifacts,omitempty"`
	Warnings               []string         `json:"warnings,omitempty"`
}

type RoutedArtifact struct {
	Route          string   `json:"route"`
	ID             string   `json:"id"`
	CandidateID    string   `json:"candidate_id,omitempty"`
	PlanPath       string   `json:"plan_path,omitempty"`
	ReviewPath     string   `json:"review_path,omitempty"`
	ReviewYAMLPath string   `json:"review_yaml_path,omitempty"`
	EvidenceRefs   []string `json:"evidence_refs"`
}

func Apply(homePath string, input ApplyInput, now time.Time) (ApplyReport, error) {
	out := ApplyReport{
		SchemaVersion: ApplyReportSchemaVersion,
		Status:        "review_required",
		CreatedAt:     now.UTC().Format(time.RFC3339),
	}
	for _, finding := range input.Preferences {
		candidateID, err := recordPreferenceCandidate(homePath, finding, now)
		if err != nil {
			return out, err
		}
		out.PreferenceCandidateIDs = append(out.PreferenceCandidateIDs, candidateID)
		out.RoutedArtifacts = append(out.RoutedArtifacts, RoutedArtifact{
			Route: "memory_candidate", ID: candidateID, EvidenceRefs: finding.EvidenceRefs,
		})
	}
	for _, finding := range input.RulesToRetire {
		artifact, err := recordRuleRetireProposal(homePath, finding, now)
		if err != nil {
			return out, err
		}
		out.RetireProposalIDs = append(out.RetireProposalIDs, artifact.ID)
		out.RoutedArtifacts = append(out.RoutedArtifacts, artifact)
	}
	for _, finding := range input.SkillUpdates {
		artifact, err := recordSkillLifecycleProposal(homePath, finding, now)
		if err != nil {
			return out, err
		}
		out.SkillUpdateProposalIDs = append(out.SkillUpdateProposalIDs, artifact.ID)
		out.RoutedArtifacts = append(out.RoutedArtifacts, artifact)
	}
	if len(out.PreferenceCandidateIDs) == 0 && len(out.RetireProposalIDs) == 0 && len(out.SkillUpdateProposalIDs) == 0 {
		out.Status = "ok"
		out.Warnings = append(out.Warnings, "no routed findings met deterministic thresholds")
	}
	sort.Strings(out.PreferenceCandidateIDs)
	sort.Strings(out.RetireProposalIDs)
	sort.Strings(out.SkillUpdateProposalIDs)
	return out, nil
}

func SemanticKey(kind, identity string) string {
	kind = reviewartifact.Slugify(kind, 32)
	if kind == "" {
		kind = "finding"
	}
	return kind + ":" + digest(strings.ToLower(strings.TrimSpace(identity)))[:16]
}

func findingSemanticKey(finding Finding) string {
	if key := strings.TrimSpace(finding.SemanticKey); key != "" {
		return key
	}
	return SemanticKey(finding.Kind, finding.Summary)
}

func recordPreferenceCandidate(homePath string, finding Finding, now time.Time) (string, error) {
	if err := requireEvidence(finding); err != nil {
		return "", err
	}
	store, err := mem.Open(homePath)
	if err != nil {
		return "", err
	}
	semanticKey := findingSemanticKey(finding)
	existingID, err := existingPreferenceCandidateID(store, semanticKey)
	if err != nil || existingID != "" {
		return existingID, err
	}
	recorded, err := store.Capture([]mem.Candidate{{
		Entity: "operator", Predicate: "preference." + reviewartifact.Slugify(finding.Kind, 32), Object: finding.Summary,
		ObservationDate: now.UTC().Format(time.RFC3339), Confidence: finding.Confidence,
		Derivation: "deterministic", ProvenanceLane: "deterministic", ClaimType: "preference",
		Qualifiers: map[string]string{
			"domain": "behavioral", "source": "sessions_mine", "finding_kind": finding.Kind, "semantic_key": semanticKey,
		},
		SourceTraceRef: finding.EvidenceRefs[0],
		SnippetHash:    "sha256:" + digest(finding.Summary, strings.Join(finding.EvidenceRefs, "\n")),
		Visibility:     "contextual",
		Reason:         "session-mined behavioral preference candidate; requires adjudication before recall",
	}}, now)
	if err != nil {
		return "", err
	}
	if len(recorded) == 0 {
		return "", fmt.Errorf("preference candidate was not recorded for finding %q", finding.Summary)
	}
	return recorded[0].ID, nil
}

func existingPreferenceCandidateID(store *mem.Store, semanticKey string) (string, error) {
	all, err := store.AllCandidates()
	if err != nil {
		return "", err
	}
	var ids []string
	for _, candidate := range all {
		if candidate.Qualifiers["source"] == "sessions_mine" && candidate.Qualifiers["semantic_key"] == semanticKey {
			ids = append(ids, candidate.ID)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

func recordRuleRetireProposal(homePath string, finding Finding, now time.Time) (RoutedArtifact, error) {
	if err := requireEvidence(finding); err != nil {
		return RoutedArtifact{}, err
	}
	candidateID := "session_rule_retire_" + reviewartifact.Digest12(findingSemanticKey(finding))
	planID := reviewartifact.ID("imp_", candidateID, "")
	reviewDir, err := reviewartifact.Dir(homePath, distiller.ImprovementReviewArea)
	if err != nil {
		return RoutedArtifact{}, err
	}
	paths := distiller.ImprovementPlanPaths{
		PlanPath: filepath.Join(reviewDir, planID+".json"), ReviewPath: filepath.Join(reviewDir, planID+".md"),
		ReviewYAMLPath: filepath.Join(reviewDir, planID+".review.yaml"),
	}
	if info, statErr := os.Lstat(paths.PlanPath); statErr == nil {
		if !info.Mode().IsRegular() {
			return RoutedArtifact{}, fmt.Errorf("existing session rule retirement plan %s is not a regular file", paths.PlanPath)
		}
		return RoutedArtifact{
			Route: "distiller_improvement_plan", ID: planID, CandidateID: candidateID,
			PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath, ReviewYAMLPath: paths.ReviewYAMLPath,
			EvidenceRefs: compactRefs(finding.EvidenceRefs...),
		}, nil
	} else if !os.IsNotExist(statErr) {
		return RoutedArtifact{}, statErr
	}
	candidate := distiller.Candidate{
		ID: candidateID, Kind: distiller.KindPolicyPatch, Subject: "session-mined rule retirement proposal",
		Statement:  finding.Summary,
		Detail:     "Review whether this rule only creates review friction without changing future execution. Do not retire any rule without an operator-recorded review.",
		Confidence: finding.Confidence, Derivation: "deterministic", EvidenceRefs: compactRefs(finding.EvidenceRefs...),
		PolicyVersion: distiller.DefaultPolicy().Stamp(), Verdict: candidates.NoPromote,
	}
	if err := candidates.WriteCandidate(homePath, candidates.PolicyPatch, candidateID, candidate); err != nil {
		return RoutedArtifact{}, err
	}
	plan, err := distiller.BuildImprovementPlan(homePath, candidateID, "", now)
	if err != nil {
		return RoutedArtifact{}, err
	}
	if plan.ID != planID {
		return RoutedArtifact{}, fmt.Errorf("session rule retirement plan id %s, want %s", plan.ID, planID)
	}
	paths, err = distiller.RecordImprovementPlan(homePath, plan)
	if err != nil {
		return RoutedArtifact{}, err
	}
	return RoutedArtifact{
		Route: "distiller_improvement_plan", ID: plan.ID, CandidateID: candidateID,
		PlanPath: paths.PlanPath, ReviewPath: paths.ReviewPath, ReviewYAMLPath: paths.ReviewYAMLPath,
		EvidenceRefs: plan.EvidenceRefs,
	}, nil
}

func recordSkillLifecycleProposal(homePath string, finding Finding, now time.Time) (RoutedArtifact, error) {
	if err := requireEvidence(finding); err != nil {
		return RoutedArtifact{}, err
	}
	capabilityRef := "session-mining:" + findingSemanticKey(finding)
	if existing, ok, err := existingSkillLifecycleProposal(homePath, capabilityRef, finding.EvidenceRefs); err != nil || ok {
		return existing, err
	}
	proposal, err := capgraph.BuildCapabilityLifecycleProposal(capgraph.CapabilityLifecycleProposalInput{
		ProposalKind: "revise", Subject: "session-mined skill update proposal", Statement: finding.Summary,
		Detail:     "Review whether the repeated session pattern should revise, narrow, or retire an existing capability. Do not install or mutate skills from this proposal.",
		Confidence: finding.Confidence, EvidenceRefs: finding.EvidenceRefs, CapabilityRef: capabilityRef,
	}, now)
	if err != nil {
		return RoutedArtifact{}, err
	}
	paths, err := capgraph.RecordCapabilityLifecycleProposal(homePath, proposal)
	if err != nil {
		return RoutedArtifact{}, err
	}
	return RoutedArtifact{
		Route: "capability_lifecycle_proposal", ID: proposal.ID, PlanPath: paths.PlanPath,
		ReviewPath: paths.ReviewPath, EvidenceRefs: compactRefs(finding.EvidenceRefs...),
	}, nil
}

func existingSkillLifecycleProposal(homePath, capabilityRef string, evidenceRefs []string) (RoutedArtifact, bool, error) {
	dir, err := reviewartifact.Dir(homePath, capgraph.CapabilityLifecycleProposalReviewArea)
	if err != nil {
		return RoutedArtifact{}, false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return RoutedArtifact{}, false, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "clp_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		planPath := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(planPath)
		if err != nil {
			return RoutedArtifact{}, false, err
		}
		var proposal capgraph.CapabilityLifecycleProposal
		if err := json.Unmarshal(data, &proposal); err != nil {
			return RoutedArtifact{}, false, fmt.Errorf("read existing session capability proposal %s: %w", planPath, err)
		}
		if proposal.Draft.CapabilityRef == capabilityRef {
			return RoutedArtifact{
				Route: "capability_lifecycle_proposal", ID: proposal.ID, PlanPath: planPath,
				ReviewPath: filepath.Join(dir, proposal.ID+".md"), EvidenceRefs: compactRefs(evidenceRefs...),
			}, true, nil
		}
	}
	return RoutedArtifact{}, false, nil
}

func requireEvidence(finding Finding) error {
	if len(compactRefs(finding.EvidenceRefs...)) == 0 {
		return fmt.Errorf("finding %q missing session evidence refs", finding.Summary)
	}
	return nil
}

func compactRefs(values ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func digest(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])
}
