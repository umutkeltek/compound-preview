package act

import (
	"bytes"
	"context"
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

//go:embed runtime_config_install_program.py
var runtimeConfigInstallProgramBody string

func runtimeConfigInstallProgram() string {
	return "import types\n_VERIFIER=types.ModuleType(" + strconv.Quote("closed_verifier") + ")\nexec(" + strconv.Quote(credentialVerificationProgram) + ", _VERIFIER.__dict__)\n" + runtimeConfigInstallProgramBody
}

func RuntimeConfigInstallProgramSHA256() string {
	sum := sha256.Sum256([]byte(runtimeConfigInstallProgram()))
	return hex.EncodeToString(sum[:])
}

// The owner holds native admission while invoking the exact fixed OS start.
type runtimeConfigInstallAdmissionGuard func(start func() error) error
type runtimeConfigInstallNativeTransport struct {
	home      string
	alignment *CredentialPasswordAligner
}

func trustedRuntimeConfigInstallFile(path string, private bool) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, runtimeConfigInstallError("transport_custody")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, runtimeConfigInstallError("transport_custody")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(st.Uid) != os.Geteuid() && st.Uid != 0) || st.Nlink != 1 || info.Mode().Perm()&0022 != 0 {
		return nil, runtimeConfigInstallError("transport_custody")
	}
	if private && info.Mode().Perm() != 0600 {
		return nil, runtimeConfigInstallError("transport_custody")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		p, err := os.Lstat(parent)
		if err != nil || !p.IsDir() || p.Mode()&os.ModeSymlink != 0 || p.Mode().Perm()&0022 != 0 {
			return nil, runtimeConfigInstallError("transport_custody")
		}
		ps, ok := p.Sys().(*syscall.Stat_t)
		if !ok || (int(ps.Uid) != os.Geteuid() && ps.Uid != 0) {
			return nil, runtimeConfigInstallError("transport_custody")
		}
		if parent == string(filepath.Separator) {
			break
		}
	}
	return info, nil
}

func runtimeConfigInstallLiteralHost(host string) bool {
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

func runtimeConfigInstallSSHString(raw []byte) (value, rest []byte, ok bool) {
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

func runtimeConfigInstallPositiveMPInt(raw []byte) (*big.Int, bool) {
	if len(raw) == 0 || raw[0]&0x80 != 0 || raw[0] == 0 && (len(raw) == 1 || raw[1]&0x80 == 0) {
		return nil, false
	}
	n := new(big.Int).SetBytes(raw)
	return n, n.Sign() > 0
}

func runtimeConfigInstallPublicHostKey(kind string, key []byte) bool {
	if len(key) > 16*1024 {
		return false
	}
	algorithm, rest, ok := runtimeConfigInstallSSHString(key)
	if !ok || string(algorithm) != kind {
		return false
	}
	switch kind {
	case "ssh-ed25519":
		public, tail, ok := runtimeConfigInstallSSHString(rest)
		return ok && len(public) == 32 && len(tail) == 0
	case "ssh-rsa":
		exponent, rest, ok := runtimeConfigInstallSSHString(rest)
		if !ok {
			return false
		}
		modulus, tail, ok := runtimeConfigInstallSSHString(rest)
		if !ok || len(tail) != 0 {
			return false
		}
		e, eOK := runtimeConfigInstallPositiveMPInt(exponent)
		n, nOK := runtimeConfigInstallPositiveMPInt(modulus)
		return eOK && nOK && e.BitLen() <= 31 && e.Int64() >= 3 && e.Bit(0) == 1 &&
			n.BitLen() >= 1024 && n.BitLen() <= 16384 && n.Bit(0) == 1 && n.Cmp(e) > 0
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		curveName, rest, ok := runtimeConfigInstallSSHString(rest)
		if !ok || "ecdsa-sha2-"+string(curveName) != kind {
			return false
		}
		point, tail, ok := runtimeConfigInstallSSHString(rest)
		if !ok || len(tail) != 0 || len(point) == 0 {
			return false
		}
		return true
	default:
		return false
	}
}

func verifyRuntimeConfigInstallKnownHosts(raw []byte, r policy.RuntimeConfigInstallRequest) error {
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != r.SSH.KnownHostsSHA256 || !runtimeConfigInstallLiteralHost(r.SSH.Alias) {
		return runtimeConfigInstallError("transport_hostkey")
	}
	matches := 0
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) < 3 || !runtimeConfigInstallLiteralHost(fields[0]) {
			return runtimeConfigInstallError("transport_hostkey")
		}
		key, err := base64.StdEncoding.Strict().DecodeString(fields[2])
		if err != nil || base64.StdEncoding.EncodeToString(key) != fields[2] || !runtimeConfigInstallPublicHostKey(fields[1], key) {
			return runtimeConfigInstallError("transport_hostkey")
		}
		if fields[0] != r.SSH.Alias {
			continue
		}
		fp := sha256.Sum256(key)
		if "SHA256:"+base64.RawStdEncoding.EncodeToString(fp[:]) != r.SSH.HostKeySHA256 {
			return runtimeConfigInstallError("transport_hostkey")
		}
		matches++
	}
	if matches != 1 {
		return runtimeConfigInstallError("transport_hostkey")
	}
	return nil
}

func runtimeConfigInstallTransportPreflight(r policy.RuntimeConfigInstallRequest) (func() error, error) {
	identity, err := trustedRuntimeConfigInstallFile(r.SSH.IdentityFile, true)
	if err != nil {
		return nil, err
	}
	known, err := trustedRuntimeConfigInstallFile(r.SSH.KnownHostsFile, false)
	if err != nil || known.Size() > 1024*1024 {
		return nil, runtimeConfigInstallError("transport_custody")
	}
	raw, err := runtimeConfigInstallReadTransportFile(r.SSH.KnownHostsFile, known)
	if err != nil {
		return nil, err
	}
	if err = verifyRuntimeConfigInstallKnownHosts(raw, r); err != nil {
		return nil, err
	}
	return func() error {
		for _, item := range []struct {
			path    string
			before  os.FileInfo
			private bool
		}{{r.SSH.IdentityFile, identity, true}, {r.SSH.KnownHostsFile, known, false}} {
			after, err := trustedRuntimeConfigInstallFile(item.path, item.private)
			if err != nil || !runtimeConfigInstallSameTransportMetadata(item.before, after) {
				return runtimeConfigInstallError("transport_custody_drift")
			}
		}
		return nil
	}, nil
}

// An expected regular path may be replaced after lstat. Open nonblocking and
// validate the actual descriptor before reading so FIFOs cannot hold admission.
func runtimeConfigInstallReadTransportFile(path string, expected os.FileInfo) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, runtimeConfigInstallError("transport_custody") }
	fd, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return fail()
	}
	defer fd.Close()
	before, e := fd.Stat()
	if e != nil || !before.Mode().IsRegular() || !runtimeConfigInstallSameTransportMetadata(expected, before) {
		return fail()
	}
	raw, e := io.ReadAll(io.LimitReader(fd, 1024*1024+1))
	after, ae := fd.Stat()
	ce := fd.Close()
	if e != nil || ae != nil || ce != nil || !runtimeConfigInstallSameTransportMetadata(before, after) || int64(len(raw)) != expected.Size() || len(raw) > 1024*1024 {
		return fail()
	}
	return raw, nil
}

func runtimeConfigInstallSameTransportMetadata(a, b os.FileInfo) bool {
	if a == nil || b == nil || !os.SameFile(a, b) || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) || a.Mode() != b.Mode() {
		return false
	}
	x, xok := a.Sys().(*syscall.Stat_t)
	y, yok := b.Sys().(*syscall.Stat_t)
	if !xok || !yok || x.Uid != y.Uid || x.Gid != y.Gid || x.Nlink != y.Nlink {
		return false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		cx := reflect.ValueOf(x).Elem().FieldByName(name)
		cy := reflect.ValueOf(y).Elem().FieldByName(name)
		if cx.IsValid() && cy.IsValid() && !reflect.DeepEqual(cx.Interface(), cy.Interface()) {
			return false
		}
	}
	return true
}

func runtimeConfigInstallShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func runtimeConfigInstallSSHArgs(r policy.RuntimeConfigInstallRequest) []string {
	command := "/usr/bin/sudo -n -- /usr/bin/python3 -I -B -c " + runtimeConfigInstallShellQuote(runtimeConfigInstallProgram()) + " " + runtimeConfigInstallShellQuote(RuntimeConfigInstallProgramSHA256())
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

type runtimeConfigInstallLimitedOutput struct {
	mu       sync.Mutex
	data     []byte
	overflow bool
}

func (b *runtimeConfigInstallLimitedOutput) Write(p []byte) (int, error) {
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

type runtimeConfigInstallNativeChild struct {
	cmd    *exec.Cmd
	stdout runtimeConfigInstallLimitedOutput
	stderr runtimeConfigInstallLimitedOutput
	done   chan struct{}
	failed bool
}

func (t runtimeConfigInstallNativeTransport) Start(ctx context.Context, r policy.RuntimeConfigInstallRequest, selected map[string]string, operation string, deadline time.Time, guard runtimeConfigInstallAdmissionGuard) (runtimeConfigInstallChild, error) {
	if ctx.Err() != nil || !time.Now().Before(deadline) || guard == nil {
		return nil, runtimeConfigInstallError("transport_deadline")
	}
	recheck, err := runtimeConfigInstallTransportPreflight(r)
	if err != nil {
		return nil, err
	}
	owner := &RuntimeConfigInstaller{home: t.home, now: time.Now, alignment: t.alignment}
	acceptances, err := owner.alignmentAcceptances(r)
	if err != nil {
		return nil, err
	}
	if acceptances == nil {
		acceptances = []credentialPasswordAlignmentAcceptance{}
	}
	wire := struct {
		Request              policy.RuntimeConfigInstallRequest      `json:"request"`
		Selected             map[string]string                       `json:"selected"`
		Operation            string                                  `json:"operation"`
		IssuedNS             string                                  `json:"issued_ns"`
		DeadlineNS           string                                  `json:"deadline_ns"`
		AlignmentAcceptances []credentialPasswordAlignmentAcceptance `json:"alignment_acceptances"`
	}{Request: r, Selected: selected, Operation: operation, IssuedNS: strconv.FormatInt(time.Now().UnixNano(), 10), DeadlineNS: strconv.FormatInt(deadline.UnixNano(), 10), AlignmentAcceptances: acceptances}
	payload, err := json.Marshal(wire)
	if err != nil || len(payload) > 131072 {
		return nil, runtimeConfigInstallError("transport_request")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", runtimeConfigInstallSSHArgs(r)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "LANG=C"}
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(payload)
	child := &runtimeConfigInstallNativeChild{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &child.stdout
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
	err = guard(func() error {
		if err := recheck(); err != nil {
			return err
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return runtimeConfigInstallError("transport_deadline")
		}
		if err := cmd.Start(); err != nil {
			return runtimeConfigInstallError("transport_start")
		}
		started = true
		go func() { child.failed = cmd.Wait() != nil; close(child.done) }()
		return nil
	})
	if err != nil {
		if started {
			return child, runtimeConfigInstallError("transport_launch")
		}
		return nil, err
	}
	return child, nil
}

type runtimeConfigInstallDiscard struct{}

func (runtimeConfigInstallDiscard) Write(p []byte) (int, error) { return len(p), nil }
func (c *runtimeConfigInstallNativeChild) Wait() ([]byte, error) {
	<-c.done
	c.stdout.mu.Lock()
	defer c.stdout.mu.Unlock()
	if c.failed || c.stdout.overflow || len(c.stderr.data) > 0 || c.stderr.overflow {
		return nil, runtimeConfigInstallError("transport_failed")
	}
	return append([]byte(nil), c.stdout.data...), nil
}
func (c *runtimeConfigInstallNativeChild) Stop(ctx context.Context) error {
	select {
	case <-c.done:
		return nil
	default:
	}
	if c.cmd.Process != nil {
		if err := syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return runtimeConfigInstallError("transport_cleanup")
		}
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return runtimeConfigInstallError("transport_cleanup_deadline")
	}
}
