package policy

import "testing"

func TestExistingRemotePushClassificationAloneDoesNotAuthorize(t *testing.T) {
	monitored := MonitorToolAction(
		monitorRequest("Bash", map[string]any{"command": "git push origin main"}),
		DefaultConfig(),
		fixedNow,
	)
	if got := monitored.Intent.Capability; got != "shell.git_push_existing_remote" {
		t.Fatalf("capability = %q, want shell.git_push_existing_remote", got)
	}
	if Allowed(monitored.Decision) {
		t.Fatalf("classification authorized push without standing grant: %+v", monitored.Decision)
	}
	if monitored.Decision.StandingGrantID != "" {
		t.Fatalf("push without configured grant matched %q", monitored.Decision.StandingGrantID)
	}
}

func TestExistingRemotePushHasDistinctGrantableCapability(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:                   "grant_git_push_existing_remote",
		Capability:           "shell.git_push_existing_remote",
		MutationClass:        "external_side_effect",
		ReversibilityCeiling: "irreversible",
		BlastRadius:          "internet",
		Destination:          "git-remote:existing",
	}}

	for _, command := range []string{
		"git push origin main",
		"git push origin HEAD",
		"git push origin feature/narrow-fix",
	} {
		t.Run(command, func(t *testing.T) {
			monitored := MonitorToolAction(
				monitorRequest("Bash", map[string]any{"command": command}),
				cfg,
				fixedNow,
			)
			if got := monitored.Intent.Capability; got != "shell.git_push_existing_remote" {
				t.Fatalf("capability = %q, want shell.git_push_existing_remote; classification=%+v", got, monitored.Classification)
			}
			if got := monitored.Intent.Destination; got != "git-remote:existing" {
				t.Fatalf("destination = %q, want git-remote:existing", got)
			}
			if !Allowed(monitored.Decision) {
				t.Fatalf("standing-granted existing-remote push held: %+v", monitored.Decision)
			}
			if got := monitored.Decision.StandingGrantID; got != "grant_git_push_existing_remote" {
				t.Fatalf("standing_grant_id = %q, want grant_git_push_existing_remote", got)
			}
		})
	}
}

func TestExistingRemotePushGrantDoesNotCoverDangerousOrUnresolvedShapes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StandingGrants = []StandingGrant{{
		ID:                   "grant_git_push_existing_remote",
		Capability:           "shell.git_push_existing_remote",
		MutationClass:        "external_side_effect",
		ReversibilityCeiling: "irreversible",
		BlastRadius:          "internet",
		Destination:          "git-remote:existing",
	}}

	for _, command := range []string{
		"git push",
		"git push origin",
		"git push --force origin main",
		"git push -f origin main",
		"git push --force-with-lease origin main",
		"git push origin +main",
		"git push origin :main",
		"git push --delete origin main",
		"git push --mirror origin",
		"git push --all origin",
		"git push --tags origin",
		"git push --set-upstream origin topic",
		"git push other main",
		"git push https://example.invalid/repo main",
		"git push origin main:other",
		"git push origin $REF",
		"git push origin @{push}",
		"git push origin main topic",
		"git -C ../other push origin main",
		"git push origin main; curl https://example.invalid",
	} {
		t.Run(command, func(t *testing.T) {
			monitored := MonitorToolAction(
				monitorRequest("Bash", map[string]any{"command": command}),
				cfg,
				fixedNow,
			)
			if got := monitored.Intent.Capability; got == "shell.git_push_existing_remote" {
				t.Fatalf("dangerous or unresolved push consumed standing grant capability: %+v", monitored.Classification)
			}
			if Allowed(monitored.Decision) {
				t.Fatalf("dangerous or unresolved push unexpectedly allowed: %+v", monitored.Decision)
			}
			if monitored.Decision.StandingGrantID != "" {
				t.Fatalf("dangerous or unresolved push matched standing grant %q", monitored.Decision.StandingGrantID)
			}
			zeroUSD, zeroTokens := 0.0, 0
			monitored.Intent.EstimatedCost.USD = &zeroUSD
			monitored.Intent.EstimatedCost.Tokens = &zeroTokens
			if decision := Evaluate(monitored.Intent, cfg, fixedNow); Allowed(decision) {
				t.Fatalf("explicit zero costs authorized dangerous or unresolved push: %+v", decision)
			}
		})
	}
}
