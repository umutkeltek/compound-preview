package work

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateSourcePublishCLISeparatesReadOnlyPlanningFromExactApply(t *testing.T) {
	for _, tc := range []struct {
		args            []string
		valid, writable bool
	}{
		{[]string{"--work", "work_fixture", "--actor", "codex:fixture", "--repo", "/synthetic/source", "--review-action", "act_fixture", "--json"}, true, false},
		{[]string{"--apply", "--request", "request.json", "--json"}, true, true},
		{[]string{"--release-tag", "v1.2.3", "--request", "publication.json", "--json"}, true, false},
		{[]string{"--release-tag", "v1.2.3"}, false, false},
		{[]string{"--release-tag", "v1.2.3/extra", "--request", "publication.json"}, false, false},
		{[]string{"--release-tag", "v1.2.3", "--request", "publication.json", "--apply"}, false, false},
		{[]string{"--release-tag", "v1.2.3", "--request", "publication.json", "--actor", "override"}, false, false},
		{[]string{"--request", "request.json"}, false, false},
		{[]string{"--apply"}, false, false},
		{[]string{"--apply", "--request", "request.json", "--repo", "/override"}, false, false},
		{[]string{"--apply", "--request", "one.json", "--request", "two.json"}, false, false},
		{[]string{"--apply", "--request", "request.json", "--force"}, false, false},
		{[]string{"--apply", "--request", "request.json", "--", "push"}, false, false},
	} {
		_, err := parsePrivateSourcePublishCLI(tc.args)
		if (err == nil) != tc.valid || PrivateSourcePublishCLIRequiresWritableStore(tc.args) != tc.writable {
			t.Fatalf("publication parsing/admission drift: %q %v", tc.args, err)
		}
		if !tc.valid {
			var out, errs bytes.Buffer
			if code := RunPrivateSourcePublishCLI(nil, tc.args, &out, &errs); code != 2 || out.Len() != 0 {
				t.Fatalf("invalid command reached store: code=%d", code)
			}
		}
	}
}

func TestPrivateReleaseTagCLIPlansReadOnlyAndAppliesDistinctStrictRequest(t *testing.T) {
	f := newPrivateReleaseTagFixture(t, false)
	s := f.publication.f.store
	publicationFile := filepath.Join(t.TempDir(), "publication.json")
	if err := os.WriteFile(publicationFile, publicReviewJSON(t, f.request), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(s.home)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotHome(t, s.home)
	var out, errs bytes.Buffer
	code := RunPrivateSourcePublishCLI(reader, []string{"--release-tag", "v1.2.3", "--request", publicationFile, "--json"}, &out, &errs)
	if code != 0 || snapshotHome(t, s.home) != before || f.pushes != 0 {
		t.Fatalf("tag CLI planning mutated or failed: %d %s", code, errs.String())
	}
	var request PrivateReleaseTagRequest
	if err := publicReviewStrictJSON(out.Bytes(), &request); err != nil || request.SchemaVersion != PrivateReleaseTagSchema {
		t.Fatalf("tag CLI did not emit its own concrete request: %v", err)
	}
	f.approve(t, request)
	tagFile := filepath.Join(t.TempDir(), "tag.json")
	if err := os.WriteFile(tagFile, publicReviewJSON(t, request), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errs.Reset()
	if code := RunPrivateSourcePublishCLI(s, []string{"--apply", "--request", tagFile, "--json"}, &out, &errs); code != 0 || f.pushes != 1 {
		t.Fatalf("tag CLI did not consume distinct native action: %d %s", code, errs.String())
	}
	for _, body := range []string{
		`{"schema_version":"unsupported"}`,
		`{"schema_version":"` + PrivateReleaseTagSchema + `","schema_version":"` + PrivateReleaseTagSchema + `"}`,
		strings.TrimSuffix(string(publicReviewJSON(t, request)), "}") + `,"override_ref":"refs/heads/extra"}`,
	} {
		if err := os.WriteFile(tagFile, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		errs.Reset()
		if code := RunPrivateSourcePublishCLI(s, []string{"--apply", "--request", tagFile}, &out, &errs); code != 2 || out.Len() != 0 || f.pushes != 1 {
			t.Fatalf("invalid deciding JSON reached metadata action: %d %s", code, errs.String())
		}
	}
}

func TestPrivateSourcePublishCLIProducesConcretePlanWithoutPublishing(t *testing.T) {
	p := newPrivatePublisherFixture(t, false)
	var out, errs bytes.Buffer
	code := RunPrivateSourcePublishCLI(p.f.store, []string{"--work", p.f.workID, "--actor", proofActorAlpha, "--repo", p.f.repo, "--review-action", p.f.action.ID, "--json"}, &out, &errs)
	if code != 0 || p.pushes != 0 {
		t.Fatalf("planning mutated destination: %d %s", code, errs.String())
	}
	var request PrivateSourcePublishRequest
	if err := json.Unmarshal(out.Bytes(), &request); err != nil || request.Action.ID == "" || request.ExpectedRemoteSHA != p.tip || !strings.HasPrefix(request.ProofRef, "proof-check:pfrs_") {
		t.Fatalf("invalid concrete plan: %+v %v", request, err)
	}
	if _, err := p.f.store.Get(p.f.workID); err != nil {
		t.Fatal(err)
	}
}
