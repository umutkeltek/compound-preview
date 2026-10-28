package act

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"compound/internal/envelope"
)

func TestCredentialPasswordAlignmentStageTransportAcceptsOnlyFailedEmptyOutputSingleton(t *testing.T) {
	const marker = "credential_password_align:refused:inspection_catalog_scram\n"
	for _, tc := range []struct {
		name, stderr, stdout string
		failed, accepted     bool
	}{
		{"exact", marker, "", true, true},
		{"success exit", marker, "", false, false},
		{"stdout", marker, "fixture-private-output", true, false},
		{"unknown stage", "credential_password_align:refused:fixture-private-diagnostic\n", "", true, false},
		{"extra line", marker + "fixture-private-diagnostic\n", "", true, false},
		{"missing newline", strings.TrimSuffix(marker, "\n"), "", true, false},
		{"wrong owner", "credential_verify:refused:app_existing\n", "", true, false},
		{"overflow", strings.Repeat("fixture-private-diagnostic", 20), "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := &credentialPasswordAlignNativeChild{&credentialVerifyNativeChild{done: make(chan struct{}), failed: tc.failed}}
			_, _ = child.stderr.Write([]byte(tc.stderr))
			_, _ = child.stdout.Write([]byte(tc.stdout))
			close(child.done)
			raw, err := child.Wait()
			var staged credentialPasswordAlignStageError
			got := errors.As(err, &staged)
			if got != tc.accepted || len(raw) != 0 || err == nil || strings.Contains(err.Error(), "fixture-private") {
				t.Fatalf("unexpected bounded failure: staged=%v accepted=%v bytes=%d err=%v", got, tc.accepted, len(raw), err)
			}
			if got && staged.stage != "inspection_catalog_scram" {
				t.Fatal("wrong fixed stage")
			}
		})
	}
}

func TestCredentialPasswordAlignmentFailedStageReceiptsNeverBecomeObservations(t *testing.T) {
	for _, stage := range []string{"inspection_catalog_read", "inspection_catalog_scram", "inspection_closed_state", "cleanup", "fixture-private-diagnostic"} {
		t.Run(stage, func(t *testing.T) {
			f := newAlignmentFixture(t)
			f.transport.waitErr = credentialPasswordAlignStageError{stage: stage}
			p, err := f.owner.Plan(f.r, f.workID, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			f.approve(t, p)
			result, err := f.owner.Execute(context.Background(), p.PlanRef, "fixture")
			if err == nil || result.State != "failed" || result.ObservationRef != "" || !result.ApprovalConsumed {
				t.Fatal("failure was accepted or lost consumed approval")
			}
			var receipt envelope.Receipt
			if err := f.owner.event(result.ReceiptID, &receipt); err != nil {
				t.Fatal(err)
			}
			store, _ := NewArtifactStore(f.owner.home)
			found := false
			for _, ref := range receipt.ProofRefs {
				if !strings.HasPrefix(ref, "artifact:sha256:") {
					continue
				}
				raw, _ := store.Get(ref)
				var status map[string]any
				if json.Unmarshal(raw, &status) != nil || status["schema_version"] != "compound.credential_password_alignment_status.v0.1" {
					continue
				}
				found = true
				_, hasStage := status["failure_stage"]
				if hasStage != credentialPasswordAlignKnownFailureStage(stage) || hasStage && status["failure_stage"] != stage || status["code"] != "operation_failed" || status["configuration_accepted"] != false || status["credential_qualification_accepted"] != false || status["application_activated"] != false || strings.Contains(string(raw), "fixture-private-diagnostic") {
					t.Fatal("unbounded or accepted status")
				}
			}
			if !found {
				t.Fatal("missing actual native failure status")
			}
		})
	}
}
