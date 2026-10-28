package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/product"
	"compound/internal/sourceobs"
)

// TestReleaseAwareness_LiveConsumerOutput exercises the full path
// (intake -> persisted evidence -> BuildLoopModel -> product loops JSON)
// and asserts the JSON shape the orient consumer surfaces. This is the
// end-to-end proof the wire is wired: command output of `compound
// product loops --json` includes release_awareness when observations
// exist, and omits the field otherwise.
func TestReleaseAwareness_LiveConsumerOutput(t *testing.T) {
	homePath := t.TempDir()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	// 1. Apply a product registry so BuildLoopModel has something to enumerate.
	bundle := product.Bundle{
		SchemaVersion: product.SchemaVersion,
		Product: product.Product{
			ID:              "product:alpha",
			Name:            "Alpha",
			OwnerEntity:     "project:alpha",
			Surfaces:        []product.Surface{{ID: "app-store-page", Kind: "app_store", Ref: "appstore:1"}},
			Channels:        []string{"app_store"},
			Positioning:     "synthetic",
			PrivacyPosture:  "local_first",
			ReleaseIdentity: product.ReleaseIdentity{Version: "1.0.0", Build: "10"},
		},
		EvidenceRefs: []string{"seed:synthetic"},
	}
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatalf("mem.Open: %v", err)
	}
	if _, err := product.Apply(homePath, store, bundle, filepath.Join(homePath, "bundle.yaml"), now); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 2. Intake a typed release observation through the carrier door.
	obs := sourceobs.ReleaseObservation{
		ProductID:       "product:alpha",
		Channel:         "app_store_connect",
		Platform:        "ios",
		ObservedVersion: "1.2.0",
		ObservedBuild:   "build-12",
		ObservedState:   "live",
		ObservedAt:      now.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
		SourceRevision:  "rev-synthetic-1.2.0",
		EvidenceRef:     "appstore:1:version:1.2.0",
	}
	src, err := sourceobs.FromReleaseObservation(obs, now)
	if err != nil {
		t.Fatalf("FromReleaseObservation: %v", err)
	}
	if _, err := sourceobs.Apply(homePath, src, now); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// 3. Build the loop model and marshal to JSON — this is the exact
	//    `compound product loops --json` shape.
	model, err := product.BuildLoopModel(homePath, product.LoopOptions{ProductID: "product:alpha", Now: now, StaleAfter: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("BuildLoopModel: %v", err)
	}
	data, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	t.Logf("LIVE consumer output (compound product loops --json):\n%s", got)

	// Required assertions:
	if !strings.Contains(got, `"release_awareness"`) {
		t.Fatalf("loops JSON missing release_awareness: %s", got)
	}
	if !strings.Contains(got, `"status": "current"`) {
		t.Fatalf("loops JSON missing status=current: %s", got)
	}
	if !strings.Contains(got, `"outdated_contract": true`) {
		t.Fatalf("loops JSON missing outdated_contract=true: %s", got)
	}
	if !strings.Contains(got, `"channel_platforms"`) {
		t.Fatalf("loops JSON missing channel_platforms: %s", got)
	}
	if !strings.Contains(got, `"app_store_connect"`) {
		t.Fatalf("loops JSON missing channel name: %s", got)
	}
	if !strings.Contains(got, `"ios"`) {
		t.Fatalf("loops JSON missing platform name: %s", got)
	}
	if !strings.Contains(got, `"contract_version": "1.0.0"`) {
		t.Fatalf("loops JSON missing recalled contract_version: %s", got)
	}
	if !strings.Contains(got, `"evidence_only"`) && !strings.Contains(got, `"boundary"`) {
		t.Fatalf("loops JSON missing boundary or evidence-only marker: %s", got)
	}

	// 4. The operator consumer reads the product-owned compact summary.
	orientModel, err := buildOrientProductLoops(homePath, 20, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(orientModel.Products) != 1 {
		t.Fatalf("orient consumer omitted release-only product: %+v", orientModel)
	}
	orientView := orientModel.Products[0].ReleaseAwareness
	if orientView == nil {
		t.Fatalf("orient consumer returned nil for non-empty awareness")
	}
	orientData, err := json.MarshalIndent(orientView, "", "  ")
	if err != nil {
		t.Fatalf("Marshal orient: %v", err)
	}
	t.Logf("orient_release_awareness (subset of compound orient --operator --json):\n%s", string(orientData))

	if !strings.Contains(string(orientData), `"status": "current"`) {
		t.Fatalf("orient view missing status=current: %s", string(orientData))
	}
	if !strings.Contains(string(orientData), `"outdated_contract": true`) {
		t.Fatalf("orient view missing outdated_contract=true: %s", string(orientData))
	}
	if !strings.Contains(string(orientData), `"channel": "app_store_connect"`) {
		t.Fatalf("orient view missing channel: %s", string(orientData))
	}
	fmt.Printf("[CONSUMER-PROOF] orient_release_awareness projected\n")
}
