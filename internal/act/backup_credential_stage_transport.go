package act

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"time"

	"compound/internal/policy"
	"golang.org/x/sys/unix"
)

//go:embed backup_credential_stage_program.py
var backupCredentialStageProgram string

func BackupCredentialStageProgramSHA256() string {
	return runtimeConfigBytesHash([]byte(backupCredentialStageProgram))
}

type backupCredentialStageAdmissionGuard = runtimeConfigInstallAdmissionGuard
type backupCredentialStageNativeTransport struct{}

func backupCredentialStageFileStat(info os.FileInfo, sha string) (policy.RuntimeConfigInstallStat, error) {
	s, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return policy.RuntimeConfigInstallStat{}, backupCredentialStageError("source_stat")
	}
	ct := reflect.ValueOf(s).Elem().FieldByName("Ctim")
	if !ct.IsValid() {
		ct = reflect.ValueOf(s).Elem().FieldByName("Ctimespec")
	}
	if !ct.IsValid() {
		return policy.RuntimeConfigInstallStat{}, backupCredentialStageError("source_stat")
	}
	ns := ct.FieldByName("Sec").Int()*1_000_000_000 + ct.FieldByName("Nsec").Int()
	return policy.RuntimeConfigInstallStat{Device: uint64(s.Dev), Inode: uint64(s.Ino), Bytes: info.Size(), MtimeNS: strconv.FormatInt(info.ModTime().UnixNano(), 10), CtimeNS: strconv.FormatInt(ns, 10), Mode: fmt.Sprintf("%04o", info.Mode().Perm()), UID: s.Uid, GID: s.Gid, Links: uint64(s.Nlink), SHA256: sha}, nil
}
func backupCredentialStageMatch(info os.FileInfo, want policy.RuntimeConfigInstallStat) bool {
	actual, e := backupCredentialStageFileStat(info, want.SHA256)
	return e == nil && actual == want
}
func backupCredentialStageSourceMetadata(r policy.BackupCredentialStageRequest) error {
	// No content is opened here. Planning only inspects exact custody metadata.
	root, e := os.Lstat(r.SourceDirectory)
	if e != nil || !root.IsDir() || root.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !backupCredentialStageMatch(root, r.SourceDirectoryStat) || r.SourceDirectoryStat.UID != uint32(os.Geteuid()) {
		return backupCredentialStageError("source_directory")
	}
	for parent := filepath.Dir(r.SourceDirectory); ; parent = filepath.Dir(parent) {
		info, e := os.Lstat(parent)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return backupCredentialStageError("source_ancestor")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) || (info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0) {
			return backupCredentialStageError("source_ancestor")
		}
		if parent == string(filepath.Separator) {
			break
		}
	}
	for _, f := range r.Files {
		info, e := os.Lstat(f.SourcePath)
		if e != nil || f.Stat == nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !backupCredentialStageMatch(info, *f.Stat) {
			return backupCredentialStageError("source_metadata")
		}
	}
	return nil
}
func backupCredentialStageReadSources(r policy.BackupCredentialStageRequest) ([4][]byte, error) {
	var out [4][]byte
	fail := func() ([4][]byte, error) {
		for i := range out {
			clear(out[i])
		}
		return [4][]byte{}, backupCredentialStageError("source_custody")
	}
	if backupCredentialStageSourceMetadata(r) != nil {
		return fail()
	}
	n, e := unix.Open(r.SourceDirectory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return fail()
	}
	root := os.NewFile(uintptr(n), "backup-stage-source")
	defer root.Close()
	info, e := root.Stat()
	if e != nil || !backupCredentialStageMatch(info, r.SourceDirectoryStat) {
		return fail()
	}
	for i, f := range r.Files {
		raw, e := backupCredentialStageReadSourceAt(n, f)
		if e != nil {
			return fail()
		}
		out[i] = raw
	}
	info, e = root.Stat()
	if e != nil || !backupCredentialStageMatch(info, r.SourceDirectoryStat) || backupCredentialStageSourceMetadata(r) != nil {
		return fail()
	}
	return out, nil
}
func backupCredentialStageReadSourceAt(directory int, f policy.BackupCredentialStageFile) ([]byte, error) {
	fail := func() ([]byte, error) { return nil, backupCredentialStageError("source_custody") }
	fd, e := unix.Openat(directory, f.Basename, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return fail()
	}
	file := os.NewFile(uintptr(fd), "backup-stage-credential")
	defer file.Close()
	before, e := file.Stat()
	if e != nil || f.Stat == nil || !before.Mode().IsRegular() || !backupCredentialStageMatch(before, *f.Stat) {
		return fail()
	}
	raw, e := io.ReadAll(io.LimitReader(file, 65537))
	after, statErr := file.Stat()
	if e != nil || statErr != nil || len(raw) > 65536 || int64(len(raw)) != f.Stat.Bytes || !backupCredentialStageMatch(after, *f.Stat) || (f.Stat.SHA256 != "" && runtimeConfigBytesHash(raw) != f.Stat.SHA256) {
		clear(raw)
		return fail()
	}
	return raw, nil
}

func backupCredentialStageSSHRequest(r policy.BackupCredentialStageRequest) policy.RuntimeConfigInstallRequest {
	return policy.RuntimeConfigInstallRequest{Resource: r.Resource, SSH: policy.RuntimeConfigInstallSSH{Alias: r.SSH.Alias, User: r.SSH.User, Port: 22, IdentityFile: r.SSH.IdentityFile, KnownHostsFile: r.SSH.KnownHostsFile, KnownHostsSHA256: r.SSH.KnownHostsSHA256, HostKeySHA256: r.SSH.HostKeySHA256}}
}
func backupCredentialStageTransportPins(r policy.BackupCredentialStageRequest) (func() error, error) {
	old := backupCredentialStageSSHRequest(r)
	recheck, e := runtimeConfigInstallTransportPreflight(old)
	if e != nil {
		return nil, backupCredentialStageError("ssh_custody")
	}
	pinned := []struct {
		path string
		stat policy.RuntimeConfigInstallStat
	}{{r.SSH.IdentityFile, r.SSH.IdentityStat}, {r.SSH.KnownHostsFile, r.SSH.KnownHostsStat}}
	check := func() error {
		if recheck() != nil {
			return backupCredentialStageError("ssh_custody")
		}
		for _, p := range pinned {
			s, e := os.Lstat(p.path)
			if e != nil || !backupCredentialStageMatch(s, p.stat) {
				return backupCredentialStageError("ssh_metadata")
			}
		}
		return nil
	}
	ssh, e := trustedRuntimeConfigInstallFile("/usr/bin/ssh", false)
	if e != nil || !ssh.Mode().IsRegular() {
		return nil, backupCredentialStageError("ssh_executable")
	}
	file, e := os.OpenFile("/usr/bin/ssh", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, backupCredentialStageError("ssh_executable")
	}
	raw, e := io.ReadAll(io.LimitReader(file, 32*1024*1024))
	file.Close()
	if e != nil || runtimeConfigBytesHash(raw) != r.SSH.SSHSHA256 {
		return nil, backupCredentialStageError("ssh_executable")
	}
	if e = check(); e != nil {
		return nil, e
	}
	return func() error {
		if e := check(); e != nil {
			return e
		}
		current, e := os.Lstat("/usr/bin/ssh")
		if e != nil || !runtimeConfigInstallSameTransportMetadata(ssh, current) {
			return backupCredentialStageError("ssh_executable")
		}
		return nil
	}, nil
}
func (backupCredentialStageNativeTransport) Start(ctx context.Context, r policy.BackupCredentialStageRequest, selected map[string]string, operation string, deadline time.Time, guard backupCredentialStageAdmissionGuard) (backupCredentialStageChild, error) {
	if guard == nil {
		return nil, backupCredentialStageError("native_guard_required")
	}
	recheck, e := backupCredentialStageTransportPins(r)
	if e != nil {
		return nil, e
	}
	args := runtimeConfigInstallSSHArgs(backupCredentialStageSSHRequest(r))
	command := "/usr/bin/sudo -n -- /usr/bin/python3 -I -B -c " + runtimeConfigInstallShellQuote(backupCredentialStageProgram) + " " + runtimeConfigInstallShellQuote(BackupCredentialStageProgramSHA256())
	args[len(args)-1] = command
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "LANG=C"}
	cmd.Dir = "/"
	child := &runtimeConfigInstallNativeChild{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &child.stdout
	cmd.Stderr = &child.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(e, syscall.ESRCH) {
			return nil
		}
		return e
	}
	started := false
	e = guard(func() error {
		if recheck() != nil || ctx.Err() != nil || !time.Now().Before(deadline) {
			return backupCredentialStageError("transport_precondition")
		}
		credentials, e := backupCredentialStageReadSources(r)
		if e != nil {
			return e
		}
		defer func() {
			for i := range credentials {
				clear(credentials[i])
			}
		}()
		var hashes [4]string
		for i, raw := range credentials {
			hashes[i] = runtimeConfigBytesHash(raw)
			selected[fmt.Sprintf("source_sha256_%d", i)] = hashes[i]
		}
		wire := struct {
			Request          policy.BackupCredentialStageRequest `json:"request"`
			CredentialSHA256 [4]string                           `json:"credential_sha256"`
			Credentials      [4][]byte                           `json:"credentials"`
			Operation        string                              `json:"operation"`
			DeadlineNS       string                              `json:"deadline_ns"`
		}{r, hashes, credentials, operation, strconv.FormatInt(deadline.UnixNano(), 10)}
		payload, e := json.Marshal(wire)
		if e != nil || len(payload) > 524288 {
			return backupCredentialStageError("transport_payload")
		}
		cmd.Stdin = bytes.NewReader(payload)
		if recheck() != nil || ctx.Err() != nil || !time.Now().Before(deadline) {
			clear(payload)
			return backupCredentialStageError("transport_precondition")
		}
		if e = cmd.Start(); e != nil {
			clear(payload)
			return backupCredentialStageError("transport_start")
		}
		started = true
		go func() { child.failed = cmd.Wait() != nil; clear(payload); close(child.done) }()
		return nil
	})
	if e != nil {
		if started {
			return child, e
		}
		return nil, e
	}
	return child, nil
}
