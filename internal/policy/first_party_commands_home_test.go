package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func TestRequireHomeCompatibilityUsesExplicitHomeAroundSubcommand(t *testing.T) {
	ambientHome := filepath.Join(t.TempDir(), "ambient")
	if err := home.Init(ambientHome, "ambient-profile", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	selectedHome := filepath.Join(t.TempDir(), "selected")
	if err := os.Mkdir(selectedHome, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("schema_version: " + home.SchemaVersion + "\nkind: compound_server\nhome_kind: selected-profile\n")
	if err := os.WriteFile(filepath.Join(selectedHome, home.ManifestName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMPOUND_HOME", ambientHome)

	catalog := NewFirstPartyCommandCatalog(map[string]FirstPartyCommandAdmission{
		"home": FirstPartyCommandGoverned,
	}).WithSubcommands("home", map[string]FirstPartyCommandAdmission{
		"private-custody": FirstPartyCommandGoverned,
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "home before subcommand", args: []string{"--home", selectedHome, "private-custody", "repair"}},
		{name: "home after subcommand", args: []string{"private-custody", "repair", "--home", selectedHome}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := catalog.RequireHomeCompatibility("home", tc.args)
			if err == nil || !strings.Contains(err.Error(), "manifest kind") {
				t.Fatalf("RequireHomeCompatibility(%q) error = %v, want selected-home manifest-kind refusal", tc.args, err)
			}
		})
	}
}

func TestRequireHomeCompatibilityExemptsPortableProofCheckSubsets(t *testing.T) {
	// Point COMPOUND_HOME at a directory with no manifest so any
	// non-exempted governed command fails closed.
	t.Setenv("COMPOUND_HOME", t.TempDir())

	catalog := NewFirstPartyCommandCatalog(map[string]FirstPartyCommandAdmission{
		"proof-check": FirstPartyCommandGoverned,
	})

	for _, args := range [][]string{
		{"--ci"},
		{"--ci", "--json", "--base", "origin/main"},
		{"--ci-console"},
	} {
		if err := catalog.RequireHomeCompatibility("proof-check", args); err != nil {
			t.Fatalf("RequireHomeCompatibility(proof-check, %v) = %v, want nil (fresh-clone-portable subset)", args, err)
		}
	}

	// The canonical work-driven path keeps the full admission: the same
	// missing home must fail closed without a portable flag.
	err := catalog.RequireHomeCompatibility("proof-check", []string{"--work", "work_x", "--actor", "operator:y"})
	if err == nil || !strings.Contains(err.Error(), "home manifest is missing") {
		t.Fatalf("RequireHomeCompatibility(proof-check, canonical) = %v, want missing-manifest refusal", err)
	}
}

func TestRequireHomeCompatibilityExemptsMalformedProofCheckReviewInvocations(t *testing.T) {
	// A bare machine has no home manifest. cmdProofCheck refuses these
	// shapes with a usage error before touching any home, so admission must
	// let them reach that refusal instead of replacing it with a
	// missing-manifest error (the CI runner saw exactly that replacement).
	t.Setenv("COMPOUND_HOME", t.TempDir())

	catalog := NewFirstPartyCommandCatalog(map[string]FirstPartyCommandAdmission{
		"proof-check": FirstPartyCommandGoverned,
	})

	for _, args := range [][]string{
		{"--redact-review"},
		{"--redact-review", "--ci"},
		{"--redact-review", "act_unapproved", "--dev"},
		{"--redact-review", "act_unapproved", "--dev=lane"},
		{"--redact-review", "act_unapproved", "--presubmit"},
		{"--redact-review", "act_unapproved", "--ci"},
		{"--redact-review", "act_unapproved", "--ci-console"},
		{"--redact-review", "act_unapproved", "--dry-run"},
		{"--redact-review", "act_unapproved", "--timings"},
		{"--redact-review", "act_unapproved", "--recover-legacy-retained"},
	} {
		if err := catalog.RequireHomeCompatibility("proof-check", args); err != nil {
			t.Fatalf("RequireHomeCompatibility(proof-check, %v) = %v, want nil (usage refusal owns this shape)", args, err)
		}
	}

	// A well-formed review is a real canonical proof request and keeps the
	// full mutation admission.
	err := catalog.RequireHomeCompatibility("proof-check", []string{"--redact-review", "act_approved", "--work", "work_x", "--actor", "operator:y"})
	if err == nil || !strings.Contains(err.Error(), "home manifest is missing") {
		t.Fatalf("RequireHomeCompatibility(proof-check, well-formed review) = %v, want missing-manifest refusal", err)
	}
}

func TestRequireHomeCompatibilityExemptsOnlySoleProductRootHelp(t *testing.T) {
	t.Setenv("COMPOUND_HOME", filepath.Join(t.TempDir(), "missing-home"))
	catalog := NewFirstPartyCommandCatalog(map[string]FirstPartyCommandAdmission{
		"product": FirstPartyCommandGoverned,
	})
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		if err := catalog.RequireHomeCompatibility("product", args); err != nil {
			t.Fatalf("sole help %v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		nil, {"unknown"}, {"import", "payload", "--help"},
		{"--help", "import", "payload"}, {"--help", "--unknown"}, {"-h", "import"},
	} {
		if err := catalog.RequireHomeCompatibility("product", args); err == nil {
			t.Fatalf("non-sole help %v bypassed home admission", args)
		}
	}
}

func TestRequireHomeCompatibilityExemptsOnlySoleSourceObserveHelp(t *testing.T) {
	missingHome := filepath.Join(t.TempDir(), "missing-home")
	t.Setenv("COMPOUND_HOME", missingHome)
	catalog := NewFirstPartyCommandCatalog(map[string]FirstPartyCommandAdmission{
		"source": FirstPartyCommandHold,
	})
	for _, args := range [][]string{{"observe", "--help"}, {"observe", "-h"}} {
		if err := catalog.RequireHomeCompatibility("source", args); err != nil {
			t.Fatalf("sole source observe help %v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		nil, {"unknown"}, {"observe"},
		{"observe", "obs.yaml"},
		{"observe", "--help", "obs.yaml"},
		{"observe", "obs.yaml", "--help"},
		{"observe", "--help", "--json"},
		{"--help", "observe"},
		{"observe-batch", "--help"},
		{"observations", "--help"},
	} {
		err := catalog.RequireHomeCompatibility("source", args)
		if err == nil || !strings.Contains(err.Error(), "home manifest is missing") {
			t.Fatalf("non-sole source observe help %v escaped admission: %v", args, err)
		}
	}
	// Mixed help and plain observe keep the mutating classification: the flag
	// admission table must not grow a blanket observe-help read entry.
	for _, args := range [][]string{
		{"observe"},
		{"observe", "obs.yaml"},
		{"observe", "obs.yaml", "--help"},
		{"observe", "--help", "obs.yaml"},
	} {
		admission, ok := catalog.LookupSubcommandWithFlags("source", "observe", args)
		if !ok || admission != FirstPartyCommandHold {
			t.Fatalf("source observe classification for %v = %q (ok=%t), want hold", args, admission, ok)
		}
	}
}

func TestRequireHomeCompatibilityExemptsOnlySoleActiveRootHelp(t *testing.T) {
	missingParent := filepath.Join(t.TempDir(), "missing-parent")
	t.Setenv("COMPOUND_HOME", filepath.Join(missingParent, "home"))
	commands := []string{"finding", "knowledge", "writeback", "placement", "contract", "envelope", "schema", "runtime"}
	admissions := map[string]FirstPartyCommandAdmission{"unrelated": FirstPartyCommandGoverned}
	for _, command := range commands {
		admissions[command] = FirstPartyCommandGoverned
	}
	catalog := NewFirstPartyCommandCatalog(admissions)
	for _, command := range commands {
		for _, args := range [][]string{{"--help"}, {"-h"}} {
			if err := catalog.RequireHomeCompatibility(command, args); err != nil {
				t.Fatalf("%s sole help %v: %v", command, args, err)
			}
		}
		for _, args := range [][]string{nil, {"unknown"}, {"--help", "mutate"}, {"-h", "mutate"}, {"--help", "--unknown"}, {"mutate", "payload", "--help"}, {"mutate", "-h"}} {
			if err := catalog.RequireHomeCompatibility(command, args); err == nil || !strings.Contains(err.Error(), "home manifest is missing") {
				t.Fatalf("%s non-sole help %v escaped admission: %v", command, args, err)
			}
		}
	}
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		if err := catalog.RequireHomeCompatibility("unrelated", args); err == nil {
			t.Fatalf("unrelated command help %v bypassed admission", args)
		}
	}
	if _, err := os.Lstat(missingParent); !os.IsNotExist(err) {
		t.Fatalf("compatibility check created missing home parent: %v", err)
	}
}
