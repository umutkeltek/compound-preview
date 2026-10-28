package home

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAutonomyAdmissionLockSerializesWriters(t *testing.T) {
	home := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithAutonomyAdmissionLock(home, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- WithAutonomyAdmissionLock(home, func() error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second autonomy writer entered before the first released")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second autonomy writer did not enter after release")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestAutonomyAdmissionLockHeldProbe(t *testing.T) {
	home := t.TempDir()
	held, err := AutonomyAdmissionLockHeld(home)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("unheld autonomy admission lock reported held")
	}
	if err := WithAutonomyAdmissionLock(home, func() error {
		held, err := AutonomyAdmissionLockHeld(home)
		if err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("held autonomy admission lock reported free")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAutonomyAdmissionLockEpochSpansAcquisitionAndRestoresPriorBytes(t *testing.T) {
	home := t.TempDir()
	lockPath, err := autonomyAdmissionLockPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	prior := []byte("prior lock-file bytes\n")
	if err := os.WriteFile(lockPath, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	var first string
	if err := WithAutonomyAdmissionLock(home, func() error {
		held, epoch, err := AutonomyAdmissionLockState(home)
		if err != nil {
			return err
		}
		if !held || epoch == "" {
			return fmt.Errorf("held autonomy lock has no validated epoch: held=%t epoch=%q", held, epoch)
		}
		first = epoch
		held, second, err := AutonomyAdmissionLockState(home)
		if err != nil {
			return err
		}
		if !held || second != first {
			return fmt.Errorf("one acquisition changed epoch: first=%q second=%q held=%t", first, second, held)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	held, epoch, err := AutonomyAdmissionLockState(home)
	if err != nil {
		t.Fatal(err)
	}
	if held || epoch != "" {
		t.Fatalf("released autonomy lock retained live state: held=%t epoch=%q", held, epoch)
	}
	restored, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(prior) {
		t.Fatalf("prior lock-file bytes not restored: got %q want %q", restored, prior)
	}
}

func TestAutonomyAdmissionLockEpochChangesAfterReleaseAndReacquire(t *testing.T) {
	home := t.TempDir()
	readEpoch := func() string {
		t.Helper()
		var observed string
		if err := WithAutonomyAdmissionLock(home, func() error {
			held, epoch, err := AutonomyAdmissionLockState(home)
			if err != nil {
				return err
			}
			if !held || epoch == "" {
				return fmt.Errorf("acquisition epoch unavailable: held=%t epoch=%q", held, epoch)
			}
			observed = epoch
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return observed
	}
	first := readEpoch()
	second := readEpoch()
	if first == second {
		t.Fatalf("release/reacquire reused epoch %q", first)
	}
}

func TestAutonomyAdmissionLockReportsSubprocessEpochAndIgnoresCrashStaleBytes(t *testing.T) {
	home := t.TempDir()
	cmd, entered, err := startAutonomyLockHelper(home, "hold")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-entered:
		if line != "entered" {
			t.Fatalf("unexpected child handshake: %q", line)
		}
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child process did not acquire lock")
	}
	held, childEpoch, err := AutonomyAdmissionLockState(home)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	if !held || childEpoch == "" {
		_ = cmd.Process.Kill()
		t.Fatalf("subprocess lock has no validated epoch: held=%t epoch=%q", held, childEpoch)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	var epoch string
	held, epoch, err = AutonomyAdmissionLockState(home)
	if err != nil {
		t.Fatal(err)
	}
	if held || epoch != "" {
		t.Fatalf("crash-stale epoch treated as live: held=%t epoch=%q", held, epoch)
	}
	var reacquired string
	if err := WithAutonomyAdmissionLock(home, func() error {
		held, epoch, err := AutonomyAdmissionLockState(home)
		if err != nil {
			return err
		}
		if !held || epoch == "" {
			return fmt.Errorf("reacquired lock has no validated epoch: held=%t epoch=%q", held, epoch)
		}
		reacquired = epoch
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if reacquired == childEpoch {
		t.Fatalf("post-crash acquisition reused stale epoch %q", childEpoch)
	}
}

func TestAutonomyAdmissionLockSerializesProcesses(t *testing.T) {
	home := t.TempDir()
	var cmd *exec.Cmd
	var entered <-chan string
	err := WithAutonomyAdmissionLock(home, func() error {
		var startErr error
		cmd, entered, startErr = startAutonomyLockHelper(home, "enter")
		if startErr != nil {
			return startErr
		}
		select {
		case line := <-entered:
			return fmt.Errorf("child crossed parent process lock before release: %q", line)
		case <-time.After(50 * time.Millisecond):
			return nil
		}
	})
	if err != nil {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Fatal(err)
	}
	select {
	case line := <-entered:
		if line != "entered" {
			t.Fatalf("unexpected child handshake after release: %q", line)
		}
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child process did not acquire lock after parent released")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestAutonomyAdmissionLockReleasesWhenProcessDies(t *testing.T) {
	home := t.TempDir()
	cmd, entered, err := startAutonomyLockHelper(home, "hold")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-entered:
		if line != "entered" {
			t.Fatalf("unexpected child handshake: %q", line)
		}
	case <-time.After(time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child process did not acquire lock")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	done := make(chan error, 1)
	go func() { done <- WithAutonomyAdmissionLock(home, func() error { return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("lock remained held after child process died")
	}
}

func TestOperationalArchiveAndRestoreKeepMovePlusIndexAtomicToAdmission(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".compound")
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := Init(home, "test", now); err != nil {
		t.Fatal(err)
	}
	hotPath := filepath.Join(home, "work", "work", "work_archive_lock.json")
	if err := os.MkdirAll(filepath.Dir(hotPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hotPath, []byte(`{"id":"work_archive_lock","state":"completed"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Journal-first design: the only durable-gap window is per-file, between
	// the rename and the committed line. The legacy batch seam
	// (operationalArchiveBeforeIndexCommit) no longer fires for the archive
	// path; the journal-first seam (operationalArchiveBeforeJournalCommit)
	// captures the per-file state instead.
	type archiveGap struct {
		entry OperationalArchiveEntry
	}
	type restoreGap struct {
		entries []OperationalArchiveEntry
	}
	archiveGapCh := make(chan archiveGap, 1)
	releaseArchiveGap := make(chan struct{})
	restoreGapCh := make(chan restoreGap, 1)
	releaseRestoreGap := make(chan struct{})

	previousArchiveHook := operationalArchiveBeforeJournalCommit
	operationalArchiveBeforeJournalCommit = func(entry OperationalArchiveEntry) {
		archiveGapCh <- archiveGap{entry: entry}
		<-releaseArchiveGap
	}
	defer func() { operationalArchiveBeforeJournalCommit = previousArchiveHook }()

	previousRestoreHook := operationalArchiveBeforeIndexCommit
	operationalArchiveBeforeIndexCommit = func(operation string, entries []OperationalArchiveEntry) {
		if operation != "restored" {
			return
		}
		restoreGapCh <- restoreGap{entries: append([]OperationalArchiveEntry(nil), entries...)}
		<-releaseRestoreGap
	}
	defer func() { operationalArchiveBeforeIndexCommit = previousRestoreHook }()

	assertAdmissionReaderWaits := func() <-chan error {
		t.Helper()
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			close(started)
			done <- WithAutonomyAdmissionLock(home, func() error { return nil })
		}()
		<-started
		select {
		case err := <-done:
			t.Fatalf("admission reader crossed archive move/index gap: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		return done
	}

	archiveDone := make(chan error, 1)
	go func() {
		_, err := ArchiveOperationalHistory(home, OperationalArchiveOptions{Kinds: []string{"work"}}, now)
		archiveDone <- err
	}()
	archivedGap := <-archiveGapCh
	if archivedGap.entry.Operation != "archived" || archivedGap.entry.OriginalPath != "work/work/work_archive_lock.json" {
		t.Fatalf("archive gap entry = %+v", archivedGap.entry)
	}
	if _, err := os.Stat(hotPath); !os.IsNotExist(err) {
		t.Fatalf("hot work should already be moved in archive gap: %v", err)
	}
	if active, err := LoadOperationalArchiveIndex(home); err != nil || len(active) != 1 || active[0].Operation != "archived_pending" {
		t.Fatalf("archive index in gap should hold only the pending line: active=%+v err=%v", active, err)
	}
	readerDone := assertAdmissionReaderWaits()
	releaseArchiveGap <- struct{}{}
	if err := <-archiveDone; err != nil {
		t.Fatal(err)
	}
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}

	restoreDone := make(chan error, 1)
	go func() {
		_, err := RestoreOperationalArchive(home, OperationalRestoreOptions{All: true}, now.Add(time.Minute))
		restoreDone <- err
	}()
	_ = <-restoreGapCh
	if _, err := os.Stat(hotPath); err != nil {
		t.Fatalf("hot work should already be restored in restore gap: %v", err)
	}
	if active, err := LoadOperationalArchiveIndex(home); err != nil || len(active) != 1 || active[0].Operation != "archived" {
		t.Fatalf("restore index in gap should still describe the archived state: active=%+v err=%v", active, err)
	}
	readerDone = assertAdmissionReaderWaits()
	releaseRestoreGap <- struct{}{}
	if err := <-restoreDone; err != nil {
		t.Fatal(err)
	}
	if err := <-readerDone; err != nil {
		t.Fatal(err)
	}
	if active, err := LoadOperationalArchiveIndex(home); err != nil || len(active) != 0 {
		t.Fatalf("restore index did not commit: active=%+v err=%v", active, err)
	}
}

func TestAutonomyAdmissionLockProcessHelper(t *testing.T) {
	mode := os.Getenv("COMPOUND_AUTONOMY_LOCK_HELPER")
	if mode == "" {
		return
	}
	home := os.Getenv("COMPOUND_AUTONOMY_LOCK_HOME")
	fmt.Println("attempting")
	if err := WithAutonomyAdmissionLock(home, func() error {
		fmt.Println("entered")
		if mode == "hold" {
			for {
				time.Sleep(time.Hour)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func startAutonomyLockHelper(home, mode string) (*exec.Cmd, <-chan string, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestAutonomyAdmissionLockProcessHelper$")
	cmd.Env = append(os.Environ(),
		"COMPOUND_AUTONOMY_LOCK_HELPER="+mode,
		"COMPOUND_AUTONOMY_LOCK_HOME="+home,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "attempting" {
		_ = cmd.Process.Kill()
		return nil, nil, fmt.Errorf("child did not announce lock attempt")
	}
	entered := make(chan string, 1)
	go func() {
		if scanner.Scan() {
			entered <- scanner.Text()
			return
		}
		entered <- ""
	}()
	return cmd, entered, nil
}
