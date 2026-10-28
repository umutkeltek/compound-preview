package harness

import (
	"strings"
	"testing"
)

// TestOhMyPiApprovalArgsMapsGovernedPostureExplicitly pins the single source
// of truth for the oh_my_pi posture translation. The wrapper at
// a generated lane wrapper may fold EVERY arg into the prompt — a
// regression that already broke headless mutation on this host — so the only
// way a governed OMP launch can carry its posture is the explicit argv
// this function adds. Every mode in the table must map; an unknown mode
// MUST fail closed (a silent fallback to always-ask would re-create the
// same headless-write defect).
func TestOhMyPiApprovalArgsMapsGovernedPostureExplicitly(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		wantFlag    string
		wantErrFrag string
	}{
		{"bypass_to_yolo", "bypass", "--approval-mode=yolo", ""},
		{"acceptEdits_to_write", "acceptEdits", "--approval-mode=write", ""},
		{"plan_to_always_ask", "plan", "--approval-mode=always-ask", ""},
		{"default_to_always_ask", "default", "--approval-mode=always-ask", ""},
		{"empty_to_always_ask", "", "--approval-mode=always-ask", ""},
		{"unknown_fails_closed", "guess-mode", "", "unknown permission mode"},
		{"typo_fails_closed", "byapss", "", "unknown permission mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := OhMyPiApprovalArgs(tc.mode)
			if tc.wantErrFrag != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil argv %v", tc.wantErrFrag, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrFrag) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrFrag)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			found := false
			for _, a := range got {
				if a == tc.wantFlag {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("argv %v missing %q", got, tc.wantFlag)
			}
		})
	}
}

// TestOhMyPiHookArgsRefusesEmpty is the empty-shim refusal: a bypass
// launch with no loadable shim would be a launch with NO enforcement
// surface, exactly the silent fail-open the packet exists to fix. The
// refusal is the contract, not the side effect; we surface it as a
// function error so buildCmd fails before minting a RunAttempt.
func TestOhMyPiHookArgsRefusesEmpty(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantErrFrag string
	}{
		{"empty_refused", "", "required"},
		{"whitespace_refused", "   ", "required"},
		{"valid_path_returns_flag", "/tmp/compound.ts", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := OhMyPiHookArgs(tc.path)
			if tc.wantErrFrag != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil argv %v", tc.wantErrFrag, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrFrag) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrFrag)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 || !strings.HasPrefix(got[0], "--hook=") {
				t.Fatalf("expected single --hook= flag, got %v", got)
			}
		})
	}
}
