package arch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPushGrantDoctrine = "Normal `git push` to the existing remote is covered by the standing grant. A clean `redact` scan is a separate required gate. Force-push, history rewrite, or a new remote still requires explicit confirmation.\n"

func writeStandingGrantPushFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	mustWriteTestFile(t, filepath.Join(repo, "docs", "architecture", "standing-grants.md"), "StandingGrant scope budget blast_radius reversibility target_path max_usd. Leaving the envelope causes a hold at the PolicyDecision adapter gate.\n")
	mustWriteTestFile(t, filepath.Join(repo, "AGENTS.md"), validPushGrantDoctrine)
	for _, ceremony := range []string{"compound-land", "compound-execute"} {
		mustWriteTestFile(t, filepath.Join(repo, "skills", ceremony, "SKILL.md"), validPushGrantDoctrine)
	}
	return repo
}

func mustWriteTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStandingGrantContractRejectsContradictoryPushCeremony(t *testing.T) {
	for _, rel := range []string{"AGENTS.md", "skills/compound-land/SKILL.md", "skills/compound-execute/SKILL.md"} {
		t.Run(rel, func(t *testing.T) {
			repo := writeStandingGrantPushFixture(t)
			path := filepath.Join(repo, filepath.FromSlash(rel))
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body = append(body, []byte("Never push to a public remote without explicit operator confirmation.\n")...)
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}

			findings := checkStandingGrantContract(repo)
			if len(findings) != 1 || findings[0].Severity != Fail {
				t.Fatalf("findings = %+v, want one failure for contradictory %s", findings, rel)
			}
			if !strings.Contains(findings[0].Message, rel) {
				t.Fatalf("failure should identify contradictory surface %s: %+v", rel, findings)
			}
		})
	}
}

func TestStandingGrantContractRequiresEveryPushBoundaryOnEverySurface(t *testing.T) {
	mutations := []struct {
		name string
		old  string
		new  string
	}{
		{name: "existing remote grant", old: "existing remote is covered by the standing grant", new: "repository may be pushed"},
		{name: "separate clean redact gate", old: "clean `redact` scan is a separate required gate", new: "redact may be run"},
		{name: "confirmation exceptions", old: "Force-push, history rewrite, or a new remote still requires explicit confirmation", new: "Force-push and history rewrite need care"},
	}
	for _, rel := range []string{"AGENTS.md", "skills/compound-land/SKILL.md", "skills/compound-execute/SKILL.md"} {
		for _, mutation := range mutations {
			t.Run(rel+"/"+mutation.name, func(t *testing.T) {
				repo := writeStandingGrantPushFixture(t)
				path := filepath.Join(repo, filepath.FromSlash(rel))
				body := strings.Replace(validPushGrantDoctrine, mutation.old, mutation.new, 1)
				if body == validPushGrantDoctrine {
					t.Fatalf("test mutation %q did not change fixture", mutation.name)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}

				findings := checkStandingGrantContract(repo)
				if len(findings) != 1 || findings[0].Severity != Fail {
					t.Fatalf("findings = %+v, want one failure for missing %s in %s", findings, mutation.name, rel)
				}
				if !strings.Contains(findings[0].Message, rel) {
					t.Fatalf("failure should identify incomplete surface %s: %+v", rel, findings)
				}
			})
		}
	}
}
