package act

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"compound/internal/envelope"
)

// remote.go adds TWO governed outbound adapters that ride the SAME Execute gate as the
// config-key repoint: a DNS host->target record on a router, and an NPM vhost block on an
// apps host. Both are plain grammars over ONE shared "remote managed-file" core, and both
// touch the outside world only through an injectable RemoteRunner — the transport every
// test fakes. There is NO new governance path here: a mutation still requires an allowing
// PolicyDecision for its exact ActionIntent, because the mutating adapter method (write)
// stays unexported and Execute is its only caller.
//
// Shape decision (OPEN #2): ONE shared core + two grammars, not two hand-rolled adapters.
// Both operations are "append a marked block to a remote config file, then reload"; the
// only differences are the rendered block, the idempotency marker, and the reload command
// — all data. Sharing the ssh/idempotency/rollback logic keeps each adapter's failure
// modes IDENTICAL and legible in receipts, while the grammar keeps DNS vs vhost distinct.
// This is an abstraction for the two cases that exist, not a speculative third.

// RemoteOp is one transport operation. Kind gives the fake its semantics; Command is the
// exact shell string that runs on the host (recorded in receipts / audit); Payload is the
// content an apply/restore writes. The production runner executes Command; a fake keys off
// Kind + Payload so tests never depend on parsing shell strings.
type RemoteOp struct {
	Kind    string // RemoteOpRead | RemoteOpApply | RemoteOpRestore
	Path    string // the remote file the op targets
	Command string // the exact shell command executed on the host
	Payload []byte // bytes an apply appends / a restore writes
}

const (
	RemoteOpRead    = "read"    // read the current remote file (pre/post image)
	RemoteOpApply   = "apply"   // append the additive block + reload
	RemoteOpRestore = "restore" // write the whole file back to a captured pre-image + reload
)

// RemoteEndpoint is where a RemoteRunner connects. It is supplied at runtime (synthetic in
// tests; real values only in the operator's untracked invocation) — never hard-coded.
type RemoteEndpoint struct {
	User string
	Host string
	Port int
}

// RemoteRunner is the injectable transport — the ONE remote-client seam both adapters ride.
// The production default (sshRemoteRunner) shells out to ssh; every test injects a fake that
// records commands and never opens a connection. Exporting this interface does NOT weaken the
// governance seal: only package act can construct an Adapter (write is unexported), and a
// RemoteRunner only carries an already-gated write to the host — it can never reach one.
type RemoteRunner interface {
	Run(endpoint RemoteEndpoint, op RemoteOp) ([]byte, error)
}

// defaultRemoteRunner is the package-var default transport (swappable in-package for tests,
// like recordEnvelopeFn). A constructor's nil runner falls back to it.
var defaultRemoteRunner RemoteRunner = sshRemoteRunner{}

// sshRemoteRunner runs op.Command over ssh. It is the real transport; tests never reach it
// (they inject a fake), so it is deliberately thin. It refuses an empty host/command rather
// than shelling out to something ambiguous.
type sshRemoteRunner struct{}

func (sshRemoteRunner) Run(ep RemoteEndpoint, op RemoteOp) ([]byte, error) {
	if strings.TrimSpace(ep.Host) == "" {
		return nil, fmt.Errorf("remote runner: endpoint host required")
	}
	if strings.TrimSpace(op.Command) == "" {
		return nil, fmt.Errorf("remote runner: op command required")
	}
	target := ep.Host
	if u := strings.TrimSpace(ep.User); u != "" {
		target = u + "@" + ep.Host
	}
	var args []string
	if ep.Port != 0 {
		args = append(args, "-p", strconv.Itoa(ep.Port))
	}
	args = append(args, target, op.Command)
	return exec.Command("ssh", args...).CombinedOutput()
}

// remoteGrammar is the per-adapter data: the target kind, the reload command, and how to
// render an additive block (returning the idempotency marker + the block text) from the
// record's name/value. It carries NO ssh/rollback logic — that all lives in the shared core.
type remoteGrammar struct {
	kind          string
	defaultReload string
	render        func(name, value string) (marker string, block string)
}

// remoteManagedFileAdapter is the shared core. ReadCurrent captures the whole remote file
// as a schema-tagged pre-image; write either APPENDS an additive block (a forward value) or
// RESTORES the whole file (a pre-image value) — so Execute's afterWriteFailure rollback and
// RevertIntent both restore exact bytes through the same seam, exactly like FileArtifactAdapter.
type remoteManagedFileAdapter struct {
	runner   RemoteRunner
	endpoint RemoteEndpoint
	path     string
	reload   string
	grammar  remoteGrammar
}

var _ Adapter = remoteManagedFileAdapter{}

// NewDNSRecordAdapter returns the DNS host->target adapter as an Adapter. A nil runner uses
// the default ssh transport; a nil/empty reload uses the grammar default. External callers
// obtain the adapter here and hand it to Execute — they cannot call its write directly.
func NewDNSRecordAdapter(runner RemoteRunner, endpoint RemoteEndpoint, path, reload string) Adapter {
	return newRemoteAdapter(runner, endpoint, path, reload, dnsGrammar())
}

// NewVhostAdapter returns the NPM vhost-block adapter as an Adapter (same governance seal,
// vhost grammar).
func NewVhostAdapter(runner RemoteRunner, endpoint RemoteEndpoint, path, reload string) Adapter {
	return newRemoteAdapter(runner, endpoint, path, reload, vhostGrammar())
}

func newRemoteAdapter(runner RemoteRunner, endpoint RemoteEndpoint, path, reload string, grammar remoteGrammar) Adapter {
	if runner == nil {
		runner = defaultRemoteRunner
	}
	if strings.TrimSpace(reload) == "" {
		reload = grammar.defaultReload
	}
	return remoteManagedFileAdapter{runner: runner, endpoint: endpoint, path: path, reload: reload, grammar: grammar}
}

// dnsGrammar renders an OpenWrt/dnsmasq additive host->address mapping as a config-file
// line (`address=/<host>/<addr>`) and reloads dnsmasq. The marker is `address=/<host>/`, so
// any existing mapping for that host is a conflict — an additive create never shadows one.
func dnsGrammar() remoteGrammar {
	return remoteGrammar{
		kind:          "dns_record",
		defaultReload: "/etc/init.d/dnsmasq reload",
		render: func(host, addr string) (string, string) {
			marker := "address=/" + host + "/"
			return marker, marker + addr
		},
	}
}

// vhostGrammar renders an nginx (NPM custom-conf) server block proxying a server_name to an
// upstream, tagged with a compound marker comment used for idempotency, and reloads nginx.
func vhostGrammar() remoteGrammar {
	return remoteGrammar{
		kind:          "vhost",
		defaultReload: "nginx -s reload",
		render: func(serverName, upstream string) (string, string) {
			marker := "# compound-managed vhost " + serverName
			block := marker + "\n" +
				"server {\n" +
				"    server_name " + serverName + ";\n" +
				"    location / {\n" +
				"        proxy_pass " + upstream + ";\n" +
				"    }\n" +
				"}"
			return marker, block
		},
	}
}

// --- forward-value encoding (what the CLI hands Execute as newValue) ---

const (
	remoteForwardSchema  = "compound.remote_block.forward.v0.1"
	remotePreImageSchema = "compound.remote_file.preimage.v0.1"
)

type remoteForward struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

type remotePreImage struct {
	Schema  string `json:"schema"`
	Content []byte `json:"content,omitempty"` // whole-file bytes (base64 in JSON)
}

// DNSRecordForwardValue encodes the additive DNS record (host -> address) the CLI hands to
// Execute as newValue. write decodes it as a forward append (distinct from a pre-image restore).
func DNSRecordForwardValue(host, address string) []byte { return forwardValue(host, address) }

// VhostForwardValue encodes the additive vhost record (server_name -> upstream).
func VhostForwardValue(serverName, upstream string) []byte { return forwardValue(serverName, upstream) }

func forwardValue(name, value string) []byte {
	b, _ := json.Marshal(remoteForward{Schema: remoteForwardSchema, Name: name, Value: value})
	return b
}

func encodeRemotePreImage(content []byte) []byte {
	b, _ := json.Marshal(remotePreImage{Schema: remotePreImageSchema, Content: content})
	return b
}

// ReadCurrent captures the whole remote file as a schema-tagged pre-image (empty content
// when the managed file does not yet exist). The tag is interpreted only by the distinct
// restorePreImage path; forward append bytes never become rollback control data.
func (a remoteManagedFileAdapter) ReadCurrent(target envelope.ActionTarget) ([]byte, error) {
	content, err := a.readRemote()
	if err != nil {
		return nil, err
	}
	return encodeRemotePreImage(content), nil
}

// write is the forward-only path: it accepts an additive block and never
// interprets pre-image control bytes.
func (a remoteManagedFileAdapter) write(target envelope.ActionTarget, newValue []byte) error {
	if fwd, ok := decodeForward(newValue); ok {
		return a.appendBlock(fwd.Name, fwd.Value)
	}
	return fmt.Errorf("%s adapter: unrecognized forward write value", a.grammar.kind)
}

func (a remoteManagedFileAdapter) restorePreImage(_ envelope.ActionTarget, preImage []byte) error {
	pre, ok := decodePreImage(preImage)
	if !ok {
		return fmt.Errorf("%s adapter: unrecognized captured pre-image", a.grammar.kind)
	}
	return a.restore(pre.Content)
}

func decodeForward(b []byte) (remoteForward, bool) {
	var f remoteForward
	if err := json.Unmarshal(b, &f); err == nil && f.Schema == remoteForwardSchema {
		return f, true
	}
	return remoteForward{}, false
}

func decodePreImage(b []byte) (remotePreImage, bool) {
	var p remotePreImage
	if err := json.Unmarshal(b, &p); err == nil && p.Schema == remotePreImageSchema {
		return p, true
	}
	return remotePreImage{}, false
}

func (a remoteManagedFileAdapter) appendBlock(name, value string) error {
	if err := validateRemoteToken(a.grammar.kind+" name", name); err != nil {
		return err
	}
	if err := validateRemoteToken(a.grammar.kind+" value", value); err != nil {
		return err
	}
	if err := validateRemotePath(a.path); err != nil {
		return err
	}
	marker, block := a.grammar.render(name, value)

	cur, err := a.readRemote()
	if err != nil {
		return err
	}
	if bytes.Contains(cur, []byte(marker)) {
		// Refuse-as-conflict: an additive create whose record already exists is drift the
		// operator must see, not a silent no-op (Execute's receipt would falsely claim changed).
		return fmt.Errorf("%s %q is already present in %s — additive-only act refuses (reconcile the existing entry first)", a.grammar.kind, name, a.path)
	}

	payload := []byte(block + "\n")
	cmd := "printf '%s\\n' " + singleQuote(block) + " >> " + singleQuote(a.path) + " && " + a.reload
	_, err = a.runner.Run(a.endpoint, RemoteOp{Kind: RemoteOpApply, Path: a.path, Command: cmd, Payload: payload})
	return err
}

func (a remoteManagedFileAdapter) restore(content []byte) error {
	if err := validateRemotePath(a.path); err != nil {
		return err
	}
	eof, err := restoreHeredocDelimiter(content)
	if err != nil {
		return err
	}
	cmd := "cat > " + singleQuote(a.path) + " <<'" + eof + "'\n" + string(content) + "\n" + eof + "\n" + a.reload
	_, err = a.runner.Run(a.endpoint, RemoteOp{Kind: RemoteOpRestore, Path: a.path, Command: cmd, Payload: content})
	return err
}

// restoreHeredocDelimiterAttempts bounds delimiter regeneration so content
// engineered to always collide fails closed instead of looping forever.
const restoreHeredocDelimiterAttempts = 8

// restoreHeredocDelimiter picks a per-invocation heredoc terminator that does
// not occur anywhere in content. restore() previously used a single static
// delimiter (COMPOUND_RESTORE_EOF) over unsanitized remote file content: a
// captured pre-image containing that literal line closed the heredoc early
// and handed the remainder of the file to the remote shell as commands — a
// command-injection surface in the governed DNS/vhost rollback path (E6).
// Randomizing the delimiter per call and scanning content for a collision
// (regenerating on one) closes it.
func restoreHeredocDelimiter(content []byte) (string, error) {
	for attempt := 0; attempt < restoreHeredocDelimiterAttempts; attempt++ {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("restore: generate heredoc delimiter: %w", err)
		}
		candidate := "COMPOUND_RESTORE_" + hex.EncodeToString(raw[:])
		if !bytes.Contains(content, []byte(candidate)) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("restore: could not generate a heredoc delimiter absent from content after %d attempts", restoreHeredocDelimiterAttempts)
}

func (a remoteManagedFileAdapter) readRemote() ([]byte, error) {
	cmd := "cat -- " + singleQuote(a.path) + " 2>/dev/null || true"
	out, err := a.runner.Run(a.endpoint, RemoteOp{Kind: RemoteOpRead, Path: a.path, Command: cmd})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- input hardening: reject anything that could break out of single-quoting ---

var safeToken = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

func validateRemoteToken(label, tok string) error {
	if strings.TrimSpace(tok) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if !safeToken.MatchString(tok) {
		return fmt.Errorf("%s %q contains characters outside the safe set [A-Za-z0-9._:/@-]", label, tok)
	}
	return nil
}

func validateRemotePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("remote path is required")
	}
	if !safeToken.MatchString(path) || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("remote path %q must be an absolute path over the safe set [A-Za-z0-9._:/@-]", path)
	}
	return nil
}

// singleQuote wraps a value in single quotes. Safe ONLY because every interpolated token /
// path is validated to exclude the single-quote character (and shell metacharacters); the
// rendered block itself is built only from such validated tokens plus fixed literals.
func singleQuote(s string) string { return "'" + s + "'" }
