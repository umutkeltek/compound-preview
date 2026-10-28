package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/capgraph"
	"compound/internal/routeevidence"
)

func TestCapabilityResolveComposesOwnedSurfaceAndRouteWithoutMutation(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	addCapabilityGraphFixture(t, homePath)
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), cliTestNow); err != nil {
		t.Fatal(err)
	}
	costReport, err := routeevidence.DoctorProviderLedgerCosts(filepath.Join("..", "..", "fixtures", "route", "provider-calls-cost-gaps.ndjson"), routeevidence.ProviderLedgerImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.RecordCostDoctorObservation(homePath, costReport, cliTestNow); err != nil {
		t.Fatal(err)
	}

	before := snapshotCapabilityResolveHome(t, homePath)
	out := captureStdout(t, func() {
		cmdCapability([]string{"resolve", "--harness", "codex", "--json", "extract metrics from the export"})
	})
	after := snapshotCapabilityResolveHome(t, homePath)
	if before != after {
		t.Fatalf("read-only capability resolve mutated COMPOUND_HOME\nbefore=%s\nafter=%s", before, after)
	}

	var report capabilityResolutionReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse capability resolve output: %v\n%s", err, out)
	}
	if report.SchemaVersion != capabilityResolutionSchema || report.Status != "ok" {
		t.Fatalf("unexpected resolution header: %+v", report)
	}
	if report.Classification.CapabilityClass != "classify-extract" {
		t.Fatalf("wrong classification: %+v", report.Classification)
	}
	if report.CapabilitySurface.AuthorityRef != capgraph.CapabilityGraphAuthorityRef || report.CapabilitySurface.Harness != "codex" {
		t.Fatalf("capability authority/harness missing: %+v", report.CapabilitySurface)
	}
	if !validGraphAuthorityRevision(report.CapabilitySurface.AuthorityRevision) {
		t.Fatalf("capability authority revision missing: %+v", report.CapabilitySurface)
	}
	if report.CapabilitySurface.OfferCount < 1 || report.CapabilitySurface.Offers[0].Node.ID != "tool:fixture-extractor" {
		t.Fatalf("expected fixture offer: %+v", report.CapabilitySurface)
	}
	if report.RoutePlan.AuthorityRef != routeevidence.ModelCapabilitiesAuthorityRef || report.RoutePlan.SourceAuthorityID == "" || report.RoutePlan.SourceRevision == "" {
		t.Fatalf("model-router authority proof missing: %+v", report.RoutePlan)
	}
	if len(report.RoutePlan.Chain) != 3 {
		t.Fatalf("unexpected route chain: %+v", report.RoutePlan.Chain)
	}
	if len(report.RoutePlan.PendingObservations) != 0 {
		t.Fatalf("bounded capability resolution must not embed pending route observation bodies: %+v", report.RoutePlan.PendingObservations)
	}
	if report.Classification.AuthorityRef != routeevidence.CapabilityTaskClassifierAuthorityRef || !validSHA256Revision(report.Classification.AuthorityRevision) {
		t.Fatalf("classification authority missing: %+v", report.Classification)
	}
	if report.AuthorityRefs[0] != routeevidence.CapabilityTaskClassifierAuthorityRef || report.AuthorityRefs[1] != capgraph.CapabilityGraphAuthorityRef || report.AuthorityRefs[2] != routeevidence.ModelCapabilitiesAuthorityRef {
		t.Fatalf("authority refs = %v", report.AuthorityRefs)
	}
	if report.Limits.MaxJSONBytes != capabilityResolutionMaxJSONBytes || report.Limits.PendingObservationBodiesRead {
		t.Fatalf("resolution bounds missing: %+v", report.Limits)
	}
}

func TestCapabilityResolveAppliesPrivacyAtCompoundRouteOwner(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	store := addCapabilityGraphFixture(t, homePath)
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), cliTestNow); err != nil {
		t.Fatal(err)
	}

	report, err := buildCapabilityResolutionReport(store, homePath, "extract private records", "", true, false, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RoutePlan.Chain) != 1 || report.RoutePlan.Chain[0].Tier != "local-batch" {
		t.Fatalf("privacy route must contain only Compound-selected local lanes: %+v", report.RoutePlan)
	}
	if len(report.RoutePlan.Omitted) != 2 {
		t.Fatalf("privacy route must report omitted non-local lanes: %+v", report.RoutePlan.Omitted)
	}
}

func TestCapabilityResolveRejectsMissingCapabilityGraphAuthority(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), cliTestNow); err != nil {
		t.Fatal(err)
	}
	store, err := capgraph.OpenReadOnly(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildCapabilityResolutionReport(store, homePath, "extract metrics", "", false, false, cliTestNow); err == nil || !strings.Contains(err.Error(), "capability graph authority has no nodes") {
		t.Fatalf("missing graph authority error = %v", err)
	}
}

func TestCapabilityResolveAcceptsNativeGraphWithoutSourceHandoff(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Add(capgraph.Node{ID: "tool:fixture-extractor", Kind: "tool", Name: "fixture extractor", Triggers: []string{"extract"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), cliTestNow); err != nil {
		t.Fatal(err)
	}
	report, err := buildCapabilityResolutionReport(store, homePath, "extract metrics", "", false, false, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if report.CapabilitySurface.SourceRecordCount != 0 || !validGraphAuthorityRevision(report.CapabilitySurface.AuthorityRevision) {
		t.Fatalf("native graph authority should bind serving state without requiring source handoff evidence: %+v", report.CapabilitySurface)
	}
}

func TestCapabilityResolveRejectsMissingModelRouterProvenance(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	store := addCapabilityGraphFixture(t, homePath)
	source := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routeevidence.ModelCapabilitiesPath(homePath), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildCapabilityResolutionReport(store, homePath, "extract metrics", "", false, false, cliTestNow); err == nil || !strings.Contains(err.Error(), "model-router authority has no source provenance") {
		t.Fatalf("missing router provenance error = %v", err)
	}
}

func TestCapabilityResolveRejectsStaleModelRouterProvenance(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	store := addCapabilityGraphFixture(t, homePath)
	source := filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml")
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, source, cliTestNow); err != nil {
		t.Fatal(err)
	}
	authorityPath := routeevidence.ModelCapabilitiesPath(homePath)
	data, err := os.ReadFile(authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorityPath, append(data, []byte("\n# changed after source binding\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildCapabilityResolutionReport(store, homePath, "extract metrics", "", false, false, cliTestNow); err == nil || !strings.Contains(err.Error(), "does not match parsed authority") {
		t.Fatalf("stale router provenance error = %v", err)
	}
}

func TestCapabilityResolveRejectsPrivacyPlanWithoutEligibleLane(t *testing.T) {
	t.Parallel()
	homePath := initializedTestHome(t)
	store := addCapabilityGraphFixture(t, homePath)
	if _, err := routeevidence.ImportModelCapabilitiesYAML(homePath, filepath.Join("..", "..", "fixtures", "route", "model-capabilities.yaml"), cliTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := buildCapabilityResolutionReport(store, homePath, "audit private records", "", true, false, cliTestNow); err == nil || !strings.Contains(err.Error(), "no eligible lane") {
		t.Fatalf("empty privacy route error = %v", err)
	}
}

func TestCapabilityResolutionBoundsRouteAndEncodedBytes(t *testing.T) {
	t.Parallel()
	plan := routeevidence.CapabilityRoutePlan{
		Chain:             make([]routeevidence.CapabilityRouteLane, capabilityResolutionRouteLaneLimit+4),
		Omitted:           make([]routeevidence.CapabilityRouteLane, capabilityResolutionPolicyOmitLimit+3),
		Warnings:          make([]string, capabilityResolutionWarningLimit+2),
		LaneJustification: make([]string, capabilityResolutionRouteLaneLimit+4),
		PendingObservations: []routeevidence.PendingRouteObservation{
			{CandidateID: "route-observation", Detail: strings.Repeat("x", 1024)},
		},
	}
	bounded := boundCapabilityRoutePlan(plan)
	if len(bounded.Chain) != capabilityResolutionRouteLaneLimit || bounded.OmittedRouteLaneCount != 4 {
		t.Fatalf("route chain not bounded: %+v", bounded)
	}
	if len(bounded.Omitted) != capabilityResolutionPolicyOmitLimit || bounded.HiddenPolicyOmitCount != 3 {
		t.Fatalf("policy omissions not bounded: %+v", bounded)
	}
	if len(bounded.Warnings) != capabilityResolutionWarningLimit || bounded.OmittedWarningCount != 2 || len(bounded.PendingObservations) != 0 {
		t.Fatalf("route metadata not bounded: %+v", bounded)
	}
	oversize := capabilityResolutionReport{Task: strings.Repeat("x", capabilityResolutionMaxJSONBytes+1)}
	if err := ensureCapabilityResolutionBound(oversize); err == nil {
		t.Fatal("oversize capability resolution should fail closed")
	}
	empty := capabilityResolutionReport{}
	base, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	exact := empty
	exact.Task = strings.Repeat("x", capabilityResolutionMaxJSONBytes-len(base))
	encoded, err := json.Marshal(exact)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != capabilityResolutionMaxJSONBytes {
		t.Fatalf("exact boundary fixture bytes = %d", len(encoded))
	}
	if err := ensureCapabilityResolutionBound(exact); err == nil {
		t.Fatal("exact JSON limit must reject because CLI encoding adds a newline")
	}
}

const capabilityResolveMissingHomeHelper = "COMPOUND_CAPABILITY_RESOLVE_MISSING_HOME_HELPER"

func TestCapabilityResolveMissingHomeFailsWithoutMutation(t *testing.T) {
	t.Parallel()
	if os.Getenv(capabilityResolveMissingHomeHelper) == "1" {
		cmdCapability([]string{"resolve", "--json", "extract metrics"})
		return
	}

	homePath := filepath.Join(t.TempDir(), "missing-home")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCapabilityResolveMissingHomeFailsWithoutMutation$")
	cmd.Env = append(os.Environ(), capabilityResolveMissingHomeHelper+"=1", "COMPOUND_HOME="+homePath)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("missing home should exit 1, err=%v output=%s", err, output)
	}
	if _, err := os.Stat(homePath); !os.IsNotExist(err) {
		t.Fatalf("read-only resolve created missing home %s: %v", homePath, err)
	}
}

func addCapabilityGraphFixture(t *testing.T, homePath string) *capgraph.Store {
	t.Helper()
	store, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	node := capgraph.Node{ID: "tool:fixture-extractor", Kind: "tool", Name: "fixture extractor", Triggers: []string{"extract", "metrics", "audit"}}
	if err := store.Add(node); err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.RecordSourceAuthority(homePath, capgraph.SourceAuthorityInput{
		SourcePath:     "fixtures/capability-test.yaml",
		SourceKind:     "synthetic_fixture",
		SourceClass:    "capability",
		SourceRevision: "sha256:" + strings.Repeat("b", 64),
		ImportedVia:    "capability resolver test fixture",
		NodeIDs:        []string{node.ID},
		EvidenceRefs:   []string{"fixture:capability-resolver"},
	}, cliTestNow); err != nil {
		t.Fatal(err)
	}
	return store
}

func snapshotCapabilityResolveHome(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			_, _ = h.Write([]byte("dir"))
			_, _ = h.Write([]byte{0})
			_, _ = h.Write([]byte(rel))
			_, _ = h.Write([]byte{0})
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = h.Write([]byte("file"))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(body)
		_, _ = h.Write([]byte{0})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
