package work

import (
	"errors"
	"os"
	"testing"
	"time"

	"compound/internal/policy"
)

func TestActionApprovalHandoffRecordFailureBurnsGrantWithoutHistory(t *testing.T) {
	s, w, ai := approvalHandoffFixture(t)
	if _, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	grant := approveHandoffFixture(t, s, ai)
	original := writeStagedJSONFile
	t.Cleanup(func() { writeStagedJSONFile = original })
	writeStagedJSONFile = func(*os.File, []byte) error { return errors.New("fixture work persistence failure") }
	result, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(5*time.Second))
	writeStagedJSONFile = original
	if err == nil || result.Recorded {
		t.Fatalf("failed persistence reported success: %+v %v", result, err)
	}
	status, err := policy.NewExactActionApprovalStore(s.home).Check(grant.ID, fixedNow.Add(6*time.Second))
	if err != nil || status.Status != policy.ExactApprovalStatusConsumed {
		t.Fatalf("grant not consumed: %+v %v", status, err)
	}
	after, err := s.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ActionIntentIDs) != len(w.ActionIntentIDs) || after.PolicyDecisionID != w.PolicyDecisionID {
		t.Fatal("failed record changed performed history")
	}
	if retry, err := s.EvaluateAndRecordAction(w.ID, ai, fixedNow.Add(7*time.Second)); err == nil || retry.Recorded {
		t.Fatalf("burned grant replay: %+v %v", retry, err)
	}
}
