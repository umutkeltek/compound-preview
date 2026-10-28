package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/eventsource"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
)

// runEventShowCLI mirrors runEventStoreCLI's convention: drive the testable
// core directly with fresh buffers so an error path's os.Exit-equivalent
// return code is assertable without crashing the test binary.
func runEventShowCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runEventShow(args, &out, &errb)
	return out.String(), errb.String(), code
}

// seedEventStoreRecord writes a record straight through the store's own put
// seam (never a hand-built file), honoring the frame-id-match law: the
// record's own "id" field must equal the put id or the segment engine rejects
// the frame as invalid.
func seedEventStoreRecord(t *testing.T, homePath, id, schemaVersion string, extra map[string]any) {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	value := map[string]any{
		"id":             id,
		"schema_version": schemaVersion,
		"created_at":     "2026-07-16T00:00:00Z",
	}
	for k, v := range extra {
		value[k] = v
	}
	if _, err := store.PutJSON(id, value, "  "); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// F1: on a TEST home seeded with a Receipt record, `compound event show
// rec_<id>` prints the receipt body. RED at base (errors: "event show:
// unknown trace reference..." / not-found via ShowTrace's forced trace_
// prepend); GREEN after the store-read dispatch lands.
func TestEventShowResolvesReceiptRecordFromStore(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	seedEventStoreRecord(t, homePath, "rec_f1_receipt", "compound.receipt.v0.1", map[string]any{
		"summary": "falsifier receipt body",
	})

	jsonOut, jsonErr, code := runEventShowCLI(t, "rec_f1_receipt", "--json")
	if code != 0 {
		t.Fatalf("show --json exit = %d, stderr=%s", code, jsonErr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
		t.Fatalf("parse show --json: %v\n%s", err, jsonOut)
	}
	if decoded["id"] != "rec_f1_receipt" || decoded["summary"] != "falsifier receipt body" {
		t.Fatalf("show --json body = %+v", decoded)
	}

	humanOut, humanErr, code := runEventShowCLI(t, "rec_f1_receipt")
	if code != 0 {
		t.Fatalf("show human exit = %d, stderr=%s", code, humanErr)
	}
	if !strings.Contains(humanOut, "rec_f1_receipt") ||
		!strings.Contains(humanOut, "- kind: receipt") ||
		!strings.Contains(humanOut, "falsifier receipt body") {
		t.Fatalf("show human output =\n%s", humanOut)
	}
}

// F2: the same store-read dispatch resolves at least pol_ and hook_ kinds.
func TestEventShowResolvesPolicyAndHookRecordsFromStore(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	seedEventStoreRecord(t, homePath, "pol_f2_policy", "compound.policy_decision.v0.1", map[string]any{
		"outcome": "allow",
	})
	seedEventStoreRecord(t, homePath, "hook_f2_hook", "compound.hook_event.v0.1", map[string]any{
		"hook_name": "pre-tool-use",
	})

	polOut, polErr, code := runEventShowCLI(t, "pol_f2_policy", "--json")
	if code != 0 {
		t.Fatalf("show policy exit = %d, stderr=%s", code, polErr)
	}
	var pol map[string]any
	if err := json.Unmarshal([]byte(polOut), &pol); err != nil {
		t.Fatalf("parse policy show: %v\n%s", err, polOut)
	}
	if pol["id"] != "pol_f2_policy" || pol["outcome"] != "allow" {
		t.Fatalf("policy show body = %+v", pol)
	}

	hookOut, hookErr, code := runEventShowCLI(t, "hook_f2_hook", "--json")
	if code != 0 {
		t.Fatalf("show hook exit = %d, stderr=%s", code, hookErr)
	}
	var hook map[string]any
	if err := json.Unmarshal([]byte(hookOut), &hook); err != nil {
		t.Fatalf("parse hook show: %v\n%s", err, hookOut)
	}
	if hook["id"] != "hook_f2_hook" || hook["hook_name"] != "pre-tool-use" {
		t.Fatalf("hook show body = %+v", hook)
	}
}

// F3: regression. `event show trace_<id>` and the current bare-trace-id
// behavior stay byte-identical to the untouched eventsource.ShowTrace path —
// dispatch never diverts a real trace id or a bare id to the new store path.
func TestEventShowTraceAndBareIDRegressionUnchanged(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	fixture := filepath.Join("..", "..", "fixtures", "events", "os-events.ndjson")
	captureStdout(t, func() {
		cmdEvent([]string{"import-os", fixture, "--stream", "os-control-events"})
	})
	authorityOut := captureStdout(t, func() {
		cmdEvent([]string{"authority", "--json"})
	})
	var authorityReport eventsource.AuthorityReport
	if err := json.Unmarshal([]byte(authorityOut), &authorityReport); err != nil {
		t.Fatalf("parse authority: %v\n%s", err, authorityOut)
	}
	if len(authorityReport.RecentTraces) == 0 {
		t.Fatalf("no traces to show")
	}
	traceID := authorityReport.RecentTraces[0].ID
	if !strings.HasPrefix(traceID, "trace_") {
		t.Fatalf("fixture trace id missing trace_ prefix: %q", traceID)
	}
	bareID := strings.TrimPrefix(traceID, "trace_")

	directDetail, err := eventsource.ShowTrace(homePath, traceID)
	if err != nil {
		t.Fatalf("direct ShowTrace: %v", err)
	}
	wantJSON, err := json.MarshalIndent(directDetail, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	fullOut, fullErr, code := runEventShowCLI(t, traceID, "--json")
	if code != 0 {
		t.Fatalf("show trace_ exit = %d, stderr=%s", code, fullErr)
	}
	if strings.TrimRight(fullOut, "\n") != string(wantJSON) {
		t.Fatalf("show trace_<id> --json diverged from direct ShowTrace:\ngot=%s\nwant=%s", fullOut, wantJSON)
	}

	bareOut, bareErr, code := runEventShowCLI(t, bareID, "--json")
	if code != 0 {
		t.Fatalf("show bare id exit = %d, stderr=%s", code, bareErr)
	}
	if bareOut != fullOut {
		t.Fatalf("bare-id show diverged from trace_-prefixed show:\nbare=%s\nfull=%s", bareOut, fullOut)
	}
}

// F4: an unknown, non-trace-like prefix errors naming the supported
// prefixes, exits 1, and never reaches ShowTrace (whose own not-found error
// text is absent from stderr — proof the two paths are genuinely distinct).
func TestEventShowUnknownPrefixErrorsBeforeShowTrace(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)

	out, errOut, code := runEventShowCLI(t, "zzz_123")
	if code != 1 {
		t.Fatalf("unknown prefix exit = %d, want 1; stdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "unknown id prefix") || !strings.Contains(errOut, "zzz_") {
		t.Fatalf("unknown prefix error missing prefix detail: %s", errOut)
	}
	for _, want := range []string{"trace_", "rec_", "hook_", "act_", "pol_", "tr_"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("unknown prefix error must name supported prefix %q: %s", want, errOut)
		}
	}
	if strings.Contains(errOut, "not found in event store (unknown trace id") {
		t.Fatalf("unknown prefix must never reach ShowTrace, got: %s", errOut)
	}
}

// classifyEventShowID unit coverage for the dispatch edges that a CLI-level
// test would otherwise have to reconstruct indirectly: the act_/act_live_
// dual-prefix family, the historical/logical trace reference forms, and the
// non-collision between "trace_" and "tr_" (transition).
func TestClassifyEventShowID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		id            string
		wantKind      string
		wantShowTrace bool
		wantErr       bool
	}{
		{"trace prefix", "trace_abc123", "", true, false},
		{"bare id no prefix", "abc123def456", "", true, false},
		{"logical trace ref", "compound://events/trace_abc123", "", true, false},
		{"historical trace ref", "events/trace_abc123.json", "", true, false},
		{"receipt prefix", "rec_abc123", "receipt", false, false},
		{"hook prefix", "hook_abc123", "hook_event", false, false},
		{"action prefix", "act_abc123", "action_intent", false, false},
		{"action live prefix", "act_live_abc123", "action_intent", false, false},
		{"policy prefix", "pol_abc123", "policy_decision", false, false},
		{"transition prefix", "tr_abc123", "transition", false, false},
		{"approval plan prefix", "approval_plan_abc123", "exact_action_approval_plan", false, false},
		{"approval grant prefix", "approval_grant_abc123", "exact_action_approval", false, false},
		{"unknown approval prefix", "approval_other_abc123", "", false, true},
		{"unknown prefix", "zzz_abc123", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, useShowTrace, err := classifyEventShowID(tc.id)
			if (err != nil) != tc.wantErr {
				t.Fatalf("classifyEventShowID(%q) err = %v, wantErr %v", tc.id, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if kind != tc.wantKind || useShowTrace != tc.wantShowTrace {
				t.Fatalf("classifyEventShowID(%q) = (%q, %v), want (%q, %v)", tc.id, kind, useShowTrace, tc.wantKind, tc.wantShowTrace)
			}
		})
	}
}

func TestEventShowExactApprovalsFromPolicyProducer(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COMPOUND_HOME", root)
	if err := home.Init(root, "test", cliTestNow); err != nil {
		t.Fatal(err)
	}
	cfg := policy.DefaultConfig()
	monitored := policy.MonitorToolAction(policy.ToolActionRequest{
		WorkID: "work_reader_fixture", Actor: "codex", Harness: "codex", Tool: "browser.submit",
		Arguments:          map[string]any{"target": "publish", "form": "prepared-fixture"},
		ToolSchemaRevision: "builtin:browser.submit",
	}, cfg, cliTestNow)
	events, err := eventstore.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.PutJSON(monitored.Intent.ID, monitored.Intent, "  "); err != nil {
		t.Fatal(err)
	}
	approvals := policy.NewExactActionApprovalStore(root)
	plan, _, err := approvals.Plan(monitored.Intent.ID, cfg, 5*time.Minute, cliTestNow)
	if err != nil {
		t.Fatal(err)
	}
	grant, _, err := approvals.Approve(plan.ID, "operator:fixture", cfg, cliTestNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{plan.ID, grant.ID} {
		t.Run(id, func(t *testing.T) {
			stored, err := events.Read(id)
			if err != nil {
				t.Fatal(err)
			}
			out, stderr, code := runEventShowCLI(t, id, "--json")
			if code != 0 {
				t.Fatalf("read native approval exit=%d stderr=%s", code, stderr)
			}
			if out != string(ensureTrailingNewline(stored.Bytes)) {
				t.Fatalf("approval bytes changed: got=%s want=%s", out, stored.Bytes)
			}
			human, stderr, code := runEventShowCLI(t, id)
			if code != 0 || !strings.Contains(human, id) {
				t.Fatalf("human approval read exit=%d stdout=%s stderr=%s", code, human, stderr)
			}
		})
	}
	status, err := approvals.Check(grant.ID, cliTestNow.Add(2*time.Second))
	if err != nil || status.Status != policy.ExactApprovalStatusReady {
		t.Fatalf("read changed approval availability: status=%+v err=%v", status, err)
	}
	out, stderr, code := runEventShowCLI(t, "approval_grant_missing", "--json")
	if code != 1 || out != "" || !strings.Contains(stderr, "not found in event store") {
		t.Fatalf("missing approval must use store absence: exit=%d out=%s stderr=%s", code, out, stderr)
	}
}
