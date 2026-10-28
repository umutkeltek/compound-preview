package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/routeevidence"
)

const capabilityResolutionSchema = "compound.capability_resolution.v0.1"

const (
	capabilityResolutionOfferLimit      = 12
	capabilityResolutionRouteLaneLimit  = 16
	capabilityResolutionPolicyOmitLimit = 16
	capabilityResolutionWarningLimit    = 16
	capabilityResolutionMaxJSONBytes    = 1 << 20
)

type capabilityResolutionLimits struct {
	OfferLimit                   int  `json:"offer_limit"`
	RouteLaneLimit               int  `json:"route_lane_limit"`
	PolicyOmittedLaneLimit       int  `json:"policy_omitted_lane_limit"`
	WarningLimit                 int  `json:"warning_limit"`
	MaxJSONBytes                 int  `json:"max_json_bytes"`
	PendingObservationBodiesRead bool `json:"pending_observation_bodies_read"`
}

type boundedCapabilityRoutePlan struct {
	routeevidence.CapabilityRoutePlan
	RouteLaneLimit         int `json:"route_lane_limit"`
	OmittedRouteLaneCount  int `json:"omitted_route_lane_count,omitempty"`
	PolicyOmittedLaneLimit int `json:"policy_omitted_lane_limit"`
	HiddenPolicyOmitCount  int `json:"hidden_policy_omitted_lane_count,omitempty"`
	WarningLimit           int `json:"warning_limit"`
	OmittedWarningCount    int `json:"omitted_warning_count,omitempty"`
}

type capabilityResolutionReport struct {
	SchemaVersion     string                           `json:"schema_version"`
	Status            string                           `json:"status"`
	Task              string                           `json:"task"`
	Harness           string                           `json:"harness,omitempty"`
	PrivacySensitive  bool                             `json:"privacy_sensitive"`
	Classification    routeevidence.TaskClassification `json:"classification"`
	CapabilitySurface capgraph.SurfaceReport           `json:"capability_surface"`
	RoutePlan         boundedCapabilityRoutePlan       `json:"route_plan"`
	Limits            capabilityResolutionLimits       `json:"limits"`
	AuthorityRefs     []string                         `json:"authority_refs"`
	selectionChain    []routeevidence.CapabilityRouteLane
}

func buildCapabilityResolutionReport(store *capgraph.Store, homePath, task, harness string, privacySensitive, suppressRefusalEvidence bool, now time.Time) (capabilityResolutionReport, error) {
	classification := routeevidence.ClassifyCapabilityTask(task)
	surface, err := buildCapabilitySurfaceReport(store, homePath, task, harness)
	if err != nil {
		return capabilityResolutionReport{}, err
	}
	if surface.NodeCount == 0 {
		return capabilityResolutionReport{}, fmt.Errorf("capability graph authority has no nodes")
	}
	if !validGraphAuthorityRevision(surface.AuthorityRevision) {
		return capabilityResolutionReport{}, fmt.Errorf("capability graph authority has no bound revision")
	}
	surface = surface.LimitOffers(capabilityResolutionOfferLimit)
	plan, err := routeevidence.PlanCapabilityRoute(homePath, classification.CapabilityClass, routeevidence.CapabilityRoutePlanOptions{
		PrivacySensitive:            privacySensitive,
		SuppressPendingObservations: true,
		SuppressRefusalEvidence:     suppressRefusalEvidence,
	}, now)
	if err != nil {
		return capabilityResolutionReport{}, err
	}
	if strings.TrimSpace(plan.SourceAuthorityID) == "" || !validSHA256Revision(plan.SourceRevision) {
		return capabilityResolutionReport{}, fmt.Errorf("model-router authority has no source provenance")
	}
	if plan.Status != "planned" || len(plan.Chain) == 0 {
		return capabilityResolutionReport{}, fmt.Errorf("model-router authority returned no eligible lane for capability class %q", classification.CapabilityClass)
	}
	report := capabilityResolutionReport{
		SchemaVersion:     capabilityResolutionSchema,
		Status:            "ok",
		Task:              surface.Task,
		Harness:           surface.Harness,
		PrivacySensitive:  privacySensitive,
		Classification:    classification,
		CapabilitySurface: surface,
		RoutePlan:         boundCapabilityRoutePlan(plan),
		selectionChain:    append([]routeevidence.CapabilityRouteLane(nil), plan.Chain...),
		Limits: capabilityResolutionLimits{
			OfferLimit:                   capabilityResolutionOfferLimit,
			RouteLaneLimit:               capabilityResolutionRouteLaneLimit,
			PolicyOmittedLaneLimit:       capabilityResolutionPolicyOmitLimit,
			WarningLimit:                 capabilityResolutionWarningLimit,
			MaxJSONBytes:                 capabilityResolutionMaxJSONBytes,
			PendingObservationBodiesRead: false,
		},
		AuthorityRefs: []string{
			routeevidence.CapabilityTaskClassifierAuthorityRef,
			capgraph.CapabilityGraphAuthorityRef,
			routeevidence.ModelCapabilitiesAuthorityRef,
		},
	}
	if err := ensureCapabilityResolutionBound(report); err != nil {
		return capabilityResolutionReport{}, err
	}
	return report, nil
}

func boundCapabilityRoutePlan(plan routeevidence.CapabilityRoutePlan) boundedCapabilityRoutePlan {
	bounded := boundedCapabilityRoutePlan{
		CapabilityRoutePlan:    plan,
		RouteLaneLimit:         capabilityResolutionRouteLaneLimit,
		PolicyOmittedLaneLimit: capabilityResolutionPolicyOmitLimit,
		WarningLimit:           capabilityResolutionWarningLimit,
	}
	bounded.PendingObservations = nil
	if len(bounded.Chain) > capabilityResolutionRouteLaneLimit {
		bounded.OmittedRouteLaneCount = len(bounded.Chain) - capabilityResolutionRouteLaneLimit
		bounded.Chain = append([]routeevidence.CapabilityRouteLane{}, bounded.Chain[:capabilityResolutionRouteLaneLimit]...)
	}
	if len(bounded.LaneJustification) > capabilityResolutionRouteLaneLimit {
		bounded.LaneJustification = append([]string{}, bounded.LaneJustification[:capabilityResolutionRouteLaneLimit]...)
	}
	if len(bounded.Omitted) > capabilityResolutionPolicyOmitLimit {
		bounded.HiddenPolicyOmitCount = len(bounded.Omitted) - capabilityResolutionPolicyOmitLimit
		bounded.Omitted = append([]routeevidence.CapabilityRouteLane{}, bounded.Omitted[:capabilityResolutionPolicyOmitLimit]...)
	}
	if len(bounded.Warnings) > capabilityResolutionWarningLimit {
		bounded.OmittedWarningCount = len(bounded.Warnings) - capabilityResolutionWarningLimit
		bounded.Warnings = append([]string{}, bounded.Warnings[:capabilityResolutionWarningLimit]...)
	}
	return bounded
}

func ensureCapabilityResolutionBound(report capabilityResolutionReport) error {
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode capability resolution: %w", err)
	}
	// json.Encoder.Encode appends one newline to the validated payload.
	if len(data)+1 > capabilityResolutionMaxJSONBytes {
		return fmt.Errorf("capability resolution exceeds %d-byte response limit", capabilityResolutionMaxJSONBytes)
	}
	return nil
}

func validSHA256Revision(revision string) bool {
	if !strings.HasPrefix(revision, "sha256:") || len(revision) != len("sha256:")+64 {
		return false
	}
	for _, c := range revision[len("sha256:"):] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func validGraphAuthorityRevision(revision string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(revision, prefix) || len(revision) != len(prefix)+12 {
		return false
	}
	for _, c := range revision[len(prefix):] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func printCapabilityResolution(report capabilityResolutionReport) {
	fmt.Printf("# capability resolution for %q (offered, NOT executed)\n", report.Task)
	fmt.Printf("- status: %s\n", report.Status)
	fmt.Printf("- capability_class: %s\n", report.Classification.CapabilityClass)
	fmt.Printf("- classification_authority: %s revision=%s\n", report.Classification.AuthorityRef, report.Classification.AuthorityRevision)
	fmt.Printf("- classification_rule: %s\n", report.Classification.RuleID)
	fmt.Printf("- deterministic_first: %s\n", report.Classification.DeterministicFirst)
	fmt.Printf("- evaluation_method: %s\n", report.Classification.EvaluationMethod)
	fmt.Printf("- capability_authority: %s\n", report.CapabilitySurface.AuthorityRef)
	fmt.Printf("- model_router_authority: %s source=%s revision=%s\n", report.RoutePlan.AuthorityRef, report.RoutePlan.SourceAuthorityID, report.RoutePlan.SourceRevision)
	fmt.Printf("- offers: %d shown=%d omitted=%d\n", report.CapabilitySurface.OfferCount, len(report.CapabilitySurface.Offers), report.CapabilitySurface.OmittedOfferCount)
	for _, offer := range report.CapabilitySurface.Offers {
		fmt.Printf("  - %s kind=%s score=%.2f reason=%s\n", offer.Node.ID, offer.Node.Kind, offer.Score, offer.Why)
	}
	fmt.Printf("- route_lanes: %d\n", len(report.RoutePlan.Chain))
	for _, lane := range report.RoutePlan.Chain {
		fmt.Printf("  - %d %s tier=%s role=%s claim=%s\n", lane.Position, lane.ModelID, lane.Tier, lane.GateRole, lane.ClaimLevel)
	}
	fmt.Println("- verdict: projection_only_not_task_success")
}
