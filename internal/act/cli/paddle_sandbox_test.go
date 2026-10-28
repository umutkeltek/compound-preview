package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/act"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

const paddleCLITestKey = "synthetic-paddle-sandbox-key"
const paddleCLISettingID = "ntfset_01234567890123456789012345"

var paddleCLINow = time.Now().UTC().Truncate(time.Second)

type paddleCLIRoundTrip func(*http.Request) (*http.Response, error)

func (f paddleCLIRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type paddleCLIProvider struct {
	settingID string
	state     map[string]any
	gets      int
	patches   int
	errorBody string
	beforeGET func(int)
}

func newPaddleCLIProvider(t *testing.T) *paddleCLIProvider {
	t.Helper()
	p := &paddleCLIProvider{
		settingID: paddleCLISettingID,
		state: map[string]any{
			"id":                       paddleCLISettingID,
			"destination":              "https://old.shop.invalid/webhooks/paddle",
			"active":                   true,
			"api_version":              1,
			"description":              "synthetic fixture",
			"include_sensitive_fields": false,
			"endpoint_secret_key":      "synthetic-secret-never-rendered",
			"traffic_source":           "all",
			"type":                     "url",
			"subscribed_events": []any{
				map[string]any{"name": "subscription.created"},
				map[string]any{"name": "transaction.completed"},
			},
		},
	}
	previous := http.DefaultTransport
	http.DefaultTransport = paddleCLIRoundTrip(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+paddleCLITestKey {
			return nil, fmt.Errorf("missing sandbox authorization")
		}
		wantPath := "/notification-settings/" + p.settingID
		if r.URL.Scheme != "https" || r.URL.Host != "sandbox-api.paddle.com" || r.URL.Path != wantPath {
			return nil, fmt.Errorf("unexpected sandbox request target")
		}
		if p.errorBody != "" {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Status:     "500 Internal Server Error",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(p.errorBody)),
				Request:    r,
			}, nil
		}
		switch r.Method {
		case http.MethodGet:
			p.gets++
			if p.beforeGET != nil {
				p.beforeGET(p.gets)
			}
		case http.MethodPatch:
			p.patches++
			var patch map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				return nil, err
			}
			if len(patch) != 2 || patch["destination"] == nil || patch["subscribed_events"] == nil {
				return nil, fmt.Errorf("overbroad patch")
			}
			for key, raw := range patch {
				var value any
				if err := json.Unmarshal(raw, &value); err != nil {
					return nil, err
				}
				p.state[key] = value
			}
		default:
			return nil, fmt.Errorf("unexpected sandbox method")
		}
		body, _ := json.Marshal(map[string]any{"data": p.state})
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	return p
}

type paddleCLIOutput struct {
	Mode             string   `json:"mode"`
	NoOp             bool     `json:"no_op"`
	ActionIntentID   string   `json:"action_intent_id"`
	ApprovalPlanID   string   `json:"approval_plan_id"`
	CurrentSHA256    string   `json:"current_sha256"`
	RerunCommand     string   `json:"rerun_command"`
	ReceiptID        string   `json:"receipt_id"`
	TransitionID     string   `json:"transition_id"`
	PolicyOutcome    string   `json:"policy_outcome"`
	ApprovalRequired bool     `json:"approval_required"`
	ProposedEvents   []string `json:"proposed_events"`
	approvalID       string
}

func newPaddleCLIHomeAndWork(t *testing.T, actor string) (string, string) {
	t.Helper()
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", paddleCLINow); err != nil {
		t.Fatal(err)
	}
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Create(work.CreateInput{
		Owner:            "sandbox-adapter-test",
		SuccessPredicate: "sandbox notification setting changes through governed act",
	}, paddleCLINow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(w.ID, actor, paddleCLINow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return homePath, w.ID
}

func runPaddleCLI(t *testing.T, args ...string) (int, paddleCLIOutput, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunPaddleSandboxNotification(args, &stdout, &stderr)
	var output paddleCLIOutput
	if stdout.Len() > 0 && json.Unmarshal(stdout.Bytes(), &output) != nil {
		// Text output is allowed outside --json; callers still receive it verbatim.
	}
	return code, output, stdout.String(), stderr.String()
}

func paddleCLIArgs(homePath, workID, actor, toURL string, extra ...string) []string {
	args := []string{
		"--setting", paddleCLISettingID,
		"--to-url", toURL,
		"--work", workID,
		"--actor", actor,
		"--home", homePath,
		"--json",
	}
	return append(args, extra...)
}

func planAndApprovePaddleCLI(t *testing.T, homePath, workID, actor, toURL string, now time.Time) paddleCLIOutput {
	t.Helper()
	setPaddleCLINow(t, now)
	code, planned, stdout, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, actor, toURL, "--plan")...)
	if code != 0 {
		t.Fatalf("plan exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if planned.ActionIntentID == "" || planned.ApprovalPlanID == "" || planned.CurrentSHA256 == "" {
		t.Fatalf("incomplete plan output: %+v", planned)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Read(planned.ActionIntentID); err != nil {
		t.Fatalf("planned ActionIntent is not persisted: %v", err)
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		t.Fatal(err)
	}
	approval, _, err := policy.NewExactActionApprovalStore(homePath).Approve(
		planned.ApprovalPlanID,
		"operator:fixture",
		cfg,
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("approve exact plan: %v", err)
	}
	planned.approvalID = approval.ID
	return planned
}

func setPaddleCLINow(t *testing.T, now time.Time) {
	t.Helper()
	previous := paddleSandboxNow
	paddleSandboxNow = func() time.Time { return now }
	t.Cleanup(func() { paddleSandboxNow = previous })
}

func receiptCount(t *testing.T, homePath string) int {
	t.Helper()
	store, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.Scan(eventstore.ScanOptions{Prefixes: []string{"rec_"}, RegularOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func importPaddleCLIGrant(t *testing.T, homePath, capability, targetPath string) {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "standing-grants.yaml")
	body := "generated_at: \"2026-09-12T18:00:00Z\"\n" +
		"authority:\n" +
		"  decision_ref: \"grant:paddle-cli-test\"\n" +
		"  reviewed_by: \"operator:fixture\"\n" +
		"grants:\n" +
		"  - grant_id: grant_paddle_cli_test\n" +
		"    capabilities: [\"" + capability + "\"]\n" +
		"    target_scope: \"" + targetPath + "\"\n" +
		"    mutation_class: external_side_effect\n" +
		"    reversibility_ceiling: irreversible\n" +
		"    blast_radius: one_sandbox_notification_setting\n" +
		"    max_usd: 1\n" +
		"    max_tokens: 1000\n"
	if err := os.WriteFile(sourcePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, sourcePath, paddleCLINow); err != nil {
		t.Fatalf("import standing grant: %v", err)
	}
}

func TestPaddleSandboxPreviewAndPlanAreReadOnlyAtProvider(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	provider := newPaddleCLIProvider(t)
	homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
	toURL := "https://new.shop.invalid/webhooks/paddle"

	code, preview, stdout, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL)...)
	if code != 0 || preview.Mode != "preview" || len(preview.ProposedEvents) != 6 || provider.gets != 1 || provider.patches != 0 {
		t.Fatalf("preview exit=%d output=%+v gets=%d patches=%d stdout=%q stderr=%q", code, preview, provider.gets, provider.patches, stdout, stderr)
	}
	if strings.Contains(stdout, "synthetic-secret-never-rendered") || strings.Contains(stdout, "endpoint_secret_key") {
		t.Fatalf("preview exposed a provider secret field: %q", stdout)
	}

	code, planned, stdout, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL, "--plan")...)
	if code != 0 || planned.Mode != "plan" || planned.ApprovalPlanID == "" || provider.patches != 0 {
		t.Fatalf("plan exit=%d output=%+v patches=%d stdout=%q stderr=%q", code, planned, provider.patches, stdout, stderr)
	}
	if !strings.Contains(planned.RerunCommand, planned.CurrentSHA256) || !strings.Contains(planned.RerunCommand, "--confirm") {
		t.Fatalf("plan must print exact fingerprint-bound rerun command: %+v", planned)
	}
}

func TestPaddleSandboxConfirmNeedsLiveExactAuthority(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	provider := newPaddleCLIProvider(t)
	homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
	toURL := "https://new.shop.invalid/webhooks/paddle"
	setPaddleCLINow(t, paddleCLINow)

	adapter := act.NewPaddleSandboxNotificationAdapter(paddleCLITestKey)
	plan, err := adapter.Plan(paddleCLISettingID, toURL)
	if err != nil {
		t.Fatal(err)
	}
	code, _, _, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", plan.BeforeDigest())...)
	if code == 0 || provider.patches != 0 || !strings.Contains(stderr, "approval") {
		t.Fatalf("held confirm exit=%d patches=%d stderr=%q", code, provider.patches, stderr)
	}

	planned := planAndApprovePaddleCLI(t, homePath, workID, "codex:fixture", toURL, paddleCLINow.Add(time.Minute))
	setPaddleCLINow(t, paddleCLINow.Add(12*time.Minute))
	code, _, _, stderr = runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", planned.CurrentSHA256)...)
	if code == 0 || provider.patches != 0 || !strings.Contains(stderr, "expired") {
		t.Fatalf("expired approval exit=%d patches=%d stderr=%q", code, provider.patches, stderr)
	}
}

func TestPaddleSandboxStaleOrChangedBindingDoesNotConsumeApproval(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	provider := newPaddleCLIProvider(t)
	homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
	toURL := "https://new.shop.invalid/webhooks/paddle"
	planned := planAndApprovePaddleCLI(t, homePath, workID, "codex:fixture", toURL, paddleCLINow)

	code, _, _, _ := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", "sha256:"+strings.Repeat("0", 64))...)
	if code == 0 || provider.patches != 0 {
		t.Fatalf("stale fingerprint exit=%d patches=%d", code, provider.patches)
	}

	code, _, _, _ = runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", "https://different.shop.invalid/webhooks/paddle",
		"--confirm", "--expected-current-sha256", planned.CurrentSHA256)...)
	if code == 0 || provider.patches != 0 {
		t.Fatalf("changed URL exit=%d patches=%d", code, provider.patches)
	}

	alternateID := "ntfset_" + strings.Repeat("a", 26)
	provider.settingID = alternateID
	provider.state["id"] = alternateID
	alternatePlan, err := act.NewPaddleSandboxNotificationAdapter(paddleCLITestKey).Plan(alternateID, toURL)
	if err != nil {
		t.Fatal(err)
	}
	alternateArgs := paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", alternatePlan.BeforeDigest())
	alternateArgs[1] = alternateID
	code, _, _, _ = runPaddleCLI(t, alternateArgs...)
	if code == 0 || provider.patches != 0 {
		t.Fatalf("changed setting exit=%d patches=%d", code, provider.patches)
	}
	provider.settingID = paddleCLISettingID
	provider.state["id"] = paddleCLISettingID

	// The stale/mismatched attempts did not consume the exact approval; the exact rerun succeeds.
	code, confirmed, stdout, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", planned.CurrentSHA256)...)
	if code != 0 || provider.patches != 1 || confirmed.ReceiptID == "" || confirmed.TransitionID == "" {
		t.Fatalf("exact confirm exit=%d output=%+v patches=%d stdout=%q stderr=%q", code, confirmed, provider.patches, stdout, stderr)
	}
	events, _ := eventstore.Open(homePath)
	if _, err := events.Read(confirmed.ReceiptID); err != nil {
		t.Fatalf("receipt not resolvable: %v", err)
	}
	if _, err := events.Read(confirmed.TransitionID); err != nil {
		t.Fatalf("transition not resolvable: %v", err)
	}
}

func TestPaddleSandboxConsumedApprovalReplayAndNoOpDoNotWrite(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	provider := newPaddleCLIProvider(t)
	homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
	toURL := "https://new.shop.invalid/webhooks/paddle"
	original, _ := json.Marshal(provider.state)
	planned := planAndApprovePaddleCLI(t, homePath, workID, "codex:fixture", toURL, paddleCLINow)

	code, _, _, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", planned.CurrentSHA256)...)
	if code != 0 || provider.patches != 1 {
		t.Fatalf("first confirm exit=%d patches=%d stderr=%q", code, provider.patches, stderr)
	}

	// Matching destination+fixed event set is a no-op before approval lookup or receipt creation.
	noOpPlan := planAndApprovePaddleCLI(t, homePath, workID, "codex:fixture", toURL, paddleCLINow.Add(time.Minute))
	beforeReceipts := receiptCount(t, homePath)
	code, output, stdout, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", noOpPlan.CurrentSHA256)...)
	if code != 0 || !output.NoOp || provider.patches != 1 || receiptCount(t, homePath) != beforeReceipts {
		t.Fatalf("no-op exit=%d output=%+v patches=%d stdout=%q stderr=%q", code, output, provider.patches, stdout, stderr)
	}
	noOpStatus, err := policy.NewExactActionApprovalStore(homePath).Check(noOpPlan.approvalID, paddleCLINow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if noOpStatus.Status != policy.ExactApprovalStatusReady {
		t.Fatalf("no-op consumed its approval: %+v", noOpStatus)
	}

	// Restore the old provider representation out of band to test one-use replay authority.
	if err := json.Unmarshal(original, &provider.state); err != nil {
		t.Fatal(err)
	}
	code, _, _, stderr = runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", planned.CurrentSHA256)...)
	if code == 0 || provider.patches != 1 || !strings.Contains(stderr, "consumed") {
		t.Fatalf("consumed replay exit=%d patches=%d stderr=%q", code, provider.patches, stderr)
	}
}

func TestPaddleSandboxScopedStandingGrantCanExecute(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	provider := newPaddleCLIProvider(t)
	homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
	toURL := "https://new.shop.invalid/webhooks/paddle"
	adapter := act.NewPaddleSandboxNotificationAdapter(paddleCLITestKey)
	plan, err := adapter.Plan(paddleCLISettingID, toURL)
	if err != nil {
		t.Fatal(err)
	}
	ai, err := act.BuildPaddleSandboxNotificationIntent(plan, workID, "codex:fixture", paddleCLINow)
	if err != nil {
		t.Fatal(err)
	}
	importPaddleCLIGrant(t, homePath, ai.Capability, ai.Target.Path)

	code, output, stdout, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", plan.BeforeDigest())...)
	if code != 0 || provider.patches != 1 || output.ReceiptID == "" {
		t.Fatalf("standing grant confirm exit=%d output=%+v patches=%d stdout=%q stderr=%q", code, output, provider.patches, stdout, stderr)
	}
}

func TestPaddleSandboxRechecksWorkAndHaltAfterProviderRead(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string, string)
	}{
		{
			name: "work leaves executable state",
			change: func(t *testing.T, homePath, workID string) {
				store, err := work.Open(homePath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Advance(workID, work.AdvanceInput{To: "held", Actor: "codex:fixture"}, paddleCLINow.Add(2*time.Minute)); err != nil {
					t.Fatalf("move work during provider read: %v", err)
				}
			},
		},
		{
			name: "autonomy halt engages",
			change: func(t *testing.T, homePath, _ string) {
				if err := policy.WriteAutonomyHalt(homePath, policy.AutonomyHalt{Global: true, Reason: "synthetic concurrent halt"}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newPaddleCLIProvider(t)
			homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
			toURL := "https://new.shop.invalid/webhooks/paddle"
			planned := planAndApprovePaddleCLI(t, homePath, workID, "codex:fixture", toURL, paddleCLINow)
			provider.beforeGET = func(read int) {
				if read == 2 {
					tc.change(t, homePath, workID)
				}
			}

			code, _, _, _ := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
				"--confirm", "--expected-current-sha256", planned.CurrentSHA256)...)
			if code == 0 || provider.patches != 0 {
				t.Fatalf("concurrent authority change exit=%d patches=%d", code, provider.patches)
			}
			status, err := policy.NewExactActionApprovalStore(homePath).Check(planned.approvalID, paddleCLINow.Add(3*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if status.Status != policy.ExactApprovalStatusReady {
				t.Fatalf("approval was consumed after concurrent authority change: %+v", status)
			}
		})
	}
}

func TestPaddleSandboxRechecksExactAuthorityAtFinalPrePatchRead(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string, string)
	}{
		{
			name: "work becomes held",
			change: func(t *testing.T, homePath, workID string) {
				store, err := work.Open(homePath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Advance(workID, work.AdvanceInput{To: "held", Actor: "codex:fixture"}, paddleCLINow.Add(2*time.Minute)); err != nil {
					t.Fatalf("hold work during final provider read: %v", err)
				}
			},
		},
		{
			name: "autonomy halt engages",
			change: func(t *testing.T, homePath, _ string) {
				if err := policy.WriteAutonomyHalt(homePath, policy.AutonomyHalt{Global: true, Reason: "synthetic final-read halt"}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "lease is stolen",
			change: func(t *testing.T, homePath, workID string) {
				store, err := work.Open(homePath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Advance(workID, work.AdvanceInput{
					To:          "running",
					Actor:       "codex:other",
					Steal:       true,
					StealReason: "synthetic final-read lease transfer",
				}, paddleCLINow.Add(2*time.Minute)); err != nil {
					t.Fatalf("steal lease during final provider read: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := newPaddleCLIProvider(t)
			homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
			toURL := "https://new.shop.invalid/webhooks/paddle"
			planned := planAndApprovePaddleCLI(t, homePath, workID, "codex:fixture", toURL, paddleCLINow)
			provider.beforeGET = func(read int) {
				if read == 4 {
					tc.change(t, homePath, workID)
				}
			}

			code, _, _, _ := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
				"--confirm", "--expected-current-sha256", planned.CurrentSHA256)...)
			if code == 0 || provider.patches != 0 {
				t.Fatalf("final-read authority change exit=%d patches=%d", code, provider.patches)
			}
		})
	}
}

func TestPaddleSandboxWorkAndCredentialGatesPrecedePatch(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	provider := newPaddleCLIProvider(t)
	homePath, workID := newPaddleCLIHomeAndWork(t, "codex:holder")
	toURL := "https://new.shop.invalid/webhooks/paddle"

	for _, tc := range []struct {
		name   string
		workID string
		actor  string
	}{
		{name: "unknown work", workID: "work_missing", actor: "codex:holder"},
		{name: "lease mismatch", workID: workID, actor: "codex:other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _, _ := runPaddleCLI(t, paddleCLIArgs(homePath, tc.workID, tc.actor, toURL,
				"--confirm", "--expected-current-sha256", "sha256:"+strings.Repeat("0", 64))...)
			if code == 0 || provider.patches != 0 {
				t.Fatalf("exit=%d patches=%d", code, provider.patches)
			}
		})
	}

	previousRequests := provider.gets + provider.patches
	t.Setenv("PADDLE_SANDBOX_API_KEY", "")
	code, _, _, _ := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:holder", toURL)...)
	if code == 0 || provider.gets+provider.patches != previousRequests {
		t.Fatalf("missing key exit=%d requests before=%d after=%d", code, previousRequests, provider.gets+provider.patches)
	}
}

func TestPaddleSandboxTerminalWorkAndAutonomyHaltPrecedePatch(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	provider := newPaddleCLIProvider(t)
	homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
	toURL := "https://new.shop.invalid/webhooks/paddle"
	adapter := act.NewPaddleSandboxNotificationAdapter(paddleCLITestKey)
	plan, err := adapter.Plan(paddleCLISettingID, toURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.WriteAutonomyHalt(homePath, policy.AutonomyHalt{Global: true, Reason: "synthetic incident"}); err != nil {
		t.Fatal(err)
	}
	code, _, _, _ := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", plan.BeforeDigest())...)
	if code == 0 || provider.patches != 0 {
		t.Fatalf("halted confirm exit=%d patches=%d", code, provider.patches)
	}

	// A terminal envelope is rejected even if a stale lease file remains.
	workPath := filepath.Join(homePath, "work", "work", workID+".json")
	body, err := os.ReadFile(workPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	raw["state"] = "completed"
	body, _ = json.MarshalIndent(raw, "", "  ")
	if err := os.WriteFile(workPath, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := policy.WriteAutonomyHalt(homePath, policy.AutonomyHalt{}); err != nil {
		t.Fatal(err)
	}
	code, _, _, _ = runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", toURL,
		"--confirm", "--expected-current-sha256", plan.BeforeDigest())...)
	if code == 0 || provider.patches != 0 {
		t.Fatalf("terminal confirm exit=%d patches=%d", code, provider.patches)
	}
}

func TestPaddleSandboxOutputNeverLeaksCredentialOrProviderBody(t *testing.T) {
	t.Setenv("PADDLE_SANDBOX_API_KEY", paddleCLITestKey)
	provider := newPaddleCLIProvider(t)
	provider.errorBody = "raw-provider-body-with-secret-123"
	homePath, workID := newPaddleCLIHomeAndWork(t, "codex:fixture")
	code, _, stdout, stderr := runPaddleCLI(t, paddleCLIArgs(homePath, workID, "codex:fixture", "https://new.shop.invalid/webhooks/paddle")...)
	if code == 0 {
		t.Fatalf("provider error unexpectedly succeeded")
	}
	combined := stdout + stderr
	for _, secret := range []string{paddleCLITestKey, provider.errorBody} {
		if strings.Contains(combined, secret) {
			t.Fatalf("output leaked secret %q: %q", secret, combined)
		}
	}
}

func TestPaddleSandboxRerunQuotesShellMetacharacters(t *testing.T) {
	opts := paddleSandboxOptions{
		setting:  paddleCLISettingID,
		toURL:    "https://new.shop.invalid/webhooks/paddle",
		workID:   "work_$HOME",
		actor:    "codex:fixture",
		homePath: "/tmp/operator's $HOME",
	}
	command := paddleSandboxRerun(opts, "sha256:"+strings.Repeat("a", 64))
	if strings.Contains(command, "\"$HOME\"") || !strings.Contains(command, `'/tmp/operator'"'"'s $HOME'`) {
		t.Fatalf("rerun command is not shell-safe: %s", command)
	}
}
