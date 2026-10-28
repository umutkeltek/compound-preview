package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"compound/internal/flagaudit"
)

// TestFlagValidationRatchetChild is the subprocess half of the flag-validation
// ratchet (contract: internal/flagaudit): re-exec'd by the census run, it
// decodes the probe argv and calls main(), so the probe travels the real
// dispatch path — global args, registry lookup, handler Run.
func TestFlagValidationRatchetChild(t *testing.T) {
	if os.Getenv(flagaudit.ChildEnvActivate) != "1" {
		return
	}
	raw, err := base64.RawStdEncoding.DecodeString(os.Getenv(flagaudit.ChildEnvArgs))
	if err != nil {
		t.Fatalf("decode probe argv: %v", err)
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("parse probe argv: %v", err)
	}
	os.Args = append([]string{"compound"}, args...)
	main()
}

// TestFirstPartyFlagValidationRatchet pins, as a ceiling, the census of every
// first-party command path the CLI runtime registry exposes that silently
// accepts an unknown flag instead of refusing it — the generalized form of
// the `compound work list --json` table-swallow defect (internal/work/
// list_flags.go). A registry command with no flag validation swallows a
// garbage flag and still exits 0 without naming it, lands in the census, and
// fails the ceiling here.
func TestFirstPartyFlagValidationRatchet(t *testing.T) {
	seen := map[string]bool{}
	var probes []flagaudit.Probe
	add := func(name string, args ...string) {
		if !seen[name] {
			seen[name] = true
			probes = append(probes, flagaudit.Probe{Name: name, Args: args})
		}
	}
	for name, command := range compoundCommandRegistry() {
		add(name, name)
		for sub := range command.SubcommandAdmission {
			add(name+" "+sub, name, sub)
		}
		for sub := range command.SubcommandFlagAdmission {
			add(name+" "+sub, name, sub)
		}
	}
	sort.Slice(probes, func(i, j int) bool { return probes[i].Name < probes[j].Name })

	census := flagaudit.RunCensus(t, probes, flagaudit.Options{})
	if err := flagaudit.EnforceCeiling(census, flagaudit.PinnedSilentAcceptors); err != nil {
		t.Fatalf("flag-validation ratchet: %v\ncensus is %d of %d probed paths:\n  %s",
			err, len(census), len(probes), strings.Join(census, "\n  "))
	}
	t.Logf("flag-validation ratchet holds: %d/%d registry paths refuse unknown flags (pinned ceiling %d)",
		len(probes)-len(census), len(probes), len(flagaudit.PinnedSilentAcceptors))
}
