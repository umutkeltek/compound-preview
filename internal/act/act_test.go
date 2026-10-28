package act

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/finding"
	"compound/internal/policy"
)

// These tests drive compound's ONE generic governed outbound-write wrapper against
// two SCRATCH configs (never a live config): one shaped like a switchback egress
// alias, one like a headroom config key. The same envelope governs both — only
// Target.Path/Target.ID differ — so proving the switchback shape and re-proving the
// headroom shape demonstrates outbound generality at zero schema cost. Every proof
// mutates only a mktemp file; nothing under a real operator home is touched.

// SEAL NOTE (structural proof — the guarantee is a compile error, not a runtime check):
// Adapter.write is unexported, so an external package — e.g. `package act_test` — is
// structurally UNABLE to call an adapter's write to mutate a config without a
// PolicyDecision. Both `act.NewConfigKeyAdapter().write(target, b)` and
// `act.ConfigKeyAdapter{}.write(target, b)` FAIL TO COMPILE ("write undefined (cannot
// refer to unexported method)"). That compile failure IS the by-construction seal:
// Execute — which refuses without an allowing PolicyDecision for the exact intent — is
// the only reachable path to a write. The tests below only ever mutate through Execute,
// obtaining the adapter via NewConfigKeyAdapter() exactly as an external caller must.

func fixedNow() time.Time { return time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC) }

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// mkIntent builds a valid repoint ActionIntent for a config-key write. It depends
// only on the exported envelope contract, so the test pins the PUBLIC shape of a
// governed outbound write, not any wrapper internal.
func mkIntent(t *testing.T, workID, capability, path, key string, now time.Time) envelope.ActionIntent {
	t.Helper()
	sum := sha256Hex(strings.Join([]string{workID, capability, path, key}, "|"))
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_" + sum[:12],
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "codex",
		Capability:    capability,
		Action:        "repoint_config_key",
		Target:        envelope.ActionTarget{Kind: "config_key", ID: key, Path: path},
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "repoint " + key,
		RollbackPlan:  "restore the captured pre-image of " + key,
	}
	if err := ai.Validate(); err != nil {
		t.Fatalf("test intent invalid: %v", err)
	}
	return ai
}

// grantConfig returns the built-in policy plus ONE StandingGrant scoped to this
// intent's capability, external_side_effect class, easy reversibility ceiling,
// local blast radius, and exact target path — the D3 "set the envelope" shape.
func grantConfig(ai envelope.ActionIntent) policy.Config {
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID:                   "grant_outbound_config_key",
		Capability:           ai.Capability,
		MutationClass:        "external_side_effect",
		ReversibilityCeiling: "easy",
		BlastRadius:          "local",
		TargetPath:           ai.Target.Path,
	})
	return cfg
}

func scratch(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write scratch %s: %v", name, err)
	}
	return path
}

func newStore(t *testing.T) *ArtifactStore {
	t.Helper()
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	return store
}

func mustRead(t *testing.T, adapter Adapter, target envelope.ActionTarget) string {
	t.Helper()
	v, err := adapter.ReadCurrent(target)
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	return string(v)
}

// Assertion 1: gate by construction — no valid allowing PolicyDecision => error,
// and the scratch config is byte-identical (Write was never called).
func TestExecuteRefusesWithoutPolicyDecision(t *testing.T) {
	now := fixedNow()
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	adapter := NewConfigKeyAdapter()
	ai := mkIntent(t, "work_gate", "outbound.config_key.write", path, "egress.auto/judge", now)
	before, _ := os.ReadFile(path)

	_, err := Execute(newStore(t), adapter, ai, envelope.PolicyDecision{}, []byte("providerB"), now)
	if err == nil {
		t.Fatal("expected refusal when handed no valid PolicyDecision")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatalf("config mutated despite refusal:\nbefore=%q\nafter=%q", before, after)
	}
	if got := mustRead(t, adapter, ai.Target); got != "providerA" {
		t.Fatalf("value changed by a refused write: %q", got)
	}
}

// A decision minted for a DIFFERENT intent must not authorize this one. This test
// fails if the gate ignores action_intent_id (i.e. if depth is wrong).
func TestExecuteRefusesMismatchedDecision(t *testing.T) {
	now := fixedNow()
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	adapter := NewConfigKeyAdapter()
	ai := mkIntent(t, "work_mismatch", "outbound.config_key.write", path, "egress.auto/judge", now)
	other := mkIntent(t, "work_other", "outbound.config_key.write", path, "egress.auto/judge", now)
	otherPD := policy.Evaluate(other, grantConfig(other), now)
	if !policy.Allowed(otherPD) {
		t.Fatalf("precondition: other decision should allow, got %s", otherPD.Outcome)
	}
	if otherPD.ActionIntentID == ai.ID {
		t.Fatal("precondition: the two intents must have distinct ids")
	}
	before, _ := os.ReadFile(path)
	if _, err := Execute(newStore(t), adapter, ai, otherPD, []byte("providerB"), now); err == nil {
		t.Fatal("expected refusal when the decision authorizes a different intent")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("config mutated despite a mismatched decision")
	}
}

// Assertion 2: ungranted external_side_effect => hold => refuse, scratch unchanged.
func TestExecuteRefusesUngranted(t *testing.T) {
	now := fixedNow()
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	adapter := NewConfigKeyAdapter()
	ai := mkIntent(t, "work_hold", "outbound.config_key.write", path, "egress.auto/judge", now)

	pd := policy.Evaluate(ai, policy.DefaultConfig(), now)
	if policy.Allowed(pd) {
		t.Fatalf("precondition: ungranted external_side_effect must hold, got %s", pd.Outcome)
	}
	if pd.Outcome != "hold" {
		t.Fatalf("precondition: want hold, got %s", pd.Outcome)
	}
	before, _ := os.ReadFile(path)
	if _, err := Execute(newStore(t), adapter, ai, pd, []byte("providerB"), now); err == nil {
		t.Fatal("expected refusal on a hold decision")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("config mutated despite a hold decision")
	}
}

func TestExecuteRejectsRevertIntentOnForwardPath(t *testing.T) {
	now := fixedNow()
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	store := newStore(t)
	adapter := NewConfigKeyAdapter()
	ai := mkIntent(t, "work_revert_boundary", "outbound.config_key.write", path, "egress.auto/judge", now)
	cfg := grantConfig(ai)
	pd := policy.Evaluate(ai, cfg, now)
	res, err := Execute(store, adapter, ai, pd, []byte("providerB"), now)
	if err != nil {
		t.Fatalf("forward write failed: %v", err)
	}
	preImage, err := store.Get(res.PreImageRef)
	if err != nil {
		t.Fatal(err)
	}
	revertAt := now.Add(time.Second)
	revert := RevertIntent(ai, res, revertAt)
	revertPD := policy.Evaluate(revert, cfg, revertAt)

	if _, err := Execute(store, adapter, revert, revertPD, preImage, revertAt); err == nil {
		t.Fatal("forward Execute accepted a revert intent instead of requiring ExecuteRevert")
	} else if !strings.Contains(err.Error(), "ExecuteRevert") {
		t.Fatalf("error = %q, want ExecuteRevert redirect", err)
	}
	if got := mustRead(t, adapter, ai.Target); got != "providerB" {
		t.Fatalf("refused forward-path revert mutated target: %q", got)
	}
}

// runGovernedRoundTrip exercises assertions 3 + 4 against one scratch config:
// granted forward write flips A->B and emits a valid outward_delta receipt, then a
// revert built from the pre-image restores A with its own valid outward_delta
// receipt.
func runGovernedRoundTrip(t *testing.T, path, key, from, to string) {
	t.Helper()
	now := fixedNow()
	store := newStore(t)
	adapter := NewConfigKeyAdapter()
	ai := mkIntent(t, "work_"+key, "outbound.config_key.write", path, key, now)
	cfg := grantConfig(ai)

	// Assertion 3: granted => allow_with_obligations => write applies.
	pd := policy.Evaluate(ai, cfg, now)
	if !policy.Allowed(pd) {
		t.Fatalf("granted decision must allow, got %s (%v)", pd.Outcome, pd.Reasons)
	}
	if pd.Outcome != "allow_with_obligations" {
		t.Fatalf("granted non-read write must be allow_with_obligations, got %s", pd.Outcome)
	}
	if len(pd.Obligations) == 0 {
		t.Fatal("allow_with_obligations must carry obligations")
	}
	res, err := Execute(store, adapter, ai, pd, []byte(to), now)
	if err != nil {
		t.Fatalf("granted write failed: %v", err)
	}
	if got := mustRead(t, adapter, ai.Target); got != to {
		t.Fatalf("forward write did not apply: key=%q got %q want %q", key, got, to)
	}
	if res.Receipt.MoveType != "outward_delta" || res.Receipt.Outcome != "completed" {
		t.Fatalf("forward receipt shape wrong: move=%s outcome=%s", res.Receipt.MoveType, res.Receipt.Outcome)
	}
	if !res.Receipt.StateDelta.Changed {
		t.Fatal("forward receipt must set state_delta.changed=true")
	}
	if err := res.Receipt.Validate(); err != nil {
		t.Fatalf("forward receipt invalid: %v", err)
	}
	if err := res.Transition.Validate(); err != nil {
		t.Fatalf("forward transition invalid: %v", err)
	}
	assertProof(t, res, from, to)

	// Assertion 4: revert round-trips from the pre-image artifact.
	later := now.Add(time.Second)
	revert := RevertIntent(ai, res, later)
	if revert.ID == ai.ID {
		t.Fatal("revert must be a distinct intent")
	}
	revertPD := policy.Evaluate(revert, cfg, later)
	if !policy.Allowed(revertPD) {
		t.Fatalf("revert must be governed the same way, got %s", revertPD.Outcome)
	}
	preImage, err := store.Get(res.PreImageRef)
	if err != nil {
		t.Fatalf("load pre-image artifact: %v", err)
	}
	if string(preImage) != from {
		t.Fatalf("pre-image artifact wrong: got %q want %q", preImage, from)
	}
	revRes, err := ExecuteRevert(store, adapter, revert, revertPD, preImage, later)
	if err != nil {
		t.Fatalf("revert write failed: %v", err)
	}
	if got := mustRead(t, adapter, ai.Target); got != from {
		t.Fatalf("revert did not restore: key=%q got %q want %q", key, got, from)
	}
	if revRes.Receipt.MoveType != "outward_delta" {
		t.Fatalf("revert receipt must be outward_delta, got %s", revRes.Receipt.MoveType)
	}
	if err := revRes.Receipt.Validate(); err != nil {
		t.Fatalf("revert receipt invalid: %v", err)
	}
}

func assertProof(t *testing.T, res Result, from, to string) {
	t.Helper()
	if res.PreImageRef == "" || res.PostImageRef == "" {
		t.Fatal("result must carry pre and post image refs")
	}
	if !containsRef(res.Receipt.ProofRefs, res.PreImageRef) || !containsRef(res.Receipt.ProofRefs, res.PostImageRef) {
		t.Fatalf("receipt proof_refs must carry both artifact refs, got %v", res.Receipt.ProofRefs)
	}
	if len(res.Transition.BeforeRefs) != 1 || res.Transition.BeforeRefs[0] != res.PreImageRef {
		t.Fatalf("transition before_refs must be [pre], got %v", res.Transition.BeforeRefs)
	}
	if len(res.Transition.AfterRefs) != 1 || res.Transition.AfterRefs[0] != res.PostImageRef {
		t.Fatalf("transition after_refs must be [post], got %v", res.Transition.AfterRefs)
	}
	if string(res.PreImage) != from {
		t.Fatalf("captured pre-image wrong: got %q want %q", res.PreImage, from)
	}
	if string(res.PostImage) != to {
		t.Fatalf("captured post-image wrong: got %q want %q", res.PostImage, to)
	}
}

func containsRef(refs []string, want string) bool {
	for _, r := range refs {
		if r == want {
			return true
		}
	}
	return false
}

// Assertion 6 (canonical persistence): a governed write does not merely RETURN its receipt and
// transition in memory — it RECORDS them to the home's canonical event store
// (home/events/<id>.json), so a caller's ledger receipt_id/transition_id resolve to real
// envelope files and the event readers (executorlens, eventsource, capgraph, distiller) can see
// the governed action. This fails if Execute leaves the receipt/transition as a dangling
// in-memory reference — the exact P1 the external audit flagged.
func TestExecutePersistsReceiptAndTransitionToEvents(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	adapter := NewConfigKeyAdapter()
	ai := mkIntent(t, "work_persist", "outbound.config_key.write", path, "egress.auto/judge", now)
	pd := policy.Evaluate(ai, grantConfig(ai), now)
	if !policy.Allowed(pd) {
		t.Fatalf("precondition: granted decision must allow, got %s", pd.Outcome)
	}

	res, err := Execute(store, adapter, ai, pd, []byte("providerB"), now)
	if err != nil {
		t.Fatalf("governed write failed: %v", err)
	}

	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind    string
		id      string
		payload any
	}{
		{"receipt", res.Receipt.ID, res.Receipt},
		{"transition", res.Transition.ID, res.Transition},
	} {
		record, err := events.Read(tc.id)
		if err != nil {
			t.Fatalf("%s must resolve through the canonical event store: %v", tc.kind, err)
		}
		data := record.Bytes
		want, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatalf("marshal expected %s: %v", tc.kind, err)
		}
		want = append(want, '\n')
		if !bytes.Equal(data, want) {
			t.Fatalf("recorded %s bytes changed\ngot:  %q\nwant: %q", tc.kind, data, want)
		}
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			t.Fatalf("recorded %s is not valid JSON: %v", tc.kind, err)
		}
		if probe.ID != tc.id {
			t.Fatalf("recorded %s id mismatch: file has %q, result has %q", tc.kind, probe.ID, tc.id)
		}
	}

	// The recorded receipt round-trips through its real validator — a faithful envelope, not a stub.
	recRecord, err := events.Read(res.Receipt.ID)
	if err != nil {
		t.Fatalf("read recorded receipt: %v", err)
	}
	recData := recRecord.Bytes
	var rec envelope.Receipt
	if err := json.Unmarshal(recData, &rec); err != nil {
		t.Fatalf("recorded receipt unmarshal: %v", err)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("recorded receipt invalid: %v", err)
	}
}

// Assertion 3+4, first consumer: switchback-shaped egress alias, string value.
func TestGovernedWriteSwitchbackShape(t *testing.T) {
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	runGovernedRoundTrip(t, path, "egress.auto/judge", "providerA", "providerB")
}

// Assertion 5, second consumer: headroom-shaped config key, SAME envelope, only
// Target.Path/Target.ID differ; the value flips true->false and back.
func TestGovernedWriteHeadroomShape(t *testing.T) {
	path := scratch(t, "headroom.yaml", "memory:\n  inject: true\n")
	runGovernedRoundTrip(t, path, "memory.inject", "true", "false")
}

// wedgeEventStore makes the home's canonical event store (home/events) unwritable by planting a
// regular FILE where the events directory must be — so MkdirAll (and thus any envelope record or
// the preflight probe) fails. It simulates the persistence failure that partial-failure hardening
// must never let escape as an unreceipted mutation.
func wedgeEventStore(t *testing.T, home string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "events"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("wedge event store: %v", err)
	}
}

// F1 (preflight abort): when the canonical event store is unwritable, Execute must fail BEFORE it
// mutates the target — the target stays byte-identical. This converts the common post-write
// persistence failure into a pre-write abort, so no unreceipted mutation is reachable.
func TestExecutePreflightAbortsWhenEventStoreUnwritable(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	wedgeEventStore(t, home)
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	adapter := NewConfigKeyAdapter()
	ai := mkIntent(t, "work_preflight", "outbound.config_key.write", path, "egress.auto/judge", now)
	pd := policy.Evaluate(ai, grantConfig(ai), now)
	if !policy.Allowed(pd) {
		t.Fatalf("precondition: granted decision must allow, got %s", pd.Outcome)
	}
	before, _ := os.ReadFile(path)

	if _, err := Execute(store, adapter, ai, pd, []byte("providerB"), now); err == nil {
		t.Fatal("expected a preflight abort when the event store is unwritable")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatalf("target mutated despite a preflight abort:\nbefore=%q\nafter=%q", before, after)
	}
	if got := mustRead(t, adapter, ai.Target); got != "providerA" {
		t.Fatalf("value changed by an aborted write: %q", got)
	}
}

// F2 (post-write rollback): a persistence failure AFTER a successful write must roll the target
// back to the captured pre-image value, name the rollback + pre-image ref in the returned error,
// and leave NO dangling receipt/transition under events/. recordEnvelopeFn is swapped to force the
// persistence failure the real filesystem will not produce once preflight passes.
func TestExecuteRollsBackWhenRecordFails(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	adapter := NewConfigKeyAdapter()
	ai := mkIntent(t, "work_rollback", "outbound.config_key.write", path, "egress.auto/judge", now)
	pd := policy.Evaluate(ai, grantConfig(ai), now)
	if !policy.Allowed(pd) {
		t.Fatalf("precondition: granted decision must allow, got %s", pd.Outcome)
	}

	orig := recordEnvelopeFn
	recordEnvelopeFn = func(string, string, any) error {
		return fmt.Errorf("simulated event-store persistence failure")
	}
	defer func() { recordEnvelopeFn = orig }()

	_, err = Execute(store, adapter, ai, pd, []byte("providerB"), now)
	if err == nil {
		t.Fatal("expected an error when record persistence fails after the write")
	}
	// The target VALUE is restored byte-exact to the captured pre-image ("providerA").
	if got := mustRead(t, adapter, ai.Target); got != "providerA" {
		t.Fatalf("target not rolled back to the pre-image value: got %q want %q", got, "providerA")
	}
	// The pre-image ref (content address of "providerA") must be named in the error, alongside the
	// fact that a rollback happened.
	preRef, perr := store.Put([]byte("providerA"))
	if perr != nil {
		t.Fatalf("compute pre-image ref: %v", perr)
	}
	if !strings.Contains(err.Error(), "roll") {
		t.Fatalf("error must mention the rollback, got: %v", err)
	}
	if !strings.Contains(err.Error(), preRef) {
		t.Fatalf("error must name the pre-image ref %q, got: %v", preRef, err)
	}
	// No dangling receipt/transition envelope file was left under events/.
	entries, _ := os.ReadDir(filepath.Join(home, "events"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("dangling event file after rollback: %s", e.Name())
		}
	}
}

// failingRollbackAdapter succeeds on the forward write but FAILS on the second write (the
// rollback), reaching the F3 recovery-finding path. ReadCurrent returns a stable value so pre/post
// capture and receipt/transition validation succeed up to the (swapped) persistence step.
type failingRollbackAdapter struct{ writes int }

func (a *failingRollbackAdapter) ReadCurrent(envelope.ActionTarget) ([]byte, error) {
	return []byte("current-value"), nil
}

func (a *failingRollbackAdapter) write(envelope.ActionTarget, []byte) error {
	a.writes++
	if a.writes >= 2 {
		return fmt.Errorf("simulated rollback write failure")
	}
	return nil
}

// F3 (durable marker): when persistence fails AND rollback also fails, Execute records a durable
// act-recovery finding (pre-image ref in EvidenceRefs, ProducedBy internal/act) and returns an
// error naming that finding id — the unreceipted-and-unreverted mutation is never swallowed.
func TestExecuteRecordsRecoveryFindingWhenRollbackFails(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("new artifact store: %v", err)
	}
	path := scratch(t, "switchback.yaml", "egress:\n  auto/judge: providerA\n")
	adapter := &failingRollbackAdapter{}
	ai := mkIntent(t, "work_recovery", "outbound.config_key.write", path, "egress.auto/judge", now)
	pd := policy.Evaluate(ai, grantConfig(ai), now)
	if !policy.Allowed(pd) {
		t.Fatalf("precondition: granted decision must allow, got %s", pd.Outcome)
	}

	orig := recordEnvelopeFn
	recordEnvelopeFn = func(string, string, any) error {
		return fmt.Errorf("simulated event-store persistence failure")
	}
	defer func() { recordEnvelopeFn = orig }()

	_, err = Execute(store, adapter, ai, pd, []byte("providerB"), now)
	if err == nil {
		t.Fatal("expected an error when both persistence and rollback fail")
	}

	preRef, perr := store.Put([]byte("current-value"))
	if perr != nil {
		t.Fatalf("compute pre-image ref: %v", perr)
	}
	findings, ferr := finding.List(home, "act-recovery")
	if ferr != nil {
		t.Fatalf("list findings: %v", ferr)
	}
	if len(findings) != 1 {
		t.Fatalf("want exactly one act-recovery finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if !containsRef(f.EvidenceRefs, preRef) {
		t.Fatalf("recovery finding must carry the pre-image ref %q in EvidenceRefs, got %v", preRef, f.EvidenceRefs)
	}
	if f.ProducedBy != "internal/act" {
		t.Fatalf("recovery finding ProducedBy must be internal/act, got %q", f.ProducedBy)
	}
	if !strings.Contains(err.Error(), f.ID) {
		t.Fatalf("error must name the recovery finding id %q, got: %v", f.ID, err)
	}
}

func TestRecordEnvelopeRequiresPostWriteStoreRead(t *testing.T) {
	proofErr := errors.New("injected Store.Read persistence proof failure")
	store := &persistenceProofStore{readErr: proofErr}
	if err := recordEnvelopeWithStore(store, "rec_proof", map[string]string{"id": "rec_proof"}); !errors.Is(err, proofErr) {
		t.Fatalf("recordEnvelopeWithStore error = %v, want persistence proof error", err)
	}
	if store.puts != 1 || store.reads != 1 {
		t.Fatalf("put/read calls = %d/%d, want 1/1", store.puts, store.reads)
	}
}

type persistenceProofStore struct {
	puts    int
	reads   int
	readErr error
}

func (s *persistenceProofStore) PutJSON(id string, value any, indent string) (eventstore.PutResult, error) {
	s.puts++
	ref, err := eventstore.ReferenceForID(id)
	return eventstore.PutResult{Reference: ref, Created: true}, err
}

func (s *persistenceProofStore) Read(id string) (eventstore.Record, error) {
	s.reads++
	return eventstore.Record{}, s.readErr
}
