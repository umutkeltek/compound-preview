package browser

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

func testTime() time.Time { return time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC) }

func taskPolicy() policy.Config {
	cfg := policy.DefaultConfig()
	maxUSD := 0.10
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID: "synthetic-browser-budget", Capability: "browser.goal.jev",
		MutationClass: "financial", Reversibility: "none", BlastRadius: "account", MaxUSD: &maxUSD,
	})
	return cfg
}

func testTask(home string) TaskRequest {
	grantsPath := policy.StandingGrantsPath(home)
	if err := os.MkdirAll(filepath.Dir(grantsPath), 0o755); err != nil {
		panic(err)
	}
	grants, err := json.Marshal(policy.StandingGrantRegistry{SchemaVersion: policy.StandingGrantsRegistrySchema, Grants: taskPolicy().StandingGrants})
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(grantsPath, grants, 0o600); err != nil {
		panic(err)
	}
	workID := "work_browser_test"
	if mode := os.Getenv("COMPOUND_TEST_JEV_MODE"); mode != "" {
		workID += "_" + mode
	}
	store, err := work.Open(home)
	if err != nil {
		panic(err)
	}
	if _, err := store.Get(workID); err != nil {
		_, err = store.Create(work.CreateInput{WorkID: workID, Owner: "browser-test", Actor: "browser-test", CanonicalSurface: "https://example.com/", DesiredOutcome: "Read the heading", SuccessPredicate: "Example Domain", NonGoals: []string{"Do not submit forms"}}, testTime())
		if err != nil {
			panic(err)
		}
		if _, err := store.Claim(workID, "browser-test", testTime().Add(time.Second)); err != nil {
			panic(err)
		}
	}
	return TaskRequest{Home: home, Actor: "browser-test", WorkID: workID, URL: "https://example.com/", Goal: "Read the heading", ExpectText: "Example Domain", MaxSteps: 2, MaxProviderUSD: 0.10}
}

func testBridge(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJevBridgeHelper$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_JEV_HELPER=1")
	return cmd
}

func TestJevMissingWorkCannotLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	if _, err := SetJevEnabled(home, "work_missing_browser", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	called := 0
	req := TaskRequest{Home: home, Actor: "browser-test", WorkID: "work_missing_browser", URL: "https://example.com/", Goal: "Read heading", ExpectText: "Heading", MaxSteps: 1, MaxProviderUSD: 0.10}
	_, err := runTask(context.Background(), req, taskPolicy(), func(ctx context.Context) *exec.Cmd {
		called++
		return testBridge(ctx)
	})
	if err == nil || called != 0 {
		t.Fatalf("missing work launched bridge: err=%v calls=%d", err, called)
	}
}

func TestJevRequiresClaimedMatchingWorkBeforeProviderAccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *work.Store, *TaskRequest)
	}{
		{"foreign_actor", func(_ *testing.T, _ *work.Store, req *TaskRequest) { req.Actor = "other-actor" }},
		{"goal", func(_ *testing.T, _ *work.Store, req *TaskRequest) { req.Goal = "Different goal" }},
		{"predicate", func(_ *testing.T, _ *work.Store, req *TaskRequest) { req.ExpectText = "Different text" }},
		{"path", func(_ *testing.T, _ *work.Store, req *TaskRequest) { req.URL = "https://example.com/other" }},
		{"saved_selection", func(t *testing.T, store *work.Store, req *TaskRequest) {
			_, err := store.SaveExecutionSelection(req.WorkID, 0, envelope.ExecutionSelection{LaunchProfileID: "codex", Harness: "codex", Source: "explicit", Revision: 1, SelectedBy: req.Actor, SelectedAt: testTime().Format(time.RFC3339)}, req.Actor, testTime().Add(2*time.Second))
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"ready", func(t *testing.T, store *work.Store, req *TaskRequest) {
			if _, err := store.Advance(req.WorkID, work.AdvanceInput{To: "ready", Actor: req.Actor}, testTime().Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
		}},
		{"running", func(t *testing.T, store *work.Store, req *TaskRequest) {
			if _, err := store.Advance(req.WorkID, work.AdvanceInput{To: "running", Actor: req.Actor}, testTime().Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
		}},
		{"terminal", func(t *testing.T, store *work.Store, req *TaskRequest) {
			if _, err := store.Close(req.WorkID, work.CloseInput{Outcome: "cancelled", MoveType: "audit_only", Actor: req.Actor, Summary: "synthetic cancelled work"}, testTime().Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			req := testTask(home)
			if _, err := SetJevEnabled(home, req.WorkID, true, policy.DefaultConfig(), testTime()); err != nil {
				t.Fatal(err)
			}
			store, err := work.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, store, &req)
			before, err := store.Get(req.WorkID)
			if err != nil {
				t.Fatal(err)
			}
			called := 0
			_, err = runTask(context.Background(), req, taskPolicy(), func(ctx context.Context) *exec.Cmd { called++; return testBridge(ctx) })
			if err == nil || called != 0 {
				t.Fatalf("admitted conflicting work: err=%v calls=%d", err, called)
			}
			after, err := store.Get(req.WorkID)
			if err != nil {
				t.Fatal(err)
			}
			if after.State != before.State || after.PolicyDecisionID != before.PolicyDecisionID {
				t.Fatalf("refusal changed work generation: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestJevSuccessLeavesSameWorkVerifyingAndRejectsDuplicate(t *testing.T) {
	home := t.TempDir()
	req := testTask(home)
	if _, err := SetJevEnabled(home, req.WorkID, true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	result, err := runTask(context.Background(), req, taskPolicy(), testBridge)
	if err != nil || result.Status != "verified" || !result.Verified || result.WorkState != "verifying" || result.RunAttemptID == "" {
		t.Fatalf("run = %+v, %v", result, err)
	}
	store, err := work.OpenReadOnly(home)
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Get(req.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if w.State != "verifying" || !containsString(w.RunAttemptIDs, result.RunAttemptID) {
		t.Fatalf("work lacks exact attempt: %+v", w)
	}
	attempt, err := runattempt.Open(home).Get(result.RunAttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.WorkID != req.WorkID || attempt.Actor != req.Actor || attempt.ActionIntentID == "" || attempt.Origin != runattempt.OriginHarnessExecution || attempt.Status != runattempt.StatusSucceeded {
		t.Fatalf("attempt chain = %+v", attempt)
	}
	for _, receiptID := range result.ReceiptIDs {
		if !containsString(attempt.ProofRefs, receiptID) {
			t.Fatalf("attempt lacks browser receipt %s: %+v", receiptID, attempt.ProofRefs)
		}
	}
	called := 0
	_, err = runTask(context.Background(), req, taskPolicy(), func(ctx context.Context) *exec.Cmd { called++; return testBridge(ctx) })
	if err == nil || called != 0 {
		t.Fatalf("duplicate launch: err=%v calls=%d", err, called)
	}
}

func TestJevRevokedAfterStepCannotAuthorizeNextPrediction(t *testing.T) {
	home := t.TempDir()
	req := testTask(home)
	if _, err := SetJevEnabled(home, req.WorkID, true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	t.Setenv("COMPOUND_TEST_JEV_MODE", "revoke_after_first")
	marker := filepath.Join(t.TempDir(), "step")
	t.Setenv("COMPOUND_TEST_JEV_STEP_MARKER", marker)
	done := make(chan struct {
		result TaskResult
		err    error
	}, 1)
	go func() {
		result, err := runTask(context.Background(), req, taskPolicy(), testBridge)
		done <- struct {
			result TaskResult
			err    error
		}{result, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first browser step did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	store, err := work.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(req.WorkID, work.AdvanceInput{To: "running", Actor: req.Actor}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker+".release", []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err == nil || got.result.Verified {
			t.Fatalf("revoked task certified: %+v, %v", got.result, got.err)
		}
		if got.result.Steps != 1 {
			t.Fatalf("partial step lost: %+v", got.result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("revoked task did not terminate")
	}
	if _, err := os.Stat(marker + ".second-provider"); !os.IsNotExist(err) {
		t.Fatalf("second prediction was authorized: %v", err)
	}
}

func TestJevCredentialRotationAfterStepCannotAuthorizeOldAssignment(t *testing.T) {
	home := t.TempDir()
	req := testTask(home)
	if _, err := SetJevEnabled(home, req.WorkID, true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("synthetic-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.AddConnectionSurfaceGoverned(home, capgraph.ConnectionSurface{
		NodeID: jevCredentialSurface, CredentialHandle: jevCredentialHandle(),
	}, testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "shadow-key")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	t.Setenv("COMPOUND_TEST_JEV_MODE", "revoke_after_first")
	marker := filepath.Join(t.TempDir(), "step")
	t.Setenv("COMPOUND_TEST_JEV_STEP_MARKER", marker)
	type outcome struct {
		result TaskResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runTask(context.Background(), req, taskPolicy(), testBridge)
		done <- outcome{result, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first browser step did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(secretPath, []byte("synthetic-after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker+".release", []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err == nil || !strings.Contains(got.err.Error(), "credential assignment changed") || got.result.Verified || got.result.Steps != 1 {
			t.Fatalf("rotated assignment authorized or partial result lost: %+v, %v", got.result, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rotated task did not stop")
	}
	if _, err := os.Stat(marker + ".second-provider"); !os.IsNotExist(err) {
		t.Fatalf("second prediction reached provider: %v", err)
	}
}

func TestJevDisabledBeforeFirstPredictionRecordsNoProviderAttempt(t *testing.T) {
	home := t.TempDir()
	req := testTask(home)
	if _, err := SetJevEnabled(home, req.WorkID, true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	t.Setenv("COMPOUND_TEST_JEV_MODE", "deny_first_predict")
	marker := filepath.Join(t.TempDir(), "request")
	t.Setenv("COMPOUND_TEST_JEV_STEP_MARKER", marker)
	done := make(chan struct {
		result TaskResult
		err    error
	}, 1)
	go func() {
		result, err := runTask(context.Background(), req, taskPolicy(), testBridge)
		done <- struct {
			result TaskResult
			err    error
		}{result, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge did not receive request")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := SetJevEnabled(home, req.WorkID, false, policy.DefaultConfig(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker+".release", []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err == nil || got.result.Verified || got.result.Steps != 0 {
			t.Fatalf("pre-predict denial = %+v, %v", got.result, got.err)
		}
		store, err := eventstore.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range got.result.ReceiptIDs {
			body, err := store.Read(id)
			if err != nil {
				t.Fatal(err)
			}
			var receipt envelope.Receipt
			if err := json.Unmarshal(body.Bytes, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Outcome == "completed" {
				t.Fatalf("pre-provider denial recorded completed receipt: %+v", receipt)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pre-predict denial did not finish")
	}
}

func TestJevProviderSettingsDriftBlocksPrediction(t *testing.T) {
	for _, tc := range []struct{ name, settings string }{
		{"model", `{"schema_version":"compound.browser_settings.v0.2","jev_enabled":true,"provider_model":"other-model","provider_max_usd_per_request":0.01}`},
		{"cost", `{"schema_version":"compound.browser_settings.v0.2","jev_enabled":true,"provider_model":"jev-1.13.0","provider_max_usd_per_request":0.02}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			req := testTask(home)
			if _, err := SetJevEnabled(home, req.WorkID, true, policy.DefaultConfig(), testTime()); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TYPESAFE_API_KEY", "synthetic")
			t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
			t.Setenv("COMPOUND_TEST_JEV_MODE", "deny_first_predict")
			marker := filepath.Join(t.TempDir(), "request")
			t.Setenv("COMPOUND_TEST_JEV_STEP_MARKER", marker)
			done := make(chan error, 1)
			go func() { _, err := runTask(context.Background(), req, taskPolicy(), testBridge); done <- err }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("bridge did not receive request")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := os.WriteFile(SettingsPath(home), []byte(tc.settings), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker+".release", []byte("go"), 0o600); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("provider settings drift authorized prediction")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("provider drift did not finish")
			}
		})
	}
}

func TestJevCleanupFailureCannotCertifyReadback(t *testing.T) {
	home := t.TempDir()
	req := testTask(home)
	if _, err := SetJevEnabled(home, req.WorkID, true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	t.Setenv("COMPOUND_TEST_JEV_MODE", "cleanup_bad")
	var launched *exec.Cmd
	result, err := runTask(context.Background(), req, taskPolicy(), func(ctx context.Context) *exec.Cmd { launched = testBridge(ctx); return launched })
	if launched != nil {
		for _, entry := range launched.Env {
			if strings.HasPrefix(entry, "COMPOUND_JEV_SESSION_DIR=") {
				sessionDir := strings.TrimPrefix(entry, "COMPOUND_JEV_SESSION_DIR=")
				t.Cleanup(func() { _ = os.Remove(filepath.Join(sessionDir, "chrome.pid")); _ = cleanupJevSession(sessionDir) })
				break
			}
		}
	}
	if err == nil || !strings.Contains(err.Error(), "clean Jev session") || result.Verified || result.Status != "failed" || result.WorkState != "verifying" {
		t.Fatalf("cleanup failure was certified: %+v, %v", result, err)
	}
	attempt, err := runattempt.Open(home).Get(result.RunAttemptID)
	if err != nil || attempt.Status != runattempt.StatusFailed {
		t.Fatalf("cleanup attempt = %+v, %v", attempt, err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestJevUsesCompoundCredentialAndPricingWithoutCallerEnvironment(t *testing.T) {
	home := t.TempDir()
	settings := []byte(`{"schema_version":"compound.browser_settings.v0.2","jev_enabled":true,"provider_model":"jev-1.13.0","provider_max_usd_per_request":0.003}`)
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SettingsPath(home), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("synthetic-typesafe-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := capgraph.AddConnectionSurfaceGoverned(home, capgraph.ConnectionSurface{
		NodeID: "tool:browser-jev-typesafe",
		CredentialHandle: capgraph.CredentialHandle{
			Ref: "home-secret://typesafe/jev-api-key", Custodian: "compound-home",
			EnvName: "TYPESAFE_API_KEY",
		},
	}, testTime())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "shadow-key")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.00001")
	t.Setenv("TYPESAFE_MODEL", "other-model")
	req := testTask(home)
	req.MaxProviderUSD = 0.02
	var launched *exec.Cmd
	result, err := runTask(context.Background(), req, taskPolicy(), func(ctx context.Context) *exec.Cmd {
		launched = testBridge(ctx)
		return launched
	})
	if err != nil || result.Status != "verified" || launched == nil {
		t.Fatalf("registered Jev task: result=%+v err=%v", result, err)
	}
	joined := strings.Join(launched.Env, "\n")
	if strings.Contains(joined, "TYPESAFE_API_KEY=") || !strings.Contains(joined, "TYPESAFE_MODEL=jev-1.13.0") {
		t.Fatal("secret reached setup process or pinned provider model missing")
	}
	if strings.Contains(joined, "shadow-key") || strings.Contains(joined, "other-model") {
		t.Fatal("caller provider override leaked into Jev child")
	}
	if os.Getenv("TYPESAFE_API_KEY") != "shadow-key" {
		t.Fatal("credential leaked into caller environment")
	}
}

func TestJevBridgeHelper(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_JEV_HELPER") != "1" {
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "ready"})
	decoder := json.NewDecoder(os.Stdin)
	var req map[string]any
	if err := decoder.Decode(&req); err != nil {
		os.Exit(2)
	}
	if req["typesafe_api_key"] == "" {
		os.Exit(2)
	}
	contract, err := base64.StdEncoding.DecodeString(req["contract_json"].(string))
	if err != nil {
		os.Exit(2)
	}
	digest := sha256.Sum256(contract)
	if hex.EncodeToString(digest[:]) != req["contract_digest"] {
		os.Exit(2)
	}
	var saved map[string]any
	if json.Unmarshal(contract, &saved) != nil || saved["Goal"] != req["goal"] || saved["URL"] != req["url"] || !reflect.DeepEqual(saved["NonGoals"], req["non_goals"]) {
		os.Exit(2)
	}
	if os.Getenv("COMPOUND_TEST_JEV_MODE") == "deny_first_predict" {
		marker := os.Getenv("COMPOUND_TEST_JEV_STEP_MARKER")
		_ = os.WriteFile(marker, []byte("request-received"), 0o600)
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(marker + ".release"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "predict_request", "step": 1})
	var predictionGrant map[string]bool
	if err := decoder.Decode(&predictionGrant); err != nil {
		os.Exit(2)
	}
	if !predictionGrant["allow"] {
		if marker := os.Getenv("COMPOUND_TEST_JEV_STEP_MARKER"); marker != "" {
			_ = os.WriteFile(marker+".denied", []byte("denied"), 0o600)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "result", "status": "held", "steps": 0})
		os.Exit(0)
	}
	mode := os.Getenv("COMPOUND_TEST_JEV_MODE")
	if mode == "error" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "error", "code": "predict_ValueError"})
		os.Exit(1)
	}
	proposal := map[string]any{"event": "proposal", "step": 1, "kind": "done", "role": "terminal", "url": "https://example.com/"}
	if mode == "held" {
		proposal["kind"], proposal["role"] = "fill", "textbox"
	}
	if mode == "link" {
		proposal["kind"], proposal["role"], proposal["target_url"] = "click", "link", "https://example.com/other"
	}
	if mode == "offorigin" {
		proposal["url"] = "https://other.example/"
	}
	_ = json.NewEncoder(os.Stdout).Encode(proposal)
	var reply map[string]bool
	if err := decoder.Decode(&reply); err != nil {
		os.Exit(2)
	}
	if mode == "held" || mode == "offorigin" || mode == "link" {
		if reply["allow"] {
			os.Exit(3)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "result", "status": "held", "steps": 0})
		os.Exit(0)
	}
	if !reply["allow"] {
		os.Exit(3)
	}
	actedStatus := "done"
	if mode == "badstatus" {
		actedStatus = "stale"
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "acted", "step": 1, "status": actedStatus, "url": "https://example.com/"})
	if mode == "revoke_after_first" {
		marker := os.Getenv("COMPOUND_TEST_JEV_STEP_MARKER")
		_ = os.WriteFile(marker, []byte("first-step-complete"), 0o600)
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(marker + ".release"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "predict_request", "step": 2})
		var grant map[string]bool
		if decoder.Decode(&grant) == nil && grant["allow"] {
			_ = os.WriteFile(marker+".second-provider", []byte("called"), 0o600)
		}
		os.Exit(0)
	}
	if mode == "hang" {
		time.Sleep(30 * time.Second)
		os.Exit(2)
	}
	status, verified := "verified", true
	if mode == "unverified" {
		status, verified = "unverified", false
	}
	if mode == "cleanup_bad" {
		_ = os.WriteFile(filepath.Join(os.Getenv("COMPOUND_JEV_SESSION_DIR"), "chrome.pid"), []byte("invalid-pid"), 0o600)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "result", "status": status, "verified": verified, "steps": 1, "url": "https://example.com/"})
	os.Exit(0)
}

func TestJevDisabledAndProfileNeverLaunch(t *testing.T) {
	home := t.TempDir()
	called := 0
	launch := func(ctx context.Context) *exec.Cmd { called++; return testBridge(ctx) }
	result, err := runTask(context.Background(), testTask(home), taskPolicy(), launch)
	if !errors.Is(err, ErrRoutedToPlaywright) || result.Backend != "playwright" || result.Status != "routed" || called != 0 {
		t.Fatalf("disabled route: %+v err=%v launches=%d", result, err, called)
	}
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	req := testTask(home)
	req.ProfileID = "profile_auth"
	result, err = runTask(context.Background(), req, taskPolicy(), launch)
	if !errors.Is(err, ErrRoutedToPlaywright) || result.Backend != "playwright" || result.Status != "routed" || called != 0 {
		t.Fatalf("profile route: %+v err=%v launches=%d", result, err, called)
	}
}

func TestJevSyntheticVerifiedAndHeld(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	for _, tc := range []struct {
		mode, status string
		receipts     int
	}{{"", "verified", 3}, {"unverified", "unverified", 3}, {"held", "held", 2}, {"offorigin", "held", 2}, {"link", "held", 2}} {
		t.Run(fmt.Sprintf("mode_%s", tc.mode), func(t *testing.T) {
			t.Setenv("COMPOUND_TEST_JEV_MODE", tc.mode)
			result, err := runTask(context.Background(), testTask(home), taskPolicy(), testBridge)
			if err != nil || result.Status != tc.status || result.Verified != (tc.status == "verified") || len(result.ReceiptIDs) != tc.receipts || len(result.TransitionIDs) != tc.receipts {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			store, err := eventstore.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range result.ReceiptIDs {
				if _, err := store.Read(id); err != nil {
					t.Fatalf("missing receipt %s: %v", id, err)
				}
			}
			providerBytes, _ := store.Read(result.ReceiptIDs[len(result.ReceiptIDs)-1])
			var providerReceipt envelope.Receipt
			if err := json.Unmarshal(providerBytes.Bytes, &providerReceipt); err != nil || providerReceipt.Cost.USDEstimated == nil || math.Abs(*providerReceipt.Cost.USDEstimated-0.06) > 1e-9 || providerReceipt.Cost.USDActual != nil {
				t.Fatalf("provider cost receipt = %+v, %v", providerReceipt.Cost, err)
			}
			if tc.status == "verified" {
				first, _ := store.Read(result.ReceiptIDs[0])
				var provisional envelope.Receipt
				if err := json.Unmarshal(first.Bytes, &provisional); err != nil || provisional.Outcome != "no_op" {
					t.Fatalf("DONE must be provisional: %+v %v", provisional, err)
				}
			}
			for _, id := range result.TransitionIDs {
				if _, err := store.Read(id); err != nil {
					t.Fatalf("missing transition %s: %v", id, err)
				}
			}
		})
	}
}

func TestJevRejectsInvalidScopeBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	called := 0
	launch := func(ctx context.Context) *exec.Cmd { called++; return testBridge(ctx) }
	for _, raw := range []string{"http://example.com/", "https://localhost/", "https://example.com/?token=x", "https://127.0.0.1/"} {
		req := testTask(home)
		req.URL = raw
		if _, err := runTask(context.Background(), req, taskPolicy(), launch); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if called != 0 {
		t.Fatalf("unsafe requests launched %d times", called)
	}
}

func TestJevMissingCredentialNeverLaunches(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "")
	called := 0
	launch := func(ctx context.Context) *exec.Cmd { called++; return testBridge(ctx) }
	if _, err := runTask(context.Background(), testTask(home), taskPolicy(), launch); err == nil || called != 0 {
		t.Fatalf("missing key should stop before launch: err=%v launches=%d", err, called)
	}
}

func TestJevCorruptRegistryNeverFallsBackToShellKey(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "registries"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "registries", "capability-connection-surfaces.jsonl"), []byte("{torn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic-shell-key")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.003")
	called := 0
	_, err := runTask(context.Background(), testTask(home), taskPolicy(), func(ctx context.Context) *exec.Cmd {
		called++
		return testBridge(ctx)
	})
	if err == nil || called != 0 {
		t.Fatalf("corrupt registry used shell fallback: err=%v launches=%d", err, called)
	}
}

func TestJevProviderCostPreflightStopsBeforeLaunch(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	called := 0
	launch := func(ctx context.Context) *exec.Cmd { called++; return testBridge(ctx) }
	for _, tc := range []struct {
		price, model string
		budget       float64
	}{
		{"", "", 0.1},
		{"0.000001", "", 0.1},
		{"0.05", "", 0.1},
		{"0.01", "other-model", 0.1},
		{"0.01", "", 0},
	} {
		t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", tc.price)
		t.Setenv("TYPESAFE_MODEL", tc.model)
		req := testTask(home)
		req.MaxProviderUSD = tc.budget
		if _, err := runTask(context.Background(), req, taskPolicy(), launch); err == nil {
			t.Fatalf("accepted provider cost contract %+v", tc)
		}
	}
	if called != 0 {
		t.Fatalf("provider preflight launched bridge %d times", called)
	}
}

func TestJevProviderSpendNeedsPolicyGrant(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	called := 0
	launch := func(ctx context.Context) *exec.Cmd { called++; return testBridge(ctx) }
	result, err := runTask(context.Background(), testTask(home), policy.DefaultConfig(), launch)
	if err == nil || result.Status != "held" || called != 0 || len(result.ReceiptIDs) != 1 {
		t.Fatalf("provider spend escaped policy: result=%+v err=%v launches=%d", result, err, called)
	}
}

func TestJevPolicyHoldStopsBeforeBrowserLaunch(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	cfg := policy.DefaultConfig()
	cfg.ClassOutcome["financial"] = policy.ClassRule{Outcome: "hold"}
	called := 0
	launch := func(ctx context.Context) *exec.Cmd { called++; return testBridge(ctx) }
	if _, err := runTask(context.Background(), testTask(home), cfg, launch); err == nil || called != 0 {
		t.Fatalf("policy hold should stop before launch: err=%v launches=%d", err, called)
	}
}

func TestJevBridgeErrorHasFailedReceipt(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	t.Setenv("COMPOUND_TEST_JEV_MODE", "error")
	result, err := runTask(context.Background(), testTask(home), taskPolicy(), testBridge)
	if err == nil || result.Status != "failed" || result.Verified || len(result.ReceiptIDs) != 1 || len(result.TransitionIDs) != 1 {
		t.Fatalf("failed run = %+v, %v", result, err)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(result.ReceiptIDs[0]); err != nil {
		t.Fatal(err)
	}
}

func TestJevUnexpectedActStatusFails(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	t.Setenv("COMPOUND_TEST_JEV_MODE", "badstatus")
	result, err := runTask(context.Background(), testTask(home), taskPolicy(), testBridge)
	if err == nil || result.Status != "failed" || result.Verified || len(result.ReceiptIDs) != 1 {
		t.Fatalf("unexpected act status = %+v, %v", result, err)
	}
	store, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Read(result.ReceiptIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt envelope.Receipt
	if err := json.Unmarshal(record.Bytes, &receipt); err != nil || receipt.Outcome != "failed" {
		t.Fatalf("receipt = %+v, %v", receipt, err)
	}
}

func TestJevCancellationPreservesPartialProgress(t *testing.T) {
	home := t.TempDir()
	if _, err := SetJevEnabled(home, "work_browser_test", true, policy.DefaultConfig(), testTime()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	t.Setenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST", "0.01")
	t.Setenv("COMPOUND_TEST_JEV_MODE", "hang")
	ctx, cancel := context.WithCancel(context.Background())
	req := testTask(home)
	type outcome struct {
		result TaskResult
		err    error
	}
	done := make(chan outcome, 1)
	joined := false
	defer func() {
		cancel()
		if joined {
			return
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("browser task did not stop within cancellation bound")
			<-done // fixture cleanup must not race an unfinished worker
		}
	}()
	go func() {
		result, err := runTask(ctx, req, taskPolicy(), func(ctx context.Context) *exec.Cmd {
			time.Sleep(1200 * time.Millisecond) // controlled slow admission-to-launch boundary
			return testBridge(ctx)
		})
		done <- outcome{result, err}
	}()
	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	firstActionRecorded := false
	for !firstActionRecorded {
		select {
		case got := <-done:
			joined = true
			t.Fatalf("browser task ended before first action receipt: %+v, %v", got.result, got.err)
		default:
		}
		entries, err := events.Scan(eventstore.ScanOptions{Prefixes: []string{"rec_"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			record, err := entry.Read()
			if err != nil {
				t.Fatal(err)
			}
			var receipt envelope.Receipt
			if err := json.Unmarshal(record.Bytes, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.IDs.WorkID == req.WorkID && receipt.StateDelta.Summary == "Jev browser action observed" {
				firstActionRecorded = true
				break
			}
		}
		if !firstActionRecorded {
			if time.Now().After(deadline) {
				t.Fatal("first browser action was not durably recorded before cancellation")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case got := <-done:
		joined = true
		if got.err == nil || got.result.Status != "failed" || got.result.Steps != 1 || len(got.result.ReceiptIDs) != 2 || len(got.result.TransitionIDs) != 2 {
			t.Fatalf("cancelled result = %+v, %v", got.result, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("browser task did not stop after cancellation")
	}
}

func TestJevProxyRejectsForeignAndPrivateTargets(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	script := `import runpy, socket, threading
ns = runpy.run_path("jev_bridge.py")
proxy = ns["ScopedProxy"]("example.com")
thread = threading.Thread(target=proxy.serve_forever, daemon=True)
thread.start()
def request(authority):
    client = socket.socket()
    client.settimeout(3)
    client.connect(proxy.server_address)
    client.sendall(("CONNECT " + authority + " HTTP/1.1\r\n\r\n").encode())
    answer = client.recv(128)
    client.close()
    return answer
assert b"403" in request("other.example:443")
assert b"403" in request("127.0.0.1:443")
original = socket.getaddrinfo
def private_dns(host, port, **kwargs):
    return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("127.0.0.1", 443))]
socket.getaddrinfo = private_dns
try:
    assert b"403" in request("example.com:443")
finally:
    socket.getaddrinfo = original
    proxy.shutdown()
    proxy.server_close()
thread.join(timeout=2)
print("proxy scope held")`
	cmd := exec.Command(python, "-c", script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("proxy scope: %v: %s", err, output)
	}
}
