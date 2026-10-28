package work

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
)

// sourceActionAdmissionScope requires declared source semantics and corroborating
// Git scope. A Git cwd alone does not turn a runtime effect into source delivery.
// Generic shell commands and harness prompts do not declare enough purpose here.
func sourceActionAdmissionScope(ai envelope.ActionIntent) (root string, publication bool, err error) {
	if ai.MutationClass == "read" {
		return "", false, nil
	}
	kind := strings.TrimSpace(ai.Target.Kind)
	switch kind {
	case "private_runtime", "git_worktree", "package", "installed_package":
		return "", false, nil
	}
	cap := strings.TrimSpace(ai.Capability)
	// Native public redaction review classifies findings under public authority;
	// it does not publish or edit the repository. Requiring publication readiness
	// here would make proof preparation depend on an unrelated source profile.
	// Its exact owner pair still passes the existing native policy/review gates.
	if (cap == PublicRedactionReviewCapability && ai.Action == PublicRedactionReviewAction) ||
		(cap == PrivateSourceReviewCapability && ai.Action == PrivateSourceReviewAction) {
		return "", false, nil
	}
	repoTarget := kind == "repository" || kind == "git_repository"
	publication = ai.MutationClass == "public" && (repoTarget || cap == "git-amplify-deployment" || cap == "shell.git_push_existing_remote")
	source := publication || cap == "source.edit" || (cap == "build-implement" && repoTarget) || (cap == "file.write" && (kind == "file" || kind == "directory" || repoTarget))
	if !source || !filepath.IsAbs(ai.Target.Path) {
		return "", false, nil
	}
	root, found, err := canonicalGitRootForPath(ai.Target.Path)
	if err != nil {
		return "", false, fmt.Errorf("resolve source action %s Git scope: %w", ai.ID, err)
	}
	if !found {
		return "", false, nil
	}
	// Resolve aliases and future paths before checking ignore rules. check-ignore
	// defaults to respecting tracked files; an ignore entry cannot hide source
	// already tracked in Git. A root directory declaration always includes source.
	if cap == "file.write" {
		target, err := canonicalSourceAdmissionPath(ai.Target.Path)
		if err != nil {
			return "", false, err
		}
		relative, err := filepath.Rel(root, target)
		if err != nil {
			return "", false, err
		}
		if relative != "." {
			if kind == "directory" {
				relative += string(filepath.Separator)
			}
			if kind == "directory" {
				// An ignored directory can contain force-tracked source. Only
				// a genuinely untracked runtime subtree may use this exemption.
				tracked := exec.Command("git", "--literal-pathspecs", "-C", root, "ls-files", "--cached", "-z", "--", relative)
				tracked.Env = gitrepository.ReadEnvironment()
				output, err := tracked.CombinedOutput()
				if err != nil {
					return "", false, fmt.Errorf("inspect source action %s tracked descendants: %w: %s", ai.ID, err, strings.TrimSpace(string(output)))
				}
				if len(output) != 0 {
					return root, publication, nil
				}
			}
			cmd := exec.Command("git", "-C", root, "check-ignore", "--quiet", "--", relative)
			cmd.Env = gitrepository.ReadEnvironment()
			if output, err := cmd.CombinedOutput(); err == nil {
				return "", false, nil
			} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				return "", false, fmt.Errorf("inspect source action %s ignored destination: %w: %s", ai.ID, err, strings.TrimSpace(string(output)))
			}
		}
	}
	return root, publication, nil
}

func canonicalSourceAdmissionPath(value string) (string, error) {
	current := filepath.Clean(value)
	var missing []string
	for {
		canonical, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, missing[i])
			}
			return canonical, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// validateSourceActionAdmission is a pre-effect gate, never a certificate.
// Local enrollment needs a binding but no previous profile. Public source
// delivery also requires a real supported committed profile; successful native
// proof, publication, land-check and closure remain separate requirements.
func (s *Store) validateSourceActionAdmission(w envelope.WorkEnvelope, ai envelope.ActionIntent) error {
	root, publication, err := sourceActionAdmissionScope(ai)
	if err != nil || root == "" {
		return err
	}
	if w.GitDelivery == nil {
		return fmt.Errorf("source action %s requires git_delivery before mutation; create work with complete target/root/ref/base/publication authority, or request operator-provenanced same-state binding for existing work %s", ai.ID, w.ID)
	}
	if err := s.validateBoundGitDelivery(w); err != nil {
		return fmt.Errorf("source action %s git_delivery: %w", ai.ID, err)
	}
	if publication {
		snapshot, err := gitrepository.CaptureCleanSnapshot(root, w.GitDelivery.ExpectedBaseSHA)
		if err != nil {
			return fmt.Errorf("source publication %s canonical proof profile: %w", ai.ID, err)
		}
		if snapshot.RepositoryFingerprint != w.GitDelivery.RepositoryFingerprint {
			return fmt.Errorf("source action %s repository fingerprint differs from work %s git_delivery", ai.ID, w.ID)
		}
		if err := ValidateCanonicalProofProfile(root, snapshot); err != nil {
			return fmt.Errorf("source publication %s canonical proof profile: %w; enroll the repository's supported native proof declaration before publication", ai.ID, err)
		}
		return nil
	}
	identity, err := gitrepository.ReadIdentity(root, w.GitDelivery.ExpectedBaseSHA)
	if err != nil {
		return fmt.Errorf("source action %s repository identity: %w", ai.ID, err)
	}
	if identity.Fingerprint != w.GitDelivery.RepositoryFingerprint {
		return fmt.Errorf("source action %s repository fingerprint differs from work %s git_delivery", ai.ID, w.ID)
	}
	return nil
}
