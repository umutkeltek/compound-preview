package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/act"
	"compound/internal/mem"
	"compound/internal/policy"
)

// These tests drive the `compound act dns-record` / `compound act vhost` verbs — the
// operator door onto the governed remote-outbound seam. They prove the verb's own
// responsibilities: hold-by-default without a grant, preflight refusal when the target
// service is unresolved, additive-only refusal of modify/delete, and a granted governed
// write that records a resolvable receipt. Every remote op runs through a fake runner
// (no real ssh/network), and nothing touches a real home — only t.TempDir().

type cliFakeRunner struct {
	files map[string][]byte
	ops   []act.RemoteOp
}

func newCLIFakeRunner() *cliFakeRunner { return &cliFakeRunner{files: map[string][]byte{}} }

func (f *cliFakeRunner) Run(_ act.RemoteEndpoint, op act.RemoteOp) ([]byte, error) {
	f.ops = append(f.ops, op)
	switch op.Kind {
	case act.RemoteOpRead:
		return append([]byte(nil), f.files[op.Path]...), nil
	case act.RemoteOpApply:
		f.files[op.Path] = append(f.files[op.Path], op.Payload...)
		return nil, nil
	case act.RemoteOpRestore:
		f.files[op.Path] = append([]byte(nil), op.Payload...)
		return nil, nil
	default:
		return nil, nil
	}
}

// withFakeRunner installs a fake transport for the duration of a test and restores the
// prior (nil -> real ssh) runner after.
func withFakeRunner(t *testing.T, f act.RemoteRunner) {
	t.Helper()
	prev := actRemoteRunner
	actRemoteRunner = f
	t.Cleanup(func() { actRemoteRunner = prev })
}

// registerInfraService registers one service:<bare> entity so the verb's preflight
// resolves it. It uses a temp home only.
func registerInfraService(t *testing.T, home, bare string) {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatalf("open mem: %v", err)
	}
	if _, err := store.RegisterEntity(mem.Entity{
		Slug:        "service:" + bare,
		Kind:        "server",
		Aliases:     []string{bare},
		Identifiers: []string{bare + ".internal"},
		Visibility:  "operational",
	}, time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("register service entity: %v", err)
	}
}

func importRemoteGrant(t *testing.T, home, capability, targetPath string) {
	t.Helper()
	grantSrc := filepath.Join(home, "grants.yaml")
	raw := "generated_at: \"2026-07-01T12:00:00Z\"\n" +
		"authority:\n" +
		"  decision_ref: \"grant:test-remote-act\"\n" +
		"  reviewed_by: \"operator:test\"\n" +
		"grants:\n" +
		"  - grant_id: grant_remote_act\n" +
		"    capabilities: [\"" + capability + "\"]\n" +
		"    target_scope: \"" + targetPath + "\"\n" +
		"    mutation_class: external_side_effect\n" +
		"    reversibility_ceiling: reversible\n" +
		"    blast_radius: local\n"
	if err := os.WriteFile(grantSrc, []byte(raw), 0o644); err != nil {
		t.Fatalf("write grant source: %v", err)
	}
	if _, err := policy.ImportStandingGrants(home, grantSrc, time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}
}

// F1: --confirm WITHOUT a grant holds by default — the verb refuses and NO remote command
// runs (the fake runner records zero ops).
func TestActDNSRecordHoldsByDefault(t *testing.T) {
	home := t.TempDir()
	registerInfraService(t, home, "example-router")
	fake := newCLIFakeRunner()
	withFakeRunner(t, fake)

	var out, errOut bytes.Buffer
	code := runActDNSRecord([]string{
		"--service", "example-router", "--host", "example-host.internal", "--address", "192.0.2.10",
		"--remote-host", "example-router.internal", "--remote-path", "/etc/dnsmasq.d/compound.conf",
		"--work", "work_nogrant", "--confirm", "--home", home,
	}, &out, &errOut)
	if code == 0 {
		t.Fatalf("expected refusal without a grant; stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "refused") {
		t.Fatalf("expected a refusal message, got stderr=%q", errOut.String())
	}
	if len(fake.ops) != 0 {
		t.Fatalf("no remote command may run when held by default; got %d ops", len(fake.ops))
	}
}

// F2 (CLI): with an imported grant scoped to the capability/class/reversibility/target,
// --confirm performs the governed write, the additive record lands on the (fake) remote,
// and the receipt id resolves in the canonical event store.
func TestActDNSRecordWritesWithGrant(t *testing.T) {
	home := t.TempDir()
	registerInfraService(t, home, "example-router")
	path := "/etc/dnsmasq.d/compound.conf"
	importRemoteGrant(t, home, "outbound.dns_record.write", path)
	fake := newCLIFakeRunner()
	withFakeRunner(t, fake)

	var out, errOut bytes.Buffer
	code := runActDNSRecord([]string{
		"--service", "example-router", "--host", "example-host.internal", "--address", "192.0.2.10",
		"--remote-host", "example-router.internal", "--remote-path", path,
		"--work", "work_grant", "--confirm", "--home", home, "--json",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("granted write should succeed, code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(string(fake.files[path]), "address=/example-host.internal/192.0.2.10") {
		t.Fatalf("granted write did not append the record: %q", fake.files[path])
	}
	var result struct {
		ReceiptID string `json:"receipt_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("parse output json: %v (%q)", err, out.String())
	}
	if result.ReceiptID == "" || !testEventExists(t, home, result.ReceiptID) {
		t.Fatalf("receipt %q must resolve in the canonical event store", result.ReceiptID)
	}
}

// F4: an unresolved target service is refused at preflight, BEFORE any intent is minted —
// the fake runner records zero ops.
func TestActDNSRecordUnresolvedServiceRefused(t *testing.T) {
	home := t.TempDir()
	registerInfraService(t, home, "example-router") // a DIFFERENT service exists
	fake := newCLIFakeRunner()
	withFakeRunner(t, fake)

	var out, errOut bytes.Buffer
	code := runActDNSRecord([]string{
		"--service", "ghost-service", "--host", "example-host.internal", "--address", "192.0.2.10",
		"--remote-host", "example-router.internal", "--remote-path", "/etc/dnsmasq.d/compound.conf",
		"--work", "work_unresolved", "--confirm", "--home", home,
	}, &out, &errOut)
	if code == 0 {
		t.Fatalf("expected preflight refusal for an unresolved service; stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "service") {
		t.Fatalf("refusal should name the unresolved service, got stderr=%q", errOut.String())
	}
	if len(fake.ops) != 0 {
		t.Fatalf("preflight must refuse before any remote op; got %d ops", len(fake.ops))
	}
}

// F5 (CLI): the SAME governance floor on the vhost verb — a granted --confirm appends the
// vhost block on the (fake) apps host and records a resolvable receipt.
func TestActVhostWritesWithGrant(t *testing.T) {
	home := t.TempDir()
	registerInfraService(t, home, "example-apps")
	path := "/data/nginx/custom/compound.conf"
	importRemoteGrant(t, home, "outbound.vhost.write", path)
	fake := newCLIFakeRunner()
	withFakeRunner(t, fake)

	var out, errOut bytes.Buffer
	code := runActVhost([]string{
		"--service", "example-apps", "--server-name", "example-app.internal", "--upstream", "http://192.0.2.20:8080",
		"--remote-host", "example-apps.internal", "--remote-path", path,
		"--work", "work_vhost", "--confirm", "--home", home, "--json",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("granted vhost write should succeed, code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(string(fake.files[path]), "server_name example-app.internal;") {
		t.Fatalf("granted vhost write did not append the block: %q", fake.files[path])
	}
}

// F7: a modify/delete request is refused with the additive-only error, before any remote
// op — this slice only creates new entries.
func TestActRemoteModifyDeleteRefused(t *testing.T) {
	home := t.TempDir()
	registerInfraService(t, home, "example-router")
	fake := newCLIFakeRunner()
	withFakeRunner(t, fake)

	var out, errOut bytes.Buffer
	code := runActDNSRecord([]string{
		"--service", "example-router", "--host", "example-host.internal", "--address", "192.0.2.10",
		"--remote-host", "example-router.internal", "--remote-path", "/etc/dnsmasq.d/compound.conf",
		"--work", "work_delete", "--confirm", "--home", home, "--delete",
	}, &out, &errOut)
	if code == 0 {
		t.Fatalf("expected additive-only refusal; stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "additive-only") {
		t.Fatalf("refusal should name the additive-only rule, got stderr=%q", errOut.String())
	}
	if len(fake.ops) != 0 {
		t.Fatalf("no remote op may run on a refused modify/delete; got %d ops", len(fake.ops))
	}
}

// Dry-run default (no --confirm) writes nothing and makes no remote call.
func TestActDNSRecordDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	registerInfraService(t, home, "example-router")
	fake := newCLIFakeRunner()
	withFakeRunner(t, fake)

	var out, errOut bytes.Buffer
	code := runActDNSRecord([]string{
		"--service", "example-router", "--host", "example-host.internal", "--address", "192.0.2.10",
		"--remote-host", "example-router.internal", "--remote-path", "/etc/dnsmasq.d/compound.conf",
		"--home", home,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("dry-run should succeed, code=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Fatalf("dry-run output should say so, got %q", out.String())
	}
	if len(fake.ops) != 0 {
		t.Fatalf("dry-run must make no remote call; got %d ops", len(fake.ops))
	}
}

// A remote-outbound refusal must teach: the unresolved-service preflight and the
// no-standing-grant policy gate both terminate the verb, and an operator who reads only the
// refusal must be able to recover without guessing which resolver or which grant failed.
//
// RED WITHOUT the conversion: both sites printed a single `refused: <wrapped error>` line, so
// assertTeachingRefusal fails at "refusal does not name gate" before it reaches the command
// checks — there was no gate name, no tried-trace, and no suggested command at all.
func TestActRemoteRefusalsTeach(t *testing.T) {
	t.Run("unresolved service names the resolver and the register path", func(t *testing.T) {
		home := t.TempDir()
		registerInfraService(t, home, "example-router") // a DIFFERENT service exists
		fake := newCLIFakeRunner()
		withFakeRunner(t, fake)

		var out, errOut bytes.Buffer
		code := runActDNSRecord([]string{
			"--service", "ghost-service", "--host", "example-host.internal", "--address", "192.0.2.10",
			"--remote-host", "example-router.internal", "--remote-path", "/etc/dnsmasq.d/compound.conf",
			"--work", "work_unresolved", "--confirm", "--home", home,
		}, &out, &errOut)
		if code == 0 {
			t.Fatalf("expected preflight refusal for an unresolved service; stdout=%q", out.String())
		}
		assertTeachingRefusal(t, errOut.String(), "service_entity_resolution",
			"ghost-service",
			"compound memory entity resolve ghost-service",
		)
		if len(fake.ops) != 0 {
			t.Fatalf("preflight must refuse before any remote op; got %d ops", len(fake.ops))
		}
	})

	t.Run("no standing grant names the outcome and the grant commands", func(t *testing.T) {
		home := t.TempDir()
		registerInfraService(t, home, "example-router")
		fake := newCLIFakeRunner()
		withFakeRunner(t, fake)

		var out, errOut bytes.Buffer
		code := runActDNSRecord([]string{
			"--service", "example-router", "--host", "example-host.internal", "--address", "192.0.2.10",
			"--remote-host", "example-router.internal", "--remote-path", "/etc/dnsmasq.d/compound.conf",
			"--work", "work_nogrant", "--confirm", "--home", home,
		}, &out, &errOut)
		if code == 0 {
			t.Fatalf("expected refusal without a grant; stdout=%q", out.String())
		}
		assertTeachingRefusal(t, errOut.String(), "policy_decision",
			"outbound.dns_record.write",
			"compound policy standing-grants-authority --json",
		)
		// The refusal must report the decision it actually got, not a generic "blocked":
		// hold and deny send the reader to different remedies.
		if !strings.Contains(errOut.String(), "outcome hold") && !strings.Contains(errOut.String(), "outcome deny") {
			t.Errorf("refusal must name the policy outcome it observed, got:\n%s", errOut.String())
		}
		if len(fake.ops) != 0 {
			t.Fatalf("a policy-refused write may run no remote op; got %d ops", len(fake.ops))
		}
	})
}
