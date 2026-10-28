package act

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/policy"
)

const sandboxSettingID = "ntfset_01234567890123456789012345"
const sandboxTestKey = "synthetic-api-credential"
const sandboxTestSecret = "synthetic-webhook-credential"

type sandboxRoundTrip func(*http.Request) (*http.Response, error)

func (f sandboxRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type sandboxProviderFixture struct {
	state      map[string]any
	patches    []map[string]json.RawMessage
	reads      int
	onPatch    func(http.ResponseWriter)
	beforeRead func(int)
}

func newSandboxProviderFixture(t *testing.T) (*sandboxProviderFixture, *PaddleSandboxNotificationAdapter) {
	t.Helper()
	f := &sandboxProviderFixture{state: map[string]any{
		"id": sandboxSettingID, "destination": "https://api.shop.invalid/webhooks/paddle",
		"active": true, "traffic_source": "all", "type": "url", "api_version": 1,
		"description": "synthetic fixture", "include_sensitive_fields": false,
		"endpoint_secret_key": sandboxTestSecret,
		"subscribed_events":   []any{map[string]any{"name": "subscription.created"}, map[string]any{"name": "transaction.completed"}},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+sandboxTestKey {
			t.Error("missing scoped credential")
		}
		if r.URL.Path != "/notification-settings/"+sandboxSettingID {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method == http.MethodPatch {
			var patch map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Error(err)
			}
			f.patches = append(f.patches, patch)
			if len(patch) != 2 || patch["destination"] == nil || patch["subscribed_events"] == nil {
				t.Errorf("overbroad patch keys: %v", patch)
			}
			if f.onPatch != nil {
				f.onPatch(w)
				return
			}
			for k, raw := range patch {
				var v any
				if err := json.Unmarshal(raw, &v); err != nil {
					t.Fatal(err)
				}
				f.state[k] = v
			}
		} else if r.Method == http.MethodGet {
			f.reads++
			if f.beforeRead != nil {
				f.beforeRead(f.reads)
			}
		} else {
			t.Errorf("unexpected method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": f.state})
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	a := NewPaddleSandboxNotificationAdapter(sandboxTestKey)
	a.client.Transport = sandboxRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "sandbox-api.paddle.com" {
			return nil, fmt.Errorf("non-sandbox origin")
		}
		clone := r.Clone(r.Context())
		requestURL := *r.URL
		clone.URL = &requestURL
		clone.URL.Scheme, clone.URL.Host = u.Scheme, u.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
	return f, a
}

func sandboxPlanIntent(t *testing.T, a *PaddleSandboxNotificationAdapter) (PaddleSandboxNotificationPlan, envelope.ActionIntent, envelope.PolicyDecision) {
	t.Helper()
	plan, err := a.Plan(sandboxSettingID, "https://sandbox.shop.invalid/webhooks/paddle")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ai, err := BuildPaddleSandboxNotificationIntent(plan, "work_sandbox_fixture", "codex:fixture", now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := policy.DefaultConfig()
	zeroUSD, zeroTokens := float64(0), 0
	cfg.StandingGrants = []policy.StandingGrant{{ID: "grant_fixture", Capability: ai.Capability, MutationClass: ai.MutationClass, Reversibility: ai.Reversibility, BlastRadius: ai.BlastRadius, Destination: ai.Destination, MaxUSD: &zeroUSD, MaxTokens: &zeroTokens}}
	pd := policy.Evaluate(ai, cfg, now)
	if !policy.Allowed(pd) {
		t.Fatalf("fixture grant refused: %+v", pd)
	}
	return plan, ai, pd
}

func TestPaddleSandboxNotificationReceiptsOnlyExactAdditiveChange(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, ai, pd := sandboxPlanIntent(t, a)
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.patches) != 1 {
		t.Fatalf("PATCHes=%d", len(f.patches))
	}
	var events []string
	if err := json.Unmarshal(f.patches[0]["subscribed_events"], &events); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"subscription.created", "transaction.completed", "transaction.ready", "transaction.updated", "transaction.paid", "transaction.canceled"} {
		if !strings.Contains(strings.Join(events, ","), name) {
			t.Errorf("missing event %s", name)
		}
	}
	if len(events) != 6 {
		t.Errorf("unexpected events: %v", events)
	}
	if out.Receipt.StateDelta.Changed != true || out.Receipt.ID == "" || out.Transition.ID == "" {
		t.Fatal("missing state-delta proof")
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{sandboxTestKey, sandboxTestSecret} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("credential leaked in receipt or snapshot")
		}
	}
	if f.state["endpoint_secret_key"] != sandboxTestSecret || f.state["traffic_source"] != "all" || f.state["active"] != true {
		t.Fatal("preserved fields changed")
	}
}

func TestPaddleSandboxNotificationHeldDecisionMakesNoProviderCall(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, ai, pd := sandboxPlanIntent(t, a)
	before := f.reads
	pd.Outcome = "hold"
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(store, a, ai, pd, plan.ForwardValue(), time.Now()); err == nil {
		t.Fatal("hold accepted")
	}
	if len(f.patches) != 0 || f.reads != before {
		t.Fatal("held action reached provider")
	}
}

func TestPaddleSandboxNotificationDriftStopsBeforePatch(t *testing.T) {
	for _, field := range []string{"description", "destination", "endpoint_secret_key"} {
		t.Run(field, func(t *testing.T) {
			f, a := newSandboxProviderFixture(t)
			plan, ai, pd := sandboxPlanIntent(t, a)
			f.state[field] = "changed"
			store, err := NewArtifactStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Execute(store, a, ai, pd, plan.ForwardValue(), time.Now()); err == nil {
				t.Fatal("stale preimage accepted")
			}
			if len(f.patches) != 0 {
				t.Fatal("drifted setting mutated")
			}
		})
	}
}

func TestPaddleSandboxNotificationRejectsUnsafeInputsBeforeNetwork(t *testing.T) {
	// Build synthetic userinfo structurally: preserve the invalid URL case without
	// embedding a credential-shaped URL in a public source fixture.
	credentialedCallback := &url.URL{
		Scheme: "https", Host: "sandbox.shop.invalid", Path: "/webhooks/paddle",
		User: url.UserPassword("synthetic-user", "synthetic-password"),
	}
	for _, tc := range []struct{ id, dest, key string }{
		{"../transactions", "https://sandbox.shop.invalid/webhooks/paddle", sandboxTestKey},
		{sandboxSettingID, "http://sandbox.shop.invalid/webhooks/paddle", sandboxTestKey},
		{sandboxSettingID, credentialedCallback.String(), sandboxTestKey},
		{sandboxSettingID, "https://sandbox.shop.invalid/webhooks/paddle#fragment", sandboxTestKey},
		{sandboxSettingID, "https://sandbox.shop.invalid/webhooks/paddle", ""},
	} {
		t.Run(tc.id+tc.dest+fmt.Sprint(tc.key == ""), func(t *testing.T) {
			a := NewPaddleSandboxNotificationAdapter(tc.key)
			calls := 0
			a.client.Transport = sandboxRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("must not run") })
			if _, err := a.Plan(tc.id, tc.dest); err == nil {
				t.Fatal("unsafe input accepted")
			}
			if calls != 0 {
				t.Fatal("unsafe input reached provider")
			}
		})
	}
}

func TestPaddleSandboxNotificationRejectsRedirectAndSecretErrorBody(t *testing.T) {
	a := NewPaddleSandboxNotificationAdapter(sandboxTestKey)
	calls := 0
	a.client.Transport = sandboxRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://api.paddle.com/notification-settings/" + sandboxSettingID}}, Body: io.NopCloser(strings.NewReader(sandboxTestSecret)), Request: r}, nil
	})
	_, err := a.Plan(sandboxSettingID, "https://sandbox.shop.invalid/webhooks/paddle")
	if err == nil || strings.Contains(err.Error(), sandboxTestSecret) {
		t.Fatal("redirect/error disclosure not refused")
	}
	if calls != 1 {
		t.Fatalf("followed redirect: %d requests", calls)
	}
}

type sandboxUncertainError struct{}

func (sandboxUncertainError) Error() string                 { return "synthetic lost write response" }
func (sandboxUncertainError) MutationMayHaveOccurred() bool { return true }

type sandboxUncertainAdapter struct {
	value    []byte
	restores int
}

func (a *sandboxUncertainAdapter) ReadCurrent(envelope.ActionTarget) ([]byte, error) {
	return append([]byte(nil), a.value...), nil
}
func (a *sandboxUncertainAdapter) write(_ envelope.ActionTarget, b []byte) error {
	a.value = append([]byte(nil), b...)
	return fmt.Errorf("wrapped: %w", sandboxUncertainError{})
}
func (a *sandboxUncertainAdapter) restorePreImage(_ envelope.ActionTarget, b []byte) error {
	a.value = append([]byte(nil), b...)
	a.restores++
	return nil
}

func TestExecuteUncertainWriteRestoresPreimage(t *testing.T) {
	now := time.Now().UTC()
	ai := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_uncertain_fixture", CreatedAt: now.Format(time.RFC3339), IDs: envelope.IDs{WorkID: "work_uncertain_fixture"}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1", Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: "codex:fixture", Capability: "fixture.write", Action: "write", Target: envelope.ActionTarget{Kind: "fixture", ID: "fixture"}, MutationClass: "local_reversible", Reversibility: "easy", RollbackPlan: "restore own preimage"}
	pd := policy.Evaluate(ai, policy.DefaultConfig(), now)
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &sandboxUncertainAdapter{value: []byte("before")}
	_, err = Execute(store, a, ai, pd, []byte("after"), now)
	if err == nil {
		t.Fatal("uncertain write claimed success")
	}
	if string(a.value) != "before" || a.restores != 1 {
		t.Fatalf("uncertain mutation stranded: value=%q restores=%d", a.value, a.restores)
	}
}

func TestPaddleSandboxNotificationForwardValueMustMatchApprovedIntent(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, ai, pd := sandboxPlanIntent(t, a)
	plan.ToURL = "https://different.shop.invalid/webhooks/paddle"
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
	if err == nil || len(f.patches) != 0 {
		t.Fatalf("approved arguments escaped: err=%v PATCHes=%d", err, len(f.patches))
	}
}

func TestPaddleSandboxNotificationLostResponseReconcilesWithoutRetry(t *testing.T) {
	for _, outcome := range []string{"applied", "unchanged", "third_state"} {
		t.Run(outcome, func(t *testing.T) {
			f, a := newSandboxProviderFixture(t)
			plan, ai, pd := sandboxPlanIntent(t, a)
			f.onPatch = func(w http.ResponseWriter) {
				if outcome == "applied" {
					for k, raw := range f.patches[len(f.patches)-1] {
						var value any
						_ = json.Unmarshal(raw, &value)
						f.state[k] = value
					}
				}
				if outcome == "third_state" {
					f.state["description"] = "concurrent change"
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, sandboxTestSecret)
			}
			store, err := NewArtifactStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			out, err := Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
			if outcome == "applied" {
				if err != nil || out.Receipt.ID == "" {
					t.Fatalf("confirmed applied write not receipted: %v", err)
				}
			} else if err == nil {
				t.Fatal("unconfirmed write claimed success")
			}
			if outcome == "third_state" && !strings.Contains(err.Error(), "recovery finding") {
				t.Fatalf("uncertainty not durably surfaced: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), sandboxTestSecret) {
				t.Fatal("provider error body leaked")
			}
			if len(f.patches) != 1 {
				t.Fatalf("retried or overwrote third-party state: %d PATCHes", len(f.patches))
			}
		})
	}
}

func TestPaddleSandboxNotificationNoOp(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	f.state["subscribed_events"] = []string{"transaction.canceled", "transaction.paid", "transaction.ready", "transaction.updated"}
	p, err := a.Plan(sandboxSettingID, "https://api.shop.invalid/webhooks/paddle")
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsNoOp() {
		t.Fatal("already aligned setting should not consume approval or mint a changed receipt")
	}
	if len(f.patches) != 0 {
		t.Fatal("no-op sent PATCH")
	}
}

func TestPaddleSandboxNotificationExpiredDecisionStopsBeforeProvider(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, ai, pd := sandboxPlanIntent(t, a)
	pd.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	reads := f.reads
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
	if err == nil || len(f.patches) != 0 || f.reads != reads {
		t.Fatalf("expired decision reached provider: err=%v patches=%d reads=%d", err, len(f.patches), f.reads-reads)
	}
}

func TestPaddleSandboxNotificationApprovalExpiresDuringPreRead(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, ai, pd := sandboxPlanIntent(t, a)
	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	pd.ExpiresAt = expires.Format(time.RFC3339Nano)
	a.now = func() time.Time { return now }
	f.beforeRead = func(int) { now = expires }
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Execute(store, a, ai, pd, plan.ForwardValue(), now)
	if err == nil || len(f.patches) != 0 {
		t.Fatalf("expired during read: err=%v PATCHes=%d", err, len(f.patches))
	}
}

func TestPaddleSandboxNotificationEventUnionIsInApprovedBytes(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, ai, pd := sandboxPlanIntent(t, a)
	if len(plan.ToEvents) != 6 {
		t.Fatalf("approved plan lacks exact event union: %v", plan.ToEvents)
	}
	plan.ToEvents = append(plan.ToEvents, "customer.created")
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reads := f.reads
	_, err = Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
	if err == nil || len(f.patches) != 0 || f.reads != reads {
		t.Fatalf("changed approved event payload executed: %v", err)
	}
}

func sandboxAllow(t *testing.T, ai envelope.ActionIntent) envelope.PolicyDecision {
	t.Helper()
	cfg := policy.DefaultConfig()
	usd, tokens := float64(0), 0
	cfg.StandingGrants = []policy.StandingGrant{{ID: "grant_sandbox_fixture", MutationClass: ai.MutationClass, Capability: ai.Capability, BlastRadius: ai.BlastRadius, MaxUSD: &usd, MaxTokens: &tokens}}
	return policy.Evaluate(ai, cfg, time.Now())
}

func TestPaddleSandboxNotificationFreshProcessRevert(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, ai, pd := sandboxPlanIntent(t, a)
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Rehydrate images from durable artifacts; no adapter in-memory state crosses.
	out.PreImage, err = store.Get(out.PreImageRef)
	if err != nil {
		t.Fatal(err)
	}
	out.PostImage, err = store.Get(out.PostImageRef)
	if err != nil {
		t.Fatal(err)
	}
	revert, body, err := BuildPaddleSandboxNotificationRevertIntent(ai, out, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewPaddleSandboxNotificationAdapter(sandboxTestKey)
	fresh.client = a.client
	if err := fresh.AuthorizeRevert(store, ai, out); err != nil {
		t.Fatal(err)
	}
	restored, err := ExecuteRevert(store, fresh, revert, sandboxAllow(t, revert), body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if string(restored.PostImage) != string(out.PreImage) || len(f.patches) != 2 {
		t.Fatalf("fresh revert failed: post=%s PATCHes=%d", restored.PostImage, len(f.patches))
	}
	// Replaying with a fresh adapter must not invent a changed=true receipt.
	fresh = NewPaddleSandboxNotificationAdapter(sandboxTestKey)
	fresh.client = a.client
	if err := fresh.AuthorizeRevert(store, ai, out); err != nil {
		t.Fatal(err)
	}
	_, err = ExecuteRevert(store, fresh, revert, sandboxAllow(t, revert), body, time.Now())
	if err == nil || len(f.patches) != 2 {
		t.Fatalf("replayed revert claimed a mutation: %v", err)
	}
}

func TestPaddleSandboxNotificationReceiptFailureCompensates(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, ai, pd := sandboxPlanIntent(t, a)
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := recordEnvelopeFn
	recordEnvelopeFn = func(string, string, any) error { return fmt.Errorf("synthetic receipt persistence failure") }
	t.Cleanup(func() { recordEnvelopeFn = original })
	_, err = Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
	if err == nil || len(f.patches) != 2 || f.state["destination"] != plan.Before.Destination {
		t.Fatalf("unreceipted write not compensated: err=%v PATCHes=%d", err, len(f.patches))
	}
}

func TestPaddleSandboxNotificationUnboundForgedRevertRefusesBeforeProvider(t *testing.T) {
	f, a := newSandboxProviderFixture(t)
	plan, original, _ := sandboxPlanIntent(t, a)
	before := plan.Before
	before.SubscribedEvents = []string{"customer.created"}
	forged := Result{PreImage: sandboxSnapshotBytes(before), PostImage: sandboxSnapshotBytes(plan.Before)}
	ai, body, err := BuildPaddleSandboxNotificationRevertIntent(original, forged, time.Now())
	if err != nil {
		return
	} // A builder refusal is also safe.
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reads := f.reads
	_, err = ExecuteRevert(store, a, ai, sandboxAllow(t, ai), body, time.Now())
	if err == nil || len(f.patches) != 0 || f.reads != reads {
		t.Fatalf("forged unbound restore reached provider: err=%v PATCHes=%d reads=%d", err, len(f.patches), f.reads-reads)
	}
}

func TestPaddleSandboxNotificationRevertProofCannotBeSubstituted(t *testing.T) {
	for _, mode := range []string{"missing_receipt", "changed_transition", "changed_bytes", "changed_original", "changed_actor", "different_home"} {
		t.Run(mode, func(t *testing.T) {
			f, a := newSandboxProviderFixture(t)
			plan, ai, pd := sandboxPlanIntent(t, a)
			store, err := NewArtifactStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			out, err := Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			reads := f.reads
			fresh := NewPaddleSandboxNotificationAdapter(sandboxTestKey)
			fresh.client = a.client
			switch mode {
			case "missing_receipt":
				out.Receipt.ID = "rec_missing"
			case "changed_transition":
				out.Transition.ActionRefs = []string{"act_other"}
			case "changed_bytes":
				out.PreImage = append(out.PreImage, ' ')
			case "changed_original":
				ai.ArgumentsHash = sandboxDigest([]byte("different"))
			case "changed_actor":
				ai.Actor = "codex:another-actor"
			}
			err = fresh.AuthorizeRevert(store, ai, out)
			if mode == "different_home" {
				if err != nil {
					t.Fatal(err)
				}
				revert, body, buildErr := BuildPaddleSandboxNotificationRevertIntent(ai, out, time.Now())
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				other, storeErr := NewArtifactStore(t.TempDir())
				if storeErr != nil {
					t.Fatal(storeErr)
				}
				_, err = ExecuteRevert(other, fresh, revert, sandboxAllow(t, revert), body, time.Now())
			}
			if err == nil || f.reads != reads || len(f.patches) != 1 {
				t.Fatalf("substituted proof reached provider: %v", err)
			}
		})
	}
}

func TestPaddleSandboxNotificationContractCannotBeRelabeled(t *testing.T) {
	for _, mode := range []string{"blast_radius", "actor"} {
		t.Run(mode, func(t *testing.T) {
			f, a := newSandboxProviderFixture(t)
			plan, ai, _ := sandboxPlanIntent(t, a)
			if mode == "blast_radius" {
				ai.BlastRadius = "one_other_scope"
			} else {
				ai.Actor = "codex:another-actor"
			}
			pd := sandboxAllow(t, ai)
			store, err := NewArtifactStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			reads := f.reads
			_, err = Execute(store, a, ai, pd, plan.ForwardValue(), time.Now())
			if err == nil || len(f.patches) != 0 || f.reads != reads {
				t.Fatalf("relabeling reached provider: %v", err)
			}
		})
	}
}
