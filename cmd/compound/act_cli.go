package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/act"
	actcli "compound/internal/act/cli"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/mem"
	"compound/internal/policy"
)

// DRY-RUN by default (shows current -> proposed and the PolicyDecision, writes nothing). --confirm
// performs the governed write, which REQUIRES an allowing PolicyDecision (a standing grant for the
// capability/class/target). Without one the act refuses and the target stays byte-identical. It makes
// NO provider/network call; it mutates only the named config file, only under an allowing decision.
func cmdAct(args []string) {
	if code := runAct(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// actDefaultCapability is the capability a repoint declares unless --capability overrides it; an
// operator's standing grant scopes to exactly this capability + external_side_effect class + target.
const actDefaultCapability = "outbound.config_key.write"

func actUsage() string {
	return "usage:\n" +
		"  compound act repoint --target-path P --target-id KEY --value V --work W [--confirm] [--capability CAP] [--home P] [--json]\n" +
		"  " + actcli.PaddleSandboxUsage() + "\n" +
		"  " + actcli.CredentialVerificationUsage() + "\n" +
		"  " + actcli.RuntimeConfigInstallUsage() + "\n" +
		"  " + actcli.BackupCredentialStageUsage() + "\n" +
		"  " + actcli.SourceMarkerRetirementUsage() + "\n" +
		"  compound act post (--from-work W | --draft D --at RFC3339 | --content -) [--work W] --transport NAME [--confirm] [--home P] [--json]\n" +
		"    Governed outbound write: repoint the dotted KEY inside the YAML/JSON config at P to V.\n" +
		"    DRY-RUN by default (shows current -> proposed + the PolicyDecision, writes nothing).\n" +
		"    --confirm performs the write THROUGH the act seam — it REQUIRES an allowing PolicyDecision\n" +
		"    (a standing grant for --capability, external_side_effect class, easy reversibility, local\n" +
		"    blast, scoped to --target-path). Without one the act refuses and the target is byte-identical.\n" +
		"    The adapter REPOINTS an existing key; it never creates one. Pre/post images are captured as\n" +
		"    content-addressed artifacts and the receipt/transition land in $COMPOUND_HOME/events.\n" +
		"  compound act dns-record --service S --host H --address A --remote-host RH [--remote-user U] [--remote-port N] [--remote-path P] [--reload-cmd C] --work W [--confirm] [--capability CAP] [--home P] [--json]\n" +
		"    Governed ADDITIVE DNS host->address record on a router, over an injectable ssh transport.\n" +
		"  compound act vhost --service S --server-name N --upstream U --remote-host RH [--remote-user U] [--remote-port N] [--remote-path P] [--reload-cmd C] --work W [--confirm] [--capability CAP] [--home P] [--json]\n" +
		"    Governed ADDITIVE vhost block on an apps host, over the same seam.\n" +
		"    Both are external_side_effect (HELD by default; a standing grant + --confirm executes),\n" +
		"    additive-only (create a NEW entry; modify/delete is refused), and preflight-resolve the\n" +
		"    target service:* entity before minting. Pre/post images + receipt land in $COMPOUND_HOME/events."
}

func runAct(args []string, stdout, stderr io.Writer) int {
	return runActWithInput(args, os.Stdin, stdout, stderr)
}

func runActWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(stderr, actUsage())
		return 2
	}
	switch args[0] {
	case "credential-password-align":
		return actcli.RunCredentialPasswordAlignment(args[1:], stdout, stderr)
	case "credential-verify":
		return actcli.RunCredentialVerification(args[1:], stdout, stderr)
	case "source-marker-retirement":
		return actcli.RunSourceMarkerRetirement(args[1:], stdout, stderr)
	case "backup-credential-stage":
		return actcli.RunBackupCredentialStage(args[1:], stdout, stderr)
	case "runtime-config-install":
		return actcli.RunRuntimeConfigInstall(args[1:], stdout, stderr)
	case "paddle-sandbox-notification":
		return actcli.RunPaddleSandboxNotification(args[1:], stdout, stderr)
	case "repoint":
		return runActRepoint(args[1:], stdout, stderr)
	case "post":
		return runActPost(args[1:], stdin, stdout, stderr)
	case "dns-record":
		return runActDNSRecord(args[1:], stdout, stderr)
	case "vhost":
		return runActVhost(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, actUsage())
		return 2
	}
}

// actRemoteRunner indirects the transport the remote-outbound verbs (dns-record, vhost)
// hand to their adapters. Nil means the real ssh transport (act's package default); tests
// swap in a fake that records commands and never opens a connection. It is unexported and
// swapped ONLY in tests, mirroring internal/act's own recordEnvelopeFn seam.
var actRemoteRunner act.RemoteRunner

// Honest capability strings for the two remote-outbound verbs. An operator's standing grant
// scopes to exactly this capability + external_side_effect class + remote target path.
const (
	dnsRecordDefaultCapability = "outbound.dns_record.write"
	vhostDefaultCapability     = "outbound.vhost.write"
)

// remoteActSpec is the per-verb data for the ONE shared remote-outbound orchestrator. The two
// verbs differ only in these fields; the governance path (preflight -> mint -> gate -> Execute)
// is identical, so there is no second governance path to drift.
type remoteActSpec struct {
	verb         string
	kind         string // ActionTarget.Kind
	defaultCap   string
	action       string // ActionIntent.Action
	nameFlag     string // record name flag (--host / --server-name)
	valueFlag    string // record value flag (--address / --upstream)
	defaultPath  string // default remote file (a generic tool convention, overridable)
	newAdapter   func(runner act.RemoteRunner, ep act.RemoteEndpoint, path, reload string) act.Adapter
	forwardValue func(name, value string) []byte
}

func runActDNSRecord(args []string, stdout, stderr io.Writer) int {
	return runActRemote(remoteActSpec{
		verb:         "dns-record",
		kind:         "dns_record",
		defaultCap:   dnsRecordDefaultCapability,
		action:       "create_dns_record",
		nameFlag:     "--host",
		valueFlag:    "--address",
		defaultPath:  "/etc/dnsmasq.d/compound.conf",
		newAdapter:   act.NewDNSRecordAdapter,
		forwardValue: act.DNSRecordForwardValue,
	}, args, stdout, stderr)
}

func runActVhost(args []string, stdout, stderr io.Writer) int {
	return runActRemote(remoteActSpec{
		verb:         "vhost",
		kind:         "vhost",
		defaultCap:   vhostDefaultCapability,
		action:       "create_vhost",
		nameFlag:     "--server-name",
		valueFlag:    "--upstream",
		defaultPath:  "/data/nginx/custom/compound.conf",
		newAdapter:   act.NewVhostAdapter,
		forwardValue: act.VhostForwardValue,
	}, args, stdout, stderr)
}

// runActRemote is the shared governed-remote-outbound orchestrator. Order is fail-closed:
// additive-only guard (this slice only CREATES) -> required flags -> preflight service
// resolution (refuse if unresolved, BEFORE any intent) -> mint -> policy gate inside Execute
// (held by default without a grant) -> governed write. Dry-run (no --confirm) mints nothing
// remote: it prints the proposed record + policy outcome and makes ZERO remote calls.
func runActRemote(spec remoteActSpec, args []string, stdout, stderr io.Writer) int {
	// Additive-only: modifying or deleting an existing entry is out of scope this slice.
	for _, f := range []string{"--delete", "--modify", "--replace"} {
		if hasFlag(args, f) {
			fmt.Fprintf(stderr, "error: additive-only — `compound act %s` only creates a new entry this slice (%s is out of scope; reconcile an existing entry by hand)\n", spec.verb, f)
			return 2
		}
	}

	service := strings.TrimSpace(flagValue(args, "--service"))
	name := strings.TrimSpace(flagValue(args, spec.nameFlag))
	value := strings.TrimSpace(flagValue(args, spec.valueFlag))
	remoteHost := strings.TrimSpace(flagValue(args, "--remote-host"))
	if service == "" || name == "" || value == "" || remoteHost == "" {
		fmt.Fprintf(stderr, "usage: compound act %s --service S %s NAME %s V --remote-host H [--remote-user U] [--remote-port N] [--remote-path P] [--reload-cmd C] --work W [--confirm] [--capability CAP] [--home P] [--json]\n", spec.verb, spec.nameFlag, spec.valueFlag)
		return 2
	}
	remotePath := strings.TrimSpace(flagValue(args, "--remote-path"))
	if remotePath == "" {
		remotePath = spec.defaultPath
	}
	remoteUser := strings.TrimSpace(flagValue(args, "--remote-user"))
	if remoteUser == "" {
		remoteUser = "root"
	}
	remotePort := 0
	if hasFlag(args, "--remote-port") {
		p, err := strconv.Atoi(strings.TrimSpace(flagValue(args, "--remote-port")))
		if err != nil {
			fmt.Fprintln(stderr, "error: --remote-port must be an integer")
			return 2
		}
		remotePort = p
	}
	reload := strings.TrimSpace(flagValue(args, "--reload-cmd"))
	capability := strings.TrimSpace(flagValue(args, "--capability"))
	if capability == "" {
		capability = spec.defaultCap
	}

	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	// Preflight fail-closed: the target service must resolve to a known service:* entity
	// BEFORE any intent is minted (no remote call happens here — it is a local ledger read).
	slug, err := resolveInfraServiceSlug(homePath, service)
	if err != nil {
		gateRefusal{
			Command: "compound act " + spec.verb,
			Gate:    "service_entity_resolution",
			Refused: fmt.Sprintf("--service %q does not name a service:* entity this home knows, so no ActionIntent was minted and no remote call was made", service),
			Tried: []string{
				fmt.Sprintf("--service %s against the ledger's entity resolver: %v", service, err),
			},
			DoThis: []string{
				"compound memory entity list   # the entities this home knows, with their kinds and aliases",
				fmt.Sprintf("compound memory entity resolve %s   # how this exact token resolves today", firstNonEmptyRefusalValue(service, "<service-token>")),
				fmt.Sprintf("compound memory entity register service:%s service   # register it first if the service is real but unknown here", firstNonEmptyRefusalValue(service, "<name>")),
			},
			Prevents: "a governed infra write aimed at a service compound cannot name. Resolving the target after the write would let a typo'd --service mutate a real remote host with no entity to attribute the change to.",
		}.writeTo(stderr)
		return 1
	}

	endpoint := act.RemoteEndpoint{User: remoteUser, Host: remoteHost, Port: remotePort}
	target := envelope.ActionTarget{Kind: spec.kind, ID: name, Path: remotePath}
	adapter := spec.newAdapter(actRemoteRunner, endpoint, remotePath, reload)
	now := time.Now()

	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	if !hasFlag(args, "--confirm") {
		work := strings.TrimSpace(flagValue(args, "--work"))
		if work == "" {
			work = "work_dryrun"
		}
		ai := governedRemoteIntent(work, capability, spec.action, target, name, value, now)
		pd := policy.Evaluate(ai, cfg, now)
		return printRemoteActPreview(stdout, spec, target, slug, name, value, pd, hasFlag(args, "--json"))
	}

	work := strings.TrimSpace(flagValue(args, "--work"))
	if work == "" {
		fmt.Fprintln(stderr, "error: --work W is required for --confirm (a governed act belongs to a work)")
		return 2
	}
	store, err := act.NewArtifactStore(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ai := governedRemoteIntent(work, capability, spec.action, target, name, value, now)
	pd := policy.Evaluate(ai, cfg, now)
	// The policy rung of act's by-construction gate, rendered BEFORE act.Execute so the
	// refusal can teach. act.Execute would refuse this same decision inside gate() before
	// touching the adapter, so short-circuiting here is byte-identical in effect and only
	// changes the sentence the operator reads.
	if !policy.Allowed(pd) {
		actPolicyGateRefusal("compound act "+spec.verb+" --confirm", capability, target, pd, []string{
			"compound policy standing-grants-authority --json   # which capabilities already carry a standing grant in this home",
			"compound policy import-standing-grants <grants.yaml>   # add one scoped to this capability, the external_side_effect class, and this remote path",
			fmt.Sprintf("compound act %s --service %s %s %s %s %s --remote-host <host>   # without --confirm: dry-run preview, zero remote calls", spec.verb, firstNonEmptyRefusalValue(service, "<service>"), spec.nameFlag, firstNonEmptyRefusalValue(name, "<name>"), spec.valueFlag, firstNonEmptyRefusalValue(value, "<value>")),
		}).writeTo(stderr)
		return 1
	}
	res, execErr := act.Execute(store, adapter, ai, pd, spec.forwardValue(name, value), now)
	if execErr != nil {
		// No allowing PolicyDecision (hold/deny), a conflict, or a write failure => refused,
		// with the target's pre-image restored (or a recovery finding) by the act seam.
		fmt.Fprintf(stderr, "refused: %v\n", execErr)
		return 1
	}
	return printActResult(stdout, target, res, hasFlag(args, "--json"))
}

// resolveInfraServiceSlug resolves the operator's --service token to a service:* entity via
// the mem store (read-only; never shells out). An unresolved or non-service token is a
// fail-closed refusal — a governed infra write must name a service compound already knows.
func resolveInfraServiceSlug(homePath, token string) (string, error) {
	store, err := mem.OpenReadOnly(homePath)
	if err != nil {
		return "", fmt.Errorf("service registry unavailable — import infra services first (%v)", err)
	}
	res, err := store.ResolveEntity(token)
	if err != nil {
		return "", err
	}
	if res.Status != "resolved" || !strings.HasPrefix(res.Slug, "service:") {
		return "", fmt.Errorf("target service %q did not resolve to a known service:* entity (status=%s)", token, res.Status)
	}
	return res.Slug, nil
}

// governedRemoteIntent builds the external_side_effect / easy / local ActionIntent for one
// additive remote write, carrying the concrete rollback_plan act's gate requires. It mirrors
// governedRepointIntent so the verb writes through exactly the governed path — no bespoke governance.
func governedRemoteIntent(workID, capability, action string, target envelope.ActionTarget, name, value string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actIDFor("act_", workID, capability, target.Path, target.ID, value),
			CreatedAt:     now.UTC().Format(time.RFC3339),
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
		Action:        action,
		Target:        target,
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("add %s %s -> %q in %s", target.Kind, name, value, target.Path),
		RollbackPlan:  fmt.Sprintf("remove the added %s %s from %s (restore the captured pre-image)", target.Kind, name, target.Path),
	}
}

func printRemoteActPreview(stdout io.Writer, spec remoteActSpec, target envelope.ActionTarget, serviceSlug, name, value string, pd envelope.PolicyDecision, asJSON bool) int {
	allowed := policy.Allowed(pd)
	if asJSON {
		out := map[string]any{
			"mode": "dry-run", "verb": spec.verb, "target_kind": target.Kind,
			"service": serviceSlug, "name": name, "value": value, "remote_path": target.Path,
			"policy_outcome": pd.Outcome, "allowed": allowed,
		}
		if err := encodeJSON(stdout, out); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# act %s (dry-run — nothing written, no remote call)\n", spec.verb)
	fmt.Fprintf(stdout, "- service: %s\n", serviceSlug)
	fmt.Fprintf(stdout, "- proposed: add %s %q -> %q in %s\n", target.Kind, name, value, target.Path)
	if allowed {
		fmt.Fprintf(stdout, "- policy: %s (ALLOWED) — run again with --confirm --work W to perform the governed write\n", pd.Outcome)
	} else {
		fmt.Fprintf(stdout, "- policy: %s (BLOCKED) — held by default; add a standing grant for this capability/target first\n", pd.Outcome)
	}
	return 0
}

func runActRepoint(args []string, stdout, stderr io.Writer) int {
	targetPath := strings.TrimSpace(flagValue(args, "--target-path"))
	targetID := strings.TrimSpace(flagValue(args, "--target-id"))
	if targetPath == "" || targetID == "" || !hasFlag(args, "--value") {
		fmt.Fprintln(stderr, "usage: compound act repoint --target-path P --target-id KEY --value V --work W [--confirm] [--capability CAP]")
		return 2
	}
	value := flagValue(args, "--value")
	capability := strings.TrimSpace(flagValue(args, "--capability"))
	if capability == "" {
		capability = actDefaultCapability
	}
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	adapter := act.NewConfigKeyAdapter()
	target := envelope.ActionTarget{Kind: "config_key", ID: targetID, Path: targetPath}
	now := time.Now()

	// Read the current value (pre-image) for the preview — an absent key/file is reported, never invented.
	cur, readErr := adapter.ReadCurrent(target)

	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	if !hasFlag(args, "--confirm") {
		// DRY-RUN default: show current -> proposed and the PolicyDecision; write nothing.
		work := strings.TrimSpace(flagValue(args, "--work"))
		if work == "" {
			work = "work_dryrun"
		}
		ai := governedRepointIntent(work, capability, target, value, now)
		pd := policy.Evaluate(ai, cfg, now)
		return printActPreview(stdout, target, string(cur), readErr, value, pd, hasFlag(args, "--json"))
	}

	work := strings.TrimSpace(flagValue(args, "--work"))
	if work == "" {
		fmt.Fprintln(stderr, "error: --work W is required for --confirm (a governed act belongs to a work)")
		return 2
	}
	if readErr != nil {
		fmt.Fprintf(stderr, "error: cannot repoint %q: %v (the adapter repoints an existing key, it does not create one)\n", targetID, readErr)
		return 1
	}
	store, err := act.NewArtifactStore(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ai := governedRepointIntent(work, capability, target, value, now)
	pd := policy.Evaluate(ai, cfg, now)
	// Same policy rung as runActRemote: rendered here so the refusal names the grant the
	// operator is missing instead of restating act's internal gate error.
	if !policy.Allowed(pd) {
		actPolicyGateRefusal("compound act repoint --confirm", capability, target, pd, []string{
			"compound policy standing-grants-authority --json   # which capabilities already carry a standing grant in this home",
			"compound policy import-standing-grants <grants.yaml>   # add one scoped to this capability, the external_side_effect class, and this --target-path",
			fmt.Sprintf("compound act repoint --target-path %s --target-id %s --value <new-value> --work %s   # without --confirm: dry-run preview, writes nothing", firstNonEmptyRefusalValue(targetPath, "<config-path>"), firstNonEmptyRefusalValue(targetID, "<dotted.key>"), firstNonEmptyRefusalValue(work, "<work-id>")),
		}).writeTo(stderr)
		return 1
	}
	res, execErr := act.Execute(store, adapter, ai, pd, []byte(value), now)
	if execErr != nil {
		// No allowing PolicyDecision (hold/deny) => act refused, target byte-identical.
		fmt.Fprintf(stderr, "refused: %v\n", execErr)
		return 1
	}
	return printActResult(stdout, target, res, hasFlag(args, "--json"))
}

// governedRepointIntent builds the external_side_effect / easy / local ActionIntent for one config-key
// repoint, carrying the rollback_plan act's gate requires. It mirrors the intent shape internal/act's
// own tests pin, so the CLI writes through exactly the governed path — no bespoke governance.
func governedRepointIntent(workID, capability string, target envelope.ActionTarget, value string, now time.Time) envelope.ActionIntent {
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            actIDFor("act_", workID, capability, target.Path, target.ID, value),
			CreatedAt:     now.UTC().Format(time.RFC3339),
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
		Action:        "repoint_config_key",
		Target:        target,
		MutationClass: "external_side_effect",
		Reversibility: "easy",
		BlastRadius:   "local",
		Preview:       fmt.Sprintf("repoint %s in %s to %q", target.ID, target.Path, value),
		RollbackPlan:  fmt.Sprintf("restore the captured pre-image of %s in %s", target.ID, target.Path),
	}
}

func printActPreview(stdout io.Writer, target envelope.ActionTarget, cur string, readErr error, proposed string, pd envelope.PolicyDecision, asJSON bool) int {
	allowed := policy.Allowed(pd)
	if asJSON {
		out := map[string]any{
			"mode": "dry-run", "target_id": target.ID, "target_path": target.Path,
			"current": cur, "proposed": proposed, "key_present": readErr == nil,
			"policy_outcome": pd.Outcome, "allowed": allowed,
		}
		if readErr != nil {
			out["read_error"] = readErr.Error()
		}
		if err := encodeJSON(stdout, out); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# act repoint (dry-run — nothing written)")
	fmt.Fprintf(stdout, "- target: %s in %s\n", target.ID, target.Path)
	if readErr != nil {
		fmt.Fprintf(stdout, "- current: <absent> (%v)\n", readErr)
	} else {
		fmt.Fprintf(stdout, "- current: %q -> proposed: %q\n", cur, proposed)
	}
	if allowed {
		fmt.Fprintf(stdout, "- policy: %s (ALLOWED) — run again with --confirm --work W to perform the governed write\n", pd.Outcome)
	} else {
		fmt.Fprintf(stdout, "- policy: %s (BLOCKED) — no allowing PolicyDecision; add a standing grant for this capability/target first\n", pd.Outcome)
	}
	return 0
}

func printActResult(stdout io.Writer, target envelope.ActionTarget, res act.Result, asJSON bool) int {
	if asJSON {
		out := map[string]any{
			"mode": "confirmed", "target_id": target.ID, "target_path": target.Path,
			"receipt_id": res.Receipt.ID, "transition_id": res.Transition.ID, "move_type": res.Receipt.MoveType,
			"pre_image_ref": res.PreImageRef, "post_image_ref": res.PostImageRef,
			"before": string(res.PreImage), "after": string(res.PostImage),
		}
		if err := encodeJSON(stdout, out); err != nil {
			fmt.Fprintln(stdout, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, "# act repoint (confirmed — governed write applied)")
	fmt.Fprintf(stdout, "- target: %s in %s\n", target.ID, target.Path)
	fmt.Fprintf(stdout, "- change: %q -> %q\n", string(res.PreImage), string(res.PostImage))
	fmt.Fprintf(stdout, "- receipt=%s transition=%s move=%s\n", res.Receipt.ID, res.Transition.ID, res.Receipt.MoveType)
	fmt.Fprintf(stdout, "- pre-image=%s post-image=%s (a revert restores the pre-image through the same act seam)\n", res.PreImageRef, res.PostImageRef)
	return 0
}

// actPolicyGateRefusal renders the ONE policy rung every `act ... --confirm` verb shares:
// an ActionIntent was minted and evaluated, and no standing grant allows it. The verb-specific
// recovery ladder is the caller's, because the exact re-run command differs per verb; the
// tried-trace and the prevented failure do not, so they live here rather than being restated
// (and drifting) at each site.
func actPolicyGateRefusal(command, capability string, target envelope.ActionTarget, pd envelope.PolicyDecision, doThis []string) gateRefusal {
	outcome := firstNonEmptyRefusalValue(pd.Outcome, "<no outcome>")
	reasons := "no reason recorded"
	if joined := strings.TrimSpace(strings.Join(pd.Reasons, "; ")); joined != "" {
		reasons = joined
	}
	return gateRefusal{
		Command: command,
		Gate:    "policy_decision",
		Refused: fmt.Sprintf("no standing grant allows capability %q on %s %q, so %s is byte-identical and no receipt or transition was minted", capability, target.Kind, target.ID, firstNonEmptyRefusalValue(target.Path, "the target")),
		Tried: []string{
			fmt.Sprintf("mint ActionIntent for capability %s (class external_side_effect, blast local): minted", capability),
			fmt.Sprintf("evaluate it against this home's standing grants: outcome %s (%s)", outcome, reasons),
		},
		DoThis:   doThis,
		Prevents: "an ungoverned outbound write. Every act write is authorized by a PolicyDecision that names THIS intent; without one the seam refuses rather than mutating a real target that no receipt can later explain or revert.",
	}
}

func actIDFor(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return prefix + hex.EncodeToString(sum[:])[:12]
}
