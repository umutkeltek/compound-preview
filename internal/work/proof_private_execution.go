package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"reflect"
	"sort"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/finding"
	"compound/internal/runattempt"
)

// RunSourceRedaction moves the native dispatch into its owning work organ.
// The public/default adapters and scanner arguments retain their old behavior.
func RunSourceRedaction(ctx context.Context, repo string, step ProofCheckStep, baseRef string, headRefs []string,
	readInputs func(context.Context, string, gitrepository.Snapshot) ([]PublicRedactionInput, error),
	command func(context.Context, string, ...string) *exec.Cmd,
	unreviewed func(context.Context, string, string, ...string) ([]byte, error),
) ([]byte, *PublicRedactionEvidence, *PrivateSourceEvidence, error) {
	_, private, err := PrivateSourceBindingForStep(step)
	if err != nil {
		return nil, nil, nil, err
	}
	if private {
		output, evidence, err := runReviewedPrivateSourceRedaction(ctx, repo, step, baseRef, headRefs, readInputs, command)
		return output, nil, evidence, err
	}
	_, reviewed, err := PublicRedactionBindingForStep(step)
	if err != nil {
		return nil, nil, nil, err
	}
	if reviewed {
		output, evidence, err := RunReviewedPublicRedaction(ctx, repo, step, baseRef, headRefs, readInputs, command)
		return output, evidence, nil, err
	}
	output, err := unreviewed(ctx, repo, baseRef, headRefs...)
	return output, nil, nil, err
}

func runReviewedPrivateSourceRedaction(ctx context.Context, repo string, step ProofCheckStep, baseRef string, headRefs []string,
	readInputs func(context.Context, string, gitrepository.Snapshot) ([]PublicRedactionInput, error),
	command func(context.Context, string, ...string) *exec.Cmd,
) ([]byte, *PrivateSourceEvidence, error) {
	if readInputs == nil || command == nil {
		return nil, nil, fmt.Errorf("private source scan adapters are required")
	}
	authority, err := privateSourceReviewForExecution(ctx, step, time.Now())
	if err != nil {
		return nil, nil, err
	}
	if len(headRefs) != 1 || baseRef != authority.Review.Source.BaseSHA || headRefs[0] != authority.Review.Source.HeadSHA {
		return nil, nil, fmt.Errorf("private source execution does not match exact reviewed source")
	}
	inputs, err := readInputs(ctx, repo, authority.Review.Source)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	evidence := &PrivateSourceEvidence{Binding: authority.Binding, Review: authority.Body, Scans: []PublicRedactionScan{}}
	combined := proofRedactionBoundedOutput{Limit: publicRedactionEvidenceLimit}
	retained := len(evidence.Review)
	for _, input := range inputs {
		cmd := command(ctx, "redact", "--repo-visibility", "public", "--max-bytes", "8388608", "--source-path", input.Path, "--json")
		cmd.Dir, cmd.Stdin = repo, bytes.NewReader(input.Body)
		output := proofRedactionBoundedOutput{Limit: finding.MaxBodyBytes}
		cmd.Stdout, cmd.Stderr = &output, &output
		scanErr := cmd.Run()
		exit := 0
		if scanErr != nil {
			exit = ExitCodeFromRunErr(scanErr)
		}
		hash := sha256.Sum256(input.Body)
		scan := PublicRedactionScan{Path: input.Path, SHA256: hex.EncodeToString(hash[:]), ExitCode: exit, Report: append([]byte(nil), output.Bytes()...)}
		retained += len(scan.Report) + len(scan.Path) + len(scan.SHA256)
		evidence.Scans = append(evidence.Scans, scan)
		if retained > publicRedactionEvidenceLimit || output.Exceeded {
			return combined.Bytes(), evidence, fmt.Errorf("complete private source scanner evidence exceeded bounded budget")
		}
		if _, err := combined.Write(append([]byte(input.Path+":\n"), scan.Report...)); err != nil {
			return combined.Bytes(), evidence, err
		}
	}
	// Every file is scanned, including files following a blocking scanner exit.
	if err := ValidatePublicRedactionScans(PublicRedactionReview{Files: authority.Review.Files}, evidence.Scans); err != nil {
		return combined.Bytes(), evidence, fmt.Errorf("private source review did not cover fresh complete scans: %w", err)
	}
	current, err := privateSourceReviewForExecution(ctx, step, time.Now())
	if err != nil || current.Binding != authority.Binding {
		return combined.Bytes(), evidence, fmt.Errorf("private source authority or live destination changed during scan")
	}
	evidence.Destination = current.Destination
	_, err = combined.Write([]byte("private source audience: exact native classifications consumed; PUBLIC scanner findings retained; not a public-clean certificate\n"))
	return combined.Bytes(), evidence, err
}

func ValidatePrivateSourceEvidence(step ProofCheckStep, source *gitrepository.Snapshot, workID string) error {
	binding, reviewed, err := PrivateSourceBindingForStep(step)
	if err != nil || !reviewed || step.Status != "ok" {
		return err
	}
	evidence := step.PrivateSourceEvidence
	if source == nil || evidence == nil || evidence.Binding != binding || finding.CID(string(evidence.Review)) != binding.CID {
		return fmt.Errorf("private source evidence is missing or not profile-bound")
	}
	review, err := ParsePrivateSourceReview(evidence.Review, time.Now())
	if err != nil {
		return err
	}
	if review.WorkID != workID || !reflect.DeepEqual(review.Source, *source) || !samePrivateSourceDestination(review.Destination, evidence.Destination) {
		return fmt.Errorf("private source evidence work/source/destination mismatch")
	}
	if err := validatePrivateSourceDestination(evidence.Destination, time.Now()); err != nil {
		return err
	}
	return ValidatePublicRedactionScans(PublicRedactionReview{Files: review.Files}, evidence.Scans)
}

func (s *Store) validatePrivateSourceProfileAuthority(workID, actor string, profile ProofResourceProfile, now time.Time) error {
	return s.validatePrivateSourceProfileAuthorityAt(workID, actor, profile, now, true)
}

// A complete owned failure is audit evidence, never permission to publish source.
// Preserve its historical review while retaining current authority for every
// certifying, unknown, inconsistent, or foreign completion and new reservation.
func (s *Store) validatePrivateSourceCompletionAuthority(workID, actor string, profile ProofResourceProfile, result ProofResourceResult, now time.Time) error {
	failedAudit := result.WorkID == workID && result.Actor == actor &&
		result.Status == ProofResourceFailed && result.RunAttemptStatus == runattempt.StatusFailed &&
		result.OwnedReport && result.ReportArtifactRef != "" && result.ReportStatus == "failed" &&
		result.ProducerExitKnown && result.ProducerExitCode != 0 && result.VerificationRef == ""
	return s.validatePrivateSourceProfileAuthorityAt(workID, actor, profile, now, !failedAudit)
}

func (s *Store) validatePrivateSourceProfileAuthorityAt(workID, actor string, profile ProofResourceProfile, now time.Time, requireCurrent bool) error {
	for _, gate := range profile.Gates {
		step := ProofCheckStep{Name: gate.Name, Command: gate.Command, ProofRef: gate.ProofRef}
		binding, reviewed, err := PrivateSourceBindingForStep(step)
		if err != nil {
			return err
		}
		if !reviewed {
			continue
		}
		if profile.SourceSnapshot == nil || profile.Kind != "canonical_full" {
			return fmt.Errorf("private source authority requires an exact canonical source profile")
		}
		authority, err := s.resolvePrivateSourceReview(workID, actor, binding.ActionID, profile.Repo, *profile.SourceSnapshot, now, requireCurrent)
		if err != nil || authority.Binding != binding {
			return fmt.Errorf("private source authority differs from sealed proof profile")
		}
		plan := CanonicalProofPlan{ID: "private-source-original", Repo: profile.Repo, Source: *profile.SourceSnapshot}
		for _, original := range profile.Gates {
			item := ProofCheckStep{Name: original.Name, Command: append([]string(nil), original.Command...), Dir: original.Dir, SkipIfMissing: original.SkipIfMissing, ProofRef: original.ProofRef}
			if item.Name == "redact-public" {
				item.Command, item.ProofRef = item.Command[:4], "redact-public-clean"
			}
			plan.Steps = append(plan.Steps, item)
		}
		digest, err := privateSourceProfileDigest(plan)
		if err != nil || digest != authority.Review.SourceProfileDigest {
			return fmt.Errorf("private source original profile identity changed")
		}
	}
	return nil
}
