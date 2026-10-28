package act

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/finding"
	"compound/internal/policy"
)

// These tests drive the TWO governed remote-outbound adapters (a DNS host->target
// record on a router, an NPM vhost block on an apps host) through the SAME Execute
// gate the config-key repoint uses. Every op runs through a fakeRemoteRunner that
// records the command sequence and simulates a remote file IN MEMORY: no test ever
// opens a real ssh/network connection (that would be a defect), and nothing touches a
// real operator home — only t.TempDir() artifact/event stores.

// fakeRemoteRunner is the injected transport. It records every RemoteOp (so a test can
// assert the exact command sequence) and simulates one remote file per path in memory.
// Failure knobs let a test force the post-write / rollback hazard paths deterministically.
type fakeRemoteRunner struct {
	files map[string][]byte
	ops   []RemoteOp

	applied            bool // set once an apply op has been seen
	failReadAfterApply bool // read ops after an apply fail (post-image read hazard)
	failApply          bool // apply op fails (pre-mutation write hazard)
	failRestore        bool // restore op fails (rollback hazard -> recovery finding)
}

func newFakeRunner() *fakeRemoteRunner { return &fakeRemoteRunner{files: map[string][]byte{}} }

func (f *fakeRemoteRunner) Run(_ RemoteEndpoint, op RemoteOp) ([]byte, error) {
	f.ops = append(f.ops, op)
	switch op.Kind {
	case RemoteOpRead:
		if f.applied && f.failReadAfterApply {
			return nil, fmt.Errorf("fake: forced post-write read failure")
		}
		return append([]byte(nil), f.files[op.Path]...), nil
	case RemoteOpApply:
		if f.failApply {
			return nil, fmt.Errorf("fake: forced apply failure")
		}
		f.applied = true
		f.files[op.Path] = append(f.files[op.Path], op.Payload...)
		return nil, nil
	case RemoteOpRestore:
		if f.failRestore {
			return nil, fmt.Errorf("fake: forced restore failure")
		}
		f.files[op.Path] = append([]byte(nil), op.Payload...)
		return nil, nil
	default:
		return nil, fmt.Errorf("fake: unknown op kind %q", op.Kind)
	}
}

func (f *fakeRemoteRunner) opKinds() []string {
	out := make([]string, 0, len(f.ops))
	for _, o := range f.ops {
		out = append(out, o.Kind)
	}
	return out
}

func (f *fakeRemoteRunner) countKind(kind string) int {
	n := 0
	for _, o := range f.ops {
		if o.Kind == kind {
			n++
		}
	}
	return n
}

func testEndpoint() RemoteEndpoint {
	return RemoteEndpoint{User: "root", Host: "example-router.invalid", Port: 22}
}

// mkRemoteIntent builds a valid external_side_effect / easy / local ActionIntent for a
// remote additive write. It depends only on the exported envelope contract.
func mkRemoteIntent(t *testing.T, workID, capability, kind, path, id string) envelope.ActionIntent {
	t.Helper()
	ai := envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_" + sha256Hex(strings.Join([]string{workID, capability, path, id}, "|"))[:12],
			CreatedAt:     "2026-07-22T09:00:00Z",
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "operator",
		Capability:    capability,
		Action:        "create_" + kind,
		Target:        envelope.ActionTarget{Kind: kind, ID: id, Path: path},
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       "add " + kind + " " + id,
		RollbackPlan:  "remove the added " + kind + " " + id + " from " + path + " (restore captured pre-image)",
	}
	if err := ai.Validate(); err != nil {
		t.Fatalf("remote intent invalid: %v", err)
	}
	return ai
}

func remoteGrantConfig(ai envelope.ActionIntent) policy.Config {
	cfg := policy.DefaultConfig()
	cfg.StandingGrants = append(cfg.StandingGrants, policy.StandingGrant{
		ID:                   "grant_" + ai.Capability,
		Capability:           ai.Capability,
		MutationClass:        "external_side_effect",
		ReversibilityCeiling: "easy",
		BlastRadius:          "local",
		TargetPath:           ai.Target.Path,
	})
	return cfg
}

// F2-core: a granted DNS-record act rides the SAME Execute gate: it reads the pre-image,
// refuses to open a real connection (fake runner), appends exactly one additive record,
// re-reads the post-image, and records a resolvable receipt + transition with pre/post refs.
func TestDNSRecordGovernedAppend(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	runner := newFakeRunner()
	path := "/etc/dnsmasq.d/compound.conf"
	adapter := NewDNSRecordAdapter(runner, testEndpoint(), path, "")
	ai := mkRemoteIntent(t, "work_dns", "outbound.dns_record.write", "dns_record", path, "example-host.invalid")
	pd := policy.Evaluate(ai, remoteGrantConfig(ai), now)
	if !policy.Allowed(pd) {
		t.Fatalf("granted decision must allow, got %s", pd.Outcome)
	}

	res, err := Execute(store, adapter, ai, pd, DNSRecordForwardValue("example-host.invalid", "test-address.invalid"), now)
	if err != nil {
		t.Fatalf("governed dns write failed: %v", err)
	}

	// The additive record landed in the simulated remote file, exactly once.
	got := string(runner.files[path])
	if !strings.Contains(got, "address=/example-host.invalid/test-address.invalid") {
		t.Fatalf("dns record not appended: %q", got)
	}
	if runner.countKind(RemoteOpApply) != 1 {
		t.Fatalf("want exactly one apply op, got %d (%v)", runner.countKind(RemoteOpApply), runner.opKinds())
	}
	// The command sequence is read (pre-image) ... apply ... read (post-image); no restore.
	kinds := runner.opKinds()
	firstApply := indexOf(kinds, RemoteOpApply)
	if firstApply < 0 || indexOf(kinds[:firstApply], RemoteOpRead) < 0 {
		t.Fatalf("expected a pre-image read before the apply, got %v", kinds)
	}
	if indexOf(kinds[firstApply+1:], RemoteOpRead) < 0 {
		t.Fatalf("expected a post-image read after the apply, got %v", kinds)
	}
	for _, k := range kinds {
		if k == RemoteOpRestore {
			t.Fatalf("no restore should run on the success path, got %v", kinds)
		}
	}
	// The apply op carries the exact remote command (legible in receipts / audit).
	applyCmd := runner.ops[firstApply].Command
	if !strings.Contains(applyCmd, path) || !strings.Contains(applyCmd, "address=/example-host.invalid/test-address.invalid") {
		t.Fatalf("apply command missing path or record: %q", applyCmd)
	}

	// Receipt + transition persisted to the canonical event store with pre/post refs.
	if res.PreImageRef == "" || res.PostImageRef == "" {
		t.Fatalf("missing pre/post refs: %+v", res)
	}
	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{res.Receipt.ID, res.Transition.ID} {
		if _, err := events.Read(id); err != nil {
			t.Fatalf("event %s must resolve in the canonical store: %v", id, err)
		}
	}
	if !containsRef(res.Receipt.ProofRefs, res.PreImageRef) || !containsRef(res.Receipt.ProofRefs, res.PostImageRef) {
		t.Fatalf("receipt proof_refs missing pre/post: %v", res.Receipt.ProofRefs)
	}
}

// F5-core: the SAME governance seal on the vhost grammar — only the rendered block and
// reload command differ, proving remote-outbound generality at zero governance cost.
func TestVhostGovernedAppend(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	runner := newFakeRunner()
	path := "/data/nginx/custom/compound.conf"
	adapter := NewVhostAdapter(runner, RemoteEndpoint{User: "root", Host: "example-apps.invalid", Port: 22}, path, "")
	ai := mkRemoteIntent(t, "work_vhost", "outbound.vhost.write", "vhost", path, "example-app.invalid")
	pd := policy.Evaluate(ai, remoteGrantConfig(ai), now)
	if !policy.Allowed(pd) {
		t.Fatalf("granted decision must allow, got %s", pd.Outcome)
	}

	res, err := Execute(store, adapter, ai, pd, VhostForwardValue("example-app.invalid", "https://upstream.invalid:8080"), now)
	if err != nil {
		t.Fatalf("governed vhost write failed: %v", err)
	}
	got := string(runner.files[path])
	if !strings.Contains(got, "server_name example-app.invalid;") || !strings.Contains(got, "https://upstream.invalid:8080") {
		t.Fatalf("vhost block not appended: %q", got)
	}
	if runner.countKind(RemoteOpApply) != 1 {
		t.Fatalf("want exactly one apply op, got %d", runner.countKind(RemoteOpApply))
	}
	events, err := eventstore.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.Read(res.Receipt.ID); err != nil {
		t.Fatalf("vhost receipt must resolve: %v", err)
	}
}

// F3: an intent with an empty RollbackPlan is refused by the existing gate BEFORE the
// adapter is ever touched — the fake runner records ZERO ops (target byte-identical).
func TestRemoteAdapterEmptyRollbackPlanRefused(t *testing.T) {
	now := fixedNow()
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	runner := newFakeRunner()
	path := "/etc/dnsmasq.d/compound.conf"
	adapter := NewDNSRecordAdapter(runner, testEndpoint(), path, "")
	ai := mkRemoteIntent(t, "work_norollback", "outbound.dns_record.write", "dns_record", path, "example-host.invalid")
	ai.RollbackPlan = "" // strip the plan the gate requires
	pd := policy.Evaluate(ai, remoteGrantConfig(ai), now)

	if _, err := Execute(store, adapter, ai, pd, DNSRecordForwardValue("example-host.invalid", "test-address.invalid"), now); err == nil {
		t.Fatal("expected the gate to refuse an empty rollback_plan")
	}
	if len(runner.ops) != 0 {
		t.Fatalf("no client op may run when the gate refuses; got %v", runner.opKinds())
	}
}

// Idempotency (OPEN decision 3 = refuse-as-conflict): when the record already exists
// remotely, the additive-only adapter refuses BEFORE any apply — no silent no-op that
// would mask drift, and no dishonest changed=true receipt.
func TestRemoteAdapterAlreadyExistsConflict(t *testing.T) {
	now := fixedNow()
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	runner := newFakeRunner()
	path := "/etc/dnsmasq.d/compound.conf"
	// The record for this host is already present on the remote.
	runner.files[path] = []byte("address=/example-host.invalid/legacy-address.invalid\n")
	adapter := NewDNSRecordAdapter(runner, testEndpoint(), path, "")
	ai := mkRemoteIntent(t, "work_conflict", "outbound.dns_record.write", "dns_record", path, "example-host.invalid")
	pd := policy.Evaluate(ai, remoteGrantConfig(ai), now)

	_, err = Execute(store, adapter, ai, pd, DNSRecordForwardValue("example-host.invalid", "test-address.invalid"), now)
	if err == nil {
		t.Fatal("expected a conflict refusal when the record already exists")
	}
	if !strings.Contains(err.Error(), "already") {
		t.Fatalf("error should name the already-present conflict, got: %v", err)
	}
	if runner.countKind(RemoteOpApply) != 0 {
		t.Fatalf("no apply may run on a conflict; got %v", runner.opKinds())
	}
	// The remote file is byte-identical to before.
	if string(runner.files[path]) != "address=/example-host.invalid/legacy-address.invalid\n" {
		t.Fatalf("remote file mutated on a refused conflict: %q", runner.files[path])
	}
}

// F6: a post-write failure (fake fails the post-image read) triggers best-effort rollback;
// when the rollback ALSO fails, Execute records a durable act-recovery finding (pre-image
// ref in EvidenceRefs, ProducedBy internal/act) and names it in the error — the
// unreceipted-and-unreverted mutation is never swallowed. Mirrors act_test.go:509.
func TestRemoteAdapterRollbackFindingOnWriteFailure(t *testing.T) {
	now := fixedNow()
	home := t.TempDir()
	store, err := NewArtifactStore(home)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	runner := newFakeRunner()
	runner.failReadAfterApply = true // the post-image read fails after a successful apply
	runner.failRestore = true        // ...and the rollback restore fails too
	path := "/etc/dnsmasq.d/compound.conf"
	adapter := NewDNSRecordAdapter(runner, testEndpoint(), path, "")
	ai := mkRemoteIntent(t, "work_recovery", "outbound.dns_record.write", "dns_record", path, "example-host.invalid")
	pd := policy.Evaluate(ai, remoteGrantConfig(ai), now)

	_, err = Execute(store, adapter, ai, pd, DNSRecordForwardValue("example-host.invalid", "test-address.invalid"), now)
	if err == nil {
		t.Fatal("expected an error when both the post-write read and the rollback fail")
	}
	findings, ferr := finding.List(home, "act-recovery")
	if ferr != nil {
		t.Fatalf("list findings: %v", ferr)
	}
	if len(findings) != 1 {
		t.Fatalf("want exactly one act-recovery finding, got %d", len(findings))
	}
	f := findings[0]
	if f.ProducedBy != "internal/act" {
		t.Fatalf("recovery finding ProducedBy must be internal/act, got %q", f.ProducedBy)
	}
	if !strings.Contains(err.Error(), f.ID) {
		t.Fatalf("error must name the recovery finding id %q, got: %v", f.ID, err)
	}
}

// A governed revert restores the exact pre-image bytes over the same target: Execute a
// forward append, then invoke ExecuteRevert's distinct restore path with the captured
// pre-image and prove the remote file returns to its prior content.
func TestRemoteAdapterRevertRestoresPreImage(t *testing.T) {
	now := fixedNow()
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	runner := newFakeRunner()
	path := "/etc/dnsmasq.d/compound.conf"
	runner.files[path] = []byte("address=/example-existing.invalid/legacy-address.invalid\n")
	before := append([]byte(nil), runner.files[path]...)
	adapter := NewDNSRecordAdapter(runner, testEndpoint(), path, "")
	ai := mkRemoteIntent(t, "work_revert", "outbound.dns_record.write", "dns_record", path, "example-host.invalid")
	pd := policy.Evaluate(ai, remoteGrantConfig(ai), now)

	res, err := Execute(store, adapter, ai, pd, DNSRecordForwardValue("example-host.invalid", "test-address.invalid"), now)
	if err != nil {
		t.Fatalf("forward write failed: %v", err)
	}
	if bytes.Equal(runner.files[path], before) {
		t.Fatal("forward write did not change the remote file")
	}

	// ExecuteRevert restores the captured pre-image bytes through the same governed adapter.
	pre, err := store.Get(res.PreImageRef)
	if err != nil {
		t.Fatalf("get pre-image: %v", err)
	}
	revert := RevertIntent(ai, res, now)
	revPD := policy.Evaluate(revert, remoteGrantConfig(revert), now)
	if _, err := ExecuteRevert(store, adapter, revert, revPD, pre, now); err != nil {
		t.Fatalf("governed revert failed: %v", err)
	}
	if !bytes.Equal(runner.files[path], before) {
		t.Fatalf("revert did not restore the pre-image:\nwant %q\ngot  %q", before, runner.files[path])
	}
}

// TestRemoteAdapterRestoreDelimiterNeverCollidesWithContent is the E6
// falsifier: restore() historically built its heredoc with a STATIC
// delimiter ("COMPOUND_RESTORE_EOF") over unsanitized remote file content. A
// captured pre-image containing that literal line — attacker-controlled or
// merely unlucky drift on the remote host — closes the heredoc early and
// hands everything after it to the remote shell as commands: a command
// injection surface in the governed DNS/vhost rollback path (landed
// cbe7ca28). This proves the terminator restore() actually emits can never
// collide with a line inside the content it wraps, whatever that terminator
// is — it does not hardcode the historical delimiter value, so it keeps
// falsifying after the fix randomizes it per invocation.
func TestRemoteAdapterRestoreDelimiterNeverCollidesWithContent(t *testing.T) {
	now := fixedNow()
	store, err := NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	runner := newFakeRunner()
	path := "/etc/dnsmasq.d/compound.conf"
	adapter := NewDNSRecordAdapter(runner, testEndpoint(), path, "")
	ai := mkRemoteIntent(t, "work_injection", "outbound.dns_record.write", "dns_record", path, "example-host.invalid")
	pd := policy.Evaluate(ai, remoteGrantConfig(ai), now)
	if !policy.Allowed(pd) {
		t.Fatalf("granted decision must allow, got %s", pd.Outcome)
	}

	// A captured pre-image whose content contains, as its own line, the exact
	// literal string restore() historically used as its heredoc terminator,
	// followed by an injected command it would hand to the remote shell.
	malicious := []byte("address=/existing.invalid/legacy-address.invalid\n" +
		"COMPOUND_RESTORE_EOF\n" +
		"rm -rf / #injected-by-preimage\n")

	revert := RevertIntent(ai, Result{}, now)
	revertPD := policy.Evaluate(revert, remoteGrantConfig(revert), now)
	if _, err := ExecuteRevert(store, adapter, revert, revertPD, encodeRemotePreImage(malicious), now); err != nil {
		t.Fatalf("governed restore failed: %v", err)
	}

	restoreIdx := -1
	for i, op := range runner.ops {
		if op.Kind == RemoteOpRestore {
			restoreIdx = i
		}
	}
	if restoreIdx < 0 {
		t.Fatalf("expected a restore op, got %v", runner.opKinds())
	}
	cmd := runner.ops[restoreIdx].Command
	delim := heredocTerminator(t, cmd)
	for _, line := range strings.Split(string(malicious), "\n") {
		if line == delim {
			t.Fatalf("heredoc delimiter %q collides with a content line — command injection surface: %s", delim, cmd)
		}
	}
}

// heredocTerminator extracts the delimiter word restore() actually chose from
// a `cat > 'path' <<'DELIM'\n...` command, so the test asserts a general
// safety property (whatever delimiter is chosen never collides with content)
// instead of asserting against a hardcoded value.
func heredocTerminator(t *testing.T, cmd string) string {
	t.Helper()
	const marker = "<<'"
	idx := strings.Index(cmd, marker)
	if idx < 0 {
		t.Fatalf("command has no heredoc operator: %q", cmd)
	}
	rest := cmd[idx+len(marker):]
	end := strings.IndexByte(rest, '\'')
	if end < 0 {
		t.Fatalf("command has an unterminated heredoc delimiter: %q", cmd)
	}
	return rest[:end]
}

func indexOf(hay []string, needle string) int {
	for i, v := range hay {
		if v == needle {
			return i
		}
	}
	return -1
}
