package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"compound/internal/routeevidence"
	"compound/internal/work"
)

// The lane owner already publishes where a harness keeps its session store:
// `sb profile plan <profile> --json` returns a `harness_settings` artifact, and
// its parent directory is the profile home containing `projects/`. Compound
// consumes that instead of keeping its own copy of the path.
//
// Copying it here would recreate the exact defect this fixes. The lane doctor
// already reports one path while the launcher uses another, and the harvester
// trusted the reported one and found nothing. A second hand-maintained copy in
// Compound would drift the same way. Ask the owner, or admit you do not know.
//
// The candidate enumeration and the owner consultation live in internal/work
// (delegate_session_store.go); this file only adapts the owner's process
// output to that seam.
const (
	laneLaunchProfilePlanSchema     = "switchback/launch-profile-plan@1"
	laneLaunchProfileOwner          = "switchback"
	laneLaunchProfileCompoundRole   = "consumer"
	laneLaunchProfileProjection     = "non_secret_profile_conformance"
	laneLaunchProfileTransportID    = "switchback"
	laneLaunchProfileCaptureMode    = "segmented_full_wire"
	laneLaunchProfilePlanMaxBytes   = 1 << 20
	delegateLaunchProfilePlanSchema = "compound.delegate_launch_profile_plan.v0.2"
)

type lanePlanArtifact struct {
	Kind          string  `json:"kind"`
	Path          string  `json:"path"`
	Exists        bool    `json:"exists"`
	Changed       bool    `json:"changed"`
	CurrentSHA256 *string `json:"current_sha256"`
	DesiredSHA256 string  `json:"desired_sha256"`
	ModeMatches   bool    `json:"mode_matches"`
}

type lanePlanAuthority struct {
	Owner        string `json:"owner"`
	CompoundRole string `json:"compound_role"`
	Projection   string `json:"projection"`
}

type lanePlanCapture struct {
	Mode string `json:"mode"`
}

type lanePlanProfile struct {
	ID              string          `json:"id"`
	ProviderLane    string          `json:"provider_lane"`
	ClientProfile   string          `json:"client_profile"`
	Harness         string          `json:"harness"`
	Route           string          `json:"route"`
	RequestedModel  string          `json:"requested_model"`
	RequestedEffort string          `json:"requested_effort"`
	Transport       string          `json:"transport"`
	Capture         lanePlanCapture `json:"capture"`
	Wrappers        []string        `json:"wrappers"`
}

type lanePlan struct {
	Schema            string             `json:"schema"`
	Authority         lanePlanAuthority  `json:"authority"`
	AuthorityRevision string             `json:"authority_revision"`
	Revision          string             `json:"revision"`
	Profile           lanePlanProfile    `json:"profile"`
	Changed           bool               `json:"changed"`
	Artifacts         []lanePlanArtifact `json:"artifacts"`
}

// laneProfilePlanArtifacts adapts the lane owner's `sb profile plan --json`
// output to internal/work's plan-lookup seam (artifact kind -> path). A
// profile the owner cannot plan is an error, exactly like the owner refusing
// on the command line.
func laneProfilePlanArtifacts(profile string) (map[string]string, error) {
	out, err := laneProfilePlanOutput(profile)
	if err != nil {
		return nil, err
	}
	var plan lanePlan
	if err := json.Unmarshal(out, &plan); err != nil {
		return nil, err
	}
	artifacts := make(map[string]string, len(plan.Artifacts))
	for _, a := range plan.Artifacts {
		if p := strings.TrimSpace(a.Path); p != "" {
			artifacts[a.Kind] = p
		}
	}
	return artifacts, nil
}

func laneProfilePlanOutput(profile string) ([]byte, error) {
	bin, err := exec.LookPath("sb")
	if err != nil {
		return nil, fmt.Errorf("lane profile owner (sb) not on PATH")
	}
	out, err := exec.Command(bin, "profile", "plan", profile, "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("lane profile owner could not plan profile %q", profile)
	}
	return out, nil
}

func resolveDelegateLaunchProfile(req work.DelegateLaunchProfileResolveRequest) (*work.DelegateLaunchProfileResolution, error) {
	profileID := strings.TrimSpace(req.ProfileID)
	if profileID == "" {
		return nil, fmt.Errorf("launch profile id is required")
	}
	raw, err := laneProfilePlanOutput(profileID)
	if err != nil {
		return nil, err
	}
	return normalizeDelegateLaunchProfilePlan(raw, req)
}

func normalizeDelegateLaunchProfilePlan(
	raw []byte,
	req work.DelegateLaunchProfileResolveRequest,
) (*work.DelegateLaunchProfileResolution, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("launch profile owner returned an empty plan")
	}
	if len(raw) > laneLaunchProfilePlanMaxBytes {
		return nil, fmt.Errorf("launch profile owner plan exceeds %d bytes", laneLaunchProfilePlanMaxBytes)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("launch profile owner returned invalid JSON")
	}

	var source lanePlan
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("decode launch profile owner plan: %w", err)
	}
	if source.Schema != laneLaunchProfilePlanSchema {
		return nil, fmt.Errorf("launch profile owner schema = %q, want %q", source.Schema, laneLaunchProfilePlanSchema)
	}
	if source.Authority.Owner != laneLaunchProfileOwner ||
		source.Authority.CompoundRole != laneLaunchProfileCompoundRole ||
		source.Authority.Projection != laneLaunchProfileProjection {
		return nil, fmt.Errorf("launch profile authority projection is not the Switchback consumer contract")
	}
	if !validLaneSHA256Revision(source.AuthorityRevision) {
		return nil, fmt.Errorf("launch profile authority_revision is not a sha256 revision")
	}
	if !validLaneSHA256Revision(source.Revision) {
		return nil, fmt.Errorf("launch profile revision is not a sha256 revision")
	}
	if source.Changed {
		return nil, fmt.Errorf("launch profile generated artifacts have drift")
	}
	if len(source.Artifacts) == 0 {
		return nil, fmt.Errorf("launch profile owner plan has no generated artifacts")
	}
	for _, artifact := range source.Artifacts {
		current := ""
		if artifact.CurrentSHA256 != nil {
			current = strings.TrimSpace(*artifact.CurrentSHA256)
		}
		if strings.TrimSpace(artifact.Kind) == "" ||
			!artifact.Exists ||
			artifact.Changed ||
			!artifact.ModeMatches ||
			!validLaneSHA256Revision(current) ||
			!validLaneSHA256Revision(artifact.DesiredSHA256) ||
			current != strings.TrimSpace(artifact.DesiredSHA256) {
			return nil, fmt.Errorf("launch profile artifact %q is missing or drifted", artifact.Kind)
		}
	}

	profile := source.Profile
	for name, value := range map[string]string{
		"id":               profile.ID,
		"provider_lane":    profile.ProviderLane,
		"harness":          profile.Harness,
		"route":            profile.Route,
		"requested_model":  profile.RequestedModel,
		"requested_effort": profile.RequestedEffort,
		"transport":        profile.Transport,
		"client_profile":   profile.ClientProfile,
		"capture_mode":     profile.Capture.Mode,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("launch profile %s is required", name)
		}
		if strings.TrimSpace(value) != value {
			return nil, fmt.Errorf("launch profile %s is not canonical", name)
		}
	}
	if profile.ID != strings.TrimSpace(req.ProfileID) {
		return nil, fmt.Errorf("launch profile owner returned id %q for requested id %q", profile.ID, req.ProfileID)
	}
	if !validLaneOpaqueID(profile.ID) ||
		!validLaneOpaqueID(profile.ProviderLane) ||
		!validLaneOpaqueID(profile.ClientProfile) {
		return nil, fmt.Errorf("launch profile or client_profile identity is not opaque")
	}
	if !oneOfLaneValue(profile.Transport, "gateway", "tap", "headroom") {
		return nil, fmt.Errorf("launch profile transport %q is not a supported Switchback transport", profile.Transport)
	}
	if profile.Capture.Mode != laneLaunchProfileCaptureMode {
		return nil, fmt.Errorf(
			"launch profile capture_mode = %q, want %q",
			profile.Capture.Mode,
			laneLaunchProfileCaptureMode,
		)
	}
	selectedExecutableID := filepath.Base(strings.TrimSpace(req.SelectedCommand))
	if selectedExecutableID == "" || selectedExecutableID == "." {
		return nil, fmt.Errorf("selected launch command is required")
	}
	wrapperArtifacts := make(map[string]string)
	for _, artifact := range source.Artifacts {
		if artifact.Kind != "wrapper" {
			continue
		}
		path := strings.TrimSpace(artifact.Path)
		executableID := filepath.Base(path)
		if path == "" || path != artifact.Path || !filepath.IsAbs(path) ||
			executableID == "." || !validLaneOpaqueID(executableID) {
			return nil, fmt.Errorf("launch profile wrapper artifact path is invalid")
		}
		if _, exists := wrapperArtifacts[executableID]; exists {
			return nil, fmt.Errorf("launch profile wrapper artifact %q is ambiguous", executableID)
		}
		wrapperArtifacts[executableID] = path
	}
	if len(profile.Wrappers) == 0 || len(wrapperArtifacts) != len(profile.Wrappers) {
		return nil, fmt.Errorf("launch profile wrapper artifacts do not match the owner profile")
	}
	declaredWrappers := make(map[string]bool, len(profile.Wrappers))
	for _, wrapper := range profile.Wrappers {
		if !validLaneOpaqueID(wrapper) || declaredWrappers[wrapper] {
			return nil, fmt.Errorf("launch profile wrapper %q is invalid or duplicated", wrapper)
		}
		if _, ok := wrapperArtifacts[wrapper]; !ok {
			return nil, fmt.Errorf("launch profile wrapper %q has no healthy artifact", wrapper)
		}
		declaredWrappers[wrapper] = true
	}
	executableID := selectedExecutableID
	executablePath, selectedWrapper := wrapperArtifacts[executableID]
	if !selectedWrapper {
		if len(profile.Wrappers) != 1 {
			return nil, fmt.Errorf("selected command %q does not identify one owner wrapper", selectedExecutableID)
		}
		executableID = profile.Wrappers[0]
		executablePath = wrapperArtifacts[executableID]
	}

	sourceSum := sha256.Sum256(raw)
	canonical := work.DelegateLaunchProfilePlan{
		SchemaVersion:     delegateLaunchProfilePlanSchema,
		SourceSchema:      source.Schema,
		SourceDigest:      fmt.Sprintf("sha256:%x", sourceSum),
		AuthorityRevision: source.AuthorityRevision,
		Revision:          source.Revision,
		ID:                profile.ID,
		Harness:           profile.Harness,
		ProviderLane:      profile.ProviderLane,
		ProviderRouteKind: strings.TrimSpace(req.SelectedProviderRouteKind),
		Route:             profile.Route,
		RequestedModel:    profile.RequestedModel,
		RequestedEffort:   profile.RequestedEffort,
		TransportID:       laneLaunchProfileTransportID,
		OwnerTransport:    profile.Transport,
		ClientProfileID:   profile.ClientProfile,
		CaptureMode:       profile.Capture.Mode,
		ExecutableID:      executableID,
	}
	snapshot, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("encode normalized launch profile plan: %w", err)
	}

	disposition := work.DelegateLaunchProfileIncompatible
	if routeevidence.DelegateClientHarnessesCompatible(req.SelectedHarness, req.SelectedLaneID, profile.Harness) &&
		strings.TrimSpace(req.SelectedModelID) != "" &&
		work.DelegateLaunchProfileRouteConforms(
			req.SelectedProviderRouteKind,
			req.SelectedProviderRoute,
			profile.ProviderLane,
			profile.Route,
		) &&
		strings.TrimSpace(req.SelectedTransportID) == laneLaunchProfileTransportID &&
		strings.TrimSpace(req.RequestedNativeEffort) == profile.RequestedEffort &&
		selectedExecutableID == canonical.ExecutableID {
		disposition = work.DelegateLaunchProfileCompatible
	}
	return &work.DelegateLaunchProfileResolution{
		Snapshot:       snapshot,
		ExecutablePath: executablePath,
		Compatibility: work.DelegateLaunchProfileCompatibility{
			Disposition:               disposition,
			SelectedHarness:           req.SelectedHarness,
			SelectedLaneID:            req.SelectedLaneID,
			SelectedModelID:           req.SelectedModelID,
			SelectedProviderRoute:     req.SelectedProviderRoute,
			SelectedProviderRouteKind: req.SelectedProviderRouteKind,
			SelectedTransportID:       req.SelectedTransportID,
			RequestedNativeEffort:     req.RequestedNativeEffort,
			PlanID:                    canonical.ID,
			ClientHarnessID:           canonical.Harness,
			ClientProfileID:           canonical.ClientProfileID,
			Revision:                  canonical.Revision,
			SelectedExecutableID:      selectedExecutableID,
			ExecutableID:              canonical.ExecutableID,
		},
	}, nil
}

func validLaneSHA256Revision(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	return validLaneSHA256Digest(strings.TrimPrefix(value, "sha256:"))
}

func validLaneSHA256Digest(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'))
	}) == -1
}

func validLaneOpaqueID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' ||
			r == '_' ||
			r == '.')
	}) == -1
}

func oneOfLaneValue(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func init() {
	work.DelegateSessionStorePlanLookup = laneProfilePlanArtifacts
	work.DelegateSessionStoreResolver = work.ResolveDelegateSessionStore
	work.DelegateLaunchProfileResolver = resolveDelegateLaunchProfile
}
