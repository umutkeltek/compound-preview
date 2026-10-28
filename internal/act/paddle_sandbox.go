package act

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
)

const paddleSandboxOrigin = "https://sandbox-api.paddle.com"
const paddleSandboxPlanSchema = "compound.paddle_sandbox_notification_plan.v0.2"
const paddleSandboxRevertSchema = "compound.paddle_sandbox_notification_revert.v0.1"
const paddleSandboxTargetKind = "paddle_sandbox_notification_setting"
const paddleSandboxMaxBody = 1 << 20

var paddleSandboxSettingPattern = regexp.MustCompile(`^ntfset_[a-z0-9]{26}$`)
var paddleSandboxDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type PaddleSandboxNotificationSnapshot struct {
	ID               string   `json:"id"`
	Destination      string   `json:"destination"`
	SubscribedEvents []string `json:"subscribed_events"`
	PreservedSHA256  string   `json:"preserved_sha256"`
}

type PaddleSandboxNotificationPlan struct {
	SchemaVersion string                            `json:"schema_version"`
	Before        PaddleSandboxNotificationSnapshot `json:"before"`
	ToURL         string                            `json:"to_url"`
	ToEvents      []string                          `json:"to_events"`
}

// PaddleSandboxNotificationAdapter only changes callback URL and the additive
// recovery-event union. Its private mutation methods are reachable via Execute.
// No raw provider response, key, or secret is retained in artifact images.
type PaddleSandboxNotificationAdapter struct {
	client         *http.Client
	apiKey         string
	before         *PaddleSandboxNotificationSnapshot
	expectedPost   *PaddleSandboxNotificationSnapshot
	now            func() time.Time
	expiresAt      time.Time
	used           bool
	admissionCheck func() error
	revertDigest   string
	revertHome     string
}

// SetAdmissionCheck supplies the owning CLI's fresh work/lease/halt check.
// It is called after the final provider read and before a new mutation, never
// instead of the PolicyDecision gate. Automatic compensation is not a new action.
func (a *PaddleSandboxNotificationAdapter) SetAdmissionCheck(check func() error) {
	a.admissionCheck = check
}

func NewPaddleSandboxNotificationAdapter(apiKey string) *PaddleSandboxNotificationAdapter {
	return &PaddleSandboxNotificationAdapter{apiKey: apiKey, now: time.Now, client: &http.Client{
		Transport: http.DefaultTransport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func validatePaddleCallback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" || u.Path != "/webhooks/paddle" || strings.TrimSpace(raw) != raw {
		return fmt.Errorf("paddle sandbox: callback must be an HTTPS /webhooks/paddle URL without credentials, query or fragment")
	}
	return nil
}

func (a *PaddleSandboxNotificationAdapter) validateID(id string) error {
	if a == nil || a.client == nil || strings.TrimSpace(a.apiKey) == "" || strings.ContainsAny(a.apiKey, "\r\n") {
		return fmt.Errorf("paddle sandbox: PADDLE_SANDBOX_API_KEY is required")
	}
	if !paddleSandboxSettingPattern.MatchString(id) {
		return fmt.Errorf("paddle sandbox: invalid notification setting ID")
	}
	return nil
}

func paddleSandboxTarget(id string) envelope.ActionTarget {
	return envelope.ActionTarget{Kind: paddleSandboxTargetKind, ID: id, Path: paddleSandboxOrigin + "/notification-settings/" + id}
}

func (a *PaddleSandboxNotificationAdapter) validateTarget(t envelope.ActionTarget) error {
	if err := a.validateID(t.ID); err != nil {
		return err
	}
	if t != paddleSandboxTarget(t.ID) {
		return fmt.Errorf("paddle sandbox: target does not match the sandbox notification endpoint")
	}
	return nil
}

func sandboxDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func sandboxSnapshotBytes(s PaddleSandboxNotificationSnapshot) []byte {
	b, _ := json.Marshal(s)
	return b
}
func sandboxSnapshotsEqual(a, b PaddleSandboxNotificationSnapshot) bool {
	return bytes.Equal(sandboxSnapshotBytes(a), sandboxSnapshotBytes(b))
}
func (p PaddleSandboxNotificationPlan) BeforeDigest() string {
	return sandboxDigest(sandboxSnapshotBytes(p.Before))
}
func (p PaddleSandboxNotificationPlan) ForwardValue() []byte { b, _ := json.Marshal(p); return b }

func (p PaddleSandboxNotificationPlan) IsNoOp() bool {
	return sandboxSnapshotsEqual(p.Before, sandboxDesired(p))
}

// The generic gate binds decision to intent. This adapter additionally binds
// the actual bytes, capability, schema and provider target to that intent.
func (a *PaddleSandboxNotificationAdapter) validateIntent(homePath string, ai envelope.ActionIntent, pd envelope.PolicyDecision, body []byte) error {
	if a.used {
		return fmt.Errorf("paddle sandbox: adapter supports only one execution")
	}
	forward := ai.Action == "update_sandbox_notification_setting" && ai.SchemaRevision == paddleSandboxPlanSchema
	revert := ai.Action == "revert:update_sandbox_notification_setting" && ai.SchemaRevision == paddleSandboxRevertSchema
	if ai.Capability != "paddle.notification_settings.update" || (!forward && !revert) || ai.MutationClass != "external_side_effect" {
		return fmt.Errorf("paddle sandbox: unexpected action contract")
	}
	if err := validateSandboxScope(ai); err != nil {
		return err
	}
	if err := a.validateTarget(ai.Target); err != nil {
		return err
	}
	if ai.Destination != ai.Target.Path || ai.ArgumentsHash != sandboxDigest(body) {
		return fmt.Errorf("paddle sandbox: destination or forward bytes differ from approved intent")
	}
	if revert {
		if a.revertDigest == "" || a.revertDigest != sandboxDigest(body) || a.revertHome != homePath {
			return fmt.Errorf("paddle sandbox: revert lacks verified durable original-action proof")
		}
		if _, err := decodeSandboxRevert(body, ai.Target.ID); err != nil {
			return err
		}
	} else {
		var p PaddleSandboxNotificationPlan
		if err := decodeSandboxBody(body, &p); err != nil {
			return err
		}
		if err := validateSandboxPlan(p); err != nil {
			return err
		}
		if p.Before.ID != ai.Target.ID {
			return fmt.Errorf("paddle sandbox: plan and target differ")
		}
		if err := validateSandboxForwardIdentity(ai, p); err != nil {
			return err
		}
	}
	if pd.ExpiresAt != "" {
		expires, err := time.Parse(time.RFC3339Nano, pd.ExpiresAt)
		if err != nil {
			return fmt.Errorf("paddle sandbox: invalid approval expiry")
		}
		a.expiresAt = expires
	}
	if err := a.checkApprovalExpiry(); err != nil {
		return err
	}
	a.used = true
	return nil
}

func validateSandboxScope(ai envelope.ActionIntent) error {
	if ai.Reversibility != "manual" || ai.BlastRadius != "one_sandbox_notification_setting" || ai.EstimatedCost.USD == nil || *ai.EstimatedCost.USD != 0 || ai.EstimatedCost.Tokens == nil || *ai.EstimatedCost.Tokens != 0 {
		return fmt.Errorf("paddle sandbox: fixed blast radius, recovery class and zero estimates required")
	}
	return nil
}

func validateSandboxForwardIdentity(ai envelope.ActionIntent, p PaddleSandboxNotificationPlan) error {
	created, err := time.Parse(time.RFC3339Nano, ai.CreatedAt)
	if err != nil {
		return fmt.Errorf("paddle sandbox: invalid original creation time")
	}
	expected, err := BuildPaddleSandboxNotificationIntent(p, ai.IDs.WorkID, ai.Actor, created)
	if err != nil {
		return err
	}
	if ai.ID != expected.ID || ai.ArgumentsHash != expected.ArgumentsHash {
		return fmt.Errorf("paddle sandbox: original identity does not bind its actor, work and payload")
	}
	return nil
}

func (a *PaddleSandboxNotificationAdapter) checkApprovalExpiry() error {
	if !a.expiresAt.IsZero() && !a.now().Before(a.expiresAt) {
		return fmt.Errorf("paddle sandbox: policy decision expired before mutation")
	}
	return nil
}

func sandboxEventUnion(events []string) []string {
	out := append([]string(nil), events...)
	out = append(out, "transaction.canceled", "transaction.paid", "transaction.ready", "transaction.updated")
	slices.Sort(out)
	return slices.Compact(out)
}

func sandboxDesired(p PaddleSandboxNotificationPlan) PaddleSandboxNotificationSnapshot {
	s := p.Before
	s.Destination = p.ToURL
	s.SubscribedEvents = append([]string(nil), p.ToEvents...)
	return s
}

func validateSandboxPlan(p PaddleSandboxNotificationPlan) error {
	if p.SchemaVersion != paddleSandboxPlanSchema || !paddleSandboxSettingPattern.MatchString(p.Before.ID) || !paddleSandboxDigestPattern.MatchString(p.Before.PreservedSHA256) {
		return fmt.Errorf("paddle sandbox: invalid notification plan")
	}
	if err := validatePaddleCallback(p.Before.Destination); err != nil {
		return err
	}
	if err := validatePaddleCallback(p.ToURL); err != nil {
		return err
	}
	if !slices.Equal(p.ToEvents, sandboxEventUnion(p.Before.SubscribedEvents)) {
		return fmt.Errorf("paddle sandbox: proposed events must be the exact additive recovery union")
	}
	if !slices.IsSorted(p.Before.SubscribedEvents) || len(slices.Compact(append([]string(nil), p.Before.SubscribedEvents...))) != len(p.Before.SubscribedEvents) {
		return fmt.Errorf("paddle sandbox: events must be canonical")
	}
	for _, name := range p.Before.SubscribedEvents {
		if name == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("paddle sandbox: invalid event name")
		}
	}
	return nil
}

// Plan is a bounded read. It neither records authority nor sends a PATCH.
func (a *PaddleSandboxNotificationAdapter) Plan(settingID, toURL string) (PaddleSandboxNotificationPlan, error) {
	if err := a.validateID(settingID); err != nil {
		return PaddleSandboxNotificationPlan{}, err
	}
	if err := validatePaddleCallback(toURL); err != nil {
		return PaddleSandboxNotificationPlan{}, err
	}
	s, err := a.snapshot(settingID)
	if err != nil {
		return PaddleSandboxNotificationPlan{}, err
	}
	p := PaddleSandboxNotificationPlan{SchemaVersion: paddleSandboxPlanSchema, Before: s, ToURL: toURL, ToEvents: sandboxEventUnion(s.SubscribedEvents)}
	return p, validateSandboxPlan(p)
}

func BuildPaddleSandboxNotificationIntent(p PaddleSandboxNotificationPlan, workID, actor string, now time.Time) (envelope.ActionIntent, error) {
	if err := validateSandboxPlan(p); err != nil {
		return envelope.ActionIntent{}, err
	}
	if strings.TrimSpace(workID) == "" || strings.TrimSpace(actor) == "" {
		return envelope.ActionIntent{}, fmt.Errorf("paddle sandbox: work and actor are required")
	}
	body := p.ForwardValue()
	id := sandboxDigest(append(append([]byte(workID+"\x00"+actor+"\x00"), body...), []byte(now.UTC().Format(time.RFC3339Nano))...))
	ai := envelope.ActionIntent{Header: envelope.Header{
		SchemaVersion: envelope.SVActionIntent, ID: "act_" + strings.TrimPrefix(id, "sha256:")[:24], CreatedAt: now.UTC().Format(time.RFC3339Nano),
		Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: workID},
		Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1", Uncertainty: envelope.Uncertainty{Level: "known"},
	}, Actor: actor, Capability: "paddle.notification_settings.update", Action: "update_sandbox_notification_setting",
		Target: paddleSandboxTarget(p.Before.ID), Destination: paddleSandboxTarget(p.Before.ID).Path,
		ArgumentsHash: sandboxDigest(body), SchemaRevision: paddleSandboxPlanSchema, MutationClass: "external_side_effect", Reversibility: "manual", BlastRadius: "one_sandbox_notification_setting",
		Preview:      "Add the four recovery events and set the explicit sandbox callback; preserve all other notification settings.",
		RollbackPlan: "Restore only this setting's captured destination and event set if current state still matches this action; uncertain or third-party drift requires recorded manual recovery, never blind overwrite.",
	}
	zeroUSD, zeroTokens := float64(0), 0
	ai.EstimatedCost = envelope.EstimatedCost{USD: &zeroUSD, Tokens: &zeroTokens}
	return ai, ai.Validate()
}

// snapshot hashes every untouched provider field, including the secret, as one
// opaque preservation fingerprint. Neither a secret nor a secret-only hash is
// returned. New provider fields are protected by default, not silently ignored.
func (a *PaddleSandboxNotificationAdapter) snapshot(id string) (PaddleSandboxNotificationSnapshot, error) {
	if err := a.validateID(id); err != nil {
		return PaddleSandboxNotificationSnapshot{}, err
	}
	data, err := a.request(http.MethodGet, id, nil)
	if err != nil {
		return PaddleSandboxNotificationSnapshot{}, err
	}
	var response struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Data == nil {
		return PaddleSandboxNotificationSnapshot{}, fmt.Errorf("paddle sandbox: invalid notification response")
	}
	readString := func(key string) string {
		var value string
		_ = json.Unmarshal(response.Data[key], &value)
		return value
	}
	var active bool
	_ = json.Unmarshal(response.Data["active"], &active)
	if readString("id") != id || !active || readString("type") != "url" || readString("endpoint_secret_key") == "" {
		return PaddleSandboxNotificationSnapshot{}, fmt.Errorf("paddle sandbox: expected an active URL notification setting with a secret")
	}
	destination := readString("destination")
	if err := validatePaddleCallback(destination); err != nil {
		return PaddleSandboxNotificationSnapshot{}, err
	}
	var rawEvents []json.RawMessage
	if err := json.Unmarshal(response.Data["subscribed_events"], &rawEvents); err != nil {
		return PaddleSandboxNotificationSnapshot{}, fmt.Errorf("paddle sandbox: invalid event list")
	}
	events := make([]string, 0, len(rawEvents))
	for _, raw := range rawEvents {
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			var e struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &e); err != nil {
				return PaddleSandboxNotificationSnapshot{}, fmt.Errorf("paddle sandbox: invalid event")
			}
			name = e.Name
		}
		if name == "" || strings.TrimSpace(name) != name {
			return PaddleSandboxNotificationSnapshot{}, fmt.Errorf("paddle sandbox: empty or malformed event name")
		}
		events = append(events, name)
	}
	slices.Sort(events)
	events = slices.Compact(events)
	delete(response.Data, "destination")
	delete(response.Data, "subscribed_events")
	// Decode the preserved object once before hashing so whitespace and key order
	// in a provider's JSON cannot manufacture drift.
	encoded, _ := json.Marshal(response.Data)
	var canonical any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		return PaddleSandboxNotificationSnapshot{}, fmt.Errorf("paddle sandbox: invalid preserved settings")
	}
	preserved, _ := json.Marshal(canonical)
	return PaddleSandboxNotificationSnapshot{ID: id, Destination: destination, SubscribedEvents: events, PreservedSHA256: sandboxDigest(preserved)}, nil
}

func (a *PaddleSandboxNotificationAdapter) request(method, id string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(method, paddleSandboxOrigin+"/notification-settings/"+id, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("paddle sandbox: invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paddle sandbox: %s transport failed", method)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("paddle sandbox: %s returned HTTP %d", method, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, paddleSandboxMaxBody+1))
	if err != nil || len(data) > paddleSandboxMaxBody {
		return nil, fmt.Errorf("paddle sandbox: %s response unreadable or too large", method)
	}
	return data, nil
}

func (a *PaddleSandboxNotificationAdapter) ReadCurrent(t envelope.ActionTarget) ([]byte, error) {
	if err := a.validateTarget(t); err != nil {
		return nil, err
	}
	s, err := a.snapshot(t.ID)
	if err != nil {
		return nil, err
	}
	if a.expectedPost != nil && !sandboxSnapshotsEqual(s, *a.expectedPost) {
		return nil, fmt.Errorf("paddle sandbox: post-write state or preserved fields differ")
	}
	return sandboxSnapshotBytes(s), nil
}

type paddleUncertainWrite struct{}

type paddleSandboxRevertPlan struct {
	SchemaVersion string                            `json:"schema_version"`
	Before        PaddleSandboxNotificationSnapshot `json:"before"`
	Restore       PaddleSandboxNotificationSnapshot `json:"restore"`
}

// BuildPaddleSandboxNotificationRevertIntent proposes a sealed fresh intent.
// It grants no restore authority: AuthorizeRevert must verify durable provenance,
// and ExecuteRevert separately requires a matching allowing PolicyDecision.
func BuildPaddleSandboxNotificationRevertIntent(original envelope.ActionIntent, result Result, now time.Time) (envelope.ActionIntent, []byte, error) {
	var before, after PaddleSandboxNotificationSnapshot
	if err := decodeSandboxBody(result.PreImage, &before); err != nil {
		return envelope.ActionIntent{}, nil, err
	}
	if err := decodeSandboxBody(result.PostImage, &after); err != nil {
		return envelope.ActionIntent{}, nil, err
	}
	p := paddleSandboxRevertPlan{SchemaVersion: paddleSandboxRevertSchema, Before: after, Restore: before}
	body, err := json.Marshal(p)
	if err != nil {
		return envelope.ActionIntent{}, nil, err
	}
	if _, err := decodeSandboxRevert(body, original.Target.ID); err != nil {
		return envelope.ActionIntent{}, nil, err
	}
	ai := RevertIntent(original, result, now)
	ai.SchemaRevision = paddleSandboxRevertSchema
	ai.ArgumentsHash = sandboxDigest(body)
	return ai, body, ai.Validate()
}

// AuthorizeRevert binds this one-shot adapter to a completed original action's
// canonical Receipt/Transition and content-addressed images in the execution home.
// Caller-constructed Result bytes, or even a fresh policy grant, are not provenance.
func (a *PaddleSandboxNotificationAdapter) AuthorizeRevert(store *ArtifactStore, original envelope.ActionIntent, result Result) error {
	if a.used || store == nil {
		return fmt.Errorf("paddle sandbox: fresh adapter and artifact store required")
	}
	a.revertDigest, a.revertHome = "", ""
	if original.Action != "update_sandbox_notification_setting" || original.SchemaRevision != paddleSandboxPlanSchema || original.Capability != "paddle.notification_settings.update" {
		return fmt.Errorf("paddle sandbox: original action is not a forward notification update")
	}
	if err := validateSandboxScope(original); err != nil {
		return err
	}
	if err := a.validateTarget(original.Target); err != nil {
		return err
	}
	pre, err := store.Get(result.PreImageRef)
	if err != nil {
		return fmt.Errorf("paddle sandbox: missing original pre-image")
	}
	post, err := store.Get(result.PostImageRef)
	if err != nil {
		return fmt.Errorf("paddle sandbox: missing original post-image")
	}
	if !bytes.Equal(pre, result.PreImage) || !bytes.Equal(post, result.PostImage) || bytes.Equal(pre, post) {
		return fmt.Errorf("paddle sandbox: original image bytes do not match durable change")
	}
	events, err := eventstore.Open(store.Home())
	if err != nil {
		return err
	}
	for _, entry := range []struct {
		id    string
		value any
	}{{result.Receipt.ID, result.Receipt}, {result.Transition.ID, result.Transition}} {
		record, err := events.ReadRegular(entry.id, "sandbox revert proof")
		if err != nil {
			return fmt.Errorf("paddle sandbox: missing canonical original action proof")
		}
		var decoded any
		if err := json.Unmarshal(record.Bytes, &decoded); err != nil {
			return fmt.Errorf("paddle sandbox: invalid canonical action proof")
		}
		canonical, _ := json.Marshal(decoded)
		provided, _ := json.Marshal(entry.value)
		var providedValue any
		if err := json.Unmarshal(provided, &providedValue); err != nil {
			return err
		}
		provided, _ = json.Marshal(providedValue)
		if !bytes.Equal(canonical, provided) {
			return fmt.Errorf("paddle sandbox: original proof differs from canonical record")
		}
	}
	refs := []string{result.PreImageRef, result.PostImageRef}
	r, tr := result.Receipt, result.Transition
	if r.Validate() != nil || tr.Validate() != nil || r.Outcome != "completed" || r.MoveType != "outward_delta" || !r.StateDelta.Changed || tr.StateDelta != r.StateDelta.Summary || r.IDs.WorkID != original.IDs.WorkID || tr.IDs.WorkID != original.IDs.WorkID || !slices.Equal(r.ProofRefs, refs) || !slices.Equal(tr.ActionRefs, []string{original.ID}) || !slices.Equal(tr.BeforeRefs, refs[:1]) || !slices.Equal(tr.AfterRefs, refs[1:]) {
		return fmt.Errorf("paddle sandbox: canonical proof is not this completed original action")
	}
	_, body, err := BuildPaddleSandboxNotificationRevertIntent(original, result, a.now())
	if err != nil {
		return err
	}
	p, err := decodeSandboxRevert(body, original.Target.ID)
	if err != nil {
		return err
	}
	forward := PaddleSandboxNotificationPlan{SchemaVersion: paddleSandboxPlanSchema, Before: p.Restore, ToURL: p.Before.Destination, ToEvents: p.Before.SubscribedEvents}
	if err := validateSandboxPlan(forward); err != nil {
		return err
	}
	if original.ArgumentsHash != sandboxDigest(forward.ForwardValue()) {
		return fmt.Errorf("paddle sandbox: images do not match original approved forward payload")
	}
	if err := validateSandboxForwardIdentity(original, forward); err != nil {
		return err
	}
	a.revertDigest, a.revertHome = sandboxDigest(body), store.Home()
	return nil
}

func decodeSandboxBody(body []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("paddle sandbox: invalid action body")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("paddle sandbox: trailing action data")
	}
	return nil
}

func decodeSandboxRevert(body []byte, id string) (paddleSandboxRevertPlan, error) {
	var p paddleSandboxRevertPlan
	if err := decodeSandboxBody(body, &p); err != nil {
		return p, err
	}
	if p.SchemaVersion != paddleSandboxRevertSchema || p.Before.ID != id || p.Restore.ID != id || p.Before.PreservedSHA256 != p.Restore.PreservedSHA256 {
		return p, fmt.Errorf("paddle sandbox: invalid revert binding")
	}
	for _, s := range []PaddleSandboxNotificationSnapshot{p.Before, p.Restore} {
		probe := PaddleSandboxNotificationPlan{SchemaVersion: paddleSandboxPlanSchema, Before: s, ToURL: s.Destination, ToEvents: sandboxEventUnion(s.SubscribedEvents)}
		if err := validateSandboxPlan(probe); err != nil {
			return p, err
		}
	}
	if sandboxSnapshotsEqual(p.Before, p.Restore) {
		return p, fmt.Errorf("paddle sandbox: revert contains no change")
	}
	return p, nil
}

func (paddleUncertainWrite) Error() string {
	return "paddle sandbox: PATCH outcome uncertain; reconciliation could not prove original or requested state"
}
func (paddleUncertainWrite) MutationMayHaveOccurred() bool { return true }

func (a *PaddleSandboxNotificationAdapter) patch(s PaddleSandboxNotificationSnapshot) error {
	body, _ := json.Marshal(struct {
		Destination      string   `json:"destination"`
		SubscribedEvents []string `json:"subscribed_events"`
	}{s.Destination, s.SubscribedEvents})
	_, err := a.request(http.MethodPatch, s.ID, body)
	return err
}

func (a *PaddleSandboxNotificationAdapter) write(t envelope.ActionTarget, body []byte) error {
	if err := a.validateTarget(t); err != nil {
		return err
	}
	var p PaddleSandboxNotificationPlan
	if err := decodeSandboxBody(body, &p); err != nil {
		return err
	}
	if err := validateSandboxPlan(p); err != nil {
		return err
	}
	if p.Before.ID != t.ID {
		return fmt.Errorf("paddle sandbox: plan and target differ")
	}
	return a.applyPlanned(p.Before, sandboxDesired(p))
}

func (a *PaddleSandboxNotificationAdapter) applyPlanned(before, desired PaddleSandboxNotificationSnapshot) error {
	current, err := a.snapshot(before.ID)
	if err != nil {
		return err
	}
	if !sandboxSnapshotsEqual(current, before) {
		return fmt.Errorf("paddle sandbox: notification state drifted before PATCH")
	}
	if sandboxSnapshotsEqual(current, desired) {
		return fmt.Errorf("paddle sandbox: already configured; no mutation required")
	}
	a.before = &current
	a.expectedPost = &desired
	if a.admissionCheck != nil {
		if err := a.admissionCheck(); err != nil {
			return fmt.Errorf("paddle sandbox: admission changed before PATCH: %w", err)
		}
	}
	if err := a.checkApprovalExpiry(); err != nil {
		return err
	}
	if err := a.patch(desired); err != nil {
		// Never retry an uncertain PATCH. One GET can prove it succeeded, prove
		// it did not apply, or send the uncertainty into Execute's recovery path.
		observed, readErr := a.snapshot(before.ID)
		if readErr == nil && sandboxSnapshotsEqual(observed, desired) {
			return nil
		}
		if readErr == nil && sandboxSnapshotsEqual(observed, current) {
			a.expectedPost = nil
			return err
		}
		return paddleUncertainWrite{}
	}
	return nil
}

func (a *PaddleSandboxNotificationAdapter) restorePreImage(t envelope.ActionTarget, body []byte) error {
	if err := a.validateTarget(t); err != nil {
		return err
	}
	if a.before == nil {
		p, err := decodeSandboxRevert(body, t.ID)
		if err != nil {
			return err
		}
		return a.applyPlanned(p.Before, p.Restore)
	}
	if a.before == nil || a.expectedPost == nil || !bytes.Equal(body, sandboxSnapshotBytes(*a.before)) {
		return fmt.Errorf("paddle sandbox: restore is not bound to this adapter's attempted write")
	}
	current, err := a.snapshot(t.ID)
	if err != nil {
		return err
	}
	if sandboxSnapshotsEqual(current, *a.before) {
		a.expectedPost = nil
		return nil
	}
	if !sandboxSnapshotsEqual(current, *a.expectedPost) {
		return fmt.Errorf("paddle sandbox: refusing rollback over changed or unverified provider state")
	}
	patchErr := a.patch(*a.before)
	observed, readErr := a.snapshot(t.ID)
	if readErr == nil && sandboxSnapshotsEqual(observed, *a.before) {
		a.expectedPost = nil
		return nil
	}
	if patchErr != nil {
		return patchErr
	}
	return fmt.Errorf("paddle sandbox: rollback readback could not prove the original state")
}
