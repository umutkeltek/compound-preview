package home

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func stagedRecoveryFixture(t *testing.T, body []byte) (string, string, string, string) {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	stageName := ".artifact-123456789"
	stagePath := filepath.Join(homePath, "private", "artifacts", "sha256", stageName)
	if err := os.WriteFile(stagePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	ref := ArtifactRefPrefix + hex.EncodeToString(sum[:])
	return homePath, stageName, stagePath, ref
}

func TestPrivateCustodyRecoveryRespectsWriterInAnotherProcess(t *testing.T) {
	if os.Getenv("COMPOUND_TEST_PRIVATE_STAGE_LOCK_CHILD") == "1" {
		homePath := os.Getenv("COMPOUND_TEST_PRIVATE_STAGE_LOCK_HOME")
		acquired, err := withPrivateArtifactDirectoryLock(homePath, false, func(_ *os.Root) error {
			fmt.Fprintln(os.Stdout, "READY")
			_, copyErr := io.Copy(io.Discard, os.Stdin)
			return copyErr
		})
		if err != nil || !acquired {
			t.Fatalf("child lock: acquired=%v err=%v", acquired, err)
		}
		return
	}
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPrivateCustodyRecoveryRespectsWriterInAnotherProcess$")
	cmd.Env = append(os.Environ(), "COMPOUND_TEST_PRIVATE_STAGE_LOCK_CHILD=1", "COMPOUND_TEST_PRIVATE_STAGE_LOCK_HOME="+homePath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "READY\n" {
		t.Fatalf("child did not hold lock: line=%q err=%v", line, err)
	}
	selection := PrivateCustodyOptions{RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}}
	report := RepairPrivateCustody(homePath, selection)
	if report.OK || report.Counts.Manual == 0 || report.Changed {
		t.Fatalf("recovery crossed writer process: %+v", report)
	}
	if _, err := os.Lstat(stagePath); err != nil {
		t.Fatalf("stage changed while writer held lock: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	finished = true
	second := RepairPrivateCustody(homePath, selection)
	if !second.OK {
		t.Fatalf("recovery after writer exited: %+v", second)
	}
}

func TestPrivateCustodyRecoveryRefusesHomeReplacementBeforeLock(t *testing.T) {
	body := []byte("synthetic private body")
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, body)
	movedPath := homePath + "-prior"
	hooks := privateCustodyHooks{beforeArtifactLock: func() {
		if err := os.Rename(homePath, movedPath); err != nil {
			t.Fatal(err)
		}
		if err := Init(homePath, "replacement", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stagePath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	report := privateCustodyWithHooks(homePath, PrivateCustodyOptions{Repair: true, RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}}, hooks)
	if report.OK || report.Counts.Manual == 0 || report.Changed {
		t.Fatalf("home replacement was recovered across generations: %+v", report)
	}
	for _, path := range []string{stagePath, filepath.Join(movedPath, "private", "artifacts", "sha256", stageName)} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("replacement changed stage %s: %v", path, err)
		}
	}
}

func TestPutPrivateLockedDoesNotPublishIntoReplacementHome(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("synthetic private body")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	movedPath := homePath + "-prior"
	_, _ = withPrivateArtifactDirectoryLock(homePath, false, func(root *os.Root) error {
		if err := os.Rename(homePath, movedPath); err != nil {
			t.Fatal(err)
		}
		if err := Init(homePath, "replacement", time.Now()); err != nil {
			t.Fatal(err)
		}
		_, putErr := store.putPrivateLocked(root, body)
		return putErr
	})
	if _, err := os.Lstat(filepath.Join(homePath, "private", "artifacts", "sha256", digest)); !os.IsNotExist(err) {
		t.Fatalf("writer published into replacement home: %v", err)
	}
}

func TestPutPrivateLockedDoesNotPublishIntoReplacementArtifactDirectory(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("synthetic private body")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	dir := filepath.Join(homePath, "private", "artifacts", "sha256")
	_, _ = withPrivateArtifactDirectoryLock(homePath, false, func(root *os.Root) error {
		if err := os.Rename(dir, dir+"-prior"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		_, putErr := store.putPrivateLocked(root, body)
		return putErr
	})
	if _, err := os.Lstat(filepath.Join(dir, digest)); !os.IsNotExist(err) {
		t.Fatalf("writer published into replacement artifact directory: %v", err)
	}
}

func TestPrivateCustodyCLIRecoversExactSelectedStage(t *testing.T) {
	body := []byte("synthetic orphaned private stage")
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, body)
	var out, errOut bytes.Buffer
	code := RunPrivateCustodyCLI([]string{"private-custody", "repair", "--home", homePath, "--recover-stage", stageName + "=" + ref, "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("recovery exit=%d stderr=%s report=%s", code, errOut.String(), out.String())
	}
	if _, err := os.Lstat(stagePath); !os.IsNotExist(err) {
		t.Fatalf("stage remains after recovery: %v", err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetPrivate(ref)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("recovered body: err=%v match=%v", err, bytes.Equal(got, body))
	}
	if report := DiagnosePrivateCustody(homePath); !report.OK {
		t.Fatalf("doctor after recovery: %+v", report)
	}
	if !strings.Contains(out.String(), `"repaired"`) {
		t.Fatalf("recovery report omitted repaired result: %s", out.String())
	}
}

func TestPutPrivateLeavesOnlyVerifiedSingleLinkDigest(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutPrivate([]byte("synthetic newly written body"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(homePath, "private", "artifacts", "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != strings.TrimPrefix(ref, ArtifactRefPrefix) {
		t.Fatalf("successful writer left staging names: %+v", entries)
	}
	if report := DiagnosePrivateCustody(homePath); !report.OK {
		t.Fatalf("writer left doctor dirty: %+v", report)
	}
}

func TestPutPrivateRetainsStageWhenDigestDisappearsBeforeUnlink(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	privateDir := filepath.Join(homePath, "private", "artifacts", "sha256")
	body := []byte("synthetic interrupted publication")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	previous := syncRootedDirectoryFile
	t.Cleanup(func() { syncRootedDirectoryFile = previous })
	removed := false
	syncRootedDirectoryFile = func(rel string, file *os.File) error {
		if rel == "." && !removed {
			removed = true
			if err := os.Remove(filepath.Join(privateDir, digest)); err != nil {
				return err
			}
		}
		return previous(rel, file)
	}
	if ref, err := store.PutPrivate(body); err == nil {
		t.Fatalf("writer accepted missing digest: %s", ref)
	}
	if !removed {
		t.Fatal("test did not remove published digest")
	}
	entries, err := os.ReadDir(privateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".artifact-") {
		t.Fatalf("writer did not retain recoverable stage: %+v", entries)
	}
}

func TestPutPrivateRejectsDigestDisappearanceAfterUnlink(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("synthetic post-unlink disappearance")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	previous := syncRootedDirectoryFile
	t.Cleanup(func() { syncRootedDirectoryFile = previous })
	syncCount := 0
	syncRootedDirectoryFile = func(rel string, file *os.File) error {
		if rel == "." {
			syncCount++
			if syncCount == 2 {
				if err := os.Remove(filepath.Join(homePath, "private", "artifacts", "sha256", digest)); err != nil {
					return err
				}
			}
		}
		return previous(rel, file)
	}
	if ref, err := store.PutPrivate(body); err == nil {
		t.Fatalf("writer accepted digest removed after stage unlink: %s", ref)
	}
	if syncCount != 2 {
		t.Fatalf("test did not reach post-unlink sync: calls=%d", syncCount)
	}
}

func TestPutPrivateRejectsDirectorySyncEINVALAndRetainsStage(t *testing.T) {
	homePath := filepath.Join(t.TempDir(), ".compound")
	if err := Init(homePath, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	previous := syncRootedDirectoryFile
	t.Cleanup(func() { syncRootedDirectoryFile = previous })
	syncRootedDirectoryFile = func(rel string, file *os.File) error {
		if rel == "." {
			return syscall.EINVAL
		}
		return previous(rel, file)
	}
	body := []byte("synthetic unsynced private body")
	if ref, err := store.PutPrivate(body); err == nil {
		t.Fatalf("writer accepted EINVAL directory sync: %s", ref)
	}
	entries, err := os.ReadDir(filepath.Join(homePath, "private", "artifacts", "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !strings.HasPrefix(entries[0].Name(), ".artifact-") && !strings.HasPrefix(entries[1].Name(), ".artifact-") {
		t.Fatalf("writer lost stage after EINVAL: %+v", entries)
	}
	syncRootedDirectoryFile = previous
	stageName := entries[0].Name()
	if !strings.HasPrefix(stageName, ".artifact-") {
		stageName = entries[1].Name()
	}
	sum := sha256.Sum256(body)
	ref := ArtifactRefPrefix + hex.EncodeToString(sum[:])
	retry := RepairPrivateCustody(homePath, PrivateCustodyOptions{Repair: true, RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}})
	if !retry.OK {
		t.Fatalf("writer stage could not be recovered after sync resumed: %+v", retry)
	}
}

func TestPrivateCustodyRecoveryRejectsDirectorySyncEINVAL(t *testing.T) {
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
	previous := syncRootedDirectoryFile
	t.Cleanup(func() { syncRootedDirectoryFile = previous })
	injected := false
	syncRootedDirectoryFile = func(rel string, file *os.File) error {
		if rel == "." {
			injected = true
			return syscall.EINVAL
		}
		return previous(rel, file)
	}
	selection := PrivateCustodyOptions{Repair: true, RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}}
	first := RepairPrivateCustody(homePath, selection)
	if !injected {
		t.Fatal("test did not inject private directory sync failure")
	}
	if first.OK || first.Counts.Manual == 0 {
		t.Fatalf("recovery accepted EINVAL directory sync: %+v", first)
	}
	if _, err := os.Lstat(stagePath); err != nil {
		t.Fatalf("recovery lost stage after EINVAL: %v", err)
	}
	syncRootedDirectoryFile = previous
	if retry := RepairPrivateCustody(homePath, selection); !retry.OK {
		t.Fatalf("recovery did not resume after sync resumed: %+v", retry)
	}
}

func TestPrivateCustodyRecoveryRequiresExactSelection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pairs []string
	}{
		{"missing digest", []string{".artifact-123456789"}},
		{"path traversal", []string{"../.artifact-123456789=artifact:sha256:" + strings.Repeat("a", 64)}},
		{"wrong reference", []string{".artifact-123456789=artifact:sha256:bad"}},
		{"duplicate", []string{".artifact-123456789=artifact:sha256:" + strings.Repeat("a", 64), ".artifact-123456789=artifact:sha256:" + strings.Repeat("b", 64)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, _, stagePath, _ := stagedRecoveryFixture(t, []byte("synthetic private body"))
			args := []string{"private-custody", "repair", "--home", homePath}
			for _, pair := range tc.pairs {
				args = append(args, "--recover-stage", pair)
			}
			var out, errOut bytes.Buffer
			if code := RunPrivateCustodyCLI(args, &out, &errOut); code != 2 {
				t.Fatalf("invalid selection exit=%d stderr=%s report=%s", code, errOut.String(), out.String())
			}
			if _, err := os.Lstat(stagePath); err != nil {
				t.Fatalf("invalid selection changed stage: %v", err)
			}
		})
	}
}

func TestPrivateCustodyRecoveryRefusesChangedOrUnsafeStage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, stagePath string)
	}{
		{"wrong bytes", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("different"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, path string) {
			t.Helper()
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("synthetic private body"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Link(path, filepath.Join(t.TempDir(), "external")); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
			tc.change(t, stagePath)
			report := RepairPrivateCustody(homePath, PrivateCustodyOptions{RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}})
			if report.OK || report.Counts.Manual == 0 || report.Changed {
				t.Fatalf("unsafe stage recovered: %+v", report)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(stagePath), strings.TrimPrefix(ref, ArtifactRefPrefix))); !os.IsNotExist(err) {
				t.Fatalf("unsafe stage published digest: %v", err)
			}
		})
	}
}

func TestPrivateCustodyRecoveryResumesLinkedStage(t *testing.T) {
	for _, tc := range []struct {
		name string
		link bool
	}{
		{"same inode", true},
		{"independent matching digest", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("synthetic linked stage")
			homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, body)
			digestPath := filepath.Join(filepath.Dir(stagePath), strings.TrimPrefix(ref, ArtifactRefPrefix))
			var err error
			if tc.link {
				err = os.Link(stagePath, digestPath)
			} else {
				err = os.WriteFile(digestPath, body, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			report := RepairPrivateCustody(homePath, PrivateCustodyOptions{RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}})
			if !report.OK || !report.Changed {
				t.Fatalf("linked stage recovery: %+v", report)
			}
			if _, err := os.Lstat(stagePath); !os.IsNotExist(err) {
				t.Fatalf("stage remains: %v", err)
			}
			if !DiagnosePrivateCustody(homePath).OK {
				t.Fatal("doctor remains dirty")
			}
		})
	}
}

func TestPrivateCustodyRecoveryRefusesMissingStageWithoutExactDigest(t *testing.T) {
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
	if err := os.Remove(stagePath); err != nil {
		t.Fatal(err)
	}
	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}})
	if report.OK || report.Counts.Manual == 0 || report.Changed {
		t.Fatalf("missing stage accepted: %+v", report)
	}
}

func TestPrivateCustodyRecoveryDryRunAndIdempotentRetry(t *testing.T) {
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
	selection := PrivateCustodyOptions{RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}}
	dry := selection
	dry.DryRun = true
	preview := RepairPrivateCustody(homePath, dry)
	if preview.OK || preview.Changed || len(preview.Actions) != 1 || preview.Actions[0].Status != "dry_run" {
		t.Fatalf("dry-run status: %+v", preview)
	}
	if _, err := os.Lstat(stagePath); err != nil {
		t.Fatalf("dry-run changed stage: %v", err)
	}
	first := RepairPrivateCustody(homePath, selection)
	if !first.OK || !first.Changed {
		t.Fatalf("first recovery: %+v", first)
	}
	second := RepairPrivateCustody(homePath, selection)
	if !second.OK || second.Changed || second.Status != "ok" {
		t.Fatalf("idempotent retry: %+v", second)
	}
}

func TestPrivateCustodyRecoveryDoesNotReplaceDifferentDigestObject(t *testing.T) {
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
	digestPath := filepath.Join(filepath.Dir(stagePath), strings.TrimPrefix(ref, ArtifactRefPrefix))
	other := []byte("different digest bytes")
	if err := os.WriteFile(digestPath, other, 0o600); err != nil {
		t.Fatal(err)
	}
	report := RepairPrivateCustody(homePath, PrivateCustodyOptions{RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}})
	if report.OK || report.Counts.Manual == 0 {
		t.Fatalf("conflicting digest accepted: %+v", report)
	}
	if _, err := os.Lstat(stagePath); err != nil {
		t.Fatalf("stage lost: %v", err)
	}
	got, err := os.ReadFile(digestPath)
	if err != nil || !bytes.Equal(got, other) {
		t.Fatalf("existing digest overwritten: err=%v match=%v", err, bytes.Equal(got, other))
	}
}

func TestPrivateCustodyRecoveryReturnsBusyWhileWriterHoldsDirectory(t *testing.T) {
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
	acquired, err := withPrivateArtifactDirectoryLock(homePath, false, func(_ *os.Root) error {
		report := RepairPrivateCustody(homePath, PrivateCustodyOptions{RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}})
		if report.OK || report.Counts.Manual == 0 || report.Changed {
			t.Fatalf("live writer stage was recovered: %+v", report)
		}
		if _, err := os.Lstat(stagePath); err != nil {
			t.Fatalf("live writer stage changed: %v", err)
		}
		return nil
	})
	if err != nil || !acquired {
		t.Fatalf("hold writer lock: acquired=%v err=%v", acquired, err)
	}
}

func TestPrivateCustodyRecoveryRefusesStageReplacement(t *testing.T) {
	for _, point := range []string{"before_link", "before_unlink"} {
		t.Run(point, func(t *testing.T) {
			homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
			replacement := []byte("different private body")
			replace := func(string) {
				if err := os.Remove(stagePath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stagePath, replacement, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			hooks := privateCustodyHooks{}
			if point == "before_link" {
				hooks.beforeStageLink = replace
			} else {
				hooks.beforeStageUnlink = replace
			}
			report := privateCustodyWithHooks(homePath, PrivateCustodyOptions{Repair: true, RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}}, hooks)
			if report.OK || report.Counts.Manual == 0 {
				t.Fatalf("replacement accepted: %+v", report)
			}
			if point == "before_unlink" && !report.Changed {
				t.Fatalf("partial digest publication was not reported: %+v", report)
			}
			got, err := os.ReadFile(stagePath)
			if err != nil || !bytes.Equal(got, replacement) {
				t.Fatalf("replacement stage was removed or changed: err=%v match=%v", err, bytes.Equal(got, replacement))
			}
		})
	}
}

func TestPrivateCustodyRecoveryReportsPartialSyncFailureAndRetries(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("sync_%d", failAt), func(t *testing.T) {
			homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
			selection := PrivateCustodyOptions{Repair: true, RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}}
			calls := 0
			hooks := privateCustodyHooks{syncRecoveryDir: func(root *os.Root, rel, display, kind string) error {
				calls++
				if calls == failAt {
					return errors.New("injected directory sync failure")
				}
				return syncRootedDirectory(root, rel, display, kind)
			}}
			first := privateCustodyWithHooks(homePath, selection, hooks)
			if first.OK || first.Counts.Manual == 0 || !first.Changed {
				t.Fatalf("partial sync failure not retained in report: %+v", first)
			}
			if failAt == 1 {
				if _, err := os.Lstat(stagePath); err != nil {
					t.Fatalf("stage lost before durable digest sync: %v", err)
				}
			} else {
				stillFailing := privateCustodyWithHooks(homePath, selection, privateCustodyHooks{syncRecoveryDir: func(*os.Root, string, string, string) error {
					return errors.New("directory sync still unavailable")
				}})
				if stillFailing.OK || stillFailing.Counts.Manual == 0 {
					t.Fatalf("absent-stage retry claimed durability without syncing: %+v", stillFailing)
				}
			}
			second := RepairPrivateCustody(homePath, selection)
			if !second.OK || !DiagnosePrivateCustody(homePath).OK {
				t.Fatalf("retry did not finish custody: %+v", second)
			}
		})
	}
}

func TestPrivateCustodyRecoveryRetriesAfterConflictingDigest(t *testing.T) {
	homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
	digestPath := filepath.Join(filepath.Dir(stagePath), strings.TrimPrefix(ref, ArtifactRefPrefix))
	if err := os.Symlink(stagePath, digestPath); err != nil {
		t.Fatal(err)
	}
	selection := PrivateCustodyOptions{RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}}
	first := RepairPrivateCustody(homePath, selection)
	if first.OK || first.Counts.Manual == 0 {
		t.Fatalf("conflicting digest accepted: %+v", first)
	}
	if _, err := os.Lstat(stagePath); err != nil {
		t.Fatalf("stage lost on failure: %v", err)
	}
	if err := os.Remove(digestPath); err != nil {
		t.Fatal(err)
	}
	second := RepairPrivateCustody(homePath, selection)
	if !second.OK || !second.Changed {
		t.Fatalf("retry failed: %+v", second)
	}
}

func TestPrivateCustodyRecoveryRetriesAfterLinkOrUnlinkFailure(t *testing.T) {
	for _, point := range []string{"link", "unlink"} {
		t.Run(point, func(t *testing.T) {
			homePath, stageName, stagePath, ref := stagedRecoveryFixture(t, []byte("synthetic private body"))
			dir := filepath.Dir(stagePath)
			denyWrite := func(string) {
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
			}
			hooks := privateCustodyHooks{}
			if point == "link" {
				hooks.beforeStageLink = denyWrite
			} else {
				hooks.beforeStageUnlink = denyWrite
			}
			selection := PrivateCustodyOptions{Repair: true, RecoverStages: []PrivateStageRecovery{{Name: stageName, ExpectedRef: ref}}}
			first := privateCustodyWithHooks(homePath, selection, hooks)
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if first.OK || first.Counts.Manual == 0 || first.Changed != (point == "unlink") {
				t.Fatalf("%s failure was not reported accurately: %+v", point, first)
			}
			if _, err := os.Lstat(stagePath); err != nil {
				t.Fatalf("stage lost after %s failure: %v", point, err)
			}
			second := RepairPrivateCustody(homePath, selection)
			if !second.OK || !DiagnosePrivateCustody(homePath).OK {
				t.Fatalf("%s retry failed: %+v", point, second)
			}
		})
	}
}
