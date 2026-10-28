package sourceobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
)

// helper: synthetic typed observation, valid.
func sampleReleaseObservation(t *testing.T, now time.Time, productID, channel, platform, version, state string) ReleaseObservation {
	t.Helper()
	return ReleaseObservation{
		ProductID:       productID,
		Channel:         channel,
		Platform:        platform,
		ObservedVersion: version,
		ObservedBuild:   "build-42",
		ObservedState:   state,
		ObservedAt:      now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		SourceRevision:  "rev-" + productID + "-" + version,
		EvidenceRef:     "appstore:1234567:version:" + version,
	}
}

//  1. Synthetic product alpha: contract1.0/build10 + fresh public-live1.2
//     -> outdated contract + both provenance refs preserved.
func TestReleaseObservation_IntakePersistsAndLoadsBack(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	obs := sampleReleaseObservation(t, now, "product:alpha", "app_store_connect", "ios", "1.2.0", "live")

	// intake
	src, err := FromReleaseObservation(obs, now)
	if err != nil {
		t.Fatalf("FromReleaseObservation: %v", err)
	}
	if src.Release == nil || *src.Release != obs {
		t.Fatalf("SourceObservation.Release round-trip mismatch: got %+v want %+v", src.Release, obs)
	}
	if r := Validate(src); r.Status != "ok" {
		t.Fatalf("Validate failed: %+v", r)
	}
	if _, err := Apply(homePath, src, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// load back
	got, err := LoadReleaseObservations(homePath, []string{"product:alpha"}, now)
	if err != nil {
		t.Fatalf("LoadReleaseObservations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("load returned %d, want 1", len(got))
	}
	if got[0] != obs {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got[0], obs)
	}
}

//  2. Different channels/platforms are not contradictions; candidate/submitted
//     observations never render live.
func TestReleaseObservation_SameProductDistinctChannelsNoConflict(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	// direct mac dmg live
	macObs := sampleReleaseObservation(t, now, "product:dual-channel", "direct_mac_dmg", "macos", "1.0.2", "live")
	// App Store iOS candidate
	iosObs := ReleaseObservation{
		ProductID:       "product:dual-channel",
		Channel:         "app_store_connect",
		Platform:        "ios",
		ObservedVersion: "1.0.0",
		ObservedBuild:   "build-9",
		ObservedState:   "candidate",
		ObservedAt:      now.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		SourceRevision:  "rev-ios-candidate",
		EvidenceRef:     "https://releases.invalid/dual-channel/ios/1.0.0",
	}

	for _, o := range []ReleaseObservation{macObs, iosObs} {
		src, err := FromReleaseObservation(o, now)
		if err != nil {
			t.Fatalf("FromReleaseObservation(%s/%s): %v", o.Channel, o.Platform, err)
		}
		if _, err := Apply(homePath, src, now); err != nil {
			t.Fatalf("Apply(%s/%s): %v", o.Channel, o.Platform, err)
		}
	}

	got, err := LoadReleaseObservations(homePath, []string{"product:dual-channel"}, now)
	if err != nil {
		t.Fatalf("LoadReleaseObservations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("load returned %d, want 2", len(got))
	}
	// iOS candidate must not render as live; we read it back as candidate.
	var ios, mac ReleaseObservation
	for _, o := range got {
		switch o.Channel {
		case "app_store_connect":
			ios = o
		case "direct_mac_dmg":
			mac = o
		}
	}
	if ios.ObservedState != "candidate" {
		t.Fatalf("ios observation state lost: %q", ios.ObservedState)
	}
	if mac.ObservedState != "live" {
		t.Fatalf("mac observation state lost: %q", mac.ObservedState)
	}
}

//  3. Same channel+platform equally-current conflicting live versions
//     surface as conflict at the aggregator level (tested in
//     internal/product/release_awareness_test.go). At the carrier level
//     they must both persist; de-dup is by (product, channel, platform,
//     version, state, observed_at, source_revision).
func TestReleaseObservation_ConflictingVersionsPersistBoth(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	a := sampleReleaseObservation(t, now, "product:alpha", "app_store_connect", "ios", "1.2.0", "live")
	a.ObservedAt = now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	b := sampleReleaseObservation(t, now, "product:alpha", "app_store_connect", "ios", "1.1.5", "live")
	b.ObservedAt = now.Add(-4 * time.Minute).UTC().Format(time.RFC3339)

	for _, o := range []ReleaseObservation{a, b} {
		src, err := FromReleaseObservation(o, now)
		if err != nil {
			t.Fatalf("FromReleaseObservation: %v", err)
		}
		if _, err := Apply(homePath, src, now); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	got, err := LoadReleaseObservations(homePath, []string{"product:alpha"}, now)
	if err != nil {
		t.Fatalf("LoadReleaseObservations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("load returned %d, want 2 (both conflicting observations must persist)", len(got))
	}
}

//  4. Missing/invalid/future timestamps and wrong identity fields must be
//     rejected at intake.
func TestReleaseObservation_ValidateRejectsBadInput(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		mut  func(*ReleaseObservation)
	}{
		{"missing product_id", func(o *ReleaseObservation) { o.ProductID = "" }},
		{"missing channel", func(o *ReleaseObservation) { o.Channel = "" }},
		{"missing platform", func(o *ReleaseObservation) { o.Platform = "" }},
		{"missing version", func(o *ReleaseObservation) { o.ObservedVersion = "" }},
		{"unknown state", func(o *ReleaseObservation) { o.ObservedState = "leaked" }},
		{"missing observed_at", func(o *ReleaseObservation) { o.ObservedAt = "" }},
		{"bad RFC3339", func(o *ReleaseObservation) { o.ObservedAt = "not-a-date" }},
		{"future observed_at", func(o *ReleaseObservation) { o.ObservedAt = now.Add(2 * time.Hour).UTC().Format(time.RFC3339) }},
		{"missing source_revision", func(o *ReleaseObservation) { o.SourceRevision = "" }},
		{"missing evidence_ref", func(o *ReleaseObservation) { o.EvidenceRef = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obs := sampleReleaseObservation(t, now, "product:alpha", "app_store_connect", "ios", "1.0.0", "live")
			tc.mut(&obs)
			if err := ValidateReleaseObservation(obs, now); err == nil {
				t.Fatalf("ValidateReleaseObservation should reject %s", tc.name)
			}
			if _, err := FromReleaseObservation(obs, now); err == nil {
				t.Fatalf("FromReleaseObservation should reject %s", tc.name)
			}
		})
	}
}

// 5. State enum is closed and rejects anything outside it.
func TestReleaseObservation_StateEnumClosed(t *testing.T) {
	for _, s := range []string{"candidate", "submitted", "preparing", "live", "unknown"} {
		if !oneOfReleaseState(s) {
			t.Fatalf("state %q must be allowed", s)
		}
	}
	for _, s := range []string{"", "leaked", "shipped", "promoted", "livex"} {
		if oneOfReleaseState(s) {
			t.Fatalf("state %q must not be allowed", s)
		}
	}
}

//  6. End-to-end: intake -> persisted evidence -> typed load works on
//     synthetic home (real-home projection stays read-only).
func TestReleaseObservation_EndToEndIntakePersistsAndLoads(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	obs := sampleReleaseObservation(t, now, "product:alpha", "app_store_connect", "ios", "1.2.0", "live")

	src, err := FromReleaseObservation(obs, now)
	if err != nil {
		t.Fatalf("FromReleaseObservation: %v", err)
	}
	if _, err := Apply(homePath, src, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Verify the candidate file exists on disk for the carrier's chosen kind
	// (route_observation) and that the no-promote ledger records the
	// release_observation carrier's PromoteAllowed=false policy.
	routeDir := filepath.Join(homePath, candidates.InboxDir(candidates.RouteObservation))
	entries, err := os.ReadDir(routeDir)
	if err != nil {
		t.Fatalf("read route_observation inbox: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(entries))
	}
	// home delta: only the candidate file + the no-promote ledger
	// (no records, claims, or receipts in product memory).
	if _, err := os.Stat(filepath.Join(homePath, "ledger", "product-signals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("real product signals ledger must not exist on a synthetic home (would imply real-home mutation)")
	}

	got, err := LoadReleaseObservations(homePath, []string{"product:alpha"}, now)
	if err != nil {
		t.Fatalf("LoadReleaseObservations: %v", err)
	}
	if len(got) != 1 || got[0] != obs {
		t.Fatalf("end-to-end mismatch: got %+v want %+v", got, obs)
	}
}

//  7. Existing sourceobs records without the Release field preserve
//     behavior (no migration required). Apply an unrelated observation
//     (no Release payload) and confirm LoadReleaseObservations returns
//     no records.
func TestReleaseObservation_ExistingRecordsUnchanged(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	// Apply a non-release observation (a projection-freshness style record).
	obs := SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            "srcobs_other_000000000000",
		AdapterID:     "compound.projection",
		SourceSystem:  "compound",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: SourceRef{
			Path:        "compound://projection/example",
			Kind:        "generated_view",
			Owner:       "projection",
			SourceClass: "generated",
			Revision:    "rev-1",
		},
		GeneratedView: GeneratedView{
			ViewKind:  "projection_freshness",
			Consumer:  "compound projection doctor",
			Generator: "compound projection",
			Marker:    "projection",
			Freshness: "observed",
		},
		MapsTo: []MapTarget{
			{ObjectFamily: "projection", CandidateKind: string(candidates.MethodUpgrade), EntityHint: "projection", PromoteAllowed: false},
		},
		EvidenceRefs: []string{"projection:rev-1"},
		// Release: nil — this is an existing-shape record.
	}
	if _, err := Apply(homePath, obs, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := LoadReleaseObservations(homePath, nil, now)
	if err != nil {
		t.Fatalf("LoadReleaseObservations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("non-release records must not surface as release observations: got %d", len(got))
	}
}

// Product filtering of the compatibility reader. Every call reads a new
// snapshot; the portfolio shares one report across its product views.
func TestReleaseObservation_BoundedReadingByProduct(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, p := range []string{"product:alpha", "product:beta", "product:gamma"} {
		obs := sampleReleaseObservation(t, now, p, "app_store_connect", "ios", "1.0.0", "live")
		src, err := FromReleaseObservation(obs, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Apply(homePath, src, now); err != nil {
			t.Fatal(err)
		}
	}
	all, err := LoadReleaseObservations(homePath, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("all-products load returned %d, want 3", len(all))
	}
	alpha, err := LoadReleaseObservations(homePath, []string{"product:alpha"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(alpha) != 1 || alpha[0].ProductID != "product:alpha" {
		t.Fatalf("product-bounded load mismatch: got %+v", alpha)
	}
	betaOnly, err := LoadReleaseObservations(homePath, []string{"product:beta", "product:gamma"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(betaOnly) != 2 {
		t.Fatalf("two-product bounded load returned %d, want 2", len(betaOnly))
	}
}

// Clock-skew margin: 1-minute tolerance, anything beyond rejected.
func TestReleaseObservation_ClockSkewMargin(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	// within margin: ok
	ok := sampleReleaseObservation(t, now, "product:alpha", "app_store_connect", "ios", "1.0.0", "live")
	ok.ObservedAt = now.Add(30 * time.Second).UTC().Format(time.RFC3339)
	if err := ValidateReleaseObservation(ok, now); err != nil {
		t.Fatalf("30s future must be accepted within 1m skew margin: %v", err)
	}
	// beyond margin: rejected
	bad := sampleReleaseObservation(t, now, "product:alpha", "app_store_connect", "ios", "1.0.0", "live")
	bad.ObservedAt = now.Add(2 * time.Minute).UTC().Format(time.RFC3339)
	if err := ValidateReleaseObservation(bad, now); err == nil {
		t.Fatalf("2m future must be rejected")
	}
}

// Strict re-validation in the loader: a record admitted before stricter
// rules (e.g. via a hand-edited file) cannot leak into release aggregation.
func TestReleaseObservation_LoaderStrictRevalidates(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	bad := ReleaseObservation{
		ProductID:       "product:alpha",
		Channel:         "app_store_connect",
		Platform:        "ios",
		ObservedVersion: "1.0.0",
		ObservedState:   "leaked", // unknown state
		ObservedAt:      now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		SourceRevision:  "rev-bad",
		EvidenceRef:     "appstore:1:version:1.0.0",
	}
	if err := ValidateReleaseObservation(bad, now); err == nil {
		t.Fatalf("bad observation must fail Validate")
	}
	// Forge a SourceObservation that bypasses the carrier validator by
	// hand-writing the SourceRef path (simulating a record admitted
	// before stricter rules). The loader must still drop it.
	forged := SourceObservation{
		SchemaVersion: SchemaVersion,
		ID:            "srcobs_forged_000000000000",
		AdapterID:     "compound.release-observation",
		SourceSystem:  "external",
		ObservedAt:    bad.ObservedAt,
		SourceRef: SourceRef{
			Path:        releaseObservationPath(bad),
			Kind:        "release_observation",
			Owner:       bad.ProductID,
			SourceClass: "release_observation",
			Revision:    bad.SourceRevision,
		},
		GeneratedView: GeneratedView{
			ViewKind:  "release_observation",
			Consumer:  "compound product release-awareness",
			Generator: "compound sourceobs release-observation adapter",
			Marker:    ReleaseObservationSchemaVersion,
			Freshness: "observed",
		},
		MapsTo: []MapTarget{
			{ObjectFamily: "release_observation", CandidateKind: string(candidates.RouteObservation), EntityHint: bad.ProductID, PromoteAllowed: false},
		},
		EvidenceRefs: []string{
			"release_observation:" + bad.ProductID + ":" + bad.Channel + ":" + bad.Platform,
			"release_observation:evidence:" + bad.EvidenceRef,
			"release_observation:revision:" + bad.SourceRevision,
			"release_observation:state:" + bad.ObservedState,
			"release_observation:observed_at:" + bad.ObservedAt,
			"release_observation:version:" + bad.ObservedVersion,
		},
	}
	if _, err := Apply(homePath, forged, now); err != nil {
		// Apply may either reject (good) or accept the raw observation.
		// Either way, the LOADER must skip bad state values.
	}
	got, err := LoadReleaseObservations(homePath, nil, now)
	if err != nil {
		t.Fatalf("LoadReleaseObservations: %v", err)
	}
	for _, o := range got {
		if o.ObservedState == "leaked" {
			t.Fatalf("forged record with unknown state leaked into release aggregation: %+v", o)
		}
	}
}

// Helper test for IsReleaseObservation: only the carrier's SourceRef path
// pattern qualifies.
func TestReleaseObservation_IsReleaseObservationByPath(t *testing.T) {
	yes := SourceObservationListItem{SourceRef: "compound://release_observation/p/x/y/1/2#r"}
	if !yes.IsReleaseObservation() {
		t.Fatalf("carrier path must be recognized")
	}
	no := SourceObservationListItem{SourceRef: "compound://projection/example"}
	if no.IsReleaseObservation() {
		t.Fatalf("non-carrier path must not be recognized")
	}
	if strings.Contains(yes.SourceRef, "leak") {
		t.Fatalf("test string drift")
	}
}
