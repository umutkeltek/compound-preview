package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"compound/internal/attention"
	"compound/internal/hookplan"
	"compound/internal/work"
)

const attentionReceiptOnlyJSONChildHome = "COMPOUND_ATTENTION_RECEIPT_ONLY_JSON_CHILD_HOME"
const attentionReceiptOnlyInvalidChildValue = "COMPOUND_ATTENTION_RECEIPT_ONLY_INVALID_CHILD_VALUE"

func TestPrintAttentionTextReceiptOnlyTidyFoldsOnlyNeedsYou(t *testing.T) {
	report := attention.Report{
		Counts: attention.Counts{
			NeedsYou:       7,
			NeedsYouGroups: 4,
			Watching:       1,
		},
		Groups: []attention.Group{
			{
				Key: "gate-needs-you", Band: attention.BandNeedsYou, Source: attention.SourceReviews,
				Area: "release", Count: 1, OperatorSummary: "Gate decision.", RiskGrade: attention.RiskGradeGate,
			},
			{
				Key: "judge-needs-you", Band: attention.BandNeedsYou, Source: attention.SourceReviews,
				Area: "verdict", Count: 1, OperatorSummary: "Judge outcome.", RiskGrade: attention.RiskGradeJudge,
			},
			{
				Key: "tidy-needs-you", Band: attention.BandNeedsYou, Source: attention.SourceReviews,
				Area: "bench", Count: 2, OperatorSummary: "Tidy receipts.", RiskGrade: attention.RiskGradeTidy,
			},
			{
				Key: "tidy-needs-you-second", Band: attention.BandNeedsYou, Source: attention.SourceReviews,
				Area: "bench", Count: 3, OperatorSummary: "More tidy receipts.", RiskGrade: attention.RiskGradeTidy,
			},
			{
				Key: "tidy-watching", Band: attention.BandWatching, Source: attention.SourceWorkbench,
				Area: "bench", Count: 1, OperatorSummary: "Watch tidy work.", RiskGrade: attention.RiskGradeTidy,
			},
		},
	}
	before, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report before render: %v", err)
	}

	output := captureStdout(t, func() {
		printAttentionTextWithReceiptOnlyGrade(report, attention.RiskGradeTidy)
	})

	for _, want := range []string{"Gate decision.", "Judge outcome.", "Watch tidy work."} {
		if !strings.Contains(output, want) {
			t.Fatalf("receipt-only render missing %q:\n%s", want, output)
		}
	}
	for _, folded := range []string{"Tidy receipts.", "More tidy receipts."} {
		if strings.Contains(output, folded) {
			t.Fatalf("tidy needs_you group %q remained in receipt-only render:\n%s", folded, output)
		}
	}
	if !strings.Contains(output, "5 receipt-only (risk_grade tidy) — still in the ledger; view with --json") {
		t.Fatalf("receipt-only render missing summed item burden:\n%s", output)
	}
	if !strings.Contains(output, "attention needs_you=7 groups=4") {
		t.Fatalf("receipt-only render mutated the authoritative header counts:\n%s", output)
	}
	after, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report after render: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("text-only receipt fold mutated report\nbefore=%s\nafter=%s", before, after)
	}
}

func TestPrintAttentionTextDefaultRemainsByteEquivalent(t *testing.T) {
	report := attention.Report{
		Counts: attention.Counts{
			NeedsYou:       2,
			NeedsYouGroups: 1,
			ActionableNow:  1,
			Watching:       3,
			BlockedOnYou:   1,
			FallbackGroups: 1,
		},
		Groups: []attention.Group{{
			Key: "tidy-default", Band: attention.BandNeedsYou, Source: attention.SourceReviews,
			Area: "bench", Kind: attention.KindHygiene, Count: 2, OperatorSummary: "Routine receipts.",
			OperatorUnlock: "File the receipts.", BlastRadius: attention.BlastLocalReversible,
			Reversibility: attention.Reversible, RiskGrade: attention.RiskGradeTidy, Leverage: 1.5,
			Action:         &attention.Action{ID: "receipt.file", Kind: attention.ActionCopyOnly},
			TerminalReason: "waiting for filing",
		}},
	}
	want := "attention needs_you=2 groups=1 actionable_now=1 watching=3 blocked_on_you=1 fallback_groups=1\n" +
		"- tidy 1.50 reviews/bench count=2 Routine receipts. unlock=\"File the receipts.\" action=copy_only/receipt.file reason=\"waiting for filing\"\n"

	defaultOutput := captureStdout(t, func() { printAttentionText(report) })
	if defaultOutput != want {
		t.Fatalf("default attention text changed\ngot:\n%s\nwant:\n%s", defaultOutput, want)
	}
	emptyGradeOutput := captureStdout(t, func() { printAttentionTextWithReceiptOnlyGrade(report, "") })
	if emptyGradeOutput != defaultOutput {
		t.Fatalf("absent receipt-only flag changed text\ndefault:\n%s\nempty-grade:\n%s", defaultOutput, emptyGradeOutput)
	}
}

func TestAttentionCLIReceiptOnlyTidyJSONKeepsEveryGroup(t *testing.T) {
	t.Parallel()

	if homePath := os.Getenv(attentionReceiptOnlyJSONChildHome); homePath != "" {
		cmdAttention([]string{"--json", "--receipt-only-grade", attention.RiskGradeTidy, "--home", homePath})
		os.Exit(0)
	}

	homePath, ready, held := initAttentionReceiptOnlyCLIHome(t)

	cmd := exec.Command(os.Args[0], "-test.run=^TestAttentionCLIReceiptOnlyTidyJSONKeepsEveryGroup$")
	cmd.Env = append(os.Environ(), attentionReceiptOnlyJSONChildHome+"="+homePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("receipt-only tidy JSON CLI must succeed: %v\n%s", err, output)
	}
	var report attention.Report
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("parse receipt-only attention JSON: %v\n%s", err, output)
	}
	for _, want := range []string{ready, held} {
		found := false
		for _, group := range report.Groups {
			if containsAttentionString(group.ItemIDs, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("receipt-only JSON dropped work %q: %+v", want, report.Groups)
		}
	}
	for _, group := range report.Groups {
		if group.RiskGrade == "" || group.RiskGrade != attention.Class(group) {
			t.Fatalf("group %q risk_grade = %q, want Class(group)=%q", group.Key, group.RiskGrade, attention.Class(group))
		}
	}
}

func TestAttentionCLIReceiptOnlyTidyFoldsLiveTextOnly(t *testing.T) {
	homePath, _, _ := initAttentionReceiptOnlyCLIHome(t)

	defaultOutput := captureStdout(t, func() {
		cmdAttention([]string{"--home", homePath})
	})
	if !strings.Contains(defaultOutput, "1 open work item is blocked on operator review.") {
		t.Fatalf("default text dropped tidy needs_you group:\n%s", defaultOutput)
	}
	if strings.Contains(defaultOutput, "receipt-only") {
		t.Fatalf("default text unexpectedly enabled receipt-only folding:\n%s", defaultOutput)
	}

	receiptOnlyOutput := captureStdout(t, func() {
		cmdAttention([]string{"--receipt-only-grade", attention.RiskGradeTidy, "--home", homePath})
	})
	if strings.Contains(receiptOnlyOutput, "1 open work item is blocked on operator review.") {
		t.Fatalf("receipt-only text retained tidy needs_you group:\n%s", receiptOnlyOutput)
	}
	if !strings.Contains(receiptOnlyOutput, "1 safe open work item can move.") {
		t.Fatalf("receipt-only text dropped non-tidy group:\n%s", receiptOnlyOutput)
	}
	if !strings.Contains(receiptOnlyOutput, "1 receipt-only (risk_grade tidy) — still in the ledger; view with --json") {
		t.Fatalf("receipt-only text missing folded-group count:\n%s", receiptOnlyOutput)
	}
	if !strings.Contains(receiptOnlyOutput, "attention needs_you=1 groups=1") {
		t.Fatalf("receipt-only text changed authoritative counts:\n%s", receiptOnlyOutput)
	}
}

func TestAttentionCLIRejectsUnsupportedReceiptOnlyGrades(t *testing.T) {
	t.Parallel()

	if value := os.Getenv(attentionReceiptOnlyInvalidChildValue); value != "" {
		cmdAttention([]string{"--receipt-only-grade", value})
		return
	}

	for _, value := range []string{attention.RiskGradeGate, attention.RiskGradeJudge, "garbage", " tidy "} {
		t.Run(value, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAttentionCLIRejectsUnsupportedReceiptOnlyGrades$")
			cmd.Env = append(os.Environ(), attentionReceiptOnlyInvalidChildValue+"="+value)
			output, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("receipt-only grade %q must exit usage 2, got err=%v output=%s", value, err, output)
			}
			if exitErr.ExitCode() != 2 {
				t.Fatalf("receipt-only grade %q exit = %d, want 2; output=%s", value, exitErr.ExitCode(), output)
			}
			if !strings.Contains(string(output), attentionUsage) {
				t.Fatalf("receipt-only grade %q output missing usage: %s", value, output)
			}
		})
	}
}

func initAttentionReceiptOnlyCLIHome(t *testing.T) (string, string, string) {
	t.Helper()
	homePath := initializedTestHome(t)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	ready, err := store.Create(work.CreateInput{
		WorkID: "work_receipt_json_ready", Owner: "compound-core", State: "ready",
		SuccessPredicate: "ready work remains visible in attention",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	held, err := store.Create(work.CreateInput{
		WorkID: "work_receipt_json_held", Owner: "compound-core", State: "held",
		SuccessPredicate: "held work remains visible in attention",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create held work: %v", err)
	}
	return homePath, ready.ID, held.ID
}

func TestAttentionCLILiveProjectionSeparatesCanMoveFromNeedsOperator(t *testing.T) {
	homePath := initializedTestHome(t)
	t.Setenv("COMPOUND_HOME", homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	ready, err := store.Create(work.CreateInput{
		WorkID: "work_safe_ready", Owner: "compound-core", State: "ready",
		SuccessPredicate: "safe local work can be claimed",
	}, cliTestNow)
	if err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	if _, err := store.Create(work.CreateInput{
		WorkID: "work_real_hold", Owner: "compound-core", State: "held",
		SuccessPredicate: "held work waits for an operator decision",
	}, cliTestNow); err != nil {
		t.Fatalf("create held work: %v", err)
	}

	output := captureStdout(t, func() { cmdAttention([]string{"--json", "--home", homePath}) })
	var report attention.Report
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("parse attention JSON: %v\n%s", err, output)
	}
	if report.Counts.NeedsYou != 1 || report.Counts.NeedsYouGroups != 1 || report.Counts.Watching != 1 {
		t.Fatalf("live counts = %+v, want one held needs_you and one safe watching", report.Counts)
	}
	foundCanMove := false
	foundHeld := false
	for _, group := range report.Groups {
		switch group.Key {
		case "workbench:can-move":
			foundCanMove = true
			if group.Band != attention.BandWatching || group.Action == nil || group.Action.ID != "workbench.dispatch_next" || !containsAttentionString(group.ItemIDs, ready.ID) {
				t.Fatalf("safe live work did not project as watching/can-move: %+v", group)
			}
		case "workbench:blocked-on-you":
			foundHeld = true
			if group.Band != attention.BandNeedsYou || !containsAttentionString(group.ItemIDs, "work_real_hold") || containsAttentionString(group.ItemIDs, ready.ID) {
				t.Fatalf("held live work did not remain the sole needs_you burden: %+v", group)
			}
		case "executor:unassigned", "executor:codex":
			t.Fatalf("executor projection duplicated workbench burden: %+v", group)
		}
	}
	if !foundCanMove || !foundHeld {
		t.Fatalf("live projection missing required groups: can_move=%v held=%v groups=%+v", foundCanMove, foundHeld, report.Groups)
	}
}

func TestAttentionCLIJSONEmbedsOperatorAttentionWithoutChangingGroups(t *testing.T) {
	homePath, _, _ := initAttentionReceiptOnlyCLIHome(t)
	before := snapshotTree(t, homePath)
	first := captureStdout(t, func() {
		cmdAttention([]string{"--json", "--home", homePath})
	})
	second := captureStdout(t, func() {
		cmdAttention([]string{"--json", "--home", homePath})
	})
	after := snapshotTree(t, homePath)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("attention polls mutated home\nbefore=%v\nafter=%v", before, after)
	}

	var payload struct {
		attention.Report
		OperatorAttention hookplan.OperatorAttentionProjection `json:"operator_attention"`
	}
	if err := json.Unmarshal([]byte(first), &payload); err != nil {
		t.Fatalf("parse attention envelope: %v\n%s", err, first)
	}
	var unchanged attention.Report
	if err := json.Unmarshal([]byte(first), &unchanged); err != nil {
		t.Fatalf("parse embedded attention report: %v\n%s", err, first)
	}
	if !reflect.DeepEqual(payload.Counts, unchanged.Counts) ||
		!reflect.DeepEqual(payload.Groups, unchanged.Groups) ||
		!reflect.DeepEqual(payload.Top, unchanged.Top) {
		t.Fatalf("operator projection changed attention identity/counts:\nenvelope=%+v\nreport=%+v",
			payload.Report, unchanged)
	}
	if payload.OperatorAttention.SchemaVersion != hookplan.OperatorAttentionSchemaVersion ||
		payload.OperatorAttention.Total != 0 || len(payload.OperatorAttention.Items) != 0 {
		t.Fatalf("operator attention was not embedded separately: %+v", payload.OperatorAttention)
	}

	var repeated struct {
		OperatorAttention hookplan.OperatorAttentionProjection `json:"operator_attention"`
	}
	if err := json.Unmarshal([]byte(second), &repeated); err != nil {
		t.Fatalf("parse repeated attention envelope: %v\n%s", err, second)
	}
	if !reflect.DeepEqual(payload.OperatorAttention, repeated.OperatorAttention) {
		t.Fatalf("operator attention changed across polls:\nfirst=%+v\nsecond=%+v",
			payload.OperatorAttention, repeated.OperatorAttention)
	}
}

func containsAttentionString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
