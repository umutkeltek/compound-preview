package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/mem"
	"compound/internal/product"
	"compound/internal/sourceobs"
)

func releaseCLIHomeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel] = "directory"
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("%x", sha256.Sum256(body))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTypedReleaseActualProductCLI(t *testing.T) {
	binary := compoundTestBinary(t)
	for _, state := range []string{"live", "candidate", "submitted"} {
		t.Run(state, func(t *testing.T) {
			homePath := initializedTestHome(t)
			t.Setenv("COMPOUND_HOME", homePath)
			now := time.Now().UTC().Truncate(time.Second)
			store, err := mem.Open(homePath)
			if err != nil {
				t.Fatal(err)
			}
			bundle := product.Bundle{
				SchemaVersion: product.SchemaVersion,
				Product: product.Product{
					ID: "product:typed-alpha", Name: "Typed Alpha", OwnerEntity: "project:typed-alpha",
					Surfaces: []product.Surface{{ID: "web", Kind: "web", Ref: "https://alpha.invalid"}},
					Channels: []string{"direct"}, Positioning: "Synthetic carrier proof", PrivacyPosture: "local_first",
					ReleaseIdentity: product.ReleaseIdentity{Version: "1.0", Build: "10"},
				},
				EvidenceRefs: []string{"fixture:typed-release-cli"},
			}
			if _, err := product.Apply(homePath, store, bundle, "fixture://typed-alpha", now); err != nil {
				t.Fatal(err)
			}
			run := func() product.LoopModel {
				t.Helper()
				before := releaseCLIHomeSnapshot(t, homePath)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, "product", "loops", "--product", bundle.Product.ID, "--json")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("compound product loops: %v\n%s", err, out)
				}
				after := releaseCLIHomeSnapshot(t, homePath)
				if !reflect.DeepEqual(before, after) {
					for path, hash := range after {
						if before[path] != hash {
							t.Errorf("CLI added/changed %s", path)
						}
					}
					for path := range before {
						if _, ok := after[path]; !ok {
							t.Errorf("CLI removed %s", path)
						}
					}
					t.Fatal("read-only CLI mutated home")
				}
				var model product.LoopModel
				if err := json.Unmarshal(out, &model); err != nil {
					t.Fatalf("CLI JSON: %v\n%s", err, out)
				}
				if len(model.Products) != 1 {
					t.Fatalf("CLI products: %s", out)
				}
				return model
			}
			empty := run().Products[0].ReleaseAwareness
			if empty != nil && empty.ObservationCount != 0 {
				t.Fatalf("unseeded control: %+v", empty)
			}
			want := sourceobs.ReleaseObservation{
				ProductID: bundle.Product.ID, Channel: "direct", Platform: "desktop",
				ObservedVersion: "2.0", ObservedBuild: "20", ObservedState: state,
				ObservedAt: now.Add(-time.Hour).Format(time.RFC3339), SourceRevision: "fixture-rev-2",
				EvidenceRef: "https://releases.invalid/typed-alpha/2.0",
			}
			// Raw JSON producer: no adapter conversion or metadata evidence refs.
			raw, err := json.Marshal(sourceobs.SourceObservation{
				SchemaVersion: sourceobs.SchemaVersion, ID: "srcobs_cli_typed", AdapterID: "fixture.release", SourceSystem: "external",
				ObservedAt:   now.Format(time.RFC3339),
				SourceRef:    sourceobs.SourceRef{Path: "https://releases.invalid/typed-alpha", Kind: "release", Owner: bundle.Product.ID, SourceClass: "evidence", Revision: "fixture-rev-2"},
				MapsTo:       []sourceobs.MapTarget{{ObjectFamily: "release_observation", CandidateKind: "route_observation", EntityHint: bundle.Product.ID, PromoteAllowed: false}},
				EvidenceRefs: []string{want.EvidenceRef}, Release: &want,
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "observation.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			obs, err := sourceobs.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sourceobs.Apply(homePath, obs, now); err != nil {
				t.Fatal(err)
			}
			got := run().Products[0].ReleaseAwareness
			if got == nil || got.ObservationCount != 1 || len(got.ChannelPlatforms) != 1 {
				t.Fatalf("actual CLI lost raw release: %+v", got)
			}
			if got.ChannelPlatforms[0].Latest != want {
				t.Fatalf("CLI payload: got %+v want %+v", got.ChannelPlatforms[0].Latest, want)
			}
			if state != "live" && got.Status == "current" {
				t.Fatalf("%s claimed live: %+v", state, got)
			}
			if state == "live" && got.Status != "current" {
				t.Fatalf("live evidence absent: %+v", got)
			}
			t.Logf("actual CLI: state=%s status=%s version=%s observations=%d; home unchanged", state, got.Status, got.LatestVersion, got.ObservationCount)
		})
	}
}
