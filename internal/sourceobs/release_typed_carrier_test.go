package sourceobs

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"compound/internal/candidates"
	"gopkg.in/yaml.v3"
)

func rawTypedRelease(t *testing.T, format string, now time.Time) SourceObservation {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{
		"schema_version":"compound.source_observation.v0.1",
		"id":"srcobs_typed_fixture","adapter_id":"fixture.release","source_system":"external",
		"observed_at":%q,
		"source_ref":{"path":"https://releases.invalid/alpha","kind":"release","owner":"product:alpha","source_class":"evidence","revision":"rev-2"},
		"maps_to":[{"object_family":"release_observation","candidate_kind":"route_observation","entity_hint":"product:alpha","promote_allowed":false}],
		"evidence_refs":["https://releases.invalid/alpha/2.0"],
		"release":{"product_id":"product:alpha","channel":"direct","platform":"desktop","observed_version":"2.0","observed_build":"build-20","observed_state":"live","observed_at":%q,"source_revision":"rev-2","evidence_ref":"https://releases.invalid/alpha/2.0"}
	}`, now.Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)))
	if format == "yaml" {
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		var err error
		raw, err = yaml.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "release."+format)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	obs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func typedReleaseSnapshot(t *testing.T, root string) map[string]string {
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

func assertTypedRelease(t *testing.T, value any, want ReleaseObservation) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Release *ReleaseObservation `json:"release"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Release == nil || *got.Release != want {
		t.Errorf("typed release lost or changed: got %+v, want %+v", got.Release, want)
	}
}

func TestTypedReleaseRawIntakeProjection(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			obs := rawTypedRelease(t, format, now)
			want := *obs.Release
			homePath := t.TempDir()
			report, err := Apply(homePath, obs, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.CandidateIDs) != 1 || len(report.NoPromoteIDs) != 1 {
				t.Fatalf("intake: %+v", report)
			}
			var persisted map[string]any
			if err := candidates.ReadCandidate(homePath, candidates.RouteObservation, report.CandidateIDs[0], &persisted); err != nil {
				t.Fatal(err)
			}
			assertTypedRelease(t, persisted, want)
			before := typedReleaseSnapshot(t, homePath)
			list, err := ListSourceObservations(homePath, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 1 {
				t.Fatalf("list: %+v", list)
			}
			assertTypedRelease(t, list.Items[0], want)
			if list.Items[0].PromoteAllowed || list.Items[0].NoPromoteID == "" {
				t.Fatalf("lost evidence-only boundary: %+v", list.Items[0])
			}
			got, err := LoadReleaseObservations(homePath, []string{want.ProductID}, now)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, []ReleaseObservation{want}) {
				t.Errorf("loader: got %+v, want %+v", got, want)
			}
			if !reflect.DeepEqual(before, typedReleaseSnapshot(t, homePath)) {
				t.Fatal("read-only projection mutated home")
			}
		})
	}
}

func legacyTypedReleaseRefs(now time.Time) []string {
	return []string{
		"release_observation:product:alpha:direct:desktop",
		"release_observation:version:1.0",
		"release_observation:build:build-10",
		"release_observation:state:candidate",
		"release_observation:observed_at:" + now.Add(-2*time.Hour).Format(time.RFC3339),
		"release_observation:revision:legacy-rev",
		"release_observation:evidence:https://releases.invalid/alpha/1.0",
	}
}

func TestTypedReleasePrecedenceAndLegacyCompatibility(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, typed := range []bool{true, false} {
		t.Run(fmt.Sprintf("typed=%t", typed), func(t *testing.T) {
			obs := rawTypedRelease(t, "json", now)
			want := *obs.Release
			obs.SourceRef.Path = "compound://release_observation/product:alpha/direct/desktop"
			obs.EvidenceRefs = legacyTypedReleaseRefs(now)
			if !typed {
				obs.Release = nil
				want.ObservedVersion, want.ObservedBuild, want.ObservedState = "1.0", "build-10", "candidate"
				want.ObservedAt = now.Add(-2 * time.Hour).Format(time.RFC3339)
				want.SourceRevision, want.EvidenceRef = "legacy-rev", "https://releases.invalid/alpha/1.0"
			}
			homePath := t.TempDir()
			if _, err := Apply(homePath, obs, now); err != nil {
				t.Fatal(err)
			}
			got, err := LoadReleaseObservations(homePath, []string{want.ProductID}, now)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, []ReleaseObservation{want}) {
				t.Fatalf("precedence: got %+v, want %+v", got, want)
			}
		})
	}
}

func TestTypedReleaseMalformedIntakeIsAtomic(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, field := range []string{"product", "channel", "platform", "version", "state", "time", "revision", "evidence"} {
		t.Run(field, func(t *testing.T) {
			obs := rawTypedRelease(t, "json", now)
			switch field {
			case "product":
				obs.Release.ProductID = ""
			case "channel":
				obs.Release.Channel = ""
			case "platform":
				obs.Release.Platform = ""
			case "version":
				obs.Release.ObservedVersion = ""
			case "state":
				obs.Release.ObservedState = "published"
			case "time":
				obs.Release.ObservedAt = "not-a-time"
			case "revision":
				obs.Release.SourceRevision = ""
			case "evidence":
				obs.Release.EvidenceRef = ""
			}
			obs.EvidenceRefs = legacyTypedReleaseRefs(now)
			homePath := t.TempDir()
			before := typedReleaseSnapshot(t, homePath)
			if _, err := Apply(homePath, obs, now); err == nil {
				t.Fatal("malformed typed payload admitted using legacy evidence")
			}
			if !reflect.DeepEqual(before, typedReleaseSnapshot(t, homePath)) {
				t.Fatal("invalid intake wrote state")
			}
		})
	}
}

func TestTypedReleaseInvalidPersistedPayloadDoesNotFallBack(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	obs := rawTypedRelease(t, "json", now)
	obs.SourceRef.Path = "compound://release_observation/product:alpha/direct/desktop"
	obs.EvidenceRefs = legacyTypedReleaseRefs(now)
	// Deliberately bypass intake to model invalid historical candidate bytes.
	record := CandidateFromMap(obs, obs.MapsTo[0], now)
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(body, &stored); err != nil {
		t.Fatal(err)
	}
	obs.Release.ObservedState = "published"
	stored["release"] = obs.Release
	homePath := t.TempDir()
	if err := candidates.WriteCandidate(homePath, candidates.RouteObservation, record.ID, stored); err != nil {
		t.Fatal(err)
	}
	got, err := LoadReleaseObservations(homePath, []string{"product:alpha"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("invalid typed payload rescued by legacy refs: %+v", got)
	}
}

func TestTypedReleaseAdapterWritesOrdinaryEvidenceRefs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := *rawTypedRelease(t, "json", now).Release
	obs, err := FromReleaseObservation(want, now)
	if err != nil {
		t.Fatal(err)
	}
	assertTypedRelease(t, obs, want)
	if !reflect.DeepEqual(obs.EvidenceRefs, []string{want.EvidenceRef}) {
		t.Fatalf("adapter still encodes metadata as refs: %v", obs.EvidenceRefs)
	}
}
