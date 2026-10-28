package product

import (
	"encoding/json"
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/sourceobs"
)

// helper: import a typed release observation through the carrier door.
func importReleaseObs(t *testing.T, homePath string, now time.Time, productID, channel, platform, version, state string, ageMinutes int, revision string) {
	t.Helper()
	obs := sourceobs.ReleaseObservation{
		ProductID:       productID,
		Channel:         channel,
		Platform:        platform,
		ObservedVersion: version,
		ObservedBuild:   "build-" + version,
		ObservedState:   state,
		ObservedAt:      now.Add(-time.Duration(ageMinutes) * time.Minute).UTC().Format(time.RFC3339),
		SourceRevision:  revision,
		EvidenceRef:     "appstore:1:version:" + version + ":state:" + state,
	}
	src, err := sourceobs.FromReleaseObservation(obs, now)
	if err != nil {
		t.Fatalf("FromReleaseObservation: %v", err)
	}
	if _, err := sourceobs.Apply(homePath, src, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// Falsifier 1: Synthetic product alpha with recalled contract version 1.0.0
// and a fresh public-live observation reporting 1.2.0 -> status="current",
// OutdatedContract=true, both provenance refs preserved.
func TestReleaseAwareness_Falsifier1_OutdatedContract(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	importReleaseObs(t, homePath, now, "product:alpha", "app_store_connect", "ios", "1.2.0", "live", 30, "rev-live-1.2.0")
	aw, err := BuildReleaseAwareness(homePath, "product:alpha", ReleaseIdentity{Version: "1.0.0", Build: "10"}, ReleaseAwarenessOptions{Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("BuildReleaseAwareness: %v", err)
	}
	if aw.Status != "current" {
		t.Fatalf("status = %q, want current", aw.Status)
	}
	if !aw.OutdatedContract {
		t.Fatalf("OutdatedContract = false, want true (observed live 1.2.0 vs contract 1.0.0)")
	}
	if aw.LatestVersion != "1.2.0" {
		t.Fatalf("LatestVersion = %q, want 1.2.0", aw.LatestVersion)
	}
	if aw.ContractVersion != "1.0.0" {
		t.Fatalf("ContractVersion = %q, want 1.0.0 (recalled identity must surface with provenance, not auto-overwrite)", aw.ContractVersion)
	}
	if len(aw.ChannelPlatforms) != 1 || aw.ChannelPlatforms[0].CurrentVersion != "1.2.0" {
		t.Fatalf("ChannelPlatforms mismatch: %+v", aw.ChannelPlatforms)
	}
	if len(aw.ChannelPlatforms[0].EvidenceRefs) < 3 {
		t.Fatalf("evidence refs lost: %+v", aw.ChannelPlatforms[0].EvidenceRefs)
	}
}

// Falsifier 2: same product with direct-mac-live1.0.2 and App Store iOS
// candidate1.0.0 -> two ChannelPlatforms, no conflict (different channel+platform).
func TestReleaseAwareness_Falsifier2_ChannelSeparation(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	importReleaseObs(t, homePath, now, "product:sayframe", "direct_mac_dmg", "macos", "1.0.2", "live", 60, "rev-mac-1.0.2")
	importReleaseObs(t, homePath, now, "product:sayframe", "app_store_connect", "ios", "1.0.0", "candidate", 30, "rev-ios-1.0.0")
	aw, err := BuildReleaseAwareness(homePath, "product:sayframe", ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("BuildReleaseAwareness: %v", err)
	}
	if len(aw.ChannelPlatforms) != 2 {
		t.Fatalf("ChannelPlatforms = %d, want 2 (channel separation is load-bearing)", len(aw.ChannelPlatforms))
	}
	if len(aw.Conflict) != 0 {
		t.Fatalf("different channels must NOT raise Conflict: %+v", aw.Conflict)
	}
	// candidate must never render live: it surfaces as its own state.
	for _, view := range aw.ChannelPlatforms {
		if view.Channel == "app_store_connect" {
			if view.Latest.ObservedState != "candidate" {
				t.Fatalf("candidate rendered as %q", view.Latest.ObservedState)
			}
			if view.Latest.ObservedVersion != "1.0.0" {
				t.Fatalf("candidate version lost: %q", view.Latest.ObservedVersion)
			}
		}
		if view.Channel == "direct_mac_dmg" {
			if view.Latest.ObservedState != "live" {
				t.Fatalf("live mac observation mis-rendered: %q", view.Latest.ObservedState)
			}
		}
	}
}

// Falsifier 3: same channel+platform, equally-current conflicting live
// versions -> status="conflicting", Conflict[] populated, bounded next action.
func TestReleaseAwareness_Falsifier3_SameChannelEqualCurrentConflict(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	// Same observation instant: incompatible live versions must remain unresolved.
	importReleaseObs(t, homePath, now, "product:alpha", "app_store_connect", "ios", "1.2.0", "live", 5, "rev-a-1.2.0")
	importReleaseObs(t, homePath, now, "product:alpha", "app_store_connect", "ios", "1.1.5", "live", 5, "rev-b-1.1.5")
	aw, err := BuildReleaseAwareness(homePath, "product:alpha", ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("BuildReleaseAwareness: %v", err)
	}
	if aw.Status != "conflicting" {
		t.Fatalf("status = %q, want conflicting (same channel+platform, equally-current, disagree on version)", aw.Status)
	}
	if len(aw.Conflict) < 2 {
		t.Fatalf("Conflict[] should name both versions, got %+v", aw.Conflict)
	}
	if aw.NextAction == "" {
		t.Fatalf("NextAction required for conflict")
	}
	if !contains(aw.NextAction, "reconcile") {
		t.Fatalf("NextAction must name a bounded safe move (operator-reviewed reconcile), got %q", aw.NextAction)
	}
}

// Falsifier 4: malformed observations (future timestamps, bad RFC3339)
// must be rejected at intake (carrier Validate), so BuildReleaseAwareness
// sees none. We prove the end-to-end: the loader does not surface
// observations whose timestamps fail strict re-validation.
func TestReleaseAwareness_Falsifier4_MalformedFiltered(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	// import a valid one first
	importReleaseObs(t, homePath, now, "product:alpha", "app_store_connect", "ios", "1.0.0", "live", 30, "rev-1")
	// forge a candidate with future observed_at (would fail strict re-validation)
	forge := sourceobs.SourceObservation{
		SchemaVersion: sourceobs.SchemaVersion,
		ID:            "srcobs_forge_000000000000",
		AdapterID:     "compound.release-observation",
		SourceSystem:  "external",
		ObservedAt:    now.Add(2 * time.Hour).UTC().Format(time.RFC3339),
		SourceRef: sourceobs.SourceRef{
			Path:        "compound://release_observation/product:alpha/app_store_connect/ios/1.0.0/live/" + now.Add(2*time.Hour).UTC().Format(time.RFC3339) + "#rev-future",
			Kind:        "release_observation",
			Owner:       "product:alpha",
			SourceClass: "release_observation",
			Revision:    "rev-future",
		},
		GeneratedView: sourceobs.GeneratedView{
			ViewKind:  "release_observation",
			Consumer:  "compound product release-awareness",
			Generator: "compound sourceobs release-observation adapter",
			Marker:    sourceobs.ReleaseObservationSchemaVersion,
			Freshness: "observed",
		},
		MapsTo: []sourceobs.MapTarget{
			{ObjectFamily: "release_observation", CandidateKind: "route_observation", EntityHint: "product:alpha", PromoteAllowed: false},
		},
		EvidenceRefs: []string{
			"release_observation:product:alpha:app_store_connect:ios",
			"release_observation:evidence:appstore:fut",
			"release_observation:revision:rev-future",
			"release_observation:state:live",
			"release_observation:observed_at:" + now.Add(2*time.Hour).UTC().Format(time.RFC3339),
			"release_observation:version:9.9.9",
		},
	}
	if _, err := sourceobs.Apply(homePath, forge, now); err != nil {
		// Apply may reject; either way the LOADER must skip future timestamps.
	}
	aw, err := BuildReleaseAwareness(homePath, "product:alpha", ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("BuildReleaseAwareness: %v", err)
	}
	for _, view := range aw.ChannelPlatforms {
		if view.Latest.ObservedVersion == "9.9.9" {
			t.Fatalf("future-timestamped forged observation leaked: %+v", view.Latest)
		}
	}
}

// Falsifier 5: fresh metrics do NOT inflate stale release awareness. We
// import only an old observation (8 days ago) and verify status=stale
// even though we don't import any signal at all (the point is that
// release awareness is computed independently of any signal freshness).
func TestReleaseAwareness_Falsifier5_StaleIndependentOfMetricFreshness(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	importReleaseObs(t, homePath, now, "product:alpha", "app_store_connect", "ios", "1.0.0", "live", 60*24*8, "rev-old")
	aw, err := BuildReleaseAwareness(homePath, "product:alpha", ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("BuildReleaseAwareness: %v", err)
	}
	if aw.Status != "stale" {
		t.Fatalf("status = %q, want stale (observation older than 7d window)", aw.Status)
	}
	if aw.ObservationCount != 1 {
		t.Fatalf("ObservationCount = %d, want 1", aw.ObservationCount)
	}
}

// Falsifier 6: SourceObservation intake -> persisted evidence -> product
// loops JSON production path. BuildLoopModel must surface ReleaseAwareness.
func TestReleaseAwareness_Falsifier6_LoopsJSONSurfacing(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	importReleaseObs(t, homePath, now, "product:alpha", "app_store_connect", "ios", "1.2.0", "live", 30, "rev-1.2.0")
	// seed a minimal product registry so BuildLoopModel has something to
	// enumerate; product_id is "product:alpha".
	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:              "product:alpha",
			Name:            "Alpha",
			OwnerEntity:     "project:alpha",
			Surfaces:        []Surface{{ID: "app-store-page", Kind: "app_store", Ref: "appstore:1"}},
			Channels:        []string{"app_store"},
			Positioning:     "test",
			PrivacyPosture:  "local_first",
			ReleaseIdentity: ReleaseIdentity{Version: "1.0.0", Build: "10"},
		},
		EvidenceRefs: []string{"seed:test"},
	}
	store, err := openMemForTest(homePath)
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	if _, err := Apply(homePath, store, bundle, "seed:test", now); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	model, err := BuildLoopModel(homePath, LoopOptions{ProductID: "product:alpha", Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("BuildLoopModel: %v", err)
	}
	if len(model.Products) != 1 {
		t.Fatalf("model.Products = %d, want 1", len(model.Products))
	}
	if model.Products[0].ReleaseAwareness == nil {
		t.Fatalf("ReleaseAwareness must surface in product loops JSON")
	}
	if model.Products[0].ReleaseAwareness.Status != "current" {
		t.Fatalf("ReleaseAwareness.Status = %q, want current", model.Products[0].ReleaseAwareness.Status)
	}
	if !model.Products[0].ReleaseAwareness.OutdatedContract {
		t.Fatalf("ReleaseAwareness.OutdatedContract = false, want true")
	}
}

// Falsifier 7: backward compatibility — a product with NO release
// observations returns Status="unobserved" and the loop JSON omits the
// field (omitempty). Existing product JSON consumers see no diff.
func TestReleaseAwareness_Falsifier7_UnobservedProduct(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	aw, err := BuildReleaseAwareness(homePath, "product:nothing", ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("BuildReleaseAwareness: %v", err)
	}
	if aw.Status != "unobserved" {
		t.Fatalf("status = %q, want unobserved", aw.Status)
	}
	if aw.ObservationCount != 0 {
		t.Fatalf("ObservationCount = %d, want 0", aw.ObservationCount)
	}
	if aw.LatestVersion != "" || aw.LatestObservedAt != "" {
		t.Fatalf("unobserved product must not pretend to have observations: %+v", aw)
	}
}

// Direct-call product filtering. The counted real-reader proof is
// TestReleaseTruthPortfolioReadsOneFreshSnapshot; no cross-call cache exists.
func TestReleaseAwareness_Falsifier8_BoundedReading(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, p := range []string{"product:alpha", "product:beta", "product:gamma"} {
		importReleaseObs(t, homePath, now, p, "app_store_connect", "ios", "1.0.0", "live", 30, "rev-"+p)
	}
	for _, p := range []string{"product:alpha", "product:beta", "product:gamma"} {
		aw, err := BuildReleaseAwareness(homePath, p, ReleaseIdentity{}, ReleaseAwarenessOptions{Now: now, StaleAfter: 7 * 24 * time.Hour})
		if err != nil {
			t.Fatalf("BuildReleaseAwareness(%s): %v", p, err)
		}
		if aw.ObservationCount != 1 {
			t.Fatalf("product %s saw %d observations, want 1 (filter must scope per product)", p, aw.ObservationCount)
		}
	}
}

// Helper: the product LoopModel also has to compile with the new field
// and produce valid JSON. We round-trip the model through DecodeLoopModel
// to confirm.
func TestReleaseAwareness_LoopModelJSONRoundtrip(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	importReleaseObs(t, homePath, now, "product:alpha", "app_store_connect", "ios", "1.2.0", "live", 30, "rev-1.2.0")
	bundle := Bundle{
		SchemaVersion: SchemaVersion,
		Product: Product{
			ID:              "product:alpha",
			Name:            "Alpha",
			OwnerEntity:     "project:alpha",
			Surfaces:        []Surface{{ID: "app-store-page", Kind: "app_store", Ref: "appstore:1"}},
			Channels:        []string{"app_store"},
			Positioning:     "test",
			PrivacyPosture:  "local_first",
			ReleaseIdentity: ReleaseIdentity{Version: "1.0.0", Build: "10"},
		},
		EvidenceRefs: []string{"seed:test"},
	}
	store, err := openMemForTest(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(homePath, store, bundle, "seed:test", now); err != nil {
		t.Fatal(err)
	}
	model, err := BuildLoopModel(homePath, LoopOptions{ProductID: "product:alpha", Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	data, err := jsonMarshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), "\"release_awareness\"") {
		t.Fatalf("loop JSON must include release_awareness: %s", string(data))
	}
	if !contains(string(data), "\"outdated_contract\":true") {
		t.Fatalf("loop JSON must surface outdated_contract=true: %s", string(data))
	}
	if !contains(string(data), "\"channel_platforms\"") {
		t.Fatalf("loop JSON must include channel_platforms: %s", string(data))
	}
	// round-trip
	decoded, err := DecodeLoopModel(data)
	if err != nil {
		t.Fatalf("DecodeLoopModel: %v", err)
	}
	if decoded.Products[0].ReleaseAwareness == nil {
		t.Fatalf("decoded ReleaseAwareness lost: %+v", decoded)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func openMemForTest(homePath string) (*mem.Store, error) { return mem.Open(homePath) }
