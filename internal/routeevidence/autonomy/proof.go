// Package autonomy owns the pure, content-addressed proof that route and
// terminal-outcome evidence satisfies one class-scoped admission grant. It is
// nested under routeevidence so routing remains one owner, but deliberately
// imports neither the parent routeevidence package nor work: work can recheck
// this proof at AdmitAuto without creating work -> routeevidence -> writeback
// -> work.
package autonomy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	homepkg "compound/internal/home"

	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion               = "compound.autonomy_eligibility.v0.1"
	AuthorityRef                = "compound://artifacts/autonomy-eligibility"
	terminalSampleLookbackDays  = 30
	trustedPanelRuntimeRecorder = "trusted_panel_runtime"
)

type GrantSnapshot struct {
	DecisionID         string `json:"decision_id"`
	ClaimID            string `json:"claim_id"`
	GrantRef           string `json:"grant_ref"`
	ClaimRef           string `json:"claim_ref"`
	ReviewedBy         string `json:"reviewed_by"`
	Class              string `json:"class"`
	MinTerminalSamples int    `json:"min_terminal_samples"`
	MinWindowDays      int    `json:"min_window_days"`
	MaxFailures        int    `json:"max_failures"`
}

type LaneSnapshot struct {
	ModelID           string         `json:"model_id"`
	ProviderRoute     string         `json:"provider_route,omitempty"`
	ProviderRouteKind string         `json:"provider_route_kind,omitempty"`
	Resolution        LaneResolution `json:"resolution"`
	Tier              string         `json:"tier"`
	ClaimLevel        string         `json:"claim_level,omitempty"`
	GateRole          string         `json:"gate_role"`
}

type ResolvedTarget struct {
	Provider string `json:"provider" yaml:"provider"`
	Model    string `json:"model" yaml:"model"`
}

type LaneResolution struct {
	Mode    string           `json:"mode" yaml:"mode"`
	Targets []ResolvedTarget `json:"targets,omitempty" yaml:"targets,omitempty"`
}

type RoutePlanSnapshot struct {
	ID                string         `json:"id"`
	AuthorityPath     string         `json:"authority_path"`
	SourceAuthorityID string         `json:"source_authority_id,omitempty"`
	SourceRevision    string         `json:"source_revision"`
	CapabilityClass   string         `json:"capability_class"`
	PrivacySensitive  bool           `json:"privacy_sensitive"`
	Chain             []LaneSnapshot `json:"chain"`
}

type Sample struct {
	WorkID              string `json:"work_id"`
	WorkSHA256          string `json:"work_sha256"`
	ReceiptID           string `json:"receipt_id,omitempty"`
	ReceiptSHA256       string `json:"receipt_sha256,omitempty"`
	ReceiptCreatedAt    string `json:"receipt_created_at,omitempty"`
	ReceiptOutcome      string `json:"receipt_outcome,omitempty"`
	ReceiptChanged      bool   `json:"receipt_changed"`
	RoutePlanID         string `json:"route_plan_id,omitempty"`
	RouteEvidenceID     string `json:"route_evidence_id,omitempty"`
	RouteEvidenceSHA256 string `json:"route_evidence_sha256,omitempty"`
	RouteStatus         string `json:"route_status,omitempty"`
	RouteModelID        string `json:"route_model_id,omitempty"`
	RouteMode           string `json:"route_mode,omitempty"`
	ResolvedLaneID      string `json:"resolved_lane_id,omitempty"`
	ProviderID          string `json:"provider_id,omitempty"`
	ModelID             string `json:"model_id,omitempty"`
	ResolutionSource    string `json:"resolution_source,omitempty"`
	GateRole            string `json:"gate_role,omitempty"`
	Valid               bool   `json:"valid"`
	InvalidReason       string `json:"invalid_reason,omitempty"`
}

type Artifact struct {
	SchemaVersion              string            `json:"schema_version"`
	ID                         string            `json:"id"`
	GeneratedAt                string            `json:"generated_at"`
	Status                     string            `json:"status"`
	Class                      string            `json:"class"`
	CapabilityClass            string            `json:"capability_class"`
	Grant                      GrantSnapshot     `json:"grant"`
	RoutePlan                  RoutePlanSnapshot `json:"route_plan"`
	CohortClass                string            `json:"cohort_class"`
	LookbackDays               int               `json:"lookback_days"`
	LookbackStart              string            `json:"lookback_start"`
	LookbackEnd                string            `json:"lookback_end"`
	ExcludedStaleTerminalCount int               `json:"excluded_stale_terminal_count"`
	// ExcludedUnverifiableCount counts records excluded from eligibility history
	// because they can never be samples for this class: terminal records in the
	// ephemeral work_live_ namespace, and OFF-class recent-terminal records whose
	// custody trail is unverifiable (multiple terminal events, unparseable
	// timestamps). Class-relevant custody violations still fail the compilation
	// closed — with ONE settled exception below.
	ExcludedUnverifiableCount int `json:"excluded_unverifiable_count"`
	// ExcludedSealMismatchIDs lists recent-terminal work ids whose recorded
	// custody seal disagrees with the envelope snapshot hash (the legacy
	// unsealed-sweep-mutation shape). One such envelope is EXCLUDED with the
	// id reported here — never allowed as a sample — instead of failing the
	// whole compilation: one stale envelope must not veto every other
	// cohort's pass (the 2026-08-28 sweep reclaimed 71 envelopes without
	// re-sealing, and each hourly director run since failed globally on a
	// different one). The governed repair is `compound work reseal`; while a
	// mismatch remains unrepaired its id stays visible on the artifact.
	ExcludedSealMismatchIDs []string `json:"excluded_seal_mismatch_ids,omitempty"`
	TerminalSampleCount     int      `json:"terminal_sample_count"`
	FailureCount            int      `json:"failure_count"`
	WindowStart             string   `json:"window_start,omitempty"`
	WindowEnd               string   `json:"window_end,omitempty"`
	WindowSeconds           int64    `json:"window_seconds"`
	Samples                 []Sample `json:"samples"`
	Reasons                 []string `json:"reasons,omitempty"`
	EvidenceRefs            []string `json:"evidence_refs"`
}

type workSnapshot struct {
	Work envelope.WorkEnvelope
	Raw  []byte
}

type routeRow struct {
	SchemaVersion  string `json:"schema_version"`
	ID             string `json:"id"`
	CreatedAt      string `json:"created_at"`
	RequestID      string `json:"request_id"`
	WorkID         string `json:"work_id,omitempty"`
	ClientProfile  string `json:"client_profile"`
	Protocol       string `json:"protocol"`
	Status         string `json:"status"`
	RecordedVia    string `json:"recorded_via"`
	SelectedTarget struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Protocol string `json:"protocol"`
	} `json:"selected_target"`
	ResolvedProvider string   `json:"resolved_provider,omitempty"`
	ResolvedModel    string   `json:"resolved_model,omitempty"`
	ResolutionSource string   `json:"resolution_source,omitempty"`
	GatewayRequestID string   `json:"gateway_request_id,omitempty"`
	EvidenceRefs     []string `json:"evidence_refs,omitempty"`
}

type routeRaw struct {
	Row routeRow
	Raw []byte
}

type receiptEvidence struct {
	Refs             []string
	Terminals        []terminalEvidence
	LatestWorkSHA256 string
}

type terminalEvidence struct {
	At            string
	WorkSHA256    string
	ReceiptSHA256 string
}

type decidingRouteBinding struct {
	ClientProfile string
	Protocol      string
	Provider      string
}

type modelManifest struct {
	Models map[string]struct {
		ProviderRoute     string         `yaml:"provider_route"`
		ProviderRouteKind string         `yaml:"provider_route_kind"`
		Resolution        LaneResolution `yaml:"resolution"`
		Tier              string         `yaml:"tier"`
		Capabilities      map[string]struct {
			Level string `yaml:"level"`
		} `yaml:"capabilities"`
	} `yaml:"models"`
	RoutingDefaults map[string][]string `yaml:"routing_defaults"`
}

func GateRole(class, tier, level string) string {
	class = strings.TrimSpace(strings.ToLower(class))
	tier = strings.TrimSpace(strings.ToLower(tier))
	level = strings.TrimSpace(strings.ToLower(level))
	switch {
	case level == "avoid":
		return "avoid"
	case level == "assumed":
		return "supervised_only"
	case level == "":
		return "review_missing_claim"
	case !ValidClaimLevel(level):
		return "review_unknown_claim"
	case tier == "":
		return "review_missing_tier"
	case !ValidTier(tier):
		return "review_unknown_tier"
	case tier == "free" && (class == "judge-critique" || class == "certify"):
		return "tripwire_only"
	default:
		return "eligible"
	}
}

func ValidClaimLevel(level string) bool {
	switch strings.TrimSpace(strings.ToLower(level)) {
	case "primary", "capable", "assumed", "avoid":
		return true
	default:
		return false
	}
}

func ValidTier(tier string) bool {
	switch strings.TrimSpace(strings.ToLower(tier)) {
	case "free", "cheap", "premium", "local-batch", "browser":
		return true
	default:
		return false
	}
}

func RoutePlanID(capabilityClass string, privacySensitive bool, authorityPath, sourceRevision string) string {
	return idFor("crp_", capabilityClass, fmt.Sprint(privacySensitive), authorityPath, sourceRevision)
}

func Compile(homePath string, grant GrantSnapshot, plan RoutePlanSnapshot, now time.Time) (Artifact, error) {
	if now.IsZero() {
		now = time.Now()
	}
	evaluatedAt := now.UTC()
	cohortClass := strings.TrimSpace(grant.Class)
	lookbackStart := evaluatedAt.Add(-terminalSampleLookbackDays * 24 * time.Hour)
	artifact := Artifact{
		SchemaVersion:   SchemaVersion,
		GeneratedAt:     evaluatedAt.Format(time.RFC3339),
		Status:          "ineligible",
		Class:           cohortClass,
		CapabilityClass: strings.TrimSpace(plan.CapabilityClass),
		Grant:           grant,
		RoutePlan:       plan,
		CohortClass:     cohortClass,
		LookbackDays:    terminalSampleLookbackDays,
		LookbackStart:   lookbackStart.Format(time.RFC3339),
		LookbackEnd:     evaluatedAt.Format(time.RFC3339),
		Samples:         []Sample{},
		EvidenceRefs:    baseEvidenceRefs(grant, plan),
	}
	if reason, err := validateGrantSnapshot(grant); err != nil {
		return Artifact{}, err
	} else if reason != "" {
		artifact.Reasons = append(artifact.Reasons, reason)
	}
	if reason, err := validateRoutePlanSnapshot(homePath, plan); err != nil {
		return Artifact{}, err
	} else if reason != "" {
		artifact.Reasons = append(artifact.Reasons, reason)
	}

	works, excludedLegacy, err := loadWorks(homePath)
	if err != nil {
		return Artifact{}, err
	}
	artifact.ExcludedUnverifiableCount = len(excludedLegacy)
	receipts, err := receiptEvidenceByWork(homePath)
	if err != nil {
		return Artifact{}, err
	}
	excludedDirty, sealSkipped, err := reconcileRecentTerminalSnapshots(homePath, works, receipts, excludedLegacy, artifact.Class, lookbackStart, evaluatedAt)
	if err != nil {
		return Artifact{}, err
	}
	artifact.ExcludedUnverifiableCount += excludedDirty
	for workID := range sealSkipped {
		artifact.ExcludedSealMismatchIDs = append(artifact.ExcludedSealMismatchIDs, workID)
	}
	artifact.ExcludedSealMismatchIDs = uniqueSorted(artifact.ExcludedSealMismatchIDs)
	routes, err := loadRouteRows(homePath)
	if err != nil {
		return Artifact{}, err
	}

	var candidates []Sample
	for _, ws := range works {
		if _, skipped := sealSkipped[ws.Work.ID]; skipped {
			// Custody-seal mismatch: reported on ExcludedSealMismatchIDs, and
			// never a sample. The pass continues with the remaining cohort.
			continue
		}
		class, classErr := autonomyClassOf(ws.Work)
		if classErr != nil {
			if terminalWorkState(ws.Work.State) && autonomyTriggerMentionsClass(ws.Work, artifact.Class) {
				return Artifact{}, classErr
			}
			continue
		}
		if class != artifact.Class || !terminalWorkState(ws.Work.State) {
			continue
		}
		sample := buildSample(homePath, ws, receipts[ws.Work.ID], routes, artifact.Class, plan)
		if terminalAt, err := time.Parse(time.RFC3339, strings.TrimSpace(sample.ReceiptCreatedAt)); err == nil && terminalAt.After(evaluatedAt) {
			return Artifact{}, fmt.Errorf("terminal work %s has receipt timestamp after eligibility evaluation: %q is after lookback end %q", ws.Work.ID, sample.ReceiptCreatedAt, evaluatedAt.Format(time.RFC3339))
		}
		terminalEvidence := receipts[ws.Work.ID].Terminals
		if sample.ReceiptID != "" && len(terminalEvidence) == 1 {
			terminalAt, terminalErr := time.Parse(time.RFC3339, strings.TrimSpace(terminalEvidence[0].At))
			receiptAt, receiptErr := time.Parse(time.RFC3339, strings.TrimSpace(sample.ReceiptCreatedAt))
			if terminalErr != nil || receiptErr != nil || !receiptAt.Equal(terminalAt) {
				return Artifact{}, fmt.Errorf("terminal work %s receipt timestamp does not match terminal event: receipt %q event %q", ws.Work.ID, sample.ReceiptCreatedAt, terminalEvidence[0].At)
			}
		}
		if terminalSamplePredates(sample, lookbackStart) {
			artifact.ExcludedStaleTerminalCount++
			continue
		}
		candidates = append(candidates, sample)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ReceiptCreatedAt == candidates[j].ReceiptCreatedAt {
			return candidates[i].WorkID > candidates[j].WorkID
		}
		return candidates[i].ReceiptCreatedAt > candidates[j].ReceiptCreatedAt
	})
	limit := grant.MinTerminalSamples
	if limit > len(candidates) {
		limit = len(candidates)
	}
	artifact.Samples = append(artifact.Samples, candidates[:limit]...)
	artifact.TerminalSampleCount = len(artifact.Samples)
	if artifact.TerminalSampleCount < grant.MinTerminalSamples {
		artifact.Reasons = append(artifact.Reasons, fmt.Sprintf("terminal samples %d below minimum %d", artifact.TerminalSampleCount, grant.MinTerminalSamples))
	}

	var sampleTimes []time.Time
	for _, sample := range artifact.Samples {
		artifact.EvidenceRefs = append(artifact.EvidenceRefs, sample.ReceiptID, sample.RouteEvidenceID)
		if !sample.Valid {
			artifact.Reasons = append(artifact.Reasons, sample.WorkID+": "+sample.InvalidReason)
		}
		if sample.ReceiptOutcome != "completed" || !sample.ReceiptChanged {
			artifact.FailureCount++
		}
		if parsed, err := time.Parse(time.RFC3339, sample.ReceiptCreatedAt); err == nil {
			sampleTimes = append(sampleTimes, parsed)
		}
	}
	if len(sampleTimes) > 0 {
		sort.Slice(sampleTimes, func(i, j int) bool { return sampleTimes[i].Before(sampleTimes[j]) })
		artifact.WindowStart = sampleTimes[0].UTC().Format(time.RFC3339)
		artifact.WindowEnd = sampleTimes[len(sampleTimes)-1].UTC().Format(time.RFC3339)
		artifact.WindowSeconds = int64(sampleTimes[len(sampleTimes)-1].Sub(sampleTimes[0]).Seconds())
	}
	minimumWindow := int64(grant.MinWindowDays) * int64((24 * time.Hour).Seconds())
	if artifact.WindowSeconds < minimumWindow {
		artifact.Reasons = append(artifact.Reasons, fmt.Sprintf("window %ds below minimum %d days", artifact.WindowSeconds, grant.MinWindowDays))
	}
	if artifact.FailureCount > grant.MaxFailures {
		artifact.Reasons = append(artifact.Reasons, fmt.Sprintf("failures %d exceed maximum %d", artifact.FailureCount, grant.MaxFailures))
	}
	artifact.Reasons = uniqueSorted(artifact.Reasons)
	artifact.EvidenceRefs = uniqueSorted(artifact.EvidenceRefs)
	if len(artifact.Reasons) == 0 {
		artifact.Status = "eligible"
	}
	artifact.ID = contentID(artifact)
	return artifact, nil
}

func Record(homePath string, artifact Artifact) (string, error) {
	if err := Validate(artifact); err != nil {
		return "", err
	}
	dir := filepath.Join(homePath, "artifacts", "autonomy-eligibility")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, artifact.ID+".json")
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return "", err
	}
	if existing, err := os.ReadFile(path); err == nil {
		var prior Artifact
		if json.Unmarshal(existing, &prior) == nil && prior.ID == artifact.ID {
			return path, nil
		}
		return "", fmt.Errorf("autonomy eligibility path collision for %s", artifact.ID)
	}
	return path, os.WriteFile(path, append(data, '\n'), 0o644)
}

func LoadAndRevalidate(homePath, ref, expectedClass, expectedRoutePlanID string) (Artifact, error) {
	id := strings.TrimSpace(ref)
	id = strings.TrimPrefix(id, "eligibility:")
	if !strings.HasPrefix(id, "autelig_") {
		return Artifact{}, fmt.Errorf("eligibility ref %q is not an autelig artifact", ref)
	}
	path := filepath.Join(homePath, "artifacts", "autonomy-eligibility", id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("read eligibility artifact %s: %w", id, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var artifact Artifact
	if err := dec.Decode(&artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode eligibility artifact %s: %w", id, err)
	}
	if err := Validate(artifact); err != nil {
		return Artifact{}, err
	}
	if artifact.ID != id {
		return Artifact{}, fmt.Errorf("eligibility artifact path id %s does not match content id %s", id, artifact.ID)
	}
	if artifact.Class != expectedClass {
		return Artifact{}, fmt.Errorf("eligibility class %q does not match work class %q", artifact.Class, expectedClass)
	}
	if artifact.RoutePlan.ID != expectedRoutePlanID {
		return Artifact{}, fmt.Errorf("eligibility route plan %q does not match work route plan %q", artifact.RoutePlan.ID, expectedRoutePlanID)
	}
	generatedAt, err := time.Parse(time.RFC3339, artifact.GeneratedAt)
	if err != nil {
		return Artifact{}, fmt.Errorf("eligibility generated_at invalid: %w", err)
	}
	rebuilt, err := Compile(homePath, artifact.Grant, artifact.RoutePlan, generatedAt)
	if err != nil {
		return Artifact{}, fmt.Errorf("eligibility artifact %s is stale or source evidence changed: %w", artifact.ID, err)
	}
	before, _ := json.Marshal(artifact)
	after, _ := json.Marshal(rebuilt)
	if !bytes.Equal(before, after) {
		return Artifact{}, fmt.Errorf("eligibility artifact %s is stale or source evidence changed", artifact.ID)
	}
	return artifact, nil
}

func Validate(artifact Artifact) error {
	var problems []string
	if artifact.SchemaVersion != SchemaVersion {
		problems = append(problems, "schema_version must be "+SchemaVersion)
	}
	if artifact.Status != "eligible" && artifact.Status != "ineligible" {
		problems = append(problems, "status must be eligible|ineligible")
	}
	if strings.TrimSpace(artifact.ID) == "" || !strings.HasPrefix(artifact.ID, "autelig_") {
		problems = append(problems, "id must start with autelig_")
	} else if expected := contentID(artifact); artifact.ID != expected {
		problems = append(problems, fmt.Sprintf("content digest mismatch: expected %s", expected))
	}
	if strings.TrimSpace(artifact.Class) == "" || artifact.Class != artifact.Grant.Class {
		problems = append(problems, "class must match grant class")
	}
	if artifact.CohortClass != artifact.Class {
		problems = append(problems, "cohort_class must match artifact class")
	}
	if artifact.LookbackDays != terminalSampleLookbackDays {
		problems = append(problems, fmt.Sprintf("lookback_days must be %d", terminalSampleLookbackDays))
	}
	if artifact.ExcludedStaleTerminalCount < 0 {
		problems = append(problems, "excluded_stale_terminal_count must not be negative")
	}
	if artifact.ExcludedUnverifiableCount < 0 {
		problems = append(problems, "excluded_unverifiable_count must not be negative")
	}
	for i, id := range artifact.ExcludedSealMismatchIDs {
		if strings.TrimSpace(id) == "" {
			problems = append(problems, "excluded_seal_mismatch_ids must not contain blank ids")
			break
		}
		if i > 0 && id == artifact.ExcludedSealMismatchIDs[i-1] {
			problems = append(problems, "excluded_seal_mismatch_ids must be sorted and unique")
			break
		}
	}
	if generatedAt, err := time.Parse(time.RFC3339, artifact.GeneratedAt); err != nil {
		problems = append(problems, "generated_at must be RFC3339")
	} else {
		wantEnd := generatedAt.UTC().Format(time.RFC3339)
		wantStart := generatedAt.UTC().Add(-terminalSampleLookbackDays * 24 * time.Hour).Format(time.RFC3339)
		if artifact.LookbackStart != wantStart || artifact.LookbackEnd != wantEnd {
			problems = append(problems, "lookback bounds must derive from generated_at")
		}
	}
	if !containsExact(artifact.EvidenceRefs, terminalSampleLookbackRef(artifact.CohortClass)) {
		problems = append(problems, "evidence_refs must name the terminal-sample lookback")
	}
	if strings.TrimSpace(artifact.RoutePlan.ID) == "" || artifact.RoutePlan.ID != RoutePlanID(artifact.RoutePlan.CapabilityClass, artifact.RoutePlan.PrivacySensitive, artifact.RoutePlan.AuthorityPath, artifact.RoutePlan.SourceRevision) {
		problems = append(problems, "route plan identity missing or invalid")
	}
	if len(problems) > 0 {
		return fmt.Errorf("autonomy eligibility invalid: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (artifact Artifact) Ref() string { return "eligibility:" + artifact.ID }

func (artifact Artifact) AdmissionRefs() []string {
	return uniqueSorted(append([]string{artifact.Ref()}, artifact.EvidenceRefs...))
}

func validateGrantSnapshot(grant GrantSnapshot) (string, error) {
	if strings.TrimSpace(grant.DecisionID) == "" || strings.TrimSpace(grant.ClaimID) == "" || strings.TrimSpace(grant.ReviewedBy) == "" {
		return "grant identity, claim, and reviewed_by are required", nil
	}
	if grant.GrantRef != "grant:"+grant.DecisionID || grant.ClaimRef != "claim:"+grant.ClaimID {
		return "grant or claim ref does not match grant identity", nil
	}
	if strings.TrimSpace(grant.Class) == "" || grant.MinTerminalSamples <= 0 || grant.MinWindowDays <= 0 || grant.MaxFailures < 0 || grant.MaxFailures >= grant.MinTerminalSamples {
		return "grant threshold is incomplete or unsafe", nil
	}
	return "", nil
}

func validateRoutePlanSnapshot(homePath string, plan RoutePlanSnapshot) (string, error) {
	wantPath := filepath.Join(homePath, "registries", "model-capabilities.yaml")
	if filepath.Clean(plan.AuthorityPath) != filepath.Clean(wantPath) {
		return "route plan authority path is not the current model-router authority", nil
	}
	data, err := homepkg.ReadRootedRegularFile(homePath, wantPath, "model-router authority")
	if err != nil {
		return "", fmt.Errorf("read model-router authority: %w", err)
	}
	wantRevision := "sha256:" + shaHex(data)
	if plan.SourceRevision != wantRevision {
		return fmt.Sprintf("router revision %q is not current %q", plan.SourceRevision, wantRevision), nil
	}
	if plan.ID != RoutePlanID(plan.CapabilityClass, plan.PrivacySensitive, plan.AuthorityPath, plan.SourceRevision) {
		return "route plan id does not match current class/revision", nil
	}
	var manifest modelManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return "", fmt.Errorf("parse model-router authority: %w", err)
	}
	defaults := manifest.RoutingDefaults[plan.CapabilityClass]
	var expected []LaneSnapshot
	for _, modelID := range defaults {
		model, ok := manifest.Models[modelID]
		if !ok {
			return "route plan references an unknown model", nil
		}
		if plan.PrivacySensitive && model.Tier != "local-batch" {
			continue
		}
		level := strings.TrimSpace(model.Capabilities[plan.CapabilityClass].Level)
		if reason := invalidLaneResolutionReason(model.Resolution); reason != "" {
			return "model " + modelID + " " + reason, nil
		}
		expected = append(expected, LaneSnapshot{ModelID: modelID, ProviderRoute: strings.TrimSpace(model.ProviderRoute), ProviderRouteKind: strings.TrimSpace(model.ProviderRouteKind), Resolution: model.Resolution, Tier: strings.TrimSpace(model.Tier), ClaimLevel: level, GateRole: GateRole(plan.CapabilityClass, strings.TrimSpace(model.Tier), level)})
	}
	if !sameLanes(plan.Chain, expected) {
		return "route plan chain does not match current model-router authority", nil
	}
	return "", nil
}

func buildSample(homePath string, ws workSnapshot, receiptEvidence receiptEvidence, routes map[string][]routeRaw, autonomyClass string, plan RoutePlanSnapshot) Sample {
	sample := Sample{WorkID: ws.Work.ID, WorkSHA256: "sha256:" + shaHex(ws.Raw), ReceiptCreatedAt: ws.Work.CreatedAt, RoutePlanID: ws.Work.RouteDecisionID}
	invalid := func(reason string) Sample {
		sample.Valid = false
		sample.InvalidReason = reason
		return sample
	}
	if len(receiptEvidence.Terminals) == 1 && strings.TrimSpace(receiptEvidence.Terminals[0].At) != "" {
		sample.ReceiptCreatedAt = strings.TrimSpace(receiptEvidence.Terminals[0].At)
	}
	if ws.Work.RouteDecisionID != plan.ID || !containsExact(ws.Work.EvidenceRefs, "route_plan:"+plan.ID) {
		return invalid("route plan does not match current plan and revision")
	}
	if len(receiptEvidence.Refs) == 0 {
		return invalid("terminal work has no receipt")
	}
	if len(receiptEvidence.Refs) != 1 {
		return invalid(fmt.Sprintf("terminal work has %d receipt refs; exactly one required", len(receiptEvidence.Refs)))
	}
	receiptID, receipt, receiptRaw, err := loadBoundReceipt(homePath, receiptEvidence.Refs[0])
	if err != nil {
		return invalid(err.Error())
	}
	if len(receiptEvidence.Terminals) != 1 || strings.TrimSpace(receiptEvidence.Terminals[0].ReceiptSHA256) == "" {
		return invalid("terminal event receipt snapshot hash is required")
	}
	receiptCanonical, err := json.Marshal(receipt)
	if err != nil {
		return invalid("receipt snapshot hash unavailable: " + err.Error())
	}
	if receiptEvidence.Terminals[0].ReceiptSHA256 != "sha256:"+shaHex(receiptCanonical) {
		return invalid("receipt snapshot hash does not match terminal event")
	}
	sample.ReceiptID = receiptID
	sample.ReceiptSHA256 = "sha256:" + shaHex(receiptRaw)
	sample.ReceiptCreatedAt = receipt.CreatedAt
	sample.ReceiptOutcome = receipt.Outcome
	sample.ReceiptChanged = receiptHasGenuineDelta(receipt, routes)
	if receipt.IDs.WorkID != ws.Work.ID {
		return invalid("receipt work_id does not match")
	}
	binding, ok := decidingRouteBindingFor(autonomyClass, plan.CapabilityClass)
	if !ok {
		return invalid("no exact deciding route binding exists for autonomy class " + autonomyClass + " and capability class " + plan.CapabilityClass)
	}

	var matching []struct {
		row          routeRaw
		lane         LaneSnapshot
		resolvedLane LaneSnapshot
	}
	var nonEligibleRoles []string
	for _, ref := range receipt.ProofRefs {
		if !strings.HasPrefix(strings.TrimSpace(ref), "route_") {
			continue
		}
		rows := routes[strings.TrimSpace(ref)]
		if len(rows) == 0 {
			return invalid("route evidence ref " + ref + " is missing")
		}
		if len(rows) != 1 {
			return invalid("route evidence ref " + ref + " is ambiguous")
		}
		lanes := lanesForModel(plan.Chain, rows[0].Row.SelectedTarget.Model)
		if len(lanes) > 1 {
			return invalid("selected model is ambiguous in current route plan")
		}
		if len(lanes) == 0 {
			continue
		}
		if lanes[0].GateRole != "eligible" {
			nonEligibleRoles = append(nonEligibleRoles, lanes[0].GateRole)
			continue
		}
		resolvedProvider := strings.TrimSpace(rows[0].Row.ResolvedProvider)
		resolvedModel := strings.TrimSpace(rows[0].Row.ResolvedModel)
		resolutionSource := strings.TrimSpace(rows[0].Row.ResolutionSource)
		if resolvedProvider == "" {
			return invalid("resolved provider is required for deciding route evidence")
		}
		if resolvedModel == "" {
			return invalid("resolved model is required for deciding route evidence")
		}
		if resolutionSource != "gateway_route" && resolutionSource != "response_body" {
			return invalid("resolved target source is required for deciding route evidence")
		}
		selectedLane := lanes[0]
		var resolvedLane LaneSnapshot
		switch strings.TrimSpace(selectedLane.Resolution.Mode) {
		case "adaptive":
			if resolutionSource != "gateway_route" {
				return invalid("adaptive route requires gateway resolution evidence")
			}
			if strings.TrimSpace(rows[0].Row.GatewayRequestID) == "" {
				return invalid("adaptive route requires gateway request identity")
			}
			resolvedLanes := lanesForResolvedTarget(plan.Chain, resolvedProvider, resolvedModel)
			if len(resolvedLanes) == 0 {
				if resolvedModel == strings.TrimSpace(rows[0].Row.SelectedTarget.Model) {
					return invalid("adaptive route did not resolve to exactly one concrete lane")
				}
				return invalid("resolved target is not present in current route plan")
			}
			if len(resolvedLanes) != 1 {
				return invalid("resolved target is ambiguous in current route plan")
			}
			resolvedLane = resolvedLanes[0]
		case "direct":
			if resolutionSource == "gateway_route" && strings.TrimSpace(rows[0].Row.GatewayRequestID) == "" {
				return invalid("gateway resolution requires gateway request identity")
			}
			if !laneDeclaresResolvedTarget(selectedLane, resolvedProvider, resolvedModel) {
				return invalid("direct route resolved outside its declared target")
			}
			resolvedLanes := lanesForResolvedTarget(plan.Chain, resolvedProvider, resolvedModel)
			if len(resolvedLanes) != 1 || resolvedLanes[0].ModelID != selectedLane.ModelID {
				return invalid("direct route resolved outside its unique selected lane")
			}
			resolvedLane = selectedLane
		default:
			return invalid("selected route has unknown resolution mode")
		}
		if resolvedLane.GateRole != "eligible" {
			nonEligibleRoles = append(nonEligibleRoles, "resolved model gate role is "+resolvedLane.GateRole)
			continue
		}
		matching = append(matching, struct {
			row          routeRaw
			lane         LaneSnapshot
			resolvedLane LaneSnapshot
		}{row: rows[0], lane: selectedLane, resolvedLane: resolvedLane})
	}
	if len(matching) != 1 {
		if len(matching) == 0 && len(nonEligibleRoles) > 0 {
			return invalid("deciding lane gate role is " + strings.Join(uniqueSorted(nonEligibleRoles), ","))
		}
		return invalid(fmt.Sprintf("route evidence has %d eligible deciding rows; exactly one required", len(matching)))
	}
	chosen := matching[0]
	sample.RouteEvidenceID = chosen.row.Row.ID
	sample.RouteEvidenceSHA256 = "sha256:" + shaHex(chosen.row.Raw)
	sample.RouteStatus = chosen.row.Row.Status
	sample.RouteModelID = chosen.row.Row.SelectedTarget.Model
	sample.RouteMode = chosen.lane.Resolution.Mode
	sample.ResolvedLaneID = chosen.resolvedLane.ModelID
	sample.ModelID = strings.TrimSpace(chosen.row.Row.ResolvedModel)
	sample.ProviderID = strings.TrimSpace(chosen.row.Row.ResolvedProvider)
	sample.ResolutionSource = strings.TrimSpace(chosen.row.Row.ResolutionSource)
	sample.GateRole = chosen.resolvedLane.GateRole
	if strings.TrimSpace(chosen.row.Row.WorkID) == "" {
		return invalid("route evidence work_id is required")
	}
	if strings.TrimSpace(chosen.row.Row.WorkID) != ws.Work.ID {
		return invalid("route evidence work_id does not match")
	}
	if chosen.row.Row.SchemaVersion != "compound.execution_route_evidence.v0.1" {
		return invalid("route evidence schema_version is not compound.execution_route_evidence.v0.1")
	}
	if strings.TrimSpace(chosen.row.Row.RecordedVia) != trustedPanelRuntimeRecorder {
		return invalid("deciding route evidence requires trusted panel runtime provenance")
	}
	if strings.TrimSpace(chosen.row.Row.RequestID) == "" {
		return invalid("route evidence request_id is required")
	}
	if countRouteRequestID(routes, chosen.row.Row.RequestID) != 1 {
		return invalid("route evidence request_id is ambiguous")
	}
	if strings.TrimSpace(chosen.row.Row.ClientProfile) != binding.ClientProfile {
		return invalid("route evidence client_profile does not match deciding profile " + binding.ClientProfile)
	}
	if strings.TrimSpace(chosen.row.Row.Protocol) != binding.Protocol || strings.TrimSpace(chosen.row.Row.SelectedTarget.Protocol) != binding.Protocol {
		return invalid("route evidence protocol does not match deciding protocol " + binding.Protocol)
	}
	if strings.TrimSpace(chosen.row.Row.SelectedTarget.Provider) != binding.Provider {
		return invalid("route evidence provider does not match deciding provider " + binding.Provider)
	}
	if !containsExact(chosen.row.Row.EvidenceRefs, "compound://registries/model-router") {
		return invalid("route evidence does not cite the model-router authority")
	}
	if reason := routeTimeBindingReason(ws.Work.CreatedAt, receipt.CreatedAt, chosen.row.Row.CreatedAt); reason != "" {
		return invalid(reason)
	}
	if chosen.row.Row.Status != "completed" {
		return invalid("route evidence status is " + chosen.row.Row.Status)
	}
	if receipt.Outcome == "completed" && !sample.ReceiptChanged {
		return invalid("completed receipt has no genuine state delta independent of route evidence")
	}
	sample.Valid = true
	return sample
}

func loadWorks(homePath string) ([]workSnapshot, map[string]bool, error) {
	type workLocation struct {
		Path         string
		OriginalPath string
		FileKind     string
		Archive      *homepkg.OperationalArchiveEntry
	}
	locations := []workLocation{}
	entries, err := os.ReadDir(filepath.Join(homePath, "work", "work"))
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			originalPath := filepath.ToSlash(filepath.Join("work", "work", entry.Name()))
			locations = append(locations, workLocation{Path: filepath.Join(homePath, filepath.FromSlash(originalPath)), OriginalPath: originalPath, FileKind: "work"})
		}
	}
	archive, err := homepkg.LoadOperationalArchiveIndex(homePath)
	if err != nil {
		return nil, nil, err
	}
	indexedArchivePaths := map[string]bool{}
	archiveWorkRoot := filepath.Join(homepkg.OperationalArchiveFilesRoot(homePath), "work", "work")
	for _, entry := range archive {
		if entry.Kind != "work" {
			continue
		}
		originalPath, ok := canonicalLocalSlashPath(entry.OriginalPath)
		if !ok {
			return nil, nil, fmt.Errorf("operational archive work %s original path must be a canonical local path", entry.ID)
		}
		if filepath.Dir(originalPath) != filepath.Join("work", "work") || filepath.Ext(originalPath) != ".json" {
			return nil, nil, fmt.Errorf("operational archive work %s original path is outside the owned work root", entry.ID)
		}
		archiveRel, ok := canonicalLocalSlashPath(entry.ArchivePath)
		if !ok {
			return nil, nil, fmt.Errorf("operational archive work %s archive path must be a canonical local path", entry.ID)
		}
		expectedArchivePath := filepath.Join(homepkg.OperationalArchiveFilesRel, originalPath)
		if archiveRel != expectedArchivePath {
			return nil, nil, fmt.Errorf("operational archive work %s archive path does not match original path", entry.ID)
		}
		if !terminalWorkState(strings.TrimSpace(entry.Status)) {
			return nil, nil, fmt.Errorf("operational archive work %s status %q is not terminal", entry.ID, entry.Status)
		}
		if strings.TrimSpace(entry.SHA256) == "" {
			return nil, nil, fmt.Errorf("operational archive work %s sha256 is required", entry.ID)
		}
		wantID := "oparch_" + shaHex([]byte(filepath.ToSlash(originalPath)))[:12]
		if entry.ID != wantID {
			return nil, nil, fmt.Errorf("operational archive work id %s does not match original path", entry.ID)
		}
		archivePath := filepath.Join(homePath, archiveRel)
		if filepath.Dir(archivePath) != archiveWorkRoot {
			return nil, nil, fmt.Errorf("operational archive work %s archive path is outside the owned archive work root", entry.ID)
		}
		if indexedArchivePaths[archivePath] {
			return nil, nil, fmt.Errorf("operational archive work path %s is indexed more than once", entry.ArchivePath)
		}
		entryCopy := entry
		locations = append(locations, workLocation{Path: archivePath, OriginalPath: entry.OriginalPath, FileKind: "archive", Archive: &entryCopy})
		indexedArchivePaths[archivePath] = true
	}
	physicalArchiveWorkDir := archiveWorkRoot
	archivedFiles, err := os.ReadDir(physicalArchiveWorkDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	for _, entry := range archivedFiles {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Clean(filepath.Join(physicalArchiveWorkDir, entry.Name()))
		if !indexedArchivePaths[path] {
			return nil, nil, fmt.Errorf("operational archive contains unindexed work file %s", filepath.ToSlash(entry.Name()))
		}
	}
	sort.Slice(locations, func(i, j int) bool { return locations[i].Path < locations[j].Path })
	var out []workSnapshot
	excludedLegacy := map[string]bool{}
	seen := map[string]string{}
	for _, location := range locations {
		raw, err := readRegularFile(homePath, location.Path, location.FileKind)
		if err != nil {
			return nil, nil, err
		}
		if location.Archive != nil {
			if got := shaHex(raw); got != strings.TrimSpace(location.Archive.SHA256) {
				return nil, nil, fmt.Errorf("operational archive work %s sha256 mismatch", location.Archive.ID)
			}
			if location.Archive.Size > 0 && int64(len(raw)) != location.Archive.Size {
				return nil, nil, fmt.Errorf("operational archive work %s size mismatch", location.Archive.ID)
			}
		}
		var work envelope.WorkEnvelope
		if err := json.Unmarshal(raw, &work); err != nil {
			return nil, nil, fmt.Errorf("decode work %s: %w", location.Path, err)
		}
		if err := work.Validate(); err != nil {
			if strings.HasPrefix(work.ID, "work_live_") && terminalWorkState(strings.TrimSpace(work.State)) {
				excludedLegacy[work.ID] = true
				// A legacy ephemeral record persisted to a durable store. It can never be
				// a durable envelope or an eligibility sample; exclude it visibly instead
				// of failing every eligibility compilation over historical debris.
				continue
			}
			return nil, nil, err
		}
		if filepath.Base(filepath.FromSlash(location.OriginalPath)) != work.ID+".json" {
			return nil, nil, fmt.Errorf("work snapshot %s path does not match work identity %s", location.OriginalPath, work.ID)
		}
		if location.Archive != nil && strings.TrimSpace(location.Archive.Status) != strings.TrimSpace(work.State) {
			return nil, nil, fmt.Errorf("operational archive work %s status %q does not match work state %q", location.Archive.ID, location.Archive.Status, work.State)
		}
		if prior, ok := seen[work.ID]; ok {
			return nil, nil, fmt.Errorf("duplicate work identity %s at %s and %s", work.ID, prior, location.Path)
		}
		seen[work.ID] = location.Path
		out = append(out, workSnapshot{Work: work, Raw: bytes.TrimSpace(raw)})
	}
	return out, excludedLegacy, nil
}

// reconcileRecentTerminalSnapshots walks the recent-terminal cohort and
// verifies custody. It returns the excluded-records count, the set of work ids
// skipped for a custody-seal mismatch (work id -> reason; the mismatch is
// reported per-envelope on the artifact and the envelope can never become a
// sample), and a fatal error for every other custody violation, which still
// fails the compilation closed.
func reconcileRecentTerminalSnapshots(homePath string, works []workSnapshot, receipts map[string]receiptEvidence, excludedLegacy map[string]bool, cohortClass string, lookbackStart, lookbackEnd time.Time) (int, map[string]string, error) {
	sealSkipped := map[string]string{}
	byID := map[string][]workSnapshot{}
	for _, work := range works {
		byID[work.Work.ID] = append(byID[work.Work.ID], work)
	}
	for _, work := range works {
		class, classErr := autonomyClassOf(work.Work)
		if classErr != nil {
			if terminalWorkState(work.Work.State) && autonomyTriggerMentionsClass(work.Work, cohortClass) {
				return 0, sealSkipped, classErr
			}
			continue
		}
		if class != cohortClass || !terminalWorkState(work.Work.State) {
			continue
		}
		if len(receipts[work.Work.ID].Terminals) == 0 {
			return 0, sealSkipped, fmt.Errorf("terminal work %s has 0 terminal events; exactly one required", work.Work.ID)
		}
	}
	excluded := 0
	for workID, evidence := range receipts {
		if len(evidence.Terminals) == 0 || excludedLegacy[workID] {
			// An ephemeral work_live_ record has no durable envelope to reconcile
			// against; loadWorks already excluded it, so its events carry no
			// eligibility weight either.
			continue
		}
		recent := false
		terminalTimes := make([]time.Time, 0, len(evidence.Terminals))
		for _, terminal := range evidence.Terminals {
			parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(terminal.At))
			if err != nil {
				return excluded, sealSkipped, fmt.Errorf("terminal work %s has invalid terminal timestamp %q", workID, terminal.At)
			}
			terminalTimes = append(terminalTimes, parsed)
			if !parsed.Before(lookbackStart) {
				recent = true
			}
		}
		if !recent {
			continue
		}
		snapshots := byID[workID]
		// Custody strictness is class-scoped, but exclusion is only safe when the
		// work's class attribution is trustworthy: exactly one snapshot whose
		// recorded hash (when any terminal event carries one) verifies. A missing
		// snapshot or an ambiguous snapshot set leaves the class unknowable and
		// fails closed. A recorded-hash MISMATCH is the one settled exception:
		// the envelope was mutated after its terminal seal (the legacy
		// unsealed-sweep shape), the id is reported and the envelope skipped —
		// one stale envelope must not veto every other cohort's pass. OFF-class
		// debris with close-retry duplicates or pre-hash-era events can never be
		// a sample for this cohort and is excluded visibly (counted on the
		// artifact) instead of vetoing the class.
		verifiedOffClass := false
		if len(snapshots) == 1 {
			data, err := json.Marshal(snapshots[0].Work)
			if err != nil {
				return excluded, sealSkipped, err
			}
			snapHash := "sha256:" + shaHex(data)
			sealed := strings.TrimSpace(evidence.LatestWorkSHA256)
			if sealed != "" && sealed != snapHash {
				excluded++
				sealSkipped[workID] = fmt.Sprintf("snapshot hash does not match terminal event (sealed %s, envelope %s)", sealed, snapHash)
				continue
			}
			class, classErr := autonomyClassOf(snapshots[0].Work)
			if classErr != nil {
				verifiedOffClass = !autonomyTriggerMentionsClass(snapshots[0].Work, cohortClass)
			} else {
				verifiedOffClass = class != cohortClass
			}
		}
		if len(evidence.Terminals) != 1 {
			if verifiedOffClass {
				excluded++
				continue
			}
			return excluded, sealSkipped, fmt.Errorf("recent terminal work %s has multiple terminal events (%d)", workID, len(evidence.Terminals))
		}
		if len(snapshots) != 1 {
			return excluded, sealSkipped, fmt.Errorf("recent terminal work %s has %d snapshots; exactly one required", workID, len(snapshots))
		}
		if !terminalWorkState(snapshots[0].Work.State) {
			if verifiedOffClass {
				excluded++
				continue
			}
			return excluded, sealSkipped, fmt.Errorf("recent terminal work %s snapshot state %q is not terminal", workID, snapshots[0].Work.State)
		}
		if strings.TrimSpace(evidence.LatestWorkSHA256) == "" {
			if verifiedOffClass {
				excluded++
				continue
			}
			return excluded, sealSkipped, fmt.Errorf("recent terminal work %s terminal event snapshot hash is required", workID)
		}
		data, err := json.Marshal(snapshots[0].Work)
		if err != nil {
			return excluded, sealSkipped, err
		}
		if strings.TrimSpace(evidence.LatestWorkSHA256) != "sha256:"+shaHex(data) {
			// Defense in depth: the early single-snapshot branch already
			// skipped this shape, so this arm is unreachable while that
			// branch stands. If it ever fires (branch drift), the same
			// per-envelope skip applies rather than failing the cohort.
			excluded++
			sealSkipped[workID] = fmt.Sprintf("snapshot hash does not match terminal event (sealed %s, envelope %s)", strings.TrimSpace(evidence.LatestWorkSHA256), "sha256:"+shaHex(data))
			continue
		}
		class, classErr := autonomyClassOf(snapshots[0].Work)
		if classErr != nil {
			if autonomyTriggerMentionsClass(snapshots[0].Work, cohortClass) {
				return excluded, sealSkipped, classErr
			}
			continue
		}
		if class != cohortClass {
			continue
		}
		if strings.TrimSpace(evidence.Terminals[0].ReceiptSHA256) == "" {
			return excluded, sealSkipped, fmt.Errorf("terminal work %s receipt snapshot hash is required", workID)
		}
		if len(evidence.Refs) != 1 {
			return excluded, sealSkipped, fmt.Errorf("terminal work %s has %d receipt refs; exactly one required", workID, len(evidence.Refs))
		}
		_, receipt, _, err := loadBoundReceipt(homePath, evidence.Refs[0])
		if err != nil {
			return excluded, sealSkipped, fmt.Errorf("terminal work %s %w", workID, err)
		}
		receiptCanonical, err := json.Marshal(receipt)
		if err != nil {
			return excluded, sealSkipped, err
		}
		if evidence.Terminals[0].ReceiptSHA256 != "sha256:"+shaHex(receiptCanonical) {
			return excluded, sealSkipped, fmt.Errorf("terminal work %s receipt snapshot hash does not match terminal event", workID)
		}
		if terminalTimes[0].After(lookbackEnd) {
			return excluded, sealSkipped, fmt.Errorf("terminal work %s has terminal timestamp after eligibility evaluation: %q is after lookback end %q", workID, evidence.Terminals[0].At, lookbackEnd.UTC().Format(time.RFC3339))
		}
	}
	return excluded, sealSkipped, nil
}
func receiptEvidenceByWork(homePath string) (map[string]receiptEvidence, error) {
	out := map[string]receiptEvidence{}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return nil, err
	}
	raw, err := store.ReadWorkLogRegular("work event ledger")
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	scanner := strictJSONLScanner(raw, "work event ledger")
	for scanner.Scan() {
		var event struct {
			TS            string   `json:"ts"`
			WorkID        string   `json:"work_id"`
			From          string   `json:"from"`
			To            string   `json:"to"`
			Attached      []string `json:"attached"`
			WorkSHA256    string   `json:"work_sha256,omitempty"`
			ReceiptSHA256 string   `json:"receipt_sha256,omitempty"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		evidence := out[event.WorkID]
		// The custody seal is the MOST RECENT recorded snapshot hash for this work,
		// not the first close's. A governed re-close (audit_only) or any other
		// sealed post-close event rewrites the record and records the new hash;
		// verifying against the first close would read that legitimate re-seal as
		// tampering. Any change with NO sealing event still mismatches.
		if h := strings.TrimSpace(event.WorkSHA256); h != "" {
			evidence.LatestWorkSHA256 = h
		}
		// A terminal event is a TRANSITION INTO a terminal state. Same-state rows
		// (from == to) are annotations on already-closed work -- delegate steer and
		// exit notes write them -- and carry neither a close's snapshot hash nor a
		// receipt. Counting them as closes made one steered work look like it had
		// several conflicting terminal events and no custody hash, which failed the
		// whole eligibility compilation closed.
		terminal := terminalWorkState(event.To) && strings.TrimSpace(event.From) != strings.TrimSpace(event.To)
		if terminal && strings.TrimSpace(event.TS) != "" {
			evidence.Terminals = append(evidence.Terminals, terminalEvidence{
				At:            normalizedEvidenceTime(event.TS),
				WorkSHA256:    strings.TrimSpace(event.WorkSHA256),
				ReceiptSHA256: strings.TrimSpace(event.ReceiptSHA256),
			})
		}
		if terminal {
			for _, ref := range event.Attached {
				if strings.HasPrefix(ref, "rec_") {
					evidence.Refs = append(evidence.Refs, ref)
				}
			}
		}
		out[event.WorkID] = evidence
	}
	return out, scanner.Err()
}

func loadBoundReceipt(homePath, ref string) (string, envelope.Receipt, []byte, error) {
	receiptID, reference, err := terminalReceiptReference(ref)
	if err != nil {
		return "", envelope.Receipt{}, nil, err
	}
	store, err := eventstore.Open(homePath)
	if err != nil {
		return "", envelope.Receipt{}, nil, err
	}
	record, err := store.ReadRegular(reference.String(), "receipt")
	if err != nil {
		return "", envelope.Receipt{}, nil, fmt.Errorf("receipt unreadable: %w", err)
	}
	raw := record.Bytes
	var receipt envelope.Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return "", envelope.Receipt{}, nil, fmt.Errorf("receipt invalid: %w", err)
	}
	if err := receipt.Validate(); err != nil {
		return "", envelope.Receipt{}, nil, fmt.Errorf("receipt invalid: %w", err)
	}
	if receipt.ID != receiptID {
		return "", envelope.Receipt{}, nil, fmt.Errorf("receipt identity does not match ref: decoded %q, ref %q", receipt.ID, receiptID)
	}
	return receiptID, receipt, raw, nil
}

func terminalReceiptReference(ref string) (string, eventstore.Reference, error) {
	receiptID := strings.TrimSpace(ref)
	if receiptID == "" ||
		receiptID != ref ||
		!strings.HasPrefix(receiptID, "rec_") ||
		receiptID == "." ||
		receiptID == ".." ||
		strings.ContainsAny(receiptID, `/\`) ||
		!filepath.IsLocal(receiptID) ||
		filepath.Clean(receiptID) != receiptID ||
		filepath.Base(receiptID) != receiptID {
		return "", eventstore.Reference{}, fmt.Errorf("receipt ref must be a local basename: %q", ref)
	}
	reference, err := eventstore.ReferenceForID(receiptID)
	if err != nil {
		return "", eventstore.Reference{}, fmt.Errorf("receipt ref must be a local basename: %q", ref)
	}
	return receiptID, reference, nil
}

func canonicalLocalSlashPath(raw string) (string, bool) {
	if raw == "" || raw != strings.TrimSpace(raw) || strings.Contains(raw, `\`) {
		return "", false
	}
	local := filepath.FromSlash(raw)
	if !filepath.IsLocal(local) || filepath.Clean(local) != local || filepath.ToSlash(local) != raw {
		return "", false
	}
	return local, true
}

func readRegularFile(homePath, path, kind string) ([]byte, error) {
	return homepkg.ReadRootedRegularFile(homePath, path, kind)
}

func strictJSONLScanner(raw []byte, ledger string) *bufio.Scanner {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	scanner.Split(splitStrictJSONLLines(ledger))
	return scanner
}

func splitStrictJSONLLines(ledger string) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			end := index
			if end > 0 && data[end-1] == '\r' {
				end--
			}
			return index + 1, data[:end], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, fmt.Errorf("%s must end with newline", ledger)
		}
		return 0, nil, nil
	}
}

func normalizedEvidenceTime(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC().Format(time.RFC3339)
	}
	if nanos, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(0, nanos).UTC().Format(time.RFC3339)
	}
	return value
}

func loadRouteRows(homePath string) (map[string][]routeRaw, error) {
	out := map[string][]routeRaw{}
	path := filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl")
	raw, err := readRegularFile(homePath, path, "route evidence ledger")
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	scanner := strictJSONLScanner(raw, "route evidence ledger")
	for scanner.Scan() {
		raw := bytes.TrimSpace(append([]byte(nil), scanner.Bytes()...))
		if len(raw) == 0 {
			continue
		}
		var row routeRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		if strings.TrimSpace(row.ID) == "" {
			return nil, fmt.Errorf("route evidence row missing id")
		}
		out[row.ID] = append(out[row.ID], routeRaw{Row: row, Raw: raw})
	}
	return out, scanner.Err()
}

func decidingRouteBindingFor(autonomyClass, capabilityClass string) (decidingRouteBinding, bool) {
	// Evidence-threshold autonomy is intentionally deny-by-default. Each newly
	// enabled autonomy/capability pair must name the exact production caller
	// that can supply its deciding route row; a model match alone is not a
	// deciding-call identity.
	if strings.TrimSpace(autonomyClass) == "job_health" && strings.TrimSpace(capabilityClass) == "judge-critique" {
		return decidingRouteBinding{
			ClientProfile: "compound.panel.judge",
			Protocol:      "panel-lane",
			Provider:      "compound-route-plan",
		}, true
	}
	return decidingRouteBinding{}, false
}

func receiptHasGenuineDelta(receipt envelope.Receipt, routes map[string][]routeRaw) bool {
	if receipt.Outcome != "completed" {
		return false
	}
	for _, path := range receipt.StateDelta.FilesChanged {
		if strings.TrimSpace(path) != "" {
			return true
		}
	}
	refs := append([]string{}, receipt.ProofRefs...)
	refs = append(refs, receipt.StateDelta.Artifacts...)
	refs = append(refs, receipt.StateDelta.Verification...)
	refs = nonRoutingRefs(refs, routes)
	switch receipt.MoveType {
	case "memory_delta":
		memoryRefs := append([]string{}, receipt.Memory.Candidates...)
		memoryRefs = append(memoryRefs, receipt.Memory.Promoted...)
		memoryRefs = append(memoryRefs, receipt.Memory.Parked...)
		memoryRefs = append(memoryRefs, receipt.Memory.Rejected...)
		return len(nonEmptyStrings(memoryRefs)) > 0
	case "rule_delta":
		return hasAnyPrefix(refs, []string{"failure:", "failure_pattern:", "rule:", "check:", "guard:"})
	case "control_plane_repair", "outward_delta":
		return len(refs) > 0
	default:
		return false
	}
}

func nonRoutingRefs(refs []string, routes map[string][]routeRaw) []string {
	out := []string{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || isRoutingOnlyRef(ref, routes) {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func isRoutingOnlyRef(ref string, routes map[string][]routeRaw) bool {
	if len(routes[ref]) > 0 {
		return true
	}
	for _, prefix := range []string{
		"route_",
		"route:",
		"route_plan:",
		"router_revision:",
		"compound://ledger/execution-route-evidence",
		"compound://registries/model-router",
	} {
		if strings.HasPrefix(ref, prefix) {
			return true
		}
	}
	return false
}

func nonEmptyStrings(values []string) []string {
	out := []string{}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func hasAnyPrefix(values, prefixes []string) bool {
	for _, value := range values {
		for _, prefix := range prefixes {
			if strings.HasPrefix(value, prefix) {
				return true
			}
		}
	}
	return false
}

func countRouteRequestID(routes map[string][]routeRaw, requestID string) int {
	requestID = strings.TrimSpace(requestID)
	count := 0
	for _, rows := range routes {
		for _, row := range rows {
			if strings.TrimSpace(row.Row.RequestID) == requestID {
				count++
			}
		}
	}
	return count
}

func routeTimeBindingReason(workCreatedAt, receiptCreatedAt, routeCreatedAt string) string {
	workTime, err := time.Parse(time.RFC3339, strings.TrimSpace(workCreatedAt))
	if err != nil {
		return "work created_at is invalid for route binding"
	}
	receiptTime, err := time.Parse(time.RFC3339, strings.TrimSpace(receiptCreatedAt))
	if err != nil {
		return "receipt created_at is invalid for route binding"
	}
	routeTime, err := time.Parse(time.RFC3339, strings.TrimSpace(routeCreatedAt))
	if err != nil {
		return "route evidence created_at is invalid"
	}
	if routeTime.Before(workTime) {
		return "route evidence predates its work"
	}
	if routeTime.After(receiptTime) {
		return "route evidence postdates its receipt"
	}
	return ""
}

func baseEvidenceRefs(grant GrantSnapshot, plan RoutePlanSnapshot) []string {
	return uniqueSorted([]string{
		grant.GrantRef,
		grant.ClaimRef,
		terminalSampleLookbackRef(strings.TrimSpace(grant.Class)),
		"route_plan:" + plan.ID,
		"router_revision:" + plan.SourceRevision,
		fmt.Sprintf("threshold:min_terminal_samples=%d", grant.MinTerminalSamples),
		fmt.Sprintf("threshold:min_window_days=%d", grant.MinWindowDays),
		fmt.Sprintf("threshold:max_failures=%d", grant.MaxFailures),
	})
}

func terminalSamplePredates(sample Sample, cutoff time.Time) bool {
	terminalAt, err := time.Parse(time.RFC3339, strings.TrimSpace(sample.ReceiptCreatedAt))
	return err == nil && terminalAt.Before(cutoff)
}

func terminalSampleLookbackRef(class string) string {
	return fmt.Sprintf("cohort:terminal_samples:class=%s:lookback=%dd", strings.TrimSpace(class), terminalSampleLookbackDays)
}

func contentID(artifact Artifact) string {
	artifact.ID = ""
	data, _ := json.Marshal(artifact)
	return "autelig_" + shaHex(data)[:12]
}

func idFor(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}

func shaHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func sameLanes(a, b []LaneSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ModelID != b[i].ModelID || a[i].ProviderRoute != b[i].ProviderRoute || a[i].ProviderRouteKind != b[i].ProviderRouteKind || a[i].Tier != b[i].Tier || a[i].ClaimLevel != b[i].ClaimLevel || a[i].GateRole != b[i].GateRole || !sameLaneResolution(a[i].Resolution, b[i].Resolution) {
			return false
		}
	}
	return true
}

func sameLaneResolution(a, b LaneResolution) bool {
	if a.Mode != b.Mode || len(a.Targets) != len(b.Targets) {
		return false
	}
	for i := range a.Targets {
		if a.Targets[i] != b.Targets[i] {
			return false
		}
	}
	return true
}

func lanesForModel(lanes []LaneSnapshot, modelID string) []LaneSnapshot {
	var out []LaneSnapshot
	for _, lane := range lanes {
		if lane.ModelID == modelID {
			out = append(out, lane)
		}
	}
	return out
}

func lanesForResolvedTarget(lanes []LaneSnapshot, provider, model string) []LaneSnapshot {
	var out []LaneSnapshot
	for _, lane := range lanes {
		if laneDeclaresResolvedTarget(lane, provider, model) {
			out = append(out, lane)
		}
	}
	return out
}

func laneDeclaresResolvedTarget(lane LaneSnapshot, provider, model string) bool {
	if strings.TrimSpace(lane.Resolution.Mode) != "direct" {
		return false
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	for _, target := range lane.Resolution.Targets {
		if strings.TrimSpace(target.Provider) == provider && strings.TrimSpace(target.Model) == model {
			return true
		}
	}
	return false
}

func invalidLaneResolutionReason(resolution LaneResolution) string {
	switch strings.TrimSpace(resolution.Mode) {
	case "adaptive":
		if len(resolution.Targets) != 0 {
			return "adaptive resolution must not declare concrete targets"
		}
	case "direct":
		if len(resolution.Targets) == 0 {
			return "direct resolution requires at least one target"
		}
		seen := map[string]bool{}
		for _, target := range resolution.Targets {
			provider := strings.TrimSpace(target.Provider)
			model := strings.TrimSpace(target.Model)
			if provider == "" || model == "" {
				return "direct resolution target requires provider and model"
			}
			key := provider + "\x00" + model
			if seen[key] {
				return "direct resolution targets must be unique"
			}
			seen[key] = true
		}
	default:
		return "has unknown resolution mode"
	}
	return ""
}

func autonomyClassOf(work envelope.WorkEnvelope) (string, error) {
	var triggers []string
	for _, ref := range work.EvidenceRefs {
		ref = strings.TrimSpace(ref)
		if !strings.HasPrefix(ref, "director_trigger:") {
			continue
		}
		triggers = append(triggers, ref)
	}
	if len(triggers) == 0 {
		return "", nil
	}
	if len(triggers) != 1 {
		return "", fmt.Errorf("terminal work %s has ambiguous director_trigger identity (%d refs)", work.ID, len(triggers))
	}
	rest := strings.TrimPrefix(triggers[0], "director_trigger:")
	index := strings.Index(rest, ":")
	if index <= 0 || index == len(rest)-1 {
		return "", fmt.Errorf("terminal work %s has invalid director_trigger %q", work.ID, triggers[0])
	}
	return rest[:index], nil
}

func autonomyTriggerMentionsClass(work envelope.WorkEnvelope, class string) bool {
	prefix := "director_trigger:" + strings.TrimSpace(class) + ":"
	for _, ref := range work.EvidenceRefs {
		if strings.HasPrefix(strings.TrimSpace(ref), prefix) {
			return true
		}
	}
	return false
}

func terminalWorkState(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled"
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func uniqueSorted(values []string) []string {
	set := map[string]bool{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = true
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
