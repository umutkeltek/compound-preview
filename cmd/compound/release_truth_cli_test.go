package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/mem"
	"compound/internal/product"
	"compound/internal/sourceobs"
)

func TestReleaseTruthMixedStateActualCLI(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	bundle := product.Bundle{SchemaVersion: product.SchemaVersion, Product: product.Product{
		ID: "product:truth-cli", Name: "Truth CLI", OwnerEntity: "project:truth-cli", Surfaces: []product.Surface{{ID: "web", Kind: "web", Ref: "https://truth.invalid"}},
		Channels: []string{"direct", "store"}, Positioning: "Synthetic release proof", PrivacyPosture: "local_first", ReleaseIdentity: product.ReleaseIdentity{Version: "1.0", Build: "10"},
	}, EvidenceRefs: []string{"fixture:truth-cli"}}
	if _, err := product.Apply(homePath, store, bundle, "fixture://truth-cli", now); err != nil {
		t.Fatal(err)
	}
	binary := compoundTestBinary(t)
	run := func(t *testing.T) ([]byte, error) {
		t.Helper()
		before := releaseCLIHomeSnapshot(t, homePath)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "product", "loops", "--product", bundle.Product.ID, "--json")
		out, err := cmd.CombinedOutput()
		if !reflect.DeepEqual(before, releaseCLIHomeSnapshot(t, homePath)) {
			t.Fatal("actual CLI mutated home")
		}
		return out, err
	}
	add := func(channel, state, version string, age time.Duration, invalid bool) string {
		t.Helper()
		obs := sourceobs.ReleaseObservation{ProductID: bundle.Product.ID, Channel: channel, Platform: "desktop", ObservedVersion: version, ObservedBuild: "build-" + version, ObservedState: state, ObservedAt: now.Add(-age).Format(time.RFC3339), SourceRevision: "rev-" + version, EvidenceRef: "https://evidence.invalid/" + channel + "/" + version}
		src, err := sourceobs.FromReleaseObservation(obs, now)
		if err != nil {
			t.Fatal(err)
		}
		if invalid {
			src.ID += "_future"
			src.Release.ObservedAt = now.Add(time.Hour).Format(time.RFC3339)
		}
		report, err := sourceobs.Apply(homePath, src, now)
		if err != nil {
			t.Fatal(err)
		}
		return report.CandidateIDs[0]
	}
	assertState := func(t *testing.T, status string) product.ReleaseAwareness {
		t.Helper()
		out, err := run(t)
		if err != nil {
			t.Fatalf("CLI: %v\n%s", err, out)
		}
		var model product.LoopModel
		if err := json.Unmarshal(out, &model); err != nil {
			t.Fatalf("CLI JSON: %v\n%s", err, out)
		}
		if len(model.Products) != 1 || model.Products[0].ReleaseAwareness == nil {
			t.Fatalf("missing release view: %s", out)
		}
		got := *model.Products[0].ReleaseAwareness
		if got.Status != status {
			t.Fatalf("CLI status=%s want %s: %s", got.Status, status, out)
		}
		beforeOrient := releaseCLIHomeSnapshot(t, homePath)
		orientModel, err := buildOrientProductLoops(homePath, 20, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(orientModel.Products) != 1 || orientModel.Products[0].ProductID != bundle.Product.ID || orientModel.Products[0].ReleaseAwareness == nil {
			t.Fatalf("orient consumer omitted release-only product: %+v", orientModel)
		}
		orient := orientModel.Products[0].ReleaseAwareness
		if orient.Status != got.Status || orient.ContractBuild != got.ContractBuild || orient.Boundary != got.Boundary {
			t.Fatalf("orient consumer changed release truth: %+v", orient)
		}
		if !reflect.DeepEqual(beforeOrient, releaseCLIHomeSnapshot(t, homePath)) {
			t.Fatal("orient release projection mutated its home")
		}
		if len(orient.ChannelPlatforms) != len(got.ChannelPlatforms) {
			t.Fatal("orient lost channels")
		}
		raw, err := json.Marshal(orient)
		if err != nil {
			t.Fatal(err)
		}
		var projected struct {
			Channels []struct {
				Status         string `json:"status"`
				CurrentVersion string `json:"current_version"`
			} `json:"channel_platforms"`
		}
		if err := json.Unmarshal(raw, &projected); err != nil {
			t.Fatal(err)
		}
		for i, view := range got.ChannelPlatforms {
			if projected.Channels[i].Status != view.Status || projected.Channels[i].CurrentVersion != view.CurrentVersion {
				t.Fatal("orient lost channel proof state")
			}
		}
		t.Logf("actual CLI status=%s latest=%s evidence=%d home unchanged", got.Status, got.LatestVersion, got.ObservationCount)
		return got
	}
	add("direct", "live", "2.0", 8*24*time.Hour, false)
	add("store", "candidate", "3.0", time.Hour, false)
	partial := assertState(t, "partial")
	for _, view := range partial.ChannelPlatforms {
		if view.CurrentVersion != "" {
			t.Fatalf("CLI made stale/candidate live: %+v", view)
		}
	}
	add("direct", "live", "2.1", 30*time.Minute, false)
	current := assertState(t, "current")
	if !current.OutdatedContract {
		t.Fatal("live contract mismatch hidden")
	}
	invalidID := add("direct", "live", "9.0", time.Minute, true)
	invalid := assertState(t, "invalid")
	if !strings.Contains(strings.Join(invalid.MissingEvidence, "\n"), invalidID) {
		t.Fatal("invalid candidate provenance hidden")
	}
	for _, view := range invalid.ChannelPlatforms {
		if view.Channel == "direct" && view.CurrentVersion != "" {
			t.Fatal("invalid channel claimed current")
		}
	}
	for _, kind := range []candidates.Kind{candidates.CapabilityObs, candidates.RouteObservation} {
		t.Run(string(kind)+" inventory corruption", func(t *testing.T) {
			spec, ok := candidates.SpecFor(kind)
			if !ok {
				t.Fatal("missing candidate owner")
			}
			path := filepath.Join(homePath, spec.InboxDir, "legacy-feedback.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			// Corruption is unreachable through intake; simulate legacy bytes.
			if err := os.WriteFile(path, []byte(`{"generated":"legacy feedback","id":""}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
			})
			before := releaseCLIHomeSnapshot(t, homePath)
			if _, err := sourceobs.LoadReleaseObservationsReport(homePath, []string{bundle.Product.ID}, now); err == nil {
				t.Fatal("strict inventory reader hid corruption")
			}
			if _, err := product.BuildReleaseAwareness(homePath, bundle.Product.ID, product.ReleaseIdentity{}, product.ReleaseAwarenessOptions{Now: now}); err == nil {
				t.Fatal("direct release reader hid corruption")
			}
			invalid := assertState(t, "invalid")
			if !strings.Contains(strings.Join(invalid.MissingEvidence, "\n"), "source-observation inventory incomplete") || !strings.Contains(strings.Join(invalid.MissingEvidence, "\n"), filepath.Base(path)) {
				t.Fatalf("CLI hid actionable inventory failure: %+v", invalid)
			}
			if invalid.ObservationCount != 0 || len(invalid.ChannelPlatforms) != 0 || invalid.LatestVersion != "" || invalid.OutdatedContract || len(invalid.EvidenceRefs) != 0 {
				t.Fatalf("CLI admitted partial inventory or fabricated provenance: %+v", invalid)
			}
			if !reflect.DeepEqual(before, releaseCLIHomeSnapshot(t, homePath)) {
				t.Fatal("release readers modified corrupt inventory")
			}
		})
	}
}
