package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/work"
)

func TestScopedInstructionPackOwnDriftRemainsBlocking(t *testing.T) {
	details := []string{"effective_contract layer drifted (synthetic own authority change)", "managed pack body content_sha256 differs"}
	if got := delegateBlockingInstructionPackDrift(details); !reflect.DeepEqual(got, details) {
		t.Fatalf("own-context contract drift was suppressed: got %v, want %v", got, details)
	}
}

func TestDelegateReadinessSurvivesUnrelatedContextButRejectsOwnChange(t *testing.T) {
	fixture := buildReadyDelegateHarnessFixture(t, "work_delegate_scoped_context")
	request := work.DelegateHarnessReadinessRequest{
		HomePath: fixture.compoundHome, HarnessHome: fixture.harnessHome,
		WorkID: fixture.workID, ContextPackID: fixture.contextPackID, Workspace: fixture.workspace,
		Harness: fixture.harness, LaneID: fixture.harness, RequestedModelID: "fixture-zai",
		RequestedProviderRoute: "zai", Command: "/bin/echo", Now: fixture.now.Add(time.Minute),
	}
	assertReady := func() {
		t.Helper()
		got, err := evaluateDelegateHarnessReadiness(request)
		if err != nil || !got.Ready {
			t.Fatalf("own context lost readiness: %+v, %v", got, err)
		}
	}
	assertReady()
	unrelated := fixture.effective
	unrelated.TaskContract.Intent = "inspect a different synthetic task"
	recordDelegateRuntimeForTest(t, fixture.compoundHome, unrelated, fixture.workID, fixture.workspace, fixture.now.Add(time.Minute))
	assertReady()
	changed := fixture.effective
	changed.ActiveDecisions = []string{"decision://synthetic-own-change"}
	recordDelegateRuntimeForTest(t, fixture.compoundHome, changed, fixture.workID, fixture.workspace, fixture.now.Add(2*time.Minute))
	got, err := evaluateDelegateHarnessReadiness(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ready || !strings.Contains(strings.Join(got.Details, " "), "effective_contract layer drifted") {
		t.Fatalf("own contract change did not block readiness: %+v", got)
	}
}
