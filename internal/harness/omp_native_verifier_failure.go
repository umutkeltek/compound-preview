package harness

import (
	homepkg "compound/internal/home"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// retainFailedEvidence preserves an untrusted snapshot, including empty or
// interrupted config bytes. Its failure-only digest can never certify execution.
func (a *OMPNativeVerifierAdapter) retainFailedEvidence(ctx subprocessEvidenceContext, in RunInput, launch *ompNativeVerifierLaunch) (string, error) {
	if launch == nil || launch.capture == nil {
		return "", fmt.Errorf("OMP_FAILED_CAPTURE_MISSING")
	}
	raw := append([]byte(nil), launch.failedCapture...)
	if !launch.failedCaptureSet {
		var err error
		raw, err = launch.capture.Consume(12 << 20)
		if err != nil {
			return "", fmt.Errorf("OMP_FAILED_CAPTURE_READ")
		}
	}
	sum := sha256.Sum256(raw)
	body, err := json.Marshal(struct {
		Schema         string `json:"schema_version"`
		WorkID         string `json:"work_id"`
		RunAttemptID   string `json:"run_attempt_id"`
		ActionIntentID string `json:"action_intent_id"`
		ExitCode       int    `json:"exit_code"`
		TimedOut       bool   `json:"timed_out"`
		CapturedLength int    `json:"captured_length"`
		CapturedSHA256 string `json:"captured_sha256"`
		CapturedBase64 string `json:"captured_base64"`
	}{"compound.omp_failed_capture.v0.1", in.WorkID, ctx.RunAttemptID, in.ActionIntentID, ctx.ExitCode, ctx.TimedOut, len(raw), hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(raw)})
	if err != nil {
		return "", fmt.Errorf("OMP_FAILED_CAPTURE_ENCODE")
	}
	artifacts, err := homepkg.NewArtifactStore(a.store.Home())
	if err != nil {
		return "", fmt.Errorf("OMP_FAILED_CAPTURE_STORE")
	}
	ref, err := artifacts.PutPrivate(body)
	if err != nil {
		return "", fmt.Errorf("OMP_FAILED_CAPTURE_PUT")
	}
	if err := launch.capture.Remove(); err != nil {
		return "", fmt.Errorf("OMP_FAILED_CAPTURE_CLEANUP")
	}
	return strings.TrimPrefix(ref, homepkg.ArtifactRefPrefix), nil
}
