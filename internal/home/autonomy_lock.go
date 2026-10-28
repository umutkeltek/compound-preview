package home

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const autonomyAdmissionLockEpochPrefix = "compound.autonomy_admission_lock.v1:"

// WithAutonomyAdmissionLock serializes the evidence snapshot and admission
// decision with the canonical writers that can change that decision: work
// lifecycle, decision grants, route evidence/router authority, and the halt.
// Advisory locks are released with the file descriptor after a crash.
// Compound's supported runtime targets are Darwin and Linux.
func WithAutonomyAdmissionLock(homePath string, fn func() error) (resultErr error) {
	lockPath, err := autonomyAdmissionLockPath(homePath)
	if err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("autonomy admission lock callback required")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	prior, err := readAutonomyAdmissionLockBytes(file)
	if err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, writeAutonomyAdmissionLockBytes(file, prior))
		resultErr = errors.Join(resultErr, syscall.Flock(int(file.Fd()), syscall.LOCK_UN))
	}()
	epoch, err := newAutonomyAdmissionLockEpoch()
	if err != nil {
		return err
	}
	if err := writeAutonomyAdmissionLockBytes(file, []byte(autonomyAdmissionLockEpochPrefix+epoch+"\n")); err != nil {
		return err
	}
	return fn()
}

// AutonomyAdmissionLockState returns whether the canonical admission lock is
// held and, when available, the validated epoch written by that acquisition.
// Bytes left behind by a crashed holder are ignored whenever the flock is free.
func AutonomyAdmissionLockState(homePath string) (held bool, epoch string, err error) {
	file, missing, err := openAutonomyAdmissionLockForProbe(homePath)
	if err != nil || missing {
		return false, "", err
	}
	defer file.Close()
	held, err = autonomyAdmissionLockFileHeld(file)
	if err != nil || !held {
		return held, "", err
	}
	first, err := readAutonomyAdmissionLockEpoch(file)
	if err != nil {
		return true, "", err
	}
	held, err = autonomyAdmissionLockFileHeld(file)
	if err != nil || !held {
		return held, "", err
	}
	second, err := readAutonomyAdmissionLockEpoch(file)
	if err != nil {
		return true, "", err
	}
	if first == "" || first != second {
		return true, "", nil
	}
	return true, first, nil
}

// AutonomyAdmissionLockHeld probes the canonical admission lock without
// waiting or creating runtime state. It reports whether another open file
// description currently holds the lock.
func AutonomyAdmissionLockHeld(homePath string) (bool, error) {
	file, missing, err := openAutonomyAdmissionLockForProbe(homePath)
	if err != nil || missing {
		return false, err
	}
	defer file.Close()
	return autonomyAdmissionLockFileHeld(file)
}

func openAutonomyAdmissionLockForProbe(homePath string) (*os.File, bool, error) {
	lockPath, err := autonomyAdmissionLockPath(homePath)
	if err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	return file, false, err
}

func autonomyAdmissionLockFileHeld(file *os.File) (bool, error) {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return false, err
	}
	return false, nil
}

func newAutonomyAdmissionLockEpoch() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate autonomy admission lock epoch: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func readAutonomyAdmissionLockBytes(file *os.File) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func writeAutonomyAdmissionLockBytes(file *os.File, body []byte) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	written, err := file.Write(body)
	if err != nil {
		return err
	}
	if written != len(body) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func readAutonomyAdmissionLockEpoch(file *os.File) (string, error) {
	body, err := readAutonomyAdmissionLockBytes(file)
	if err != nil {
		return "", err
	}
	text := string(body)
	if !strings.HasPrefix(text, autonomyAdmissionLockEpochPrefix) || !strings.HasSuffix(text, "\n") {
		return "", nil
	}
	epoch := strings.TrimSuffix(strings.TrimPrefix(text, autonomyAdmissionLockEpochPrefix), "\n")
	if len(epoch) != 64 {
		return "", nil
	}
	decoded, err := hex.DecodeString(epoch)
	if err != nil || hex.EncodeToString(decoded) != epoch {
		return "", nil
	}
	return epoch, nil
}

func autonomyAdmissionLockPath(homePath string) (string, error) {
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return "", fmt.Errorf("COMPOUND_HOME required")
	}
	return filepath.Join(homePath, "lock", "autonomy-admission.lock"), nil
}
