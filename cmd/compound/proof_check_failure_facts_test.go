package main

import (
	"compound/internal/work"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestProofCheckFailedStepFactsUseFullOutputBeforeTailTruncation(t *testing.T) {
	originalResolver := proofCheckRuntimeExecutable
	proofCheckRuntimeExecutable = func(string) (string, error) { return "/bin/sh", nil }
	t.Cleanup(func() { proofCheckRuntimeExecutable = originalResolver })

	prefix := strings.Repeat("x", 5000)
	result := runProofCheckStep(t.TempDir(), proofCheckStep{
		Name:     "synthetic-failure",
		Command:  []string{"sh", "-c", "printf '%s\\n' '" + prefix + "'; printf '%s\\n' 'root failure fact' >&2; exit 1"},
		ProofRef: "synthetic-failure-proof",
	}, "")
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if strings.Contains(result.OutputTail, prefix) {
		t.Fatalf("output tail retained the deliberately truncated prefix")
	}
	want := failureFactForTest("root failure fact")
	if !containsString(result.FailureFacts, want) {
		t.Fatalf("failure_facts = %v, want %q derived from full output", result.FailureFacts, want)
	}
}

func TestProofCheckFailedStepSurfacesProcessStartError(t *testing.T) {
	t.Parallel()

	result := runProofCheckStep(t.TempDir(), proofCheckStep{
		Name:     "synthetic-start-failure",
		Command:  []string{"compound-proofcheck-definitely-missing-executable"},
		ProofRef: "synthetic-start-failure-proof",
	}, "")
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.OutputTail, "compound-proofcheck-definitely-missing-executable") {
		t.Fatalf("output tail omitted process start error: %q", result.OutputTail)
	}
}

func TestProofCheckFailureFactsNormalizeObservedEnvironmentalNondeterminism(t *testing.T) {
	t.Parallel()

	goBaseline := `panic: test timed out after 40m0s
	running tests:
		TestGatewayCallerContextTimeout (39m12s)
goroutine 84 [running]:
testing.(*M).startAlarm.func1()
	/opt/homebrew/Cellar/go/1.26.5/libexec/src/testing/testing.go:2802 +0x2cc

goroutine 156 [chan receive, 39 minutes]:
compound/internal/panel.TestGatewayCallerContextTimeout(0x10c74b400)
	/tmp/baseline/repo/internal/panel/gateway_test.go:236 +0x1a0
FAIL	compound/internal/panel	2400.639s`
	goCandidate := `panic: test timed out after 40m0s
	running tests:
		TestGatewayCallerContextTimeout (38m51s)
goroutine 85 [running]:
testing.(*M).startAlarm.func1()
	/opt/homebrew/Cellar/go/1.26.5/libexec/src/testing/testing.go:2802 +0x2cc

goroutine 163 [chan receive, 38 minutes]:
compound/internal/panel.TestGatewayCallerContextTimeout(0x14a5e5600)
	/tmp/candidate/repo/internal/panel/gateway_test.go:236 +0x1a0
FAIL	compound/internal/panel	2400.644s`
	bunBaseline := `error: Cannot find package 'react' from '/tmp/.bun/install/cache/react-dom@19.2.8@@@1/cjs/react-dom-server.bun.development.js'
Bun v1.3.9 (macOS arm64)`
	bunCandidate := `error: EISDIR reading "/tmp/.bun/install/cache/react@19.2.8@@@1\x00@@1/index.js"
Bun v1.3.9 (macOS arm64)`

	for _, tc := range []struct {
		name, gate, left, right string
	}{
		{name: "go timeout stack", gate: "go-test", left: goBaseline, right: goCandidate},
		{name: "bun cache resolution", gate: "console-coordination-smoke", left: bunBaseline, right: bunCandidate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left := work.ProofFailureFacts(tc.gate, []byte(tc.left), errors.New("exit status 1"))
			right := work.ProofFailureFacts(tc.gate, []byte(tc.right), errors.New("exit status 1"))
			if !reflect.DeepEqual(left, right) {
				t.Fatalf("facts differ:\nleft=%v\nright=%v", left, right)
			}
			if len(left) == 0 {
				t.Fatal("normalization erased the failure class")
			}
		})
	}

	otherTest := strings.ReplaceAll(goCandidate, "TestGatewayCallerContextTimeout", "TestDifferentTimeout")
	if reflect.DeepEqual(
		work.ProofFailureFacts("go-test", []byte(goBaseline), errors.New("exit status 1")),
		work.ProofFailureFacts("go-test", []byte(otherTest), errors.New("exit status 1")),
	) {
		t.Fatal("different timed-out test collapsed to the same facts")
	}

	differentRoot := strings.Replace(
		goCandidate,
		"goroutine 163 [chan receive, 38 minutes]:\ncompound/internal/panel.TestGatewayCallerContextTimeout",
		"goroutine 163 [sync.Mutex.Lock, 38 minutes]:\nsync.runtime_SemacquireMutex(0x14a5e5600)\n	/opt/homebrew/Cellar/go/1.26.5/libexec/src/runtime/sema.go:95 +0x25\ncompound/internal/panel.TestGatewayCallerContextTimeout",
		1,
	)
	if reflect.DeepEqual(
		work.ProofFailureFacts("go-test", []byte(goBaseline), errors.New("exit status 1")),
		work.ProofFailureFacts("go-test", []byte(differentRoot), errors.New("exit status 1")),
	) {
		t.Fatal("different timeout root cause collapsed to the same facts")
	}
}

func TestProofCheckFailureFactsNormalizationDoesNotHideAdditionalFailures(t *testing.T) {
	t.Parallel()

	bunCacheFailure := `error: Cannot find package 'react-dom' from '/tmp/.bun/install/cache/@radix-ui/react-portal@1.1.17@@@1/dist/index.mjs'`
	baseline := work.ProofFailureFacts("console-coordination-smoke", []byte(bunCacheFailure), errors.New("exit status 1"))
	candidate := work.ProofFailureFacts("console-coordination-smoke", []byte(bunCacheFailure+"\nerror: candidate-only assertion failed"), errors.New("exit status 1"))
	if reflect.DeepEqual(baseline, candidate) {
		t.Fatal("cache normalization hid an additional candidate-only failure")
	}

	differentPackage := work.ProofFailureFacts(
		"console-coordination-smoke",
		[]byte(`error: Cannot find package 'candidate-only-package' from '/tmp/.bun/install/cache/react-dom@19.2.8@@@1/cjs/index.js'`),
		errors.New("exit status 1"),
	)
	if reflect.DeepEqual(baseline, differentPackage) {
		t.Fatal("different missing packages collapsed to the same cache-resolution fact")
	}
	multiplePackages := work.ProofFailureFacts(
		"console-coordination-smoke",
		[]byte(bunCacheFailure+"\nerror: Cannot find package 'candidate-only-package' from '/tmp/.bun/install/cache/other@1.0.0@@@1/index.js'"),
		errors.New("exit status 1"),
	)
	if len(multiplePackages) <= len(baseline) {
		t.Fatal("multiple distinct missing packages were deduplicated")
	}

	separated := work.ProofFailureFacts(
		"console-coordination-smoke",
		[]byte("diagnostic path: /tmp/.bun/install/cache/react\nerror: Cannot find module candidate-only-package"),
		errors.New("exit status 1"),
	)
	if reflect.DeepEqual(baseline, separated) {
		t.Fatal("cache path and resolver wording on separate lines were incorrectly collapsed")
	}

	ordinaryBaseline := work.ProofFailureFacts(
		"go-test",
		[]byte("--- FAIL: TestStableName (0.01s)\n    thing_test.go:10: got 1, want 2\nFAIL\tcompound/internal/thing\t0.02s"),
		errors.New("exit status 1"),
	)
	ordinaryCandidate := work.ProofFailureFacts(
		"go-test",
		[]byte("--- FAIL: TestStableName (0.01s)\n    thing_test.go:10: got 3, want 2\nFAIL\tcompound/internal/thing\t0.02s"),
		errors.New("exit status 1"),
	)
	if reflect.DeepEqual(ordinaryBaseline, ordinaryCandidate) {
		t.Fatal("ordinary go-test assertion details were semantically collapsed")
	}

	ordinaryTimingBaseline := work.ProofFailureFacts(
		"go-test",
		[]byte("ok  	compound/internal/act	13.387s\n--- FAIL: TestStableName (19.50s)\n    thing_test.go:10: got 1, want 2\nFAIL	compound/internal/thing	132.252s\nFAIL"),
		errors.New("exit status 1"),
	)
	ordinaryTimingCandidate := work.ProofFailureFacts(
		"go-test",
		[]byte("ok  	compound/internal/act	7.102s\n--- FAIL: TestStableName (18.86s)\n    thing_test.go:10: got 1, want 2\nFAIL	compound/internal/thing	127.431s\nFAIL"),
		errors.New("exit status 1"),
	)
	if !reflect.DeepEqual(ordinaryTimingBaseline, ordinaryTimingCandidate) {
		t.Fatalf("go-test timing-only summaries changed failure identity:\nbaseline=%v\ncandidate=%v", ordinaryTimingBaseline, ordinaryTimingCandidate)
	}

	timeoutBaseline := "panic: test timed out after 40m0s\n	running tests:\n		TestGatewayCallerContextTimeout (39m12s)\n\ngoroutine 156 [chan receive, 39 minutes]:\ncompound/internal/panel.TestGatewayCallerContextTimeout(0x10c74b400)\n	/tmp/baseline/repo/internal/panel/gateway_test.go:236 +0x1a0\nFAIL	compound/internal/panel	2400.639s"
	timeoutCandidate := "panic: test timed out after 40m0s\n	running tests:\n		TestGatewayCallerContextTimeout (38m51s)\n\ngoroutine 163 [chan receive, 38 minutes]:\ncompound/internal/panel.TestGatewayCallerContextTimeout(0x14a5e5600)\n	/tmp/candidate/repo/internal/panel/gateway_test.go:236 +0x1a0\nFAIL	compound/internal/panel	2400.644s"
	timeoutWithOrdinaryFailureBaseline := timeoutBaseline + "\n--- FAIL: TestAlsoFailed (0.01s)\n    thing_test.go:10: got 1, want 2"
	timeoutWithOrdinaryFailureCandidate := timeoutCandidate + "\n--- FAIL: TestAlsoFailed (0.01s)\n    thing_test.go:10: got 3, want 2"
	if reflect.DeepEqual(
		work.ProofFailureFacts("go-test", []byte(timeoutWithOrdinaryFailureBaseline), errors.New("exit status 1")),
		work.ProofFailureFacts("go-test", []byte(timeoutWithOrdinaryFailureCandidate), errors.New("exit status 1")),
	) {
		t.Fatal("timeout normalization hid an additional ordinary test failure")
	}
	timeoutWithFatalCandidate := timeoutCandidate + "\nfatal error: concurrent map writes"
	if reflect.DeepEqual(
		work.ProofFailureFacts("go-test", []byte(timeoutBaseline), errors.New("exit status 1")),
		work.ProofFailureFacts("go-test", []byte(timeoutWithFatalCandidate), errors.New("exit status 1")),
	) {
		t.Fatal("timeout normalization hid an unrecognized candidate-only fatal failure")
	}
}

func TestProofCheckFailureFactsAreDeterministicUniqueAndEmptyOnSuccess(t *testing.T) {
	t.Parallel()

	got := work.ProofFailureFacts("generic", []byte(" beta  failure \nalpha failure\nbeta failure\n\n"), errors.New("failed"))
	want := []string{failureFactForTest("alpha failure"), failureFactForTest("beta failure")}
	if len(got) != len(want) {
		t.Fatalf("failure_facts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("failure_facts = %v, want %v", got, want)
		}
	}
	if got := work.ProofFailureFacts("generic", []byte("ignored"), nil); len(got) != 0 {
		t.Fatalf("successful output produced failure facts: %v", got)
	}
}

func failureFactForTest(line string) string {
	normalized := strings.Join(strings.Fields(line), " ")
	sum := sha256.Sum256([]byte(normalized))
	return "sha256:" + hex.EncodeToString(sum[:])
}
