package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tools are synthetic executables. No test builds with Xcode or boots a simulator.
func xcodePreflightFixture(t *testing.T, fault string) (string, ProofCheckStep, ProofRuntimeResolver) {
	t.Helper()
	developer := t.TempDir()
	bin := filepath.Join(developer, "usr", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVELOPER_DIR", developer)
	t.Setenv("DATABASE_URL", "synthetic-secret")
	t.Setenv("TOOLCHAINS", "ambient-toolchain")
	version := "printf 'Xcode 99.0\\nBuild version Synthetic\\n'"
	if fault == "version failure" {
		version = "exit 31"
	} else if fault == "slow version" {
		version = "exec sleep 30"
	} else if fault == "empty version" {
		version = "exit 0"
	} else if fault == "invalid version" {
		version = "printf 'Command Line Tools\\n'"
	}
	writeTool := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if fault == "diagnostic stderr" {
		version = "printf 'synthetic diagnostic\\n' >&2\n" + version
	}
	writeTool("xcodebuild", version)
	writeTool("xcresulttool", "exit 0")
	// Apple's Git shim also consults DEVELOPER_DIR. Keep fixture Git operations
	// on the host's original selection while product probes use the synthetic one.
	writeTool("git", "unset DEVELOPER_DIR TOOLCHAINS\nexec '"+strings.ReplaceAll(nativeGit, "'", "'\\''")+"' \"$@\"")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	findBuild := `printf '%s\n' "$DEVELOPER_DIR/usr/bin/xcodebuild"`
	if fault == "lookup failure" {
		findBuild = "exit 32"
	} else if fault == "relative lookup" {
		findBuild = "printf 'usr/bin/xcodebuild\\n'"
	} else if fault == "missing lookup" {
		findBuild = `printf '%s\n' "$DEVELOPER_DIR/usr/bin/missing"`
	} else if fault == "nonexecutable lookup" {
		if err := os.Chmod(filepath.Join(bin, "xcresulttool"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	summaryHelp, testsHelp := "printf 'summary help\\n'", "printf 'tests help\\n'"
	if fault == "unsupported summary" {
		summaryHelp = "exit 33"
	} else if fault == "empty summary help" {
		summaryHelp = "exit 0"
	} else if fault == "unsupported tests" {
		testsHelp = "exit 34"
	}
	writeTool("xcrun", fmt.Sprintf(`if [ %q = "diagnostic stderr" ]; then printf 'synthetic diagnostic\n' >&2; fi
test -z "${DATABASE_URL+x}" && test -z "${TOOLCHAINS+x}" || exit 41
case "$*" in
  '--find xcodebuild') %s ;;
  '--find xcresulttool') printf '%%s\n' "$DEVELOPER_DIR/usr/bin/xcresulttool" ;;
  'xcresulttool get test-results summary --help') %s ;;
  'xcresulttool get test-results tests --help') %s ;;
  'xcresulttool get test-results summary --path '* ) printf '%%s' '{"result":"Passed","totalTestCount":1,"passedTests":1,"failedTests":0,"skippedTests":0,"expectedFailures":0,"testFailures":[]}' ;;
  'xcresulttool get test-results tests --path '* ) printf '%%s' '{"testNodes":[{"nodeType":"Test Case","nodeIdentifier":"ActivationTests/testFirstAppearance()","result":"Passed"}]}' ;;
  *) exit 42 ;;
esac`, fault, findBuild, summaryHelp, testsHelp))
	consumer := CanonicalConsumer{ID: "activation", Command: []string{"sh", "consumer.sh"}, Dir: ".", TimeoutSeconds: 5,
		RequiredTools: []string{"sh"}, RequiredEnvironment: []string{"DEVELOPER_DIR"}, ReportKind: "xcresult", RequiredTests: []string{"ActivationTests/testFirstAppearance()"}}
	if fault == "slow version" {
		consumer.TimeoutSeconds = 1
	}
	body, err := json.Marshal(canonicalConsumerContract{SchemaVersion: "compound.proof_consumers.v1", Profile: "xcode_consumers_v1", Consumers: []CanonicalConsumer{consumer}})
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	for name, contents := range map[string]string{
		"compound-proof.json": string(body),
		"consumer.sh":         "set -eu\ntouch launched\nmkdir \"$COMPOUND_PROOF_REPORT\"\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nodeGit(t, repo, "init", "-q")
	nodeGit(t, repo, "add", "--", "compound-proof.json", "consumer.sh")
	nodeGit(t, repo, "commit", "-qm", "synthetic Xcode consumer")
	resolver := func(tool string) (string, error) {
		if tool == "xcrun" || tool == "xcodebuild" {
			if fault == "missing xcodebuild" && tool == "xcodebuild" {
				return "", fmt.Errorf("synthetic missing xcodebuild")
			}
			return filepath.Join(bin, tool), nil
		}
		return exec.LookPath(tool)
	}
	return repo, canonicalConsumerStep(consumer, body), resolver
}

func TestCanonicalXcodePreflightRefusesBeforeProductLaunch(t *testing.T) {
	for _, fault := range []string{"missing xcodebuild", "lookup failure", "relative lookup", "missing lookup", "nonexecutable lookup", "version failure", "invalid version", "empty version", "unsupported summary", "empty summary help", "unsupported tests"} {
		t.Run(fault, func(t *testing.T) {
			repo, step, resolver := xcodePreflightFixture(t, fault)
			_, evidence, err := RunCanonicalConsumer(t.Context(), repo, step, nil, exec.CommandContext, resolver)
			if err == nil {
				t.Fatal("unusable Xcode toolchain accepted")
			}
			if evidence == nil || len(evidence.Contract) == 0 || len(evidence.Report) != 0 {
				t.Fatalf("refusal lost contract or captured native success: %+v", evidence)
			}
			if _, err := os.Stat(filepath.Join(repo, "launched")); !os.IsNotExist(err) {
				t.Fatalf("product launched before toolchain refusal: %v", err)
			}
		})
	}
}

func TestCanonicalXcodePreflightUsesConsumerEnvironmentAndNativeCustody(t *testing.T) {
	for _, fault := range []string{"", "diagnostic stderr"} {
		t.Run(fault, func(t *testing.T) {
			repo, step, resolver := xcodePreflightFixture(t, fault)
			output, evidence, err := RunCanonicalConsumer(t.Context(), repo, step, nil, exec.CommandContext, resolver)
			if err != nil {
				t.Fatalf("synthetic toolchain failed: %v, %s", err, output)
			}
			step.ConsumerEvidence = evidence
			if err := ValidateCanonicalConsumerEvidence(step); err != nil {
				t.Fatalf("preflight changed native custody: %v", err)
			}
			if _, err := os.Stat(filepath.Join(repo, "launched")); err != nil {
				t.Fatalf("ready toolchain did not launch product: %v", err)
			}
		})
	}
}

func TestCanonicalXcodePreflightHonorsCancellationBeforeLaunch(t *testing.T) {
	repo, step, resolver := xcodePreflightFixture(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := RunCanonicalConsumer(ctx, repo, step, nil, exec.CommandContext, resolver)
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("cancellation not surfaced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "launched")); !os.IsNotExist(err) {
		t.Fatalf("canceled preflight launched product: %v", err)
	}
}

func TestCanonicalXcodePreflightHonorsConsumerDeadline(t *testing.T) {
	repo, step, resolver := xcodePreflightFixture(t, "slow version")
	_, _, err := RunCanonicalConsumer(t.Context(), repo, step, nil, exec.CommandContext, resolver)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled tool did not preserve consumer deadline: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "launched")); !os.IsNotExist(err) {
		t.Fatalf("expired preflight launched product: %v", err)
	}
}
