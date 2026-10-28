package harness

import (
	"context"
	"testing"

	"compound/internal/runattempt"
)

func TestOMPNativeVerifierSelectsExplicitOwnerProfile(t *testing.T) {
	f := newOMPVerifierFixture(t, "")
	profileID := f.adapter.profileID
	f.adapter.profileID = "missing-default"
	f.input.LaunchProfileID = profileID
	prepared, err := f.adapter.Preflight(context.Background(), f.input)
	if err != nil {
		t.Fatalf("explicit owner profile was not selected: %v", err)
	}
	if prepared.LaunchProfileID != profileID {
		t.Fatal("prepared profile identity was lost")
	}
	drift := prepared
	drift.LaunchProfileID = "different-owner"
	if err := ValidateOMPNativeVerifierPreparedInput(drift); err == nil {
		t.Fatal("prepared profile drift was admitted")
	}
	out, err := f.adapter.Run(context.Background(), prepared)
	if err != nil || out.RunAttempt.Status != runattempt.StatusSucceeded || out.RunAttempt.LaunchProfileID != profileID {
		t.Fatalf("selected native profile did not survive start revalidation: status=%s profile=%q err=%v", out.RunAttempt.Status, out.RunAttempt.LaunchProfileID, err)
	}
}

func TestOMPPromptOnlySelectsExplicitOwnerProfile(t *testing.T) {
	f := newOMPPromptOnlyFixture(t)
	profileID := f.adapter.profileID
	f.adapter.profileID = "missing-default"
	f.input.LaunchProfileID = profileID
	prepared, err := f.adapter.Preflight(context.Background(), f.input)
	if err != nil {
		t.Fatalf("explicit owner profile was not selected: %v", err)
	}
	if prepared.LaunchProfileID != profileID {
		t.Fatal("prepared profile identity was lost")
	}
	drift := prepared
	drift.LaunchProfileID = "different-owner"
	if err := ValidateOMPPromptOnlyPreparedInput(drift); err == nil {
		t.Fatal("prepared profile drift was admitted")
	}
	out, err := f.adapter.Run(context.Background(), governedOMPInput(prepared))
	if err != nil || out.RunAttempt.Status != runattempt.StatusSucceeded || out.RunAttempt.LaunchProfileID != profileID {
		t.Fatalf("selected consumer profile did not survive start revalidation: status=%s profile=%q err=%v", out.RunAttempt.Status, out.RunAttempt.LaunchProfileID, err)
	}
}

func TestOMPExplicitMissingProfileNeverFallsBack(t *testing.T) {
	t.Run("producer", func(t *testing.T) {
		f := newOMPVerifierFixture(t, "")
		f.input.LaunchProfileID = "missing-explicit"
		if _, err := f.adapter.Preflight(context.Background(), f.input); err == nil {
			t.Fatal("missing explicit profile fell back to default")
		}
	})
	t.Run("consumer", func(t *testing.T) {
		f := newOMPPromptOnlyFixture(t)
		f.input.LaunchProfileID = "missing-explicit"
		if _, err := f.adapter.Preflight(context.Background(), f.input); err == nil {
			t.Fatal("missing explicit profile fell back to default")
		}
	})
}
