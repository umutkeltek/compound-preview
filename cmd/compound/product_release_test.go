package main

import (
	"strings"
	"testing"

	"compound/internal/mem"
	"compound/internal/product"
)

func TestProductReleasePlanCLIUsesCompoundIdentityAndPrintsReviewDoor(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle := product.Bundle{
		SchemaVersion: product.SchemaVersion,
		Product: product.Product{
			ID:             "product:cli-release-demo",
			Name:           "CLI Release Demo",
			OwnerEntity:    "owner:demo",
			Surfaces:       []product.Surface{{ID: "source", Kind: "repo", Ref: "repo:cli-release-demo"}},
			Channels:       []string{"app_store"},
			Positioning:    "Synthetic release CLI fixture.",
			PrivacyPosture: "public_safe",
			ReleaseIdentity: product.ReleaseIdentity{
				BundleID:             "com.example.cli-release-demo",
				AppStoreConnectAppID: "app-cli-release-demo",
				Version:              "3.1.0",
				Build:                "55",
			},
		},
		EvidenceRefs: []string{"fixture:cli-release-demo"},
	}
	if _, err := product.Apply(homePath, store, bundle, "cli-release-demo.yaml", cliTestNow); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		cmdProduct([]string{
			"release-plan",
			"--product", bundle.Product.ID,
			"--change", "Ship the verified CLI release fixture.",
			"--candidate", "commit:dddddddddddddddddddddddddddddddddddddddd",
			"--rollback", "Restore the previous App Store version.",
			"--metric", "app_store_connect_units",
			"--action-card", "product-loop-review-next-cli-release-demo",
		})
	})
	for _, want := range []string{
		"# product release-plan",
		"status: review_required",
		"version: 3.1.0",
		"build: 55",
		"mechanism: app_store_connect",
		"review_yaml:",
		"external_mutation_allowed: false",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("release-plan output missing %q:\n%s", want, out)
		}
	}
}
