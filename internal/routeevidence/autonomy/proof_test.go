package autonomy_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	homepkg "compound/internal/home"
	"compound/internal/routeevidence"
	autonomy "compound/internal/routeevidence/autonomy"
	"compound/internal/work"
)

var proofNow = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

type sampleOptions struct {
	routeOnly        bool
	importRoute      bool
	directorTriggers []string
	workCreatedAt    *time.Time
	autonomyClass    string
	routeWorkID      *string
	clientProfile    string
	protocol         string
	requestID        string
	selectedModel    string
	resolvedProvider *string
	resolvedModel    *string
	resolutionSource *string
	gatewayRequestID *string
}

type seededSample struct {
	WorkID         string
	ReceiptID      string
	RouteID        string
	RequestID      string
	ReceiptChanged bool
}

func TestCompileRejectsRouteOnlyReceiptAsStateDelta(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "genuine", proofNow.Add(-48*time.Hour), sampleOptions{})
	routeOnly := seedSample(t, home, plan, "route-only", proofNow.Add(-24*time.Hour), sampleOptions{routeOnly: true})
	if !routeOnly.ReceiptChanged {
		t.Fatal("fixture did not reproduce Receipt.EffectiveChanged route-only normalization")
	}

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "ineligible" {
		t.Fatalf("route-only proof counted as task success: %+v", artifact)
	}
	assertReason(t, artifact, "no genuine state delta independent of route evidence")
	assertSample(t, artifact, routeOnly.WorkID, func(sample autonomy.Sample) {
		if sample.ReceiptChanged || sample.Valid {
			t.Fatalf("route-only sample must be unchanged and invalid: %+v", sample)
		}
	})
}

func TestCompileFailsClosedWhenTerminalReceiptIsMutatedAfterClose(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "receipt-seal-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "receipt-seal-route-only", proofNow.Add(-24*time.Hour), sampleOptions{routeOnly: true})
	baseline := compile(t, home, plan, 2)
	if baseline.Status != "ineligible" {
		t.Fatalf("route-only fixture must start ineligible: %+v", baseline)
	}
	appendReceiptProofRef(t, home, victim.ReceiptID, "check:forged-after-close")

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "receipt snapshot hash") {
		t.Fatalf("post-close receipt mutation did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnNonLocalTerminalReceiptRef(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "receipt-ref-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "receipt-ref-alias", proofNow.Add(-24*time.Hour), sampleOptions{})
	aliasedRef := "rec_/../" + victim.ReceiptID
	rewriteTerminalReceiptRef(t, home, victim.WorkID, victim.ReceiptID, aliasedRef)

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "receipt ref must be a local basename") {
		t.Fatalf("non-local terminal receipt ref did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedWhenReceiptIdentityDoesNotMatchRef(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "receipt-id-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "receipt-id-mismatch", proofNow.Add(-24*time.Hour), sampleOptions{})
	aliasID := "rec_filename_alias"
	receiptRaw := readAutonomyEvent(t, home, victim.ReceiptID, nil)
	if err := os.WriteFile(filepath.Join(home, "events", aliasID+".json"), receiptRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	rewriteTerminalReceiptRef(t, home, victim.WorkID, victim.ReceiptID, aliasID)

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "receipt identity does not match ref") {
		t.Fatalf("receipt filename/ref identity mismatch did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnSymlinkedTerminalReceipt(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "receipt-symlink-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "receipt-symlink", proofNow.Add(-24*time.Hour), sampleOptions{})
	segmentPaths, err := filepath.Glob(filepath.Join(home, "events", "segments", "receipt", "*.jsonl"))
	if err != nil || len(segmentPaths) != 1 {
		t.Fatalf("receipt segments = %v, err=%v, want one", segmentPaths, err)
	}
	receiptPath := segmentPaths[0]
	receiptRaw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	escapedPath := filepath.Join(home, "escaped-receipts", filepath.Base(receiptPath))
	if err := os.MkdirAll(filepath.Dir(escapedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escapedPath, receiptRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(receiptPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escapedPath, receiptPath); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "receipt segment file must be a regular file") {
		t.Fatalf("symlinked terminal receipt did not fail closed: %v", err)
	}
}

func TestCompileRequiresExactDecidingRouteBinding(t *testing.T) {
	tests := []struct {
		name       string
		opts       sampleOptions
		wantReason string
	}{
		{name: "empty-work-id", opts: sampleOptions{routeWorkID: stringPointer("")}, wantReason: "route evidence work_id is required"},
		{name: "wrong-work-id", opts: sampleOptions{routeWorkID: stringPointer("work_elsewhere")}, wantReason: "route evidence work_id does not match"},
		{name: "wrong-client-profile", opts: sampleOptions{clientProfile: "compound.distiller.extract"}, wantReason: "client_profile"},
		{name: "wrong-protocol", opts: sampleOptions{protocol: "openai-chat-completions"}, wantReason: "protocol"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			plan := installPlan(t, home, false)
			seedSample(t, home, plan, tc.name+"-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
			seedSample(t, home, plan, tc.name+"-bad", proofNow.Add(-24*time.Hour), tc.opts)
			artifact := compile(t, home, plan, 2)
			if artifact.Status != "ineligible" {
				t.Fatalf("bad deciding route binding escaped: %+v", artifact)
			}
			assertReason(t, artifact, tc.wantReason)
		})
	}
}

func TestCompileRejectsImportedRouteClaimingPanelJudge(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	forged := seedSample(t, home, plan, "imported-forged-judge", proofNow.Add(-24*time.Hour), sampleOptions{importRoute: true})

	artifact := compile(t, home, plan, 1)
	if artifact.Status != "ineligible" {
		t.Fatalf("imported route row forged a deciding panel call: %+v", artifact)
	}
	assertSample(t, artifact, forged.WorkID, func(sample autonomy.Sample) {
		if sample.Valid || !strings.Contains(sample.InvalidReason, "trusted panel runtime provenance") {
			t.Fatalf("imported deciding row was not rejected by provenance: %+v", sample)
		}
	})
}

func TestCompileRejectsAmbiguousDirectorTriggerClasses(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "ambiguous-class", proofNow.Add(-24*time.Hour), sampleOptions{
		autonomyClass:    "source_connection_readiness",
		directorTriggers: []string{"director_trigger:job_health:forged", "director_trigger:source_connection_readiness:real"},
	})

	if _, err := compileArtifact(home, plan, 1); err == nil || !strings.Contains(err.Error(), "ambiguous director_trigger") {
		t.Fatalf("ambiguous trigger classes did not fail closed: %v", err)
	}
}

func TestCompileRequiresEligibleResolvedModelIdentity(t *testing.T) {
	tests := []struct {
		name             string
		resolvedProvider string
		resolvedModel    string
		resolutionSource string
		gatewayRequestID string
		wantReason       string
	}{
		{name: "missing-provider", resolvedModel: "deepseek/deepseek-v4-pro", resolutionSource: "gateway_route", gatewayRequestID: "gateway-missing-provider", wantReason: "resolved provider is required"},
		{name: "missing-model", resolvedProvider: "deepseek", resolutionSource: "gateway_route", gatewayRequestID: "gateway-missing-model", wantReason: "resolved model is required"},
		{name: "unresolved-adaptive-alias", resolvedProvider: "switchback", resolvedModel: "auto/judge", resolutionSource: "gateway_route", gatewayRequestID: "gateway-unresolved-alias", wantReason: "adaptive route did not resolve to exactly one concrete lane"},
		{name: "adaptive-without-gateway-source", resolvedProvider: "deepseek", resolvedModel: "deepseek/deepseek-v4-pro", resolutionSource: "response_body", wantReason: "adaptive route requires gateway resolution evidence"},
		{name: "adaptive-without-gateway-request", resolvedProvider: "deepseek", resolvedModel: "deepseek/deepseek-v4-pro", resolutionSource: "gateway_route", wantReason: "adaptive route requires gateway request identity"},
		{name: "free-fallback", resolvedProvider: "scout", resolvedModel: "scout/chat", resolutionSource: "gateway_route", gatewayRequestID: "gateway-free", wantReason: "resolved model gate role is tripwire_only"},
		{name: "unknown-override", resolvedProvider: "provider", resolvedModel: "provider/unlisted-model", resolutionSource: "gateway_route", gatewayRequestID: "gateway-unknown", wantReason: "resolved target is not present in current route plan"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			plan := installPlan(t, home, false)
			seedSample(t, home, plan, tc.name+"-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
			seedSample(t, home, plan, tc.name+"-bad", proofNow.Add(-24*time.Hour), sampleOptions{
				resolvedProvider: stringPointer(tc.resolvedProvider),
				resolvedModel:    stringPointer(tc.resolvedModel),
				resolutionSource: stringPointer(tc.resolutionSource),
				gatewayRequestID: stringPointer(tc.gatewayRequestID),
			})

			artifact := compile(t, home, plan, 2)
			if artifact.Status != "ineligible" {
				t.Fatalf("untrusted resolved model identity escaped: %+v", artifact)
			}
			assertReason(t, artifact, tc.wantReason)
		})
	}
}

func TestCompileBindsResolvedProviderAndModelIdentity(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "resolved-first", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "resolved-second", proofNow.Add(-24*time.Hour), sampleOptions{})

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "eligible" {
		t.Fatalf("resolved paid judge evidence was rejected: %+v", artifact)
	}
	for _, sample := range artifact.Samples {
		if sample.RouteModelID != "auto/judge" || sample.RouteMode != "adaptive" || sample.ResolvedLaneID != "deepseek/deepseek-v4-pro" || sample.ProviderID != "deepseek" || sample.ModelID != "deepseek/deepseek-v4-pro" || sample.ResolutionSource != "gateway_route" || sample.GateRole != "eligible" {
			t.Fatalf("resolved provider/model identity was not sealed: %+v", sample)
		}
	}
}

func TestCompileAllowsConcreteDirectSelectedModelToEqualResolvedModel(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	opts := sampleOptions{
		selectedModel:    "deepseek/deepseek-v4-pro",
		resolvedProvider: stringPointer("deepseek"),
		resolvedModel:    stringPointer("deepseek/deepseek-v4-pro"),
		resolutionSource: stringPointer("gateway_route"),
		gatewayRequestID: stringPointer("gateway-direct-paid"),
	}
	seedSample(t, home, plan, "direct-paid-first", proofNow.Add(-48*time.Hour), opts)
	seedSample(t, home, plan, "direct-paid-second", proofNow.Add(-24*time.Hour), opts)

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "eligible" {
		t.Fatalf("direct paid selected==resolved evidence was rejected: %+v", artifact)
	}
	for _, sample := range artifact.Samples {
		if sample.RouteModelID != "deepseek/deepseek-v4-pro" || sample.RouteMode != "direct" || sample.ResolvedLaneID != "deepseek/deepseek-v4-pro" || sample.ProviderID != "deepseek" || sample.ModelID != "deepseek/deepseek-v4-pro" {
			t.Fatalf("direct paid identity was not sealed: %+v", sample)
		}
	}
}

func TestCompileAllowsEligibleLocalDirectLane(t *testing.T) {
	home := t.TempDir()
	plan := installLocalDirectPlan(t, home)
	opts := sampleOptions{
		selectedModel:    "mac/qwen-fixture",
		resolvedProvider: stringPointer("lmstudio"),
		resolvedModel:    stringPointer("qwen-fixture-served"),
		resolutionSource: stringPointer("response_body"),
		gatewayRequestID: stringPointer(""),
	}
	seedSample(t, home, plan, "local-direct-first", proofNow.Add(-48*time.Hour), opts)
	seedSample(t, home, plan, "local-direct-second", proofNow.Add(-24*time.Hour), opts)

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "eligible" {
		t.Fatalf("eligible local direct evidence was rejected: %+v", artifact)
	}
	for _, sample := range artifact.Samples {
		if sample.RouteMode != "direct" || sample.ResolvedLaneID != "mac/qwen-fixture" || sample.ProviderID != "lmstudio" || sample.ModelID != "qwen-fixture-served" || sample.ResolutionSource != "response_body" || sample.GateRole != "eligible" {
			t.Fatalf("local direct identity was not sealed: %+v", sample)
		}
	}
}

func TestCompileRejectsProviderRouteKindDriftFromCurrentAuthority(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{name: "omitted", kind: ""},
		{name: "different-kind", kind: routeevidence.ProviderRouteKindProviderClass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			plan := installLocalDirectPlan(t, home)
			plan.Chain[0].ProviderRouteKind = tc.kind

			artifact := compile(t, home, plan, 1)
			assertReason(t, artifact, "route plan chain does not match current model-router authority")
		})
	}
}

func TestCompileAcceptsLegacyManifestWithoutProviderRouteKind(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "legacy-provider-route-kind-first", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "legacy-provider-route-kind-second", proofNow.Add(-24*time.Hour), sampleOptions{})

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "eligible" {
		t.Fatalf("legacy manifest without provider_route_kind was rejected: %+v", artifact)
	}
}

func TestCompileRejectsDirectLaneResolvingToDifferentLane(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "direct-mismatch-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "direct-mismatch-bad", proofNow.Add(-24*time.Hour), sampleOptions{
		selectedModel:    "deepseek/deepseek-v4-pro",
		resolvedProvider: stringPointer("lmstudio"),
		resolvedModel:    stringPointer("qwen-fixture-served"),
		resolutionSource: stringPointer("response_body"),
		gatewayRequestID: stringPointer(""),
	})

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "ineligible" {
		t.Fatalf("direct lane resolved to a different lane: %+v", artifact)
	}
	assertReason(t, artifact, "direct route resolved outside its declared target")
}

func TestGateRoleFailsClosedForUnknownClaimLevel(t *testing.T) {
	if got := autonomy.GateRole("judge-critique", "cheap", "primray"); got == "eligible" {
		t.Fatalf("unknown claim level became authority: %q", got)
	}
}

func TestCompileRejectsReusedRouteRequestIdentity(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	second := seedSample(t, home, plan, "reused-request", proofNow.Add(-24*time.Hour), sampleOptions{})
	duplicateRouteRequest(t, home, second, proofNow.Add(-24*time.Hour-time.Second))

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "ineligible" {
		t.Fatalf("reused route request identity escaped: %+v", artifact)
	}
	assertReason(t, artifact, "request_id is ambiguous")
}

func TestLoadAndRevalidateRejectsRouterRevisionDrift(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "first", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "second", proofNow.Add(-24*time.Hour), sampleOptions{})
	artifact := compile(t, home, plan, 2)
	if artifact.Status != "eligible" {
		t.Fatalf("baseline eligibility failed: %+v", artifact)
	}
	if _, err := autonomy.Record(home, artifact); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(plan.AuthorityPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n# revision drift\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := autonomy.LoadAndRevalidate(home, artifact.Ref(), "job_health", plan.ID); err == nil || !strings.Contains(err.Error(), "stale or source evidence changed") {
		t.Fatalf("router revision drift did not invalidate sealed eligibility: %v", err)
	}
}

func TestCompileFailsClosedOnUnindexedArchivedWork(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "indexed-live", proofNow.Add(-48*time.Hour), sampleOptions{})
	orphan := seedSample(t, home, plan, "archive-gap", proofNow.Add(-24*time.Hour), sampleOptions{})
	source := filepath.Join(home, "work", "work", orphan.WorkID+".json")
	target := filepath.Join(home, "archives", "operational", "files", "work", "work", orphan.WorkID+".json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, target); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "unindexed work file") {
		t.Fatalf("archive move/index crash gap did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnDuplicateHotAndArchivedWorkIdentity(t *testing.T) {
	home := t.TempDir()
	if err := homepkg.Init(home, "local", proofNow.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "duplicate-location-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	latest := seedSample(t, home, plan, "duplicate-location", proofNow.Add(-24*time.Hour), sampleOptions{})
	entry := archiveAutonomyWork(t, home, latest.WorkID)
	archivedRaw, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(entry.ArchivePath)))
	if err != nil {
		t.Fatal(err)
	}
	hotPath := filepath.Join(home, filepath.FromSlash(entry.OriginalPath))
	if err := os.MkdirAll(filepath.Dir(hotPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hotPath, archivedRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "duplicate work identity") {
		t.Fatalf("duplicate hot/archive work identity did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnMutatedIndexedArchiveWork(t *testing.T) {
	home := t.TempDir()
	if err := homepkg.Init(home, "local", proofNow.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "archive-hash-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	latest := seedSample(t, home, plan, "archive-hash-mutated", proofNow.Add(-24*time.Hour), sampleOptions{})
	entry := archiveAutonomyWork(t, home, latest.WorkID)
	path := filepath.Join(home, filepath.FromSlash(entry.ArchivePath))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, ' '), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("mutated indexed archive work did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnArchiveWorkStatusOrPathMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*homepkg.OperationalArchiveEntry)
		want   string
	}{
		{name: "status", mutate: func(entry *homepkg.OperationalArchiveEntry) { entry.Status = "failed" }, want: "status"},
		{name: "original-path", mutate: func(entry *homepkg.OperationalArchiveEntry) { entry.OriginalPath = "work/work/work_wrong.json" }, want: "archive path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if err := homepkg.Init(home, "local", proofNow.Add(-72*time.Hour)); err != nil {
				t.Fatal(err)
			}
			plan := installPlan(t, home, false)
			seedSample(t, home, plan, tc.name+"-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
			latest := seedSample(t, home, plan, tc.name+"-mismatch", proofNow.Add(-24*time.Hour), sampleOptions{})
			entry := archiveAutonomyWork(t, home, latest.WorkID)
			rewriteArchiveEntry(t, home, entry.ID, tc.mutate)

			if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("archive %s mismatch did not fail closed: %v", tc.name, err)
			}
		})
	}
}

func TestCompileFailsClosedOnArchivePathTraversalPair(t *testing.T) {
	home := t.TempDir()
	if err := homepkg.Init(home, "local", proofNow.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "archive-traversal-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "archive-traversal", proofNow.Add(-24*time.Hour), sampleOptions{})
	entry := archiveAutonomyWork(t, home, victim.WorkID)
	escapedPath := filepath.Join(home, "escaped-archive", victim.WorkID+".json")
	if err := os.MkdirAll(filepath.Dir(escapedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(home, filepath.FromSlash(entry.ArchivePath)), escapedPath); err != nil {
		t.Fatal(err)
	}
	rewriteArchiveEntry(t, home, entry.ID, func(forged *homepkg.OperationalArchiveEntry) {
		forged.OriginalPath = "work/work/../../../../../escaped-archive/" + victim.WorkID + ".json"
		forged.ArchivePath = homepkg.OperationalArchiveFilesRel + "/" + forged.OriginalPath
		sum := sha256.Sum256([]byte(filepath.ToSlash(forged.OriginalPath)))
		forged.ID = "oparch_" + hex.EncodeToString(sum[:])[:12]
	})

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "original path must be a canonical local path") {
		t.Fatalf("traversing archive path pair did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnSymlinkedArchivedWork(t *testing.T) {
	home := t.TempDir()
	if err := homepkg.Init(home, "local", proofNow.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "archive-symlink-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "archive-symlink", proofNow.Add(-24*time.Hour), sampleOptions{})
	entry := archiveAutonomyWork(t, home, victim.WorkID)
	archivePath := filepath.Join(home, filepath.FromSlash(entry.ArchivePath))
	archiveRaw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	escapedPath := filepath.Join(home, "escaped-archive", victim.WorkID+".json")
	if err := os.MkdirAll(filepath.Dir(escapedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escapedPath, archiveRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escapedPath, archivePath); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "archive file must be a regular file") {
		t.Fatalf("symlinked archived work did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnSymlinkedHotWork(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "hot-symlink-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "hot-symlink", proofNow.Add(-24*time.Hour), sampleOptions{})
	workPath := filepath.Join(home, "work", "work", victim.WorkID+".json")
	workRaw, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	escapedPath := filepath.Join(home, "escaped-work", victim.WorkID+".json")
	if err := os.MkdirAll(filepath.Dir(escapedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(escapedPath, workRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(workPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escapedPath, workPath); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "work file must be a regular file") {
		t.Fatalf("symlinked hot work did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnRecentTerminalEventWithoutWorkSnapshot(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "missing-snapshot-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	missing := seedSample(t, home, plan, "missing-snapshot", proofNow.Add(-24*time.Hour), sampleOptions{})
	if err := os.Remove(filepath.Join(home, "work", "work", missing.WorkID+".json")); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "recent terminal work "+missing.WorkID+" has 0 snapshots") {
		t.Fatalf("missing recent terminal snapshot did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnTerminalSnapshotWithoutTerminalEvent(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "missing-event-old-clean", proofNow.Add(-72*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "missing-event-new-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	createdAt := proofNow.Add(-40 * 24 * time.Hour)
	missing := seedSample(t, home, plan, "missing-event-recent-failure", proofNow, sampleOptions{routeOnly: true, workCreatedAt: &createdAt})
	removeTerminalWorkEvent(t, home, missing.WorkID)

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "terminal work "+missing.WorkID+" has 0 terminal events") {
		t.Fatalf("terminal snapshot without terminal event did not fail closed: %v", err)
	}
}

func TestCompileIgnoresUnclassifiedTerminalSnapshotWithoutTerminalEvent(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "unclassified-old-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "unclassified-new-clean", proofNow.Add(-24*time.Hour), sampleOptions{})
	unclassified := seedSample(t, home, plan, "unclassified-terminal", proofNow, sampleOptions{routeOnly: true})
	removeWorkEvidenceRefPrefix(t, home, unclassified.WorkID, "director_trigger:job_health:")
	removeTerminalWorkEvent(t, home, unclassified.WorkID)

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "eligible" {
		t.Fatalf("unclassified terminal snapshot blocked class-scoped evidence: %+v", artifact)
	}
	for _, sample := range artifact.Samples {
		if sample.WorkID == unclassified.WorkID {
			t.Fatalf("unclassified terminal snapshot entered class-scoped cohort: %+v", artifact.Samples)
		}
	}
}

func TestCompileFailsClosedOnFutureTerminalEvent(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "future-event-current", proofNow.Add(-24*time.Hour), sampleOptions{})
	future := seedSample(t, home, plan, "future-event", proofNow.Add(48*time.Hour), sampleOptions{})

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "terminal work "+future.WorkID+" has terminal timestamp after eligibility evaluation") {
		t.Fatalf("future terminal event did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnWorkEventLedgerWithoutFinalNewline(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "jsonl-framing-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "jsonl-framing-final", proofNow.Add(-24*time.Hour), sampleOptions{})
	path := filepath.Join(home, "events", "work.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("work event fixture was not newline framed")
	}
	if err := os.WriteFile(path, raw[:len(raw)-1], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "work event ledger must end with newline") {
		t.Fatalf("valid final JSON event without newline did not fail closed: %v", err)
	}
}

func TestCompileIgnoresFutureTerminalEventFromOtherClass(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "foreign-future-old-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "foreign-future-new-clean", proofNow.Add(-24*time.Hour), sampleOptions{})
	foreign := seedSample(t, home, plan, "foreign-future", proofNow.Add(24*time.Hour), sampleOptions{autonomyClass: "other_class"})

	artifact, err := compileArtifact(home, plan, 2)
	if err != nil {
		t.Fatalf("valid foreign-class terminal event blocked job_health evidence: %v", err)
	}
	if artifact.Status != "eligible" {
		t.Fatalf("foreign-class terminal event changed job_health eligibility: %+v", artifact)
	}
	for _, sample := range artifact.Samples {
		if sample.WorkID == foreign.WorkID {
			t.Fatalf("foreign-class terminal event entered job_health cohort: %+v", artifact.Samples)
		}
	}
}

func TestCompileFailsClosedOnFutureReceiptTimestamp(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "future-receipt-current", proofNow.Add(-24*time.Hour), sampleOptions{})
	future := seedSample(t, home, plan, "future-receipt", proofNow, sampleOptions{})
	rewriteReceiptCreatedAt(t, home, future.ReceiptID, proofNow.Add(24*time.Hour))
	resealTerminalReceiptSnapshot(t, home, future.ReceiptID)

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "terminal work "+future.WorkID+" has receipt timestamp after eligibility evaluation") {
		t.Fatalf("future receipt timestamp did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedWhenRecentTerminalReceiptIsBackdated(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "backdated-receipt-old-clean", proofNow.Add(-72*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "backdated-receipt-new-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	recent := seedSample(t, home, plan, "backdated-receipt-recent-failure", proofNow, sampleOptions{routeOnly: true})
	baseline := compile(t, home, plan, 2)
	if baseline.Status != "ineligible" {
		t.Fatalf("fixture failure was not in the latest cohort: %+v", baseline)
	}
	rewriteReceiptCreatedAt(t, home, recent.ReceiptID, proofNow.Add(-31*24*time.Hour))
	resealTerminalReceiptSnapshot(t, home, recent.ReceiptID)

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "terminal work "+recent.WorkID+" receipt timestamp does not match terminal event") {
		t.Fatalf("backdated receipt hid recent terminal failure: %v", err)
	}
}

// A post-close class mutation breaks the envelope's custody seal, so under
// the settled per-envelope skip law it is EXCLUDED (reported on
// excluded_seal_mismatch_ids, never a sample) instead of vetoing the whole
// compilation. The falsifier stays load-bearing: the mutated envelope must
// not be able to influence the cohort's eligibility, and it must not be able
// to hide — its id is on the artifact. The pre-skip law failed the entire
// pass on this shape (see TestCompileSkipsSealMismatchEnvelopeInsteadOfFailingCohort
// for the incident that reformed it).
func TestCompileExcludesSealBrokenRecentWorkClassMutation(t *testing.T) {
	for _, mode := range []string{"hot", "restored"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			if err := homepkg.Init(home, "local", proofNow.Add(-96*time.Hour)); err != nil {
				t.Fatal(err)
			}
			plan := installPlan(t, home, false)
			seedSample(t, home, plan, mode+"-old-clean", proofNow.Add(-72*time.Hour), sampleOptions{})
			seedSample(t, home, plan, mode+"-new-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
			victim := seedSample(t, home, plan, mode+"-recent-failure", proofNow.Add(-24*time.Hour), sampleOptions{routeOnly: true})
			baseline := compile(t, home, plan, 2)
			if baseline.Status != "ineligible" {
				t.Fatalf("fixture failure was not in the latest cohort: %+v", baseline)
			}
			if mode == "restored" {
				entry := archiveAutonomyWork(t, home, victim.WorkID)
				if _, err := homepkg.RestoreOperationalArchive(home, homepkg.OperationalRestoreOptions{IDs: []string{entry.ID}}, proofNow.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			mutateWorkClassRef(t, filepath.Join(home, "work", "work", victim.WorkID+".json"))

			artifact := compile(t, home, plan, 2)
			reported := false
			for _, id := range artifact.ExcludedSealMismatchIDs {
				if id == victim.WorkID {
					reported = true
				}
			}
			if !reported {
				t.Fatalf("%s mutated work %s is not reported on excluded_seal_mismatch_ids: %+v", mode, victim.WorkID, artifact.ExcludedSealMismatchIDs)
			}
			for _, sample := range artifact.Samples {
				if sample.WorkID == victim.WorkID {
					t.Fatalf("%s mutated work %s influenced the cohort as a sample", mode, victim.WorkID)
				}
			}
			if err := autonomy.Validate(artifact); err != nil {
				t.Fatalf("%s artifact with seal skip must validate: %v", mode, err)
			}
		})
	}
}

func TestCompileFailsClosedWhenStaleDuplicateTerminalEventHidesRecentEvent(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "terminal-history-old-clean", proofNow.Add(-72*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "terminal-history-new-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "terminal-history-recent-failure", proofNow.Add(-24*time.Hour), sampleOptions{routeOnly: true})
	if err := os.Remove(filepath.Join(home, "work", "work", victim.WorkID+".json")); err != nil {
		t.Fatal(err)
	}
	appendWorkEvent(t, home, map[string]any{
		"ts":          proofNow.Add(-31 * 24 * time.Hour).Format(time.RFC3339),
		"work_id":     victim.WorkID,
		"from":        "running",
		"to":          "failed",
		"work_sha256": "sha256:stale-duplicate",
	})

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "multiple terminal events") {
		t.Fatalf("stale duplicate terminal event hid recent evidence: %v", err)
	}
}

func TestCompileFailsClosedWhenArchiveIndexReanchorsMutatedWork(t *testing.T) {
	home := t.TempDir()
	if err := homepkg.Init(home, "local", proofNow.Add(-96*time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "archive-reanchor-old-clean", proofNow.Add(-72*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "archive-reanchor-new-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "archive-reanchor-recent-failure", proofNow.Add(-24*time.Hour), sampleOptions{routeOnly: true})
	entry := archiveAutonomyWork(t, home, victim.WorkID)
	archivePath := filepath.Join(home, filepath.FromSlash(entry.ArchivePath))
	mutateWorkClassRef(t, archivePath)
	mutated, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(mutated)
	entry.CreatedAt = proofNow.Add(time.Minute).Format(time.RFC3339)
	entry.SHA256 = hex.EncodeToString(sum[:])
	entry.Size = int64(len(mutated))
	appendArchiveEntry(t, home, entry)

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "repeated active archived operation") {
		t.Fatalf("archive index re-anchored mutated work: %v", err)
	}
}

func TestCompileRejectsAssumedSupervisedOnlyDecidingLane(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, true)
	seedSample(t, home, plan, "first", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "second", proofNow.Add(-24*time.Hour), sampleOptions{})

	artifact := compile(t, home, plan, 2)
	if artifact.Status != "ineligible" {
		t.Fatalf("assumed lane decided autonomously: %+v", artifact)
	}
	assertReason(t, artifact, "supervised_only")
}

func TestCompileUsesLatestUniqueTerminalReceipts(t *testing.T) {
	t.Run("latest-n", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		old := seedSample(t, home, plan, "old-route-only", proofNow.Add(-72*time.Hour), sampleOptions{routeOnly: true})
		middle := seedSample(t, home, plan, "middle", proofNow.Add(-48*time.Hour), sampleOptions{})
		latest := seedSample(t, home, plan, "latest", proofNow.Add(-24*time.Hour), sampleOptions{})

		artifact := compile(t, home, plan, 2)
		if artifact.Status != "eligible" {
			t.Fatalf("an older failure displaced latest clean evidence: %+v", artifact)
		}
		if len(artifact.Samples) != 2 || artifact.Samples[0].WorkID != latest.WorkID || artifact.Samples[1].WorkID != middle.WorkID {
			t.Fatalf("samples are not latest-first: %+v", artifact.Samples)
		}
		for _, sample := range artifact.Samples {
			if sample.WorkID == old.WorkID {
				t.Fatalf("older sample entered latest-N window: %+v", artifact.Samples)
			}
		}
	})

	t.Run("duplicate-receipt-ref", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		seedSample(t, home, plan, "first", proofNow.Add(-48*time.Hour), sampleOptions{})
		latest := seedSample(t, home, plan, "latest", proofNow.Add(-24*time.Hour), sampleOptions{})
		appendWorkEvent(t, home, map[string]any{"work_id": latest.WorkID, "to": "completed", "attached": []string{latest.ReceiptID}})

		if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "receipt refs; exactly one required") {
			t.Fatalf("duplicate receipt attachment escaped: %v", err)
		}
	})

	t.Run("nonterminal-action-receipt-is-not-terminal-evidence", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		seedSample(t, home, plan, "first", proofNow.Add(-48*time.Hour), sampleOptions{})
		latest := seedSample(t, home, plan, "latest", proofNow.Add(-24*time.Hour), sampleOptions{})
		appendWorkEvent(t, home, map[string]any{
			"ts":       proofNow.Add(-23 * time.Hour).Format(time.RFC3339),
			"work_id":  latest.WorkID,
			"from":     "running",
			"to":       "running",
			"attached": []string{"rec_intermediate_action"},
		})

		artifact := compile(t, home, plan, 2)
		if artifact.Status != "eligible" {
			t.Fatalf("nonterminal action receipt polluted terminal autonomy evidence: %+v", artifact)
		}
	})
}

func TestCompileAppliesFixedTerminalSampleLookback(t *testing.T) {
	t.Run("lookback-end-is-inclusive", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		seedSample(t, home, plan, "inclusive-first", proofNow.Add(-24*time.Hour), sampleOptions{})
		seedSample(t, home, plan, "inclusive-end", proofNow, sampleOptions{})

		artifact := compile(t, home, plan, 2)
		if artifact.Status != "eligible" {
			t.Fatalf("evidence at lookback end was rejected: %+v", artifact)
		}
		if artifact.WindowEnd != artifact.LookbackEnd || artifact.WindowEnd != proofNow.Format(time.RFC3339) {
			t.Fatalf("window end = %q, lookback end = %q", artifact.WindowEnd, artifact.LookbackEnd)
		}
	})

	t.Run("stale-only-history-denies", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		seedSample(t, home, plan, "stale-first", proofNow.Add(-40*24*time.Hour), sampleOptions{})
		seedSample(t, home, plan, "stale-second", proofNow.Add(-35*24*time.Hour), sampleOptions{})

		artifact := compile(t, home, plan, 2)
		if artifact.Status != "ineligible" {
			t.Fatalf("stale-only history authorized admission: %+v", artifact)
		}
		if artifact.TerminalSampleCount != 0 || len(artifact.Samples) != 0 {
			t.Fatalf("stale samples entered the eligibility cohort: %+v", artifact.Samples)
		}
		if artifact.ExcludedStaleTerminalCount != 2 {
			t.Fatalf("excluded stale terminal count = %d, want 2", artifact.ExcludedStaleTerminalCount)
		}
		assertReason(t, artifact, "terminal samples 0 below minimum 2")
	})

	t.Run("old-samples-are-excluded", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		stale := seedSample(t, home, plan, "stale-route-only", proofNow.Add(-31*24*time.Hour), sampleOptions{routeOnly: true})
		first := seedSample(t, home, plan, "recent-first", proofNow.Add(-48*time.Hour), sampleOptions{})
		second := seedSample(t, home, plan, "recent-second", proofNow.Add(-24*time.Hour), sampleOptions{})

		artifact := compile(t, home, plan, 2)
		if artifact.Status != "eligible" {
			t.Fatalf("stale failure affected in-cohort thresholds: %+v", artifact)
		}
		if artifact.ExcludedStaleTerminalCount != 1 {
			t.Fatalf("excluded stale terminal count = %d, want 1", artifact.ExcludedStaleTerminalCount)
		}
		if len(artifact.Samples) != 2 || artifact.Samples[0].WorkID != second.WorkID || artifact.Samples[1].WorkID != first.WorkID {
			t.Fatalf("recent cohort is not latest-first: %+v", artifact.Samples)
		}
		for _, sample := range artifact.Samples {
			if sample.WorkID == stale.WorkID {
				t.Fatalf("stale sample entered the cohort: %+v", artifact.Samples)
			}
		}
	})

	t.Run("recent-manual-supervised-history-passes-for-admission-only", func(t *testing.T) {
		home := t.TempDir()
		plan := installPlan(t, home, false)
		seedSample(t, home, plan, "manual-first", proofNow.Add(-48*time.Hour), sampleOptions{})
		seedSample(t, home, plan, "manual-second", proofNow.Add(-24*time.Hour), sampleOptions{})

		artifact := compile(t, home, plan, 2)
		if artifact.Status != "eligible" {
			t.Fatalf("recent manually supervised samples did not prove admission-only eligibility: %+v", artifact)
		}
		if artifact.CohortClass != "job_health" || artifact.LookbackDays != 30 {
			t.Fatalf("cohort metadata = class %q lookback %dd", artifact.CohortClass, artifact.LookbackDays)
		}
		wantStart := proofNow.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
		if artifact.LookbackStart != wantStart || artifact.LookbackEnd != proofNow.Format(time.RFC3339) {
			t.Fatalf("lookback = %s..%s, want %s..%s", artifact.LookbackStart, artifact.LookbackEnd, wantStart, proofNow.Format(time.RFC3339))
		}
		if !containsRef(artifact.EvidenceRefs, "cohort:terminal_samples:class=job_health:lookback=30d") {
			t.Fatalf("lookback evidence ref missing from %v", artifact.EvidenceRefs)
		}
	})
}

func installPlan(t *testing.T, home string, assumed bool) autonomy.RoutePlanSnapshot {
	t.Helper()
	source := filepath.Join("..", "..", "..", "fixtures", "route", "model-capabilities.yaml")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	claimLevel := "primary"
	gateRole := "eligible"
	if assumed {
		data = []byte(strings.Replace(string(data), "judge-critique:\n        level: primary", "judge-critique:\n        level: assumed", 1))
		claimLevel = "assumed"
		gateRole = "supervised_only"
	}
	path := filepath.Join(home, "registries", "model-capabilities.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	return autonomy.RoutePlanSnapshot{
		ID:              autonomy.RoutePlanID("judge-critique", false, path, revision),
		AuthorityPath:   path,
		SourceRevision:  revision,
		CapabilityClass: "judge-critique",
		Chain: []autonomy.LaneSnapshot{
			{ModelID: "auto/judge", ProviderRoute: "auto/judge", Resolution: autonomy.LaneResolution{Mode: "adaptive"}, Tier: "cheap", ClaimLevel: claimLevel, GateRole: gateRole},
			{ModelID: "deepseek/deepseek-v4-pro", ProviderRoute: "deepseek/deepseek-v4-pro", Resolution: autonomy.LaneResolution{Mode: "direct", Targets: []autonomy.ResolvedTarget{{Provider: "deepseek", Model: "deepseek/deepseek-v4-pro"}}}, Tier: "cheap", ClaimLevel: "primary", GateRole: "eligible"},
			{ModelID: "scout/chat", ProviderRoute: "scout/chat", Resolution: autonomy.LaneResolution{Mode: "direct", Targets: []autonomy.ResolvedTarget{{Provider: "scout", Model: "scout/chat"}}}, Tier: "free", ClaimLevel: "capable", GateRole: "tripwire_only"},
		},
	}
}

func installLocalDirectPlan(t *testing.T, home string) autonomy.RoutePlanSnapshot {
	t.Helper()
	data := []byte(`version: 1
models:
  mac/qwen-fixture:
    provider_route: direct
    provider_route_kind: direct_model
    resolution:
      mode: direct
      targets:
        - provider: lmstudio
          model: qwen-fixture-served
    tier: local-batch
    capabilities:
      judge-critique:
        level: primary
routing_defaults:
  judge-critique:
    - mac/qwen-fixture
`)
	path := filepath.Join(home, "registries", "model-capabilities.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	plan := autonomy.RoutePlanSnapshot{
		AuthorityPath:   path,
		SourceRevision:  revision,
		CapabilityClass: "judge-critique",
		Chain: []autonomy.LaneSnapshot{
			{ModelID: "mac/qwen-fixture", ProviderRoute: "direct", ProviderRouteKind: routeevidence.ProviderRouteKindDirectModel, Resolution: autonomy.LaneResolution{Mode: "direct", Targets: []autonomy.ResolvedTarget{{Provider: "lmstudio", Model: "qwen-fixture-served"}}}, Tier: "local-batch", ClaimLevel: "primary", GateRole: "eligible"},
		},
	}
	plan.ID = autonomy.RoutePlanID(plan.CapabilityClass, plan.PrivacySensitive, plan.AuthorityPath, plan.SourceRevision)
	return plan
}

func compile(t *testing.T, home string, plan autonomy.RoutePlanSnapshot, samples int) autonomy.Artifact {
	t.Helper()
	artifact, err := compileArtifact(home, plan, samples)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func compileArtifact(home string, plan autonomy.RoutePlanSnapshot, samples int) (autonomy.Artifact, error) {
	return autonomy.Compile(home, autonomy.GrantSnapshot{
		DecisionID:         "job-health-autonomy-test",
		ClaimID:            "claim_job_health_autonomy_test",
		GrantRef:           "grant:job-health-autonomy-test",
		ClaimRef:           "claim:claim_job_health_autonomy_test",
		ReviewedBy:         "operator-test",
		Class:              "job_health",
		MinTerminalSamples: samples,
		MinWindowDays:      1,
		MaxFailures:        0,
	}, plan, proofNow)
}

func seedSample(t *testing.T, home string, plan autonomy.RoutePlanSnapshot, id string, closedAt time.Time, opts sampleOptions) seededSample {
	t.Helper()
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	workID := "work_autonomy_" + strings.ReplaceAll(id, "-", "_")
	createdAt := closedAt.Add(-4 * time.Minute)
	if opts.workCreatedAt != nil {
		createdAt = *opts.workCreatedAt
	}
	autonomyClass := opts.autonomyClass
	if autonomyClass == "" {
		autonomyClass = "job_health"
	}
	triggers := opts.directorTriggers
	if len(triggers) == 0 {
		triggers = []string{"director_trigger:" + autonomyClass + ":" + id}
	}
	evidenceRefs := append([]string{"proposed_by:director"}, triggers...)
	evidenceRefs = append(evidenceRefs, "route_plan:"+plan.ID)
	w, err := store.Create(work.CreateInput{
		WorkID:           workID,
		Owner:            "compound-authority",
		Actor:            "operator-test",
		SuccessPredicate: "prove autonomy sample " + id,
		EvidenceRefs:     evidenceRefs,
		RouteDecisionID:  plan.ID,
		State:            "proposed",
	}, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{Actor: "operator-test", To: "ready"}, closedAt.Add(-4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, "sample-executor", closedAt.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{Actor: "sample-executor", To: "running"}, closedAt.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	routeWorkID := w.ID
	if opts.routeWorkID != nil {
		routeWorkID = *opts.routeWorkID
	}
	clientProfile := opts.clientProfile
	if clientProfile == "" {
		clientProfile = "compound.panel.judge"
	}
	protocol := opts.protocol
	if protocol == "" {
		protocol = "panel-lane"
	}
	requestID := opts.requestID
	if requestID == "" {
		requestID = "request-" + id
	}
	resolvedModel := "deepseek/deepseek-v4-pro"
	if opts.resolvedModel != nil {
		resolvedModel = *opts.resolvedModel
	}
	resolvedProvider := "deepseek"
	if opts.resolvedProvider != nil {
		resolvedProvider = *opts.resolvedProvider
	}
	resolutionSource := "gateway_route"
	if opts.resolutionSource != nil {
		resolutionSource = *opts.resolutionSource
	}
	gatewayRequestID := "gateway-" + id
	if opts.gatewayRequestID != nil {
		gatewayRequestID = *opts.gatewayRequestID
	}
	selectedModel := opts.selectedModel
	if selectedModel == "" {
		selectedModel = "auto/judge"
	}
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:        closedAt.Add(-time.Minute),
		RequestID:        requestID,
		WorkID:           routeWorkID,
		ClientProfile:    clientProfile,
		Protocol:         protocol,
		Provider:         "compound-route-plan",
		Model:            selectedModel,
		ResolvedProvider: resolvedProvider,
		ResolvedModel:    resolvedModel,
		ResolutionSource: resolutionSource,
		GatewayRequestID: gatewayRequestID,
		Status:           "completed",
		EvidenceRefs:     []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	var joined routeevidence.ImportResult
	if opts.importRoute {
		path := filepath.Join(home, "forged-route-"+id+".json")
		data, marshalErr := json.MarshalIndent(ev, "", "  ")
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := os.WriteFile(path, append(data, '\n'), 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
		joined, err = routeevidence.ImportFile(home, path)
	} else {
		joined, err = routeevidence.RecordPanelRuntimeJoined(home, ev)
	}
	if err != nil {
		t.Fatal(err)
	}
	proofRefs := []string{joined.Evidence.ID}
	if !opts.routeOnly {
		proofRefs = append(proofRefs, "check:state-delta:"+id)
	}
	closed, err := store.Close(w.ID, work.CloseInput{
		Outcome:   "completed",
		MoveType:  "control_plane_repair",
		Actor:     "sample-executor",
		Summary:   "autonomy sample " + id + " changed owner state",
		Changed:   false,
		ProofRefs: proofRefs,
	}, closedAt)
	if err != nil {
		t.Fatal(err)
	}
	return seededSample{WorkID: w.ID, ReceiptID: closed.Receipt.ID, RouteID: joined.Evidence.ID, RequestID: requestID, ReceiptChanged: closed.Receipt.StateDelta.Changed}
}

func duplicateRouteRequest(t *testing.T, home string, sample seededSample, createdAt time.Time) {
	t.Helper()
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:        createdAt,
		RequestID:        sample.RequestID,
		WorkID:           sample.WorkID,
		ClientProfile:    "compound.panel.judge",
		Protocol:         "panel-lane",
		Provider:         "compound-route-plan",
		Model:            "auto/judge",
		ResolvedProvider: "deepseek",
		ResolvedModel:    "deepseek/deepseek-v4-pro",
		ResolutionSource: "gateway_route",
		GatewayRequestID: "gateway-duplicate-" + sample.RequestID,
		Status:           "completed",
		EvidenceRefs:     []string{routeevidence.ModelCapabilitiesAuthorityRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Canonical writers reject duplicate request IDs. Append directly to model a
	// legacy/corrupt ledger and keep the compiler's ambiguity guard falsifiable.
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	file, err := os.OpenFile(ledgerPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func appendWorkEvent(t *testing.T, home string, event any) {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "events", "work.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func rewriteTerminalReceiptRef(t *testing.T, home, workID, oldRef, newRef string) {
	t.Helper()
	path := filepath.Join(home, "events", "work.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	updated := 0
	for index, line := range lines {
		var event struct {
			WorkID   string   `json:"work_id"`
			To       string   `json:"to"`
			Attached []string `json:"attached"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.WorkID != workID || (event.To != "completed" && event.To != "failed" && event.To != "cancelled") {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		for refIndex, ref := range event.Attached {
			if ref == oldRef {
				event.Attached[refIndex] = newRef
				updated++
			}
		}
		fields["attached"] = event.Attached
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		lines[index] = string(encoded)
	}
	if updated != 1 {
		t.Fatalf("terminal receipt refs rewritten for %s = %d, want 1", workID, updated)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func removeTerminalWorkEvent(t *testing.T, home, workID string) {
	t.Helper()
	path := filepath.Join(home, "events", "work.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	kept := []string{}
	removed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event struct {
			WorkID string `json:"work_id"`
			To     string `json:"to"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.WorkID == workID && (event.To == "completed" || event.To == "failed" || event.To == "cancelled") {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed != 1 {
		t.Fatalf("removed terminal events for %s = %d, want 1", workID, removed)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rewriteReceiptCreatedAt(t *testing.T, home, receiptID string, createdAt time.Time) {
	t.Helper()
	var receipt envelope.Receipt
	readAutonomyEvent(t, home, receiptID, &receipt)
	receipt.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	replaceAutonomyEvent(t, home, receiptID, &receipt)
}

func resealTerminalReceiptSnapshot(t *testing.T, home, receiptID string) {
	t.Helper()
	var receipt envelope.Receipt
	readAutonomyEvent(t, home, receiptID, &receipt)
	canonical, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	wantHash := "sha256:" + hex.EncodeToString(sum[:])

	path := filepath.Join(home, "events", "work.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	updated := 0
	for index, line := range lines {
		var event struct {
			To       string   `json:"to"`
			Attached []string `json:"attached"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if !containsRef(event.Attached, receiptID) || (event.To != "completed" && event.To != "failed" && event.To != "cancelled") {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatal(err)
		}
		fields["receipt_sha256"] = wantHash
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		lines[index] = string(encoded)
		updated++
	}
	if updated != 1 {
		t.Fatalf("terminal events resealed for %s = %d, want 1", receiptID, updated)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendReceiptProofRef(t *testing.T, home, receiptID, proofRef string) {
	t.Helper()
	var receipt envelope.Receipt
	readAutonomyEvent(t, home, receiptID, &receipt)
	receipt.ProofRefs = append(receipt.ProofRefs, proofRef)
	replaceAutonomyEvent(t, home, receiptID, &receipt)
}

func readAutonomyEvent(t *testing.T, home, id string, target any) []byte {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if target != nil {
		if err := json.Unmarshal(record.Bytes, target); err != nil {
			t.Fatal(err)
		}
	}
	return record.Bytes
}

func replaceAutonomyEvent(t *testing.T, home, id string, value any) {
	t.Helper()
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutJSON(id, value, "  "); err != nil {
		t.Fatal(err)
	}
}

func removeWorkEvidenceRefPrefix(t *testing.T, home, workID, prefix string) {
	t.Helper()
	path := filepath.Join(home, "work", "work", workID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var item envelope.WorkEnvelope
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	kept := []string{}
	for _, ref := range item.EvidenceRefs {
		if !strings.HasPrefix(ref, prefix) {
			kept = append(kept, ref)
		}
	}
	item.EvidenceRefs = kept
	updated, err := json.MarshalIndent(item, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(updated, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompileFailsClosedOnSymlinkedHotWorkAncestor(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "hot-ancestor-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "hot-ancestor", proofNow.Add(-24*time.Hour), sampleOptions{})
	replaceWithSymlink(t, filepath.Join(home, "work", "work"))

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "work path must not contain symlink beneath compound home") {
		t.Fatalf("symlinked hot work ancestor did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnSymlinkedArchivedWorkAncestor(t *testing.T) {
	home := t.TempDir()
	if err := homepkg.Init(home, "local", proofNow.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "archive-ancestor-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	victim := seedSample(t, home, plan, "archive-ancestor", proofNow.Add(-24*time.Hour), sampleOptions{})
	archiveAutonomyWork(t, home, victim.WorkID)
	replaceWithSymlink(t, filepath.Join(homepkg.OperationalArchiveFilesRoot(home), "work", "work"))

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "archive path must not contain symlink beneath compound home") {
		t.Fatalf("symlinked archived work ancestor did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnSymlinkedEventsAncestor(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "events-ancestor-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "events-ancestor", proofNow.Add(-24*time.Hour), sampleOptions{})
	replaceWithSymlink(t, filepath.Join(home, "events"))

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "work event ledger path must not contain symlink beneath compound home") {
		t.Fatalf("symlinked events ancestor did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnSymlinkedWorkEventLedger(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "work-ledger-symlink-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "work-ledger-symlink", proofNow.Add(-24*time.Hour), sampleOptions{})
	replaceWithSymlink(t, filepath.Join(home, "events", "work.jsonl"))

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "work event ledger file must be a regular file") {
		t.Fatalf("symlinked work event ledger did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnSymlinkedRouteEvidenceLedger(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "route-ledger-symlink-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "route-ledger-symlink", proofNow.Add(-24*time.Hour), sampleOptions{})
	replaceWithSymlink(t, filepath.Join(home, "ledger", "execution-route-evidence.jsonl"))

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "route evidence ledger file must be a regular file") {
		t.Fatalf("symlinked route evidence ledger did not fail closed: %v", err)
	}
}

func TestCompileFailsClosedOnRouteEvidenceLedgerWithoutFinalNewline(t *testing.T) {
	home := t.TempDir()
	plan := installPlan(t, home, false)
	seedSample(t, home, plan, "route-framing-clean", proofNow.Add(-48*time.Hour), sampleOptions{})
	seedSample(t, home, plan, "route-framing-final", proofNow.Add(-24*time.Hour), sampleOptions{})
	path := filepath.Join(home, "ledger", "execution-route-evidence.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("route evidence fixture was not newline framed")
	}
	if err := os.WriteFile(path, raw[:len(raw)-1], 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := compileArtifact(home, plan, 2); err == nil || !strings.Contains(err.Error(), "route evidence ledger must end with newline") {
		t.Fatalf("valid final route row without newline did not fail closed: %v", err)
	}
}

func replaceWithSymlink(t *testing.T, path string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), filepath.Base(path))
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func archiveAutonomyWork(t *testing.T, home, workID string) homepkg.OperationalArchiveEntry {
	t.Helper()
	rel := filepath.ToSlash(filepath.Join("work", "work", workID+".json"))
	report, err := homepkg.ArchiveOperationalHistory(home, homepkg.OperationalArchiveOptions{Kinds: []string{"work"}, Paths: []string{rel}}, proofNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("archive entries = %+v, want one", report.Entries)
	}
	return report.Entries[0]
}

func rewriteArchiveEntry(t *testing.T, home, entryID string, mutate func(*homepkg.OperationalArchiveEntry)) {
	t.Helper()
	path := homepkg.OperationalArchiveIndexPath(home)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry homepkg.OperationalArchiveEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.ID == entryID && entry.Operation == "archived" {
			mutate(&entry)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(encoded))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mutateWorkClassRef(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var work envelope.WorkEnvelope
	if err := json.Unmarshal(raw, &work); err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, ref := range work.EvidenceRefs {
		if !strings.HasPrefix(ref, "director_trigger:job_health:") {
			refs = append(refs, ref)
		}
	}
	work.EvidenceRefs = refs
	mutated, err := json.MarshalIndent(work, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(mutated, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendArchiveEntry(t *testing.T, home string, entry homepkg.OperationalArchiveEntry) {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(homepkg.OperationalArchiveIndexPath(home), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertReason(t *testing.T, artifact autonomy.Artifact, want string) {
	t.Helper()
	if !strings.Contains(strings.Join(artifact.Reasons, "\n"), want) {
		t.Fatalf("reason %q missing from %v", want, artifact.Reasons)
	}
}

func assertSample(t *testing.T, artifact autonomy.Artifact, workID string, check func(autonomy.Sample)) {
	t.Helper()
	for _, sample := range artifact.Samples {
		if sample.WorkID == workID {
			check(sample)
			return
		}
	}
	t.Fatalf("sample %s missing from %+v", workID, artifact.Samples)
}

func containsRef(refs []string, want string) bool {
	for _, ref := range refs {
		if ref == want {
			return true
		}
	}
	return false
}

func stringPointer(value string) *string { return &value }
