package home

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

const (
	workAdmissionRecoveryMarkerRelative = "lock/capability-lifecycle-work-admission/recovery-required.json"
	workAdmissionRecoveryMarkerSchema   = "compound.work_admission_recovery.v0.1"
)

var (
	workAdmissionRecoverySequence uint64

	workAdmissionRecoveryOpenFile = func(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
		return root.OpenFile(name, flag, perm)
	}
	workAdmissionRecoveryWrite = func(file *os.File, data []byte) (int, error) {
		return file.Write(data)
	}
	workAdmissionRecoveryFileSync = func(file *os.File) error {
		return file.Sync()
	}
	workAdmissionRecoveryRename = func(root *os.Root, oldName, newName string) error {
		return root.Rename(oldName, newName)
	}
	workAdmissionRecoveryParentSync = func(file *os.File) error {
		return file.Sync()
	}
)

type workAdmissionRecoveryMarker struct {
	SchemaVersion string `json:"schema_version"`
	Status        string `json:"status"`
	WorkID        string `json:"work_id"`
	Reason        string `json:"reason"`
}

func workAdmissionRecoveryFence(homePath string) error {
	path := filepath.Join(homePath, filepath.FromSlash(workAdmissionRecoveryMarkerRelative))
	data, present, err := ReadRootedRegularFileCanonicalOnly(
		homePath,
		path,
		"work admission recovery marker",
	)
	if err != nil {
		return fmt.Errorf("work admission recovery_required: inspect recovery marker: %w", err)
	}
	if !present {
		return nil
	}
	marker, err := decodeWorkAdmissionRecoveryMarker(data)
	if err != nil {
		return fmt.Errorf("work admission recovery_required: invalid recovery marker: %w", err)
	}
	return fmt.Errorf(
		"work admission recovery_required: work authority custody was lost during %s: %s",
		marker.WorkID,
		marker.Reason,
	)
}

func publishWorkAdmissionRecoveryRequired(root *workAdmissionRoot, workID string, cause error) error {
	if root == nil || root.root == nil {
		return errors.New("persist work admission recovery_required marker: rooted home unavailable")
	}
	if err := root.verifyHomeIdentity(); err != nil {
		return fmt.Errorf("persist work admission recovery_required marker: %w", err)
	}
	parent := filepath.Dir(filepath.FromSlash(workAdmissionRecoveryMarkerRelative))
	if _, err := root.snapshotDirectory(parent); err != nil {
		return fmt.Errorf("persist work admission recovery_required marker: validate parent: %w", err)
	}
	if existing, present, err := readWorkAdmissionRecoveryMarkerRooted(root.root); err != nil {
		return fmt.Errorf("persist work admission recovery_required marker: inspect existing marker: %w", err)
	} else if present {
		if _, err := decodeWorkAdmissionRecoveryMarker(existing); err != nil {
			return fmt.Errorf("persist work admission recovery_required marker: existing marker invalid: %w", err)
		}
		return nil
	}

	marker := workAdmissionRecoveryMarker{
		SchemaVersion: workAdmissionRecoveryMarkerSchema,
		Status:        "recovery_required",
		WorkID:        workID,
		Reason:        "work authority custody changed during an admitted callback",
	}
	if cause != nil {
		marker.Reason = cause.Error()
	}
	body, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("persist work admission recovery_required marker: encode: %w", err)
	}
	body = append(body, '\n')

	sequence := atomic.AddUint64(&workAdmissionRecoverySequence, 1)
	tempRelative := filepath.Join(
		parent,
		fmt.Sprintf(".recovery-required.json.tmp-%d-%d", os.Getpid(), sequence),
	)
	file, err := workAdmissionRecoveryOpenFile(root.root, tempRelative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("persist work admission recovery_required marker: create staging file: %w", err)
	}
	cleanup := func(cause error) error {
		closeErr := file.Close()
		removeErr := root.root.Remove(tempRelative)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return errors.Join(cause, closeErr, removeErr)
	}
	if n, err := workAdmissionRecoveryWrite(file, body); err != nil {
		return cleanup(fmt.Errorf("persist work admission recovery_required marker: write staging file: %w", err))
	} else if n != len(body) {
		return cleanup(fmt.Errorf("persist work admission recovery_required marker: write staging file: %w", io.ErrShortWrite))
	}
	if err := workAdmissionRecoveryFileSync(file); err != nil {
		return cleanup(fmt.Errorf("persist work admission recovery_required marker: sync staging file: %w", err))
	}
	if err := file.Close(); err != nil {
		_ = root.root.Remove(tempRelative)
		return fmt.Errorf("persist work admission recovery_required marker: close staging file: %w", err)
	}
	file = nil
	staged, err := readWorkAdmissionRecoveryFileRooted(root.root, tempRelative)
	if err != nil {
		_ = root.root.Remove(tempRelative)
		return fmt.Errorf("persist work admission recovery_required marker: validate staging file: %w", err)
	}
	if !bytes.Equal(staged, body) {
		_ = root.root.Remove(tempRelative)
		return errors.New("persist work admission recovery_required marker: staging bytes changed before publication")
	}
	if err := workAdmissionRecoveryRename(root.root, tempRelative, filepath.FromSlash(workAdmissionRecoveryMarkerRelative)); err != nil {
		_ = root.root.Remove(tempRelative)
		return fmt.Errorf("persist work admission recovery_required marker: atomic rename: %w", err)
	}
	if err := syncWorkAdmissionRecoveryParent(root, parent); err != nil {
		return fmt.Errorf("persist work admission recovery_required marker: sync parent after rename: %w", err)
	}
	published, present, err := readWorkAdmissionRecoveryMarkerRooted(root.root)
	if err != nil {
		return fmt.Errorf("persist work admission recovery_required marker: validate published marker: %w", err)
	}
	if !present || !bytes.Equal(published, body) {
		return errors.New("persist work admission recovery_required marker: published marker bytes changed")
	}
	return nil
}

func readWorkAdmissionRecoveryMarkerRooted(root *os.Root) ([]byte, bool, error) {
	relative := filepath.FromSlash(workAdmissionRecoveryMarkerRelative)
	data, err := readWorkAdmissionRecoveryFileRooted(root, relative)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	return data, true, nil
}

func readWorkAdmissionRecoveryFileRooted(root *os.Root, relative string) ([]byte, error) {
	before, err := root.Lstat(relative)
	if err != nil {
		return nil, err
	}
	if err := validateWorkAdmissionFile(relative, before); err != nil {
		return nil, err
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	opened, statErr := file.Stat()
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	after, afterErr := root.Lstat(relative)
	if err := errors.Join(statErr, readErr, closeErr, afterErr); err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) || !os.SameFile(before, after) {
		return nil, fmt.Errorf("work admission recovery marker path changed while reading: %s", relative)
	}
	return data, nil
}

func syncWorkAdmissionRecoveryParent(root *workAdmissionRoot, relative string) error {
	before, err := root.snapshotDirectory(relative)
	if err != nil {
		return err
	}
	directory, err := root.root.Open(relative)
	if err != nil {
		return err
	}
	opened, statErr := directory.Stat()
	after, afterErr := root.snapshotDirectory(relative)
	if err := errors.Join(statErr, afterErr); err != nil {
		_ = directory.Close()
		return err
	}
	if !opened.IsDir() || !sameWorkAdmissionSnapshot(before, after) ||
		!os.SameFile(before[len(before)-1], opened) {
		_ = directory.Close()
		return fmt.Errorf("work admission recovery marker parent changed before sync: %s", relative)
	}
	syncErr := workAdmissionRecoveryParentSync(directory)
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func decodeWorkAdmissionRecoveryMarker(data []byte) (workAdmissionRecoveryMarker, error) {
	var marker workAdmissionRecoveryMarker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return marker, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return marker, err
	}
	if marker.SchemaVersion != workAdmissionRecoveryMarkerSchema ||
		marker.Status != "recovery_required" ||
		marker.WorkID == "" || marker.Reason == "" {
		return marker, errors.New("invalid work admission recovery marker fields")
	}
	return marker, nil
}
