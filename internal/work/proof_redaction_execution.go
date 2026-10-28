package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/finding"
)

func BindNativePublicRedactionReview(plan CanonicalProofPlan, homePath, workID, actor, actionID string) (CanonicalProofPlan, error) {
	store, err := OpenReadOnly(homePath)
	if err != nil {
		return plan, err
	}
	// Reconstruct the exact historical tuple without granting fresh authority.
	// Admission rechecks live permission if there is no sealed result to recover.
	authority, err := store.resolvePublicRedactionReview(workID, actor, actionID, plan.Repo, plan.Source, time.Now(), false)
	if err != nil {
		return plan, err
	}
	return BindPublicRedactionReview(plan, authority.Binding)
}

type PublicRedactionInput struct {
	Path string
	Body []byte
}

// The scanner is unchanged. JSON is requested for strict consumption; no
// allowlist, self-email, repo-public-emails or auto-redact flags are passed.
// The CLI supplies its existing source reader and barrier-aware command adapter.
func RunReviewedPublicRedaction(ctx context.Context, repo string, step ProofCheckStep, baseRef string, headRefs []string, readInputs func(context.Context, string, gitrepository.Snapshot) ([]PublicRedactionInput, error), command func(context.Context, string, ...string) *exec.Cmd) ([]byte, *PublicRedactionEvidence, error) {
	if readInputs == nil || command == nil {
		return nil, nil, fmt.Errorf("redaction execution adapters required")
	}
	authority, err := PublicRedactionReviewForExecution(ctx, step, time.Now())
	if err != nil {
		return nil, nil, err
	}
	if len(headRefs) != 1 || baseRef != authority.Review.Source.BaseSHA || headRefs[0] != authority.Review.Source.HeadSHA {
		return nil, nil, fmt.Errorf("reviewed redaction execution source does not match native review")
	}
	inputs, err := readInputs(ctx, repo, gitrepository.Snapshot{BaseSHA: baseRef, HeadSHA: headRefs[0]})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	evidence := &PublicRedactionEvidence{Binding: authority.Binding, Review: authority.Body, Scans: []PublicRedactionScan{}}
	combined := proofRedactionBoundedOutput{Limit: publicRedactionEvidenceLimit}
	retained := len(evidence.Review)
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return combined.Bytes(), evidence, err
		}
		cmd := command(ctx, "redact", "--repo-visibility", "public", "--max-bytes", "8388608", "--source-path", input.Path, "--json")
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(input.Body)
		output := proofRedactionBoundedOutput{Limit: finding.MaxBodyBytes}
		cmd.Stdout, cmd.Stderr = &output, &output
		scanErr := cmd.Run()
		exit := 0
		if scanErr != nil {
			exit = ExitCodeFromRunErr(scanErr)
		}
		hash := sha256.Sum256(input.Body)
		scan := PublicRedactionScan{Path: input.Path, SHA256: hex.EncodeToString(hash[:]), Report: append([]byte(nil), output.Bytes()...), ExitCode: exit}
		retained += len(scan.Report) + len(scan.Path) + len(scan.SHA256)
		if retained > publicRedactionEvidenceLimit {
			return combined.Bytes(), evidence, fmt.Errorf("redaction evidence exceeds bounded report budget")
		}
		evidence.Scans = append(evidence.Scans, scan)
		if _, err := combined.Write([]byte(input.Path + ":\n")); err != nil {
			return combined.Bytes(), evidence, err
		}
		if _, err := combined.Write(scan.Report); err != nil {
			return combined.Bytes(), evidence, err
		}
		if output.Exceeded {
			return combined.Bytes(), evidence, fmt.Errorf("scanner output exceeded bounded JSON budget")
		}
		// Scan the full inventory after blocking exits. A reviewed MEDIUM is
		// never recorded as a scanner success; exact matching happens below.
	}
	if err := ValidatePublicRedactionScans(authority.Review, evidence.Scans); err != nil {
		return combined.Bytes(), evidence, fmt.Errorf("review did not cover fresh scan: %w", err)
	}
	current, err := PublicRedactionReviewForExecution(ctx, step, time.Now())
	if err != nil {
		return combined.Bytes(), evidence, fmt.Errorf("review authority changed before scan acceptance: %w", err)
	}
	if current.Binding != authority.Binding {
		return combined.Bytes(), evidence, fmt.Errorf("review authority binding changed before scan acceptance")
	}
	if _, err := combined.Write([]byte("public redaction: exact native review consumed; raw scanner findings retained\n")); err != nil {
		return combined.Bytes(), evidence, err
	}
	return combined.Bytes(), evidence, nil
}

type proofRedactionBoundedOutput struct {
	buffer   bytes.Buffer
	Limit    int
	Exceeded bool
}

func (b *proofRedactionBoundedOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *proofRedactionBoundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.Limit-b.buffer.Len() {
		b.Exceeded = true
		return 0, fmt.Errorf("redaction output budget exceeded")
	}
	return b.buffer.Write(p)
}
