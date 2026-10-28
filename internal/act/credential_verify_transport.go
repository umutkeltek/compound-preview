package act

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"compound/internal/policy"
)

//go:embed credential_verify_program.py
var credentialVerificationProgram string

func CredentialVerificationProgramSHA256() string {
	sum := sha256.Sum256([]byte(credentialVerificationProgram))
	return hex.EncodeToString(sum[:])
}

// The owner holds native admission while invoking the exact fixed OS start.
type credentialVerifyAdmissionGuard func(start func() error) error
type credentialVerifyNativeTransport struct{}

func trustedCredentialTransportFile(path string, private bool) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, credentialVerifyError("transport_custody")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, credentialVerifyError("transport_custody")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(st.Uid) != os.Geteuid() && st.Uid != 0) || st.Nlink != 1 || info.Mode().Perm()&0022 != 0 {
		return nil, credentialVerifyError("transport_custody")
	}
	if private && info.Mode().Perm() != 0600 {
		return nil, credentialVerifyError("transport_custody")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		p, err := os.Lstat(parent)
		if err != nil || !p.IsDir() || p.Mode()&os.ModeSymlink != 0 || p.Mode().Perm()&0022 != 0 {
			return nil, credentialVerifyError("transport_custody")
		}
		ps, ok := p.Sys().(*syscall.Stat_t)
		if !ok || (int(ps.Uid) != os.Geteuid() && ps.Uid != 0) {
			return nil, credentialVerifyError("transport_custody")
		}
		if parent == string(filepath.Separator) {
			break
		}
	}
	return info, nil
}

// Only canonical lowercase literal DNS names are admitted. OpenSSH's patterns,
// hashes, negation, host lists, ports and markers are outside this grammar.
func credentialVerifyLiteralHost(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func credentialVerifySSHString(raw []byte) (value, rest []byte, ok bool) {
	if len(raw) < 4 {
		return nil, nil, false
	}
	n := uint64(binary.BigEndian.Uint32(raw[:4]))
	if n > uint64(len(raw)-4) {
		return nil, nil, false
	}
	end := 4 + int(n)
	return raw[4:end], raw[end:], true
}

func credentialVerifyPositiveMPInt(raw []byte) (*big.Int, bool) {
	if len(raw) == 0 || raw[0]&0x80 != 0 || raw[0] == 0 && (len(raw) == 1 || raw[1]&0x80 == 0) {
		return nil, false
	}
	n := new(big.Int).SetBytes(raw)
	return n, n.Sign() > 0
}

// Parse the complete supported public-key wire format, including unrelated
// literal rows. A valid type prefix alone is not a valid host key.
func credentialVerifyPublicHostKey(kind string, key []byte) bool {
	if len(key) > 16*1024 {
		return false
	}
	algorithm, rest, ok := credentialVerifySSHString(key)
	if !ok || string(algorithm) != kind {
		return false
	}
	switch kind {
	case "ssh-ed25519":
		public, tail, ok := credentialVerifySSHString(rest)
		return ok && len(public) == 32 && len(tail) == 0
	case "ssh-rsa":
		exponent, rest, ok := credentialVerifySSHString(rest)
		if !ok {
			return false
		}
		modulus, tail, ok := credentialVerifySSHString(rest)
		if !ok || len(tail) != 0 {
			return false
		}
		e, eOK := credentialVerifyPositiveMPInt(exponent)
		n, nOK := credentialVerifyPositiveMPInt(modulus)
		return eOK && nOK && e.BitLen() <= 31 && e.Int64() >= 3 && e.Bit(0) == 1 &&
			n.BitLen() >= 1024 && n.BitLen() <= 16384 && n.Bit(0) == 1 && n.Cmp(e) > 0
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		curveName, rest, ok := credentialVerifySSHString(rest)
		if !ok || "ecdsa-sha2-"+string(curveName) != kind {
			return false
		}
		point, tail, ok := credentialVerifySSHString(rest)
		if !ok || len(tail) != 0 {
			return false
		}
		var curve elliptic.Curve
		switch string(curveName) {
		case "nistp256":
			curve = elliptic.P256()
		case "nistp384":
			curve = elliptic.P384()
		case "nistp521":
			curve = elliptic.P521()
		default:
			return false
		}
		x, y := elliptic.Unmarshal(curve, point)
		return x != nil && y != nil
	default:
		return false
	}
}

func verifyCredentialKnownHosts(raw []byte, r policy.CredentialVerificationRequest) error {
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != r.SSH.KnownHostsSHA256 || !credentialVerifyLiteralHost(r.SSH.Alias) {
		return credentialVerifyError("transport_hostkey")
	}
	matches := 0
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) < 3 || !credentialVerifyLiteralHost(fields[0]) {
			return credentialVerifyError("transport_hostkey")
		}
		key, err := base64.StdEncoding.Strict().DecodeString(fields[2])
		if err != nil || base64.StdEncoding.EncodeToString(key) != fields[2] || !credentialVerifyPublicHostKey(fields[1], key) {
			return credentialVerifyError("transport_hostkey")
		}
		if fields[0] != r.SSH.Alias {
			continue
		}
		fp := sha256.Sum256(key)
		if "SHA256:"+base64.RawStdEncoding.EncodeToString(fp[:]) != r.SSH.HostKeySHA256 {
			return credentialVerifyError("transport_hostkey")
		}
		matches++
	}
	if matches != 1 {
		return credentialVerifyError("transport_hostkey")
	}
	return nil
}

func credentialVerifyTransportPreflight(r policy.CredentialVerificationRequest) (func() error, error) {
	identity, err := trustedCredentialTransportFile(r.SSH.IdentityFile, true)
	if err != nil {
		return nil, err
	}
	known, err := trustedCredentialTransportFile(r.SSH.KnownHostsFile, false)
	if err != nil || known.Size() > 1024*1024 {
		return nil, credentialVerifyError("transport_custody")
	}
	// SSH alone owns private-key bytes. Only the public known-hosts file is read.
	fd, err := os.OpenFile(r.SSH.KnownHostsFile, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, credentialVerifyError("transport_custody")
	}
	fi, fe := fd.Stat()
	raw, re := io.ReadAll(io.LimitReader(fd, 1024*1024+1))
	after, ae := fd.Stat()
	ce := fd.Close()
	if fe != nil || ae != nil || re != nil || ce != nil || !credentialVerifySameTransportMetadata(known, fi) || !credentialVerifySameTransportMetadata(fi, after) || int64(len(raw)) != known.Size() {
		return nil, credentialVerifyError("transport_custody")
	}
	if err = verifyCredentialKnownHosts(raw, r); err != nil {
		return nil, err
	}
	return func() error {
		for _, item := range []struct {
			path    string
			before  os.FileInfo
			private bool
		}{{r.SSH.IdentityFile, identity, true}, {r.SSH.KnownHostsFile, known, false}} {
			after, err := trustedCredentialTransportFile(item.path, item.private)
			if err != nil || !credentialVerifySameTransportMetadata(item.before, after) {
				return credentialVerifyError("transport_custody_drift")
			}
		}
		return nil
	}, nil
}

func credentialVerifySameTransportMetadata(a, b os.FileInfo) bool {
	if a == nil || b == nil || !os.SameFile(a, b) || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) || a.Mode() != b.Mode() {
		return false
	}
	x, xok := a.Sys().(*syscall.Stat_t)
	y, yok := b.Sys().(*syscall.Stat_t)
	if !xok || !yok || x.Uid != y.Uid || x.Gid != y.Gid || x.Nlink != y.Nlink {
		return false
	}
	// Darwin and Linux expose the same exact ctime with different field names.
	for _, name := range []string{"Ctim", "Ctimespec"} {
		cx, cy := reflect.ValueOf(x).Elem().FieldByName(name), reflect.ValueOf(y).Elem().FieldByName(name)
		if cx.IsValid() && cy.IsValid() {
			return reflect.DeepEqual(cx.Interface(), cy.Interface())
		}
	}
	return false
}

func credentialVerifyShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func credentialVerifySSHArgs(r policy.CredentialVerificationRequest) []string {
	// No ambient config, proxy, agent, remote command or credential argv.
	command := "/usr/bin/sudo -n -- /usr/bin/python3 -I -B -c " + credentialVerifyShellQuote(credentialVerificationProgram) + " " + credentialVerifyShellQuote(CredentialVerificationProgramSHA256())
	return []string{"-F", "/dev/null", "-T", "-p", "22", "-l", r.SSH.User, "-i", r.SSH.IdentityFile,
		"-o", "HostName=" + r.Resource.Address, "-o", "HostKeyAlias=" + r.SSH.Alias,
		"-o", "UserKnownHostsFile=" + r.SSH.KnownHostsFile, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "CheckHostIP=no",
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
		"-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "RequestTTY=no",
		"-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ProxyCommand=none", "-o", "ProxyJump=none",
		"-o", "PermitLocalCommand=no", "-o", "LocalCommand=none", "-o", "AddKeysToAgent=no",
		"-o", "PreferredAuthentications=publickey", "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
		"-o", "ConnectionAttempts=1", "-o", "ConnectTimeout=8", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1",
		"--", r.SSH.Alias, command}
}

type credentialVerifyLimitedOutput struct {
	mu       sync.Mutex
	data     []byte
	overflow bool
}

func (b *credentialVerifyLimitedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := 32769 - len(b.data)
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	if len(b.data) > 32768 {
		b.overflow = true
	}
	return n, nil
}

type credentialVerifyNativeChild struct {
	cmd    *exec.Cmd
	stdout credentialVerifyLimitedOutput
	stderr credentialVerifyStageOutput
	done   chan struct{}
	failed bool
}

func (t credentialVerifyNativeTransport) Start(ctx context.Context, r policy.CredentialVerificationRequest, deadline time.Time, guard credentialVerifyAdmissionGuard) (credentialVerifyChild, error) {
	if ctx.Err() != nil || !time.Now().Before(deadline) || guard == nil {
		return nil, credentialVerifyError("transport_deadline")
	}
	recheck, err := credentialVerifyTransportPreflight(r)
	if err != nil {
		return nil, err
	}
	wire := struct {
		Request    policy.CredentialVerificationRequest `json:"request"`
		IssuedNS   string                               `json:"issued_ns"`
		DeadlineNS string                               `json:"deadline_ns"`
	}{Request: r, IssuedNS: strconv.FormatInt(time.Now().UnixNano(), 10), DeadlineNS: strconv.FormatInt(deadline.UnixNano(), 10)}
	payload, err := json.Marshal(wire)
	if err != nil || len(payload) > 32768 {
		return nil, credentialVerifyError("transport_request")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", credentialVerifySSHArgs(r)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "LANG=C"}
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(payload)
	child := &credentialVerifyNativeChild{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &child.stdout
	// Only a finite singleton marker can become failed-only metadata. Raw remote
	// diagnostics never become an error message, observation, or artifact.
	cmd.Stderr = &child.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	started := false
	// The owner admits this exact start callback while holding native admission.
	// Reap immediately after successful Start, including later evidence failure.
	err = guard(func() error {
		if err := recheck(); err != nil {
			return err
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return credentialVerifyError("transport_deadline")
		}
		if err := cmd.Start(); err != nil {
			return credentialVerifyError("transport_start")
		}
		started = true
		go func() { child.failed = cmd.Wait() != nil; close(child.done) }()
		return nil
	})
	if err != nil {
		if started {
			return child, credentialVerifyError("transport_launch")
		}
		return nil, err
	}
	return child, nil
}

func credentialVerifyKnownFailureStage(stage string) bool {
	switch stage {
	case "request_validation", "executable", "host_identity", "protected_file", "source_format", "pre_catalog", "app_existing", "app_wrong", "migrator_existing", "migrator_wrong", "post_catalog", "postchecks", "deadline", "cleanup":
		return true
	}
	return false
}

type credentialVerifyStageError struct{ stage string }

func (credentialVerifyStageError) Error() string { return "credential_verify:verification_failed" }

func credentialVerifySafeFailureStage(err error) string {
	var staged credentialVerifyStageError
	if errors.As(err, &staged) && credentialVerifyKnownFailureStage(staged.stage) && staged.stage != "cleanup" {
		return staged.stage
	}
	return ""
}

type credentialVerifyStageOutput struct {
	mu      sync.Mutex
	data    []byte
	present bool
	invalid bool
}

func (b *credentialVerifyStageOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.present = b.present || n > 0
	if b.invalid || n > 96-len(b.data) {
		b.invalid = true
		b.data = nil
		return n, nil
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *credentialVerifyStageOutput) result() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const prefix = "credential_verify:refused:"
	if !b.invalid {
		marker := string(b.data)
		stage := strings.TrimSuffix(strings.TrimPrefix(marker, prefix), "\n")
		if credentialVerifyKnownFailureStage(stage) && marker == prefix+stage+"\n" {
			return stage, true
		}
	}
	return "", b.present
}
func (c *credentialVerifyNativeChild) Wait() ([]byte, error) {
	<-c.done
	c.stdout.mu.Lock()
	defer c.stdout.mu.Unlock()
	stage, stderr := c.stderr.result()
	if c.stdout.overflow {
		return nil, credentialVerifyError("transport_failed")
	}
	if c.failed {
		if stage != "" {
			return nil, credentialVerifyStageError{stage: stage}
		}
		return nil, credentialVerifyError("transport_failed")
	}
	if stderr {
		return nil, credentialVerifyError("transport_failed")
	}
	return append([]byte(nil), c.stdout.data...), nil
}
func (c *credentialVerifyNativeChild) Stop(ctx context.Context) error {
	select {
	case <-c.done:
		return nil
	default:
	}
	if c.cmd.Process != nil {
		if err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return credentialVerifyError("transport_cleanup")
		}
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return credentialVerifyError("transport_cleanup_deadline")
	}
}
