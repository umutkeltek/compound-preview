package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/landlease"
	"compound/internal/work"
)

func scopedLandingCLIFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	homePath := t.TempDir()
	now := time.Now().UTC()
	if err := home.Init(homePath, "scoped-landing-cli-test", now); err != nil {
		t.Fatal(err)
	}
	repo, base, _ := initLandingCLIRepo(t)
	identity, err := gitrepository.ReadIdentity(repo, base)
	if err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	makeWork := func(ref, actor string) string {
		t.Helper()
		w, err := store.Create(work.CreateInput{
			Owner: actor, Actor: actor, State: "ready",
			SuccessPredicate: "land selected repository branch",
			GitDelivery: &envelope.GitDeliveryContract{
				TargetWorktreeRoot: repo, RepositoryFingerprint: identity.Fingerprint,
				TargetLocalRef: ref, ExpectedBaseSHA: base, PublicationMode: "local_only",
			},
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Claim(w.ID, actor, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		return w.ID
	}
	mainWork := makeWork("refs/heads/main", "codex:landing-main")
	candidateWork := makeWork("refs/heads/candidate", "codex:landing-candidate")
	if err := landlease.MigrateLegacyLayout(homePath, filepath.Join(homePath, "resource-layout"), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return homePath, mainWork, candidateWork, repo
}

func TestScopedLandLeaseCLIRequiresBoundWorkAndActor(t *testing.T) {
	homePath, mainWork, _, _ := scopedLandingCLIFixture(t)
	if _, stderr, code := runLandLeaseCLI(t, "acquire", "--home", homePath, "--holder", "main-holder"); code == 0 || !strings.Contains(stderr, "--work") {
		t.Fatalf("unselected scoped acquire code=%d stderr=%q", code, stderr)
	}
	if _, stderr, code := runLandLeaseCLI(t, "acquire", "--home", homePath, "--work", mainWork, "--holder", "main-holder"); code == 0 || !strings.Contains(stderr, "--actor") {
		t.Fatalf("actorless scoped acquire code=%d stderr=%q", code, stderr)
	}
	if _, stderr, code := runLandLeaseCLI(t, "status", "--home", homePath); code == 0 || !strings.Contains(stderr, "--work") {
		t.Fatalf("unselected scoped status code=%d stderr=%q", code, stderr)
	}
	if _, stderr, code := runLandLeaseCLI(t, "status", "--home", homePath, "--proof", "--work", mainWork); code == 0 || !strings.Contains(stderr, "choose --proof or --work") {
		t.Fatalf("ambiguous scoped status code=%d stderr=%q", code, stderr)
	}
	proof, err := landlease.ProofResource(homePath).StatusReadOnly(time.Now())
	if err != nil || proof.Holder != "" {
		t.Fatalf("rejected landing commands touched proof resource: status=%+v err=%v", proof, err)
	}
}

func TestScopedLandLeaseCLIIsolatesLandingRefsAndProof(t *testing.T) {
	homePath, mainWork, candidateWork, _ := scopedLandingCLIFixture(t)
	mainHolder := "codex:main:" + mainWork
	candidateHolder := "codex:candidate:" + candidateWork
	if _, stderr, code := runLandLeaseCLI(t, "acquire", "--home", homePath, "--work", mainWork, "--actor", "codex:landing-main", "--holder", mainHolder); code != 0 {
		t.Fatalf("main acquire code=%d stderr=%q", code, stderr)
	}
	if _, stderr, code := runLandLeaseCLI(t, "acquire", "--home", homePath, "--work", candidateWork, "--actor", "codex:landing-candidate", "--holder", candidateHolder); code != 0 {
		t.Fatalf("candidate acquire code=%d stderr=%q", code, stderr)
	}
	for _, tc := range []struct{ workID, holder string }{{mainWork, mainHolder}, {candidateWork, candidateHolder}} {
		out, stderr, code := runLandLeaseCLI(t, "status", "--home", homePath, "--work", tc.workID, "--json")
		if code != 0 {
			t.Fatalf("status %s code=%d stderr=%q", tc.workID, code, stderr)
		}
		var status landlease.StatusResult
		if err := json.Unmarshal([]byte(out), &status); err != nil {
			t.Fatal(err)
		}
		if !status.Held || status.Holder != tc.holder {
			t.Fatalf("status %s = %+v", tc.workID, status)
		}
	}
	out, stderr, code := runLandLeaseCLI(t, "status", "--home", homePath, "--proof", "--json")
	if code != 0 {
		t.Fatalf("proof status code=%d stderr=%q", code, stderr)
	}
	var proof landlease.StatusResult
	if err := json.Unmarshal([]byte(out), &proof); err != nil {
		t.Fatal(err)
	}
	if proof.Held || proof.Holder != "" {
		t.Fatalf("landing lease contaminated proof status: %+v", proof)
	}
	if _, stderr, code := runLandLeaseCLI(t, "release", "--home", homePath, "--work", mainWork, "--actor", "codex:landing-main", "--holder", mainHolder); code != 0 {
		t.Fatalf("main release code=%d stderr=%q", code, stderr)
	}
	for _, tc := range []struct {
		workID string
		held   bool
	}{{mainWork, false}, {candidateWork, true}} {
		out, stderr, code := runLandLeaseCLI(t, "status", "--home", homePath, "--work", tc.workID, "--json")
		if code != 0 {
			t.Fatalf("status after release %s code=%d stderr=%q", tc.workID, code, stderr)
		}
		var status landlease.StatusResult
		if err := json.Unmarshal([]byte(out), &status); err != nil {
			t.Fatal(err)
		}
		if status.Held != tc.held {
			t.Fatalf("status after release %s = %+v", tc.workID, status)
		}
	}
}

func TestScopedLandLeaseCLIRefusesRepositoryIdentityDrift(t *testing.T) {
	homePath, mainWork, _, repo := scopedLandingCLIFixture(t)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.LandingResourceKey(mainWork)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "config", "remote.origin.url", "https://example.invalid/changed.git").CombinedOutput(); err != nil {
		t.Fatalf("change fixture origin: %v: %s", err, out)
	}
	if _, stderr, code := runLandLeaseCLI(t, "acquire", "--home", homePath, "--work", mainWork, "--actor", "codex:landing-main", "--holder", "codex:main:"+mainWork); code == 0 || !strings.Contains(stderr, "repository fingerprint") {
		t.Fatalf("drifted repository acquired code=%d stderr=%q", code, stderr)
	}
	resource, err := landlease.OpenResource(homePath, key)
	if err != nil {
		t.Fatal(err)
	}
	status, err := resource.StatusReadOnly(time.Now())
	if err != nil || status.Held {
		t.Fatalf("drifted acquire touched bound resource: %+v %v", status, err)
	}
}

func TestDoctorReportsScopedProofAndSelectedLandingHint(t *testing.T) {
	homePath, mainWork, _, _ := scopedLandingCLIFixture(t)
	if _, stderr, code := runLandLeaseCLI(t, "acquire", "--home", homePath, "--work", mainWork, "--actor", "codex:landing-main", "--holder", "codex:main:"+mainWork); code != 0 {
		t.Fatalf("landing acquire code=%d stderr=%q", code, stderr)
	}
	proofLease, err := landlease.ProofResource(homePath).Acquire("proof-holder", time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if proofLease.Holder != "proof-holder" {
		t.Fatalf("proof lease = %+v", proofLease)
	}
	var stdout, stderr bytes.Buffer
	_ = runDoctor([]string{"--home", homePath, "--base", t.TempDir(), "--binary", "/synthetic/compound", "--json"}, &stdout, &stderr)
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("doctor JSON: %v (%q; stderr %q)", err, stdout.String(), stderr.String())
	}
	if report.LeaseLayout != "scoped" || !report.ProofLeaseHeld || report.ProofLeaseHolder != "proof-holder" || report.LeaseHolder != "" {
		t.Fatalf("doctor conflated scoped proof and landing: layout=%q proof=%q legacy=%q", report.LeaseLayout, report.ProofLeaseHolder, report.LeaseHolder)
	}
	if !strings.Contains(report.LandingLeaseStatusHint, "--work") {
		t.Fatalf("doctor did not explain how to select landing status: %+v", report)
	}
}
