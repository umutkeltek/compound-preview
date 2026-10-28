package work

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourcePublicationAdmissionReadsSourceIdentityOnce(t *testing.T) {
	repo, base := initLandingRepo(t)
	enrollSnapshotAdmissionProfile(t, repo)
	s, w := sourceAdmissionWork(t, repo, base, true)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(log) + "\nexec " + quote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, public := range []bool{true, false} {
		ai := sourceAdmissionAction(w, repo, "act_snapshot_identity")
		ai.Target.Kind = "repository"
		if public {
			ai.MutationClass = "public"
		}
		if err := os.WriteFile(log, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.validateSourceActionAdmission(w, ai); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		count := strings.Count(string(b), "config --get remote.origin.url")
		if count != 2 {
			t.Fatalf("publication=%v read repository identity %d times, want bound target plus one fresh source read: %s", public, count, b)
		}
	}
}

func TestSourcePublicationAdmissionRejectsSourceFingerprintDrift(t *testing.T) {
	repo, base := initLandingRepo(t)
	enrollSnapshotAdmissionProfile(t, repo)
	s, w := sourceAdmissionWork(t, repo, base, true)
	other := filepath.Join(t.TempDir(), "source")
	landingGit(t, repo, "clone", "--no-local", repo, other)
	// Keep all bound source objects and the proof declaration, and initially
	// match the canonical repository's absent origin identity.
	landingGit(t, other, "remote", "remove", "origin")
	ai := sourceAdmissionAction(w, other, "act_publication_identity_drift")
	ai.Target.Kind, ai.MutationClass = "repository", "public"
	if err := s.validateSourceActionAdmission(w, ai); err != nil {
		t.Fatalf("matching source fixture must admit before identity drift: %v", err)
	}
	landingGit(t, other, "config", "remote.origin.url", "https://example.invalid/unrelated.git")
	if err := s.validateSourceActionAdmission(w, ai); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("publication did not compare its fresh source identity: %v", err)
	}
}

func enrollSnapshotAdmissionProfile(t *testing.T, repo string) {
	t.Helper()
	for name, body := range map[string]string{
		"package.json": "{}\n", "bun.lock": "{}\n",
		"compound-proof.json": `{"schema_version":"compound.proof_consumers.v1","profile":"bun_consumers_v1","consumers":[{"id":"product-flow","command":["bun","test"],"dir":".","timeout_seconds":60,"required_tools":["bun"],"report_kind":"bun_junit","required_tests":["[\"proof.test.ts\",\"\",\"passes\",[\"proof.test.ts\"]]"]}]}`,
	} {
		writeLandingFile(t, filepath.Join(repo, name), body)
	}
	landingGit(t, repo, "add", "package.json", "bun.lock", "compound-proof.json")
	landingGit(t, repo, "commit", "-m", "native proof")
}
