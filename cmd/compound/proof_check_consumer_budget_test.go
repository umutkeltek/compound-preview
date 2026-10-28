package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/work"
)

func TestCanonicalConsumerAggregateEvidenceRemainsBounded(t *testing.T) {
	contract := consumerContractFixture("go_consumers_v1")
	consumers := make([]any, 24)
	for i := range consumers {
		consumer := consumerContractFixture("go_consumers_v1")["consumers"].([]any)[0].(map[string]any)
		consumer["id"] = fmt.Sprintf("consumer-%d", i)
		consumer["command"] = []string{"sh", "consumer.sh"}
		consumer["required_tools"] = []string{"sh", "cp"}
		consumers[i] = consumer
	}
	contract["consumers"] = consumers
	outputEvent := `{"Action":"output","Package":"example.invalid/product/integration","Output":"` + strings.Repeat("x", 1536<<10) + `"}`
	native := strings.Replace(consumerGoReportFixture, "\n", "\n"+outputEvent+"\n", 1)
	repo := initCanonicalProofRepo(t, map[string]string{
		"go.mod":              "module example.invalid/product\n",
		"compound-proof.json": consumerContractJSON(t, contract),
		"consumer.sh":         "set -eu\ncp report-source.jsonl \"$COMPOUND_PROOF_REPORT\"\n",
		"report-source.jsonl": native,
	})
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := work.ResolveCanonicalProofPlan(repo, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the native production loop, without unrelated hygiene tools or
	// admission. This direct test cannot mint a canonical certificate.
	plan.Steps = plan.Steps[:len(consumers)]
	report, err := runProofCheckSnapshotContext(work.ContextWithCanonicalProofPlan(t.Context(), plan), repo, false, source, false, false, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	retained := 0
	for _, step := range report.Steps {
		if e := step.ConsumerEvidence; e != nil {
			retained += len(e.Contract) + len(e.Report) + len(e.Tests)
		}
	}
	if report.Status != "failed" || retained > 32<<20 {
		t.Fatalf("aggregate consumer evidence not bounded: status=%s retained=%d", report.Status, retained)
	}
}

func TestCanonicalConsumerBackgroundDeadlineRetainsPartialEvidence(t *testing.T) {
	partial := strings.Join(strings.Split(consumerGoReportFixture, "\n")[:2], "\n") + "\n"
	repo, step := consumerRunnerFixture(t, nativeReportScript(partial)+"sleep 30 >/dev/null 2>&1 &\n", func(c map[string]any) { c["timeout_seconds"] = 1 })
	before := time.Now()
	result := runProofCheckStepContext(t.Context(), repo, step, "HEAD", "HEAD")
	if result.Status != "failed" || time.Since(before) > 10*time.Second || !strings.Contains(result.OutputTail, "deadline exceeded") {
		t.Fatalf("background deadline not enforced: %+v", result)
	}
	if result.ConsumerEvidence == nil || string(result.ConsumerEvidence.Report) != partial {
		t.Fatal("background timeout lost partial native report")
	}
}
