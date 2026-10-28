package runattempt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const (
	launchLockHelperModeEnv = "COMPOUND_RUNATTEMPT_LAUNCH_LOCK_HELPER"
	launchLockHelperHomeEnv = "COMPOUND_RUNATTEMPT_LAUNCH_LOCK_HOME"
	launchLockHelperIDEnv   = "COMPOUND_RUNATTEMPT_LAUNCH_LOCK_ID"
	launchLockHelperTimeEnv = "COMPOUND_RUNATTEMPT_LAUNCH_LOCK_TIME"
)

func TestCommitLaunchCrossProcessHelper(t *testing.T) {
	mode := os.Getenv(launchLockHelperModeEnv)
	if mode == "" {
		return
	}

	home := os.Getenv(launchLockHelperHomeEnv)
	ready := os.NewFile(uintptr(3), "launch-ready")
	release := os.NewFile(uintptr(4), "launch-release")
	if ready == nil || release == nil {
		t.Fatal("launch helper pipes are unavailable")
	}
	defer ready.Close()
	defer release.Close()

	signalAndWait := func() error {
		if _, writeErr := ready.Write([]byte{1}); writeErr != nil {
			return writeErr
		}
		var token [1]byte
		_, readErr := io.ReadFull(release, token[:])
		return readErr
	}

	switch mode {
	case "hold-launch":
		id := os.Getenv(launchLockHelperIDEnv)
		launchAt, err := time.Parse(time.RFC3339Nano, os.Getenv(launchLockHelperTimeEnv))
		if err != nil {
			t.Fatalf("parse launch time: %v", err)
		}
		if _, err := Open(home).CommitLaunch(id, launchAt, signalAndWait); err != nil {
			t.Fatalf("held CommitLaunch: %v", err)
		}
	case "hold-legacy-lock":
		lockDir := filepath.Join(home, "lock")
		if err := os.MkdirAll(lockDir, 0o755); err != nil {
			t.Fatalf("create legacy lock directory: %v", err)
		}
		lockPath := filepath.Join(lockDir, "runattempts.lock")
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("open legacy lock: %v", err)
		}
		defer file.Close()
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatalf("acquire legacy lock: %v", err)
		}
		if err := signalAndWait(); err != nil {
			t.Fatalf("hold legacy lock: %v", err)
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatalf("release legacy lock: %v", err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func TestCommitLaunchPersistsPreparedBeforeStartAndCommittedAfterSuccess(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launchAt := created.Add(time.Minute)
	attempt, err := store.Mint("work_launch", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	var callbackSaw RunAttempt
	launched, err := store.CommitLaunch(attempt.ID, launchAt, func() error {
		var readErr error
		callbackSaw, readErr = Open(home).Get(attempt.ID)
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if callbackSaw.Status != StatusRunning {
		t.Fatalf("callback saw status %q, want running", callbackSaw.Status)
	}
	if callbackSaw.Launch == nil || callbackSaw.Launch.PreparedAt == nil || !callbackSaw.Launch.PreparedAt.Equal(launchAt) {
		t.Fatalf("callback did not see durable prepared launch at %s: %+v", launchAt, callbackSaw.Launch)
	}
	if callbackSaw.Launch.CommittedAt != nil || callbackSaw.Launch.StartErrorAt != nil {
		t.Fatalf("callback saw a terminal launch disposition before start returned: %+v", callbackSaw.Launch)
	}
	if launched.Launch == nil || launched.Launch.CommittedAt == nil || !launched.Launch.CommittedAt.Equal(launchAt) {
		t.Fatalf("successful launch was not committed at %s: %+v", launchAt, launched.Launch)
	}
	if launched.Launch.StartErrorAt != nil {
		t.Fatalf("successful launch marked start_error: %+v", launched.Launch)
	}
	cached, err := store.Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Launch == nil || cached.Launch.CommittedAt == nil {
		t.Fatalf("same-store cached read missed committed sidecar: %+v", cached.Launch)
	}
	mainBody, err := os.ReadFile(filepath.Join(home, "run_attempts", attempt.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(mainBody, []byte(`"launch"`)) {
		t.Fatalf("v0.1 main JSON contains erasable inline launch evidence: %s", mainBody)
	}
	launchDir := launchEvidenceDirectory(home)
	directoryInfo, err := os.Stat(launchDir)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("launch evidence directory mode = %04o, want 0700", directoryInfo.Mode().Perm())
	}
	for _, kind := range []string{launchEvidencePrepared, launchEvidenceCommitted} {
		info, err := os.Stat(launchEvidencePath(home, attempt.ID, kind))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s evidence mode = %04o, want 0600", kind, info.Mode().Perm())
		}
	}
}

func TestCommitLaunchRejectsSymlinkedEvidenceDirectoryWithoutExternalWrites(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_symlink", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, launchEvidenceDirectory(home)); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	started := false
	if _, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error {
		started = true
		return nil
	}); err == nil {
		t.Fatal("CommitLaunch accepted a symlinked launch-evidence directory")
	}
	if started {
		t.Fatal("start callback ran after launch-evidence custody failed")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("launch evidence escaped pinned home: %v", entries)
	}
}

func TestGetRejectsLaunchEvidenceFromNonPrivateDirectory(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "group-readable", mode: 0o755},
		{name: "world-writable", mode: 0o777},
	} {
		t.Run(test.name, func(t *testing.T) {
			homePath := t.TempDir()
			store := Open(homePath)
			created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
			attempt, err := store.Mint("work_launch_directory_mode", "actor:test", "harness:test", created)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(launchEvidenceDirectory(homePath), test.mode); err != nil {
				t.Fatal(err)
			}

			for _, read := range []struct {
				name  string
				store *Store
			}{
				{name: "cached", store: store},
				{name: "fresh", store: Open(homePath)},
			} {
				t.Run(read.name, func(t *testing.T) {
					got, err := read.store.Get(attempt.ID)
					if err == nil {
						t.Fatalf("Get accepted launch evidence beneath mode %04o directory: %+v", test.mode, got.Launch)
					}
					if got.Launch != nil {
						t.Fatalf("Get materialized Launch after rejecting mode %04o custody: %+v", test.mode, got.Launch)
					}
				})
			}
		})
	}
}

func TestGetRejectsNonPrivateLaunchEvidenceFile(t *testing.T) {
	for _, kind := range []string{launchEvidencePrepared, launchEvidenceCommitted} {
		t.Run(kind, func(t *testing.T) {
			homePath := t.TempDir()
			store := Open(homePath)
			created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
			attempt, err := store.Mint("work_launch_file_mode", "actor:test", "harness:test", created)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(launchEvidencePath(homePath, attempt.ID, kind), 0o644); err != nil {
				t.Fatal(err)
			}

			for _, read := range []struct {
				name  string
				store *Store
			}{
				{name: "cached", store: store},
				{name: "fresh", store: Open(homePath)},
			} {
				t.Run(read.name, func(t *testing.T) {
					got, err := read.store.Get(attempt.ID)
					if err == nil {
						t.Fatalf("Get accepted %s launch sidecar with mode 0644: %+v", kind, got.Launch)
					}
					if got.Launch != nil {
						t.Fatalf("Get materialized Launch after rejecting %s mode 0644 custody: %+v", kind, got.Launch)
					}
				})
			}
		})
	}
}

func TestGetCannotRegressNewerSameStoreCacheGeneration(t *testing.T) {
	homePath := t.TempDir()
	store := Open(homePath)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_get_cache_generation", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	launchAt := created.Add(time.Minute)
	if _, err := store.CommitLaunch(attempt.ID, launchAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	originalRead := runattemptReadLaunchEvidence
	var reads atomic.Int32
	getEnteredLaunchRead := make(chan struct{})
	releaseGet := make(chan struct{})
	runattemptReadLaunchEvidence = func(home, id, kind string) (launchEvidence, error) {
		if reads.Add(1) == 1 {
			close(getEnteredLaunchRead)
			<-releaseGet
		}
		return originalRead(home, id, kind)
	}
	defer func() { runattemptReadLaunchEvidence = originalRead }()

	firstGetDone := make(chan error, 1)
	go func() {
		_, getErr := store.Get(attempt.ID)
		firstGetDone <- getErr
	}()
	<-getEnteredLaunchRead

	heartbeatAt := created.Add(2 * time.Minute)
	if _, err := store.Heartbeat(attempt.ID, heartbeatAt); err != nil {
		t.Fatal(err)
	}
	close(releaseGet)
	if err := <-firstGetDone; err != nil {
		t.Fatal(err)
	}

	observed, err := store.Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.LastHeartbeatAt == nil || !observed.LastHeartbeatAt.Equal(heartbeatAt) {
		t.Fatalf("Get regressed newer same-store heartbeat: got %v, want %s", observed.LastHeartbeatAt, heartbeatAt)
	}
}

func TestGetCacheMissCannotOverwriteConcurrentMutation(t *testing.T) {
	homePath := t.TempDir()
	writer := Open(homePath)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := writer.Mint("work_get_cache_miss_generation", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	launchAt := created.Add(time.Minute)
	if _, err := writer.CommitLaunch(attempt.ID, launchAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	reader := Open(homePath)
	originalRead := runattemptReadLaunchEvidence
	var reads atomic.Int32
	getEnteredLaunchRead := make(chan struct{})
	releaseGet := make(chan struct{})
	runattemptReadLaunchEvidence = func(home, id, kind string) (launchEvidence, error) {
		if reads.Add(1) == 1 {
			close(getEnteredLaunchRead)
			<-releaseGet
		}
		return originalRead(home, id, kind)
	}
	defer func() { runattemptReadLaunchEvidence = originalRead }()

	firstGetDone := make(chan error, 1)
	go func() {
		_, getErr := reader.Get(attempt.ID)
		firstGetDone <- getErr
	}()
	<-getEnteredLaunchRead

	heartbeatAt := created.Add(2 * time.Minute)
	if _, err := reader.Heartbeat(attempt.ID, heartbeatAt); err != nil {
		t.Fatal(err)
	}
	close(releaseGet)
	if err := <-firstGetDone; err != nil {
		t.Fatal(err)
	}

	observed, err := reader.Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.LastHeartbeatAt == nil || !observed.LastHeartbeatAt.Equal(heartbeatAt) {
		t.Fatalf("cache-miss Get regressed concurrent heartbeat: got %v, want %s", observed.LastHeartbeatAt, heartbeatAt)
	}
}

func TestGetRejectsNonExactLaunchEvidenceJSON(t *testing.T) {
	const (
		privateKey     = "operator_private_launch_key"
		privateValue   = "operator-only launch failure detail"
		privateSchema  = "private.schema.operator-only"
		privateID      = "private operator attempt identity"
		privateKind    = "private operator launch kind"
		zeroTimestamp  = "0001-01-01T00:00:00Z"
		invalidMessage = "runattempt launch evidence: invalid document"
	)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)

	for _, test := range []struct {
		name      string
		body      func(valid map[string]any) []byte
		wantError string
	}{
		{
			name: "private unknown field",
			body: func(valid map[string]any) []byte {
				valid[privateKey] = privateValue
				return mustMarshalLaunchEvidenceDocument(t, valid)
			},
			wantError: invalidMessage,
		},
		{
			name: "private malformed timestamp",
			body: func(valid map[string]any) []byte {
				valid["at"] = privateValue
				return mustMarshalLaunchEvidenceDocument(t, valid)
			},
			wantError: invalidMessage,
		},
		{
			name: "private schema mismatch",
			body: func(valid map[string]any) []byte {
				valid["schema_version"] = privateSchema
				return mustMarshalLaunchEvidenceDocument(t, valid)
			},
			wantError: "runattempt launch evidence: unsupported version",
		},
		{
			name: "private identity mismatch",
			body: func(valid map[string]any) []byte {
				valid["attempt_id"] = privateID
				return mustMarshalLaunchEvidenceDocument(t, valid)
			},
			wantError: "runattempt launch evidence: identity mismatch",
		},
		{
			name: "private kind mismatch",
			body: func(valid map[string]any) []byte {
				valid["kind"] = privateKind
				return mustMarshalLaunchEvidenceDocument(t, valid)
			},
			wantError: "runattempt launch evidence: event type mismatch",
		},
		{
			name: "zero timestamp",
			body: func(valid map[string]any) []byte {
				valid["at"] = zeroTimestamp
				return mustMarshalLaunchEvidenceDocument(t, valid)
			},
			wantError: "runattempt launch evidence: invalid timestamp",
		},
		{
			name: "trailing JSON",
			body: func(valid map[string]any) []byte {
				body := mustMarshalLaunchEvidenceDocument(t, valid)
				return append(body, []byte("\n{\""+privateKey+"\":\""+privateValue+"\"}\n")...)
			},
			wantError: "runattempt launch evidence: trailing document",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			homePath := t.TempDir()
			store := Open(homePath)
			attempt, err := store.Mint("work_launch_exact_schema", "actor:test", "harness:test", created)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(launchEvidenceDirectory(homePath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(launchEvidenceDirectory(homePath), 0o700); err != nil {
				t.Fatal(err)
			}
			valid := map[string]any{
				"schema_version": launchEvidenceSchemaVersion,
				"attempt_id":     attempt.ID,
				"kind":           launchEvidencePrepared,
				"at":             created.Add(time.Minute).Format(time.RFC3339Nano),
			}
			if err := os.WriteFile(
				launchEvidencePath(homePath, attempt.ID, launchEvidencePrepared),
				test.body(valid),
				0o600,
			); err != nil {
				t.Fatal(err)
			}

			_, err = readLaunchEvidence(homePath, attempt.ID, launchEvidencePrepared)
			if err == nil {
				t.Fatal("readLaunchEvidence accepted a non-exact launch sidecar")
			}
			if err.Error() != test.wantError {
				t.Fatalf("read error = %q, want fixed category %q", err, test.wantError)
			}
			for _, planted := range []string{privateKey, privateValue, privateSchema, privateID, privateKind} {
				if strings.Contains(err.Error(), planted) {
					t.Fatalf("launch sidecar read error leaked planted text %q: %v", planted, err)
				}
			}

			got, getErr := Open(homePath).Get(attempt.ID)
			if getErr == nil {
				t.Fatalf("Get accepted non-exact launch sidecar: %+v", got.Launch)
			}
			if got.Launch != nil {
				t.Fatalf("Get materialized Launch from non-exact sidecar: %+v", got.Launch)
			}
			if !strings.HasSuffix(getErr.Error(), test.wantError) {
				t.Fatalf("Get error = %q, want fixed category suffix %q", getErr, test.wantError)
			}
			for _, planted := range []string{privateKey, privateValue, privateSchema, privateID, privateKind} {
				if strings.Contains(getErr.Error(), planted) {
					t.Fatalf("Get error leaked planted text %q: %v", planted, getErr)
				}
			}
		})
	}
}

func mustMarshalLaunchEvidenceDocument(t *testing.T, document map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCommitLaunchPersistsContentFreeStartError(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_error", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	privateError := errors.New("private executable path /secret/operator/bin")
	launched, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error { return privateError })
	if !errors.Is(err, privateError) {
		t.Fatalf("CommitLaunch error = %v, want original live error", err)
	}
	if launched.Launch == nil || launched.Launch.PreparedAt == nil || launched.Launch.StartErrorAt == nil {
		t.Fatalf("start_error launch lifecycle not persisted: %+v", launched.Launch)
	}
	if launched.Launch.CommittedAt != nil {
		t.Fatalf("failed launch was committed: %+v", launched.Launch)
	}
	body, err := os.ReadFile(filepath.Join(home, "run_attempts", attempt.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), privateError.Error()) || strings.Contains(string(body), "/secret/operator/bin") {
		t.Fatalf("durable launch evidence leaked the raw start error: %s", body)
	}
}

func TestCommitLaunchPersistsExactStartErrorDisposition(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launchAt := created.Add(time.Minute)
	attempt, err := store.Mint("work_launch_exact_start_error", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	privateStartErr := errors.New("private executable /operator/secret/bin failed")
	if _, err := store.CommitLaunch(attempt.ID, launchAt, func() error { return privateStartErr }); !errors.Is(err, privateStartErr) {
		t.Fatalf("CommitLaunch error = %v, want original start error", err)
	}

	startErrorPath := launchEvidencePath(home, attempt.ID, "start_error")
	body, err := os.ReadFile(startErrorPath)
	if err != nil {
		t.Fatalf("read exact start_error disposition: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	if got := document["kind"]; got != "start_error" {
		t.Fatalf("start-error sidecar kind = %#v, want start_error", got)
	}
	if _, exists := document["reason"]; exists {
		t.Fatalf("start_error sidecar persisted a free-form reason: %s", body)
	}
	if bytes.Contains(body, []byte(privateStartErr.Error())) || bytes.Contains(body, []byte("/operator/secret/bin")) {
		t.Fatalf("start_error sidecar leaked private callback text: %s", body)
	}
	if _, err := os.Lstat(launchEvidencePath(home, attempt.ID, "ambiguous")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy ambiguous disposition exists: err=%v", err)
	}

	fresh, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleBody, err := json.Marshal(fresh.Launch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(lifecycleBody, []byte(`"start_error_at"`)) || bytes.Contains(lifecycleBody, []byte("ambiguous")) {
		t.Fatalf("materialized lifecycle vocabulary is not exact: %s", lifecycleBody)
	}

	committed := launchEvidence{
		SchemaVersion: launchEvidenceSchemaVersion,
		AttemptID:     attempt.ID,
		Kind:          "committed",
		At:            launchAt,
	}
	if err := writeLaunchEvidence(home, committed); err == nil {
		t.Fatal("terminal start_error disposition accepted a later committed disposition")
	}
}

func TestLoadLaunchLifecycleRetriesConcurrentDispositionPublication(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launchAt := created.Add(time.Minute)
	attempt, err := store.Mint("work_launch_coherent_read", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}

	originalRead := runattemptReadLaunchEvidence
	var preparedReads atomic.Int32
	runattemptReadLaunchEvidence = func(homePath, id, kind string) (launchEvidence, error) {
		if kind == launchEvidencePrepared && preparedReads.Add(1) == 1 {
			// Force the rejected interleaving: this read observed prepared as
			// absent, then a legitimate writer published prepared + committed
			// before the reader checked the disposition.
			if err := writeLaunchEvidence(homePath, launchEvidence{
				SchemaVersion: launchEvidenceSchemaVersion,
				AttemptID:     id,
				Kind:          launchEvidencePrepared,
				At:            launchAt,
			}); err != nil {
				t.Fatalf("publish interleaved prepared evidence: %v", err)
			}
			if err := writeLaunchEvidence(homePath, launchEvidence{
				SchemaVersion: launchEvidenceSchemaVersion,
				AttemptID:     id,
				Kind:          launchEvidenceCommitted,
				At:            launchAt,
			}); err != nil {
				t.Fatalf("publish interleaved committed evidence: %v", err)
			}
			return launchEvidence{}, os.ErrNotExist
		}
		return originalRead(homePath, id, kind)
	}
	defer func() { runattemptReadLaunchEvidence = originalRead }()

	got, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatalf("coherent lifecycle read reported false corruption: %v", err)
	}
	if got.Launch == nil || got.Launch.PreparedAt == nil || got.Launch.CommittedAt == nil {
		t.Fatalf("coherent lifecycle read missed committed generation: %+v", got.Launch)
	}
	if got.Launch.StartErrorAt != nil {
		t.Fatalf("coherent committed lifecycle also materialized start_error: %+v", got.Launch)
	}
}

func TestCommitLaunchCommitWriteFailureReturnsDurablePreparedState(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_commit_failure", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	originalWrite := runattemptWriteLaunchEvidence
	writes := 0
	runattemptWriteLaunchEvidence = func(homePath string, evidence launchEvidence) error {
		writes++
		if writes == 2 {
			return errors.New("injected committed write failure")
		}
		return originalWrite(homePath, evidence)
	}
	defer func() { runattemptWriteLaunchEvidence = originalWrite }()

	started := false
	returned, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error {
		started = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "committed write failure") {
		t.Fatalf("CommitLaunch error = %v, want committed write failure", err)
	}
	if !started {
		t.Fatal("start callback did not run")
	}
	if returned.Launch == nil || returned.Launch.PreparedAt == nil || returned.Launch.CommittedAt != nil {
		t.Fatalf("returned record is not the durable prepared generation: %+v", returned.Launch)
	}

	runattemptWriteLaunchEvidence = originalWrite
	fresh, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Launch == nil || fresh.Launch.PreparedAt == nil || fresh.Launch.CommittedAt != nil {
		t.Fatalf("durable record after commit failure = %+v, want prepared-only", fresh.Launch)
	}
}

func TestCommitLaunchPreparedWriteFailureNeverCallsStart(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_prepare_failure", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected prepared write failure")
	originalWrite := runattemptWriteLaunchEvidence
	runattemptWriteLaunchEvidence = func(string, launchEvidence) error { return injected }
	defer func() { runattemptWriteLaunchEvidence = originalWrite }()

	started := false
	returned, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error {
		started = true
		return nil
	})
	if !errors.Is(err, injected) {
		t.Fatalf("CommitLaunch error = %v, want prepared write failure", err)
	}
	if started {
		t.Fatal("start callback ran after prepared publication failed")
	}
	if returned.ID != "" {
		t.Fatalf("returned attempt = %+v, want zero value before durable prepare", returned)
	}

	runattemptWriteLaunchEvidence = originalWrite
	fresh, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != StatusPending || fresh.Launch != nil {
		t.Fatalf("failed prepare changed durable attempt: status=%s launch=%+v", fresh.Status, fresh.Launch)
	}
}

func TestCommitLaunchStartErrorAndDispositionWriteFailureReportBothWithoutPersistingPrivateText(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_ambiguity_failure", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	privateStartErr := errors.New("private executable /operator/secret/bin failed")
	dispositionWriteErr := errors.New("injected start_error disposition write failure")
	originalWrite := runattemptWriteLaunchEvidence
	writes := 0
	runattemptWriteLaunchEvidence = func(homePath string, evidence launchEvidence) error {
		writes++
		if writes == 2 {
			return dispositionWriteErr
		}
		return originalWrite(homePath, evidence)
	}
	defer func() { runattemptWriteLaunchEvidence = originalWrite }()

	returned, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error { return privateStartErr })
	if !errors.Is(err, privateStartErr) || !errors.Is(err, dispositionWriteErr) {
		t.Fatalf("CommitLaunch error = %v, want both start and disposition-write errors", err)
	}
	if returned.Launch == nil || returned.Launch.PreparedAt == nil || returned.Launch.StartErrorAt != nil {
		t.Fatalf("returned attempt = %+v, want durable prepared-only evidence", returned.Launch)
	}

	body, err := os.ReadFile(filepath.Join(home, "run_attempts", attempt.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(privateStartErr.Error())) || bytes.Contains(body, []byte("/operator/secret/bin")) {
		t.Fatalf("prepared-only evidence persisted private start text: %s", body)
	}
}

func TestCommitLaunchDirectorySyncFailureStopsBeforeStart(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_directory_sync_failure", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected runattempt directory sync failure")
	originalSync := runattemptSyncDirectory
	runattemptSyncDirectory = func(string) error { return injected }
	defer func() { runattemptSyncDirectory = originalSync }()

	started := false
	_, err = store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error {
		started = true
		return nil
	})
	if !errors.Is(err, injected) {
		t.Fatalf("CommitLaunch error = %v, want directory sync failure", err)
	}
	if started {
		t.Fatal("start callback ran after prepared directory sync failed")
	}
}

func TestCommitLaunchAllowsUnrelatedAttemptMutation(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launching, err := store.Mint("work_launch_blocked", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.Mint("work_unrelated_heartbeat", "actor:test", "harness:test", created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(launching.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLaunch := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseLaunch()
	launchDone := make(chan error, 1)
	go func() {
		_, launchErr := store.CommitLaunch(launching.ID, created.Add(time.Minute), func() error {
			close(entered)
			<-release
			return nil
		})
		launchDone <- launchErr
	}()
	<-entered

	heartbeatAt := created.Add(2 * time.Minute)
	updated, acquired, err := Open(home).tryMutate(unrelated.ID, heartbeatMutation(heartbeatAt))
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("unrelated mutation was blocked behind an external start callback")
	}
	if updated.LastHeartbeatAt == nil || !updated.LastHeartbeatAt.Equal(heartbeatAt) {
		t.Fatalf("unrelated heartbeat = %v, want %s", updated.LastHeartbeatAt, heartbeatAt)
	}

	releaseLaunch()
	if err := <-launchDone; err != nil {
		t.Fatalf("launch: %v", err)
	}
}

func TestConcurrentCommitLaunchStartsSameAttemptOnlyOnce(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_launch_once", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, launchErr := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
		firstDone <- launchErr
	}()
	<-firstEntered

	var secondStarts atomic.Int32
	secondDone := make(chan error, 1)
	go func() {
		_, launchErr := Open(home).CommitLaunch(attempt.ID, created.Add(2*time.Minute), func() error {
			secondStarts.Add(1)
			return nil
		})
		secondDone <- launchErr
	}()
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if err := <-secondDone; err == nil {
		t.Fatal("second launch unexpectedly succeeded")
	}
	if got := secondStarts.Load(); got != 0 {
		t.Fatalf("second callback ran %d times, want 0", got)
	}
}

func TestCommitLaunchCrossProcessPerAttemptExclusion(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launching, err := store.Mint("work_cross_process_launch", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := store.Mint("work_cross_process_unrelated", "actor:test", "harness:test", created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{launching.ID, unrelated.ID} {
		if _, err := store.SetOrigin(id, OriginHarnessExecution); err != nil {
			t.Fatal(err)
		}
	}

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		readyWrite.Close()
		t.Fatal(err)
	}

	launchAt := created.Add(time.Minute)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCommitLaunchCrossProcessHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		launchLockHelperModeEnv+"=hold-launch",
		launchLockHelperHomeEnv+"="+home,
		launchLockHelperIDEnv+"="+launching.ID,
		launchLockHelperTimeEnv+"="+launchAt.Format(time.RFC3339Nano),
	)
	cmd.ExtraFiles = []*os.File{readyWrite, releaseRead}
	var helperOutput bytes.Buffer
	cmd.Stdout = &helperOutput
	cmd.Stderr = &helperOutput
	if err := cmd.Start(); err != nil {
		readyWrite.Close()
		releaseRead.Close()
		releaseWrite.Close()
		t.Fatalf("start launch helper: %v", err)
	}
	readyWrite.Close()
	releaseRead.Close()

	helperDone := make(chan error, 1)
	go func() { helperDone <- cmd.Wait() }()
	var releaseOnce sync.Once
	releaseHelper := func() {
		releaseOnce.Do(func() {
			_, _ = releaseWrite.Write([]byte{1})
			_ = releaseWrite.Close()
		})
	}
	helperWaited := false
	defer func() {
		releaseHelper()
		if helperWaited {
			return
		}
		select {
		case <-helperDone:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-helperDone
		}
	}()

	ready := make(chan error, 1)
	go func() {
		var token [1]byte
		_, readErr := io.ReadFull(readyRead, token[:])
		ready <- readErr
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("wait for durable prepared launch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launch helper did not reach the prepared callback")
	}

	callbackRan := false
	acquired, lockErr := Open(home).tryWithAttemptLock(launching.ID, func() error {
		callbackRan = true
		return nil
	})
	if lockErr != nil {
		t.Fatalf("same-attempt nonblocking probe: %v", lockErr)
	}
	if acquired || callbackRan {
		t.Fatal("same-attempt mutation escaped held cross-process launch lock")
	}

	// v0.1 work-admission contract: every CommitLaunch holds the compound-home
	// mutation guard for the entire callback duration. The helper's parked
	// start callback holds that guard. An unrelated CommitLaunch in the
	// parent cannot run while the helper holds the guard — and the helper
	// cannot release it until we write to releaseWrite. Release the helper
	// first, wait for it to finish (which releases every lock), then drive
	// attempt B's commit. Cross-process per-attempt exclusion is already
	// proven by the tryWithAttemptLock probe above while the helper is
	// parked; the unrelated-launch check below proves that a sibling attempt
	// proceeds normally once the home mutation guard is released.
	releaseHelper()
	select {
	case helperErr := <-helperDone:
		helperWaited = true
		if helperErr != nil {
			t.Fatalf("helper CommitLaunch: %v", helperErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launch helper did not complete after release")
	}

	var unrelatedStarts atomic.Int32
	if _, err := Open(home).CommitLaunch(unrelated.ID, launchAt.Add(time.Second), func() error {
		unrelatedStarts.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("attempt B launch after attempt A completes: %v", err)
	}
	if got := unrelatedStarts.Load(); got != 1 {
		t.Fatalf("attempt B start callback count = %d, want 1", got)
	}

	const transcriptRef = "transcript://cross-process-preserved"
	if _, err := Open(home).SetTranscript(launching.ID, transcriptRef); err != nil {
		t.Fatalf("attempt A mutation after per-attempt lock release: %v", err)
	}

	gotA, err := Open(home).Get(launching.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotA.TranscriptRef != transcriptRef {
		t.Fatalf("attempt A lost the queued mutation: transcript_ref = %q, want %q", gotA.TranscriptRef, transcriptRef)
	}
	if gotA.Launch == nil || gotA.Launch.CommittedAt == nil {
		t.Fatalf("attempt A lost its committed launch disposition: %+v", gotA.Launch)
	}
	gotB, err := Open(home).Get(unrelated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotB.Launch == nil || gotB.Launch.CommittedAt == nil {
		t.Fatalf("attempt B did not commit independently: %+v", gotB.Launch)
	}
}

func TestSweepStalePreparedOnlyLaunchEscalatesAmbiguous(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launchAt := created.Add(time.Minute)
	attempt, err := store.Mint("work_sweep_prepared_launch", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}

	originalWrite := runattemptWriteLaunchEvidence
	writes := 0
	runattemptWriteLaunchEvidence = func(homePath string, evidence launchEvidence) error {
		writes++
		if writes == 2 {
			return errors.New("injected launch disposition write failure")
		}
		return originalWrite(homePath, evidence)
	}
	_, launchErr := store.CommitLaunch(attempt.ID, launchAt, func() error { return nil })
	runattemptWriteLaunchEvidence = originalWrite
	if launchErr == nil || !strings.Contains(launchErr.Error(), "launch disposition write failure") {
		t.Fatalf("CommitLaunch error = %v, want injected disposition write failure", launchErr)
	}

	report, err := Open(home).SweepStale(time.Minute, launchAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Escalated != 1 || report.AutoClosed != 0 {
		t.Fatalf("prepared-only sweep classification = %+v, want escalated=1 auto_closed=0", report)
	}
	got, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SweepClassification != SweepClassificationAmbiguous {
		t.Fatalf("prepared-only persisted classification = %q, want %q", got.SweepClassification, SweepClassificationAmbiguous)
	}
}

func TestSweepStaleCommittedLaunchWithoutArtifactsEscalatesAmbiguous(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launchAt := created.Add(time.Minute)
	attempt, err := store.Mint("work_sweep_committed_launch", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitLaunch(attempt.ID, launchAt, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	report, err := Open(home).SweepStale(time.Minute, launchAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.Escalated != 1 || report.AutoClosed != 0 {
		t.Fatalf("committed no-artifact sweep classification = %+v, want escalated=1 auto_closed=0", report)
	}
	got, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SweepClassification != SweepClassificationAmbiguous {
		t.Fatalf("committed no-artifact persisted classification = %q, want %q", got.SweepClassification, SweepClassificationAmbiguous)
	}
}

func TestSweepStaleExactStartErrorAutoClosesSideEffectFree(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launchAt := created.Add(time.Minute)
	attempt, err := store.Mint("work_sweep_start_error", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	startErr := errors.New("executable refused before start")
	if _, err := store.CommitLaunch(attempt.ID, launchAt, func() error { return startErr }); !errors.Is(err, startErr) {
		t.Fatalf("CommitLaunch error = %v, want start error", err)
	}

	report, err := Open(home).SweepStale(time.Minute, launchAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if report.AutoClosed != 1 || report.Escalated != 0 {
		t.Fatalf("exact start_error sweep classification = %+v, want auto_closed=1 escalated=0", report)
	}
	got, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SweepClassification != SweepClassificationSideEffectFree {
		t.Fatalf("start_error persisted classification = %q, want %q", got.SweepClassification, SweepClassificationSideEffectFree)
	}
}

func TestSweepSkipsBusyLaunchAndContinuesToLaterAttempt(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	launching, err := store.Mint("work_busy_launch", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	later, err := store.Mint("work_later_stale", "actor:test", "harness:test", created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(launching.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(later.ID, OriginClaimReservation); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLaunch := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseLaunch()
	launchDone := make(chan error, 1)
	launchAt := created.Add(time.Minute)
	go func() {
		_, launchErr := store.CommitLaunch(launching.ID, launchAt, func() error {
			close(entered)
			<-release
			return nil
		})
		launchDone <- launchErr
	}()
	<-entered

	type sweepResult struct {
		report SweepReport
		err    error
	}
	sweepDone := make(chan sweepResult, 1)
	go func() {
		report, sweepErr := Open(home).SweepStale(time.Second, launchAt.Add(time.Hour))
		sweepDone <- sweepResult{report: report, err: sweepErr}
	}()
	select {
	case result := <-sweepDone:
		if result.err != nil {
			t.Fatalf("SweepStale: %v", result.err)
		}
		if result.report.Scanned != 2 || result.report.Swept != 1 || result.report.Skipped != 1 {
			t.Fatalf("sweep while launch busy = %+v, want later attempt swept and launch skipped", result.report)
		}
		if len(result.report.SweptIDs) != 1 || result.report.SweptIDs[0] != later.ID {
			t.Fatalf("swept ids = %v, want only %s", result.report.SweptIDs, later.ID)
		}
	case <-time.After(2 * time.Second):
		releaseLaunch()
		<-launchDone
		<-sweepDone
		t.Fatal("sweep blocked on one busy launch instead of continuing")
	}

	releaseLaunch()
	if err := <-launchDone; err != nil {
		t.Fatalf("launch: %v", err)
	}
}

func TestRunAttemptMutationHonorsLegacyGlobalCompatibilityFence(t *testing.T) {
	home := t.TempDir()
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	store := Open(home)
	attempt, err := store.Mint("work_legacy_fence", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		readyWrite.Close()
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCommitLaunchCrossProcessHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		launchLockHelperModeEnv+"=hold-legacy-lock",
		launchLockHelperHomeEnv+"="+home,
	)
	cmd.ExtraFiles = []*os.File{readyWrite, releaseRead}
	var helperOutput bytes.Buffer
	cmd.Stdout = &helperOutput
	cmd.Stderr = &helperOutput
	if err := cmd.Start(); err != nil {
		readyWrite.Close()
		releaseRead.Close()
		releaseWrite.Close()
		t.Fatalf("start legacy lock helper: %v", err)
	}
	readyWrite.Close()
	releaseRead.Close()
	helperDone := make(chan error, 1)
	go func() { helperDone <- cmd.Wait() }()
	var releaseOnce sync.Once
	releaseHelper := func() {
		releaseOnce.Do(func() {
			_, _ = releaseWrite.Write([]byte{1})
			_ = releaseWrite.Close()
		})
	}
	helperWaited := false
	defer func() {
		releaseHelper()
		if helperWaited {
			return
		}
		select {
		case <-helperDone:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-helperDone
		}
	}()

	var readyToken [1]byte
	if _, err := io.ReadFull(readyRead, readyToken[:]); err != nil {
		t.Fatalf("wait for legacy lock helper: %v\n%s", err, helperOutput.String())
	}
	callbackRan := false
	acquired, err := Open(home).tryWithAttemptLock(attempt.ID, func() error {
		callbackRan = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if acquired || callbackRan {
		t.Fatal("new mutation ignored the held legacy runattempts.lock fence")
	}

	releaseHelper()
	select {
	case err := <-helperDone:
		helperWaited = true
		if err != nil {
			t.Fatalf("legacy lock helper: %v\n%s", err, helperOutput.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("legacy lock helper did not release")
	}
	if _, err := Open(home).Heartbeat(attempt.ID, created.Add(time.Minute)); err != nil {
		t.Fatalf("mutation after legacy compatibility-fence release: %v", err)
	}
}

func TestLegacyV01RewriteCannotEraseLaunchSidecar(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_quiesced_launch", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetOrigin(attempt.ID, OriginHarnessExecution); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitLaunch(attempt.ID, created.Add(time.Minute), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	legacyView, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyView.Launch = nil // an old v0.1 decoder does not know this field
	legacyView.TranscriptRef = "transcript://legacy-rewrite"
	acquired, err := store.withLegacyRunAttemptLockMode(false, func() error {
		return runattemptWriteFile(store.path(attempt.ID), legacyView)
	})
	if err != nil {
		t.Fatalf("simulated legacy rewrite: %v", err)
	}
	if !acquired {
		t.Fatal("blocking simulated legacy rewrite did not acquire compatibility lock")
	}
	got, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Launch == nil || got.Launch.PreparedAt == nil || got.Launch.CommittedAt == nil {
		t.Fatalf("legacy v0.1 rewrite erased launch sidecar: %+v", got.Launch)
	}
	if got.TranscriptRef != "transcript://legacy-rewrite" {
		t.Fatalf("legacy v0.1 rewrite did not land ordinary evidence: %q", got.TranscriptRef)
	}
}

func TestGetIgnoresInlineLaunchWithoutSidecars(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_inline_launch_get", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}

	plantInlineLaunchSpoof(t, home, attempt.ID, map[string]any{
		"prepared_at":  created.Add(time.Minute).Format(time.RFC3339Nano),
		"committed_at": created.Add(time.Minute).Format(time.RFC3339Nano),
	})

	got, err := Open(home).Get(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Launch != nil {
		t.Fatalf("Get trusted non-authoritative inline launch JSON: %+v", got.Launch)
	}
	assertNoLaunchSidecars(t, home, attempt.ID)
}

func TestMutationCannotPromoteInlineLaunchIntoSidecars(t *testing.T) {
	home := t.TempDir()
	store := Open(home)
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	attempt, err := store.Mint("work_inline_launch_mutation", "actor:test", "harness:test", created)
	if err != nil {
		t.Fatal(err)
	}

	plantInlineLaunchSpoof(t, home, attempt.ID, map[string]any{
		"prepared_at":    created.Add(time.Minute).Format(time.RFC3339Nano),
		"start_error_at": created.Add(time.Minute).Format(time.RFC3339Nano),
	})

	updated, err := Open(home).SetTranscript(attempt.ID, "transcript://inline-spoof-regression")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Launch != nil {
		t.Fatalf("ordinary mutation trusted inline launch JSON: %+v", updated.Launch)
	}
	assertNoLaunchSidecars(t, home, attempt.ID)
}

func plantInlineLaunchSpoof(t *testing.T, homePath, id string, launch map[string]any) {
	t.Helper()
	path := filepath.Join(homePath, "run_attempts", id+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	document["launch"] = launch
	body, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertNoLaunchSidecars(t *testing.T, homePath, id string) {
	t.Helper()
	for _, kind := range []string{"prepared", "committed", "start_error", "ambiguous"} {
		path := launchEvidencePath(homePath, id, kind)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("non-authoritative inline launch materialized %s sidecar: err=%v", kind, err)
		}
	}
}

func TestRunAttemptLockRejectsSymlinksBeneathPinnedHome(t *testing.T) {
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	id := MintDeterministic("lock-symlink", created)
	for _, fixture := range []string{"lock", "runattempts", "leaf"} {
		t.Run(fixture, func(t *testing.T) {
			home := t.TempDir()
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel.lock")
			if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch fixture {
			case "lock":
				if err := os.Symlink(outside, filepath.Join(home, "lock")); err != nil {
					t.Skipf("symlink fixture unavailable: %v", err)
				}
			case "runattempts":
				if err := os.Mkdir(filepath.Join(home, "lock"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(home, "lock", "runattempts")); err != nil {
					t.Skipf("symlink fixture unavailable: %v", err)
				}
			case "leaf":
				if err := os.MkdirAll(filepath.Join(home, "lock", "runattempts"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sentinel, filepath.Join(home, "lock", "runattempts", id+".lock")); err != nil {
					t.Skipf("symlink fixture unavailable: %v", err)
				}
			}

			if _, err := Open(home).MintWithID(id, "work_symlink", "actor:test", "harness:test", created); err == nil {
				t.Fatal("RunAttempt mutation accepted a symlinked lock path")
			}
			body, err := os.ReadFile(sentinel)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != "unchanged" {
				t.Fatalf("external sentinel changed: %q", body)
			}
			for _, escaped := range []string{
				filepath.Join(outside, "runattempts", id+".lock"),
				filepath.Join(outside, id+".lock"),
			} {
				if _, err := os.Lstat(escaped); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("lock creation escaped pinned home at %s: %v", escaped, err)
				}
			}
		})
	}
}

func TestRunAttemptWriteRejectsSymlinkedRecordDirectoryWithoutExternalWrites(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "run_attempts")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	id := MintDeterministic("record-directory-symlink", created)
	if _, err := Open(home).MintWithID(id, "work_record_symlink", "actor:test", "harness:test", created); err == nil {
		t.Fatal("RunAttempt write accepted a symlinked record directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("RunAttempt bytes escaped pinned home: %v", entries)
	}
}

func TestRunAttemptLockRejectsHardLinkedLeaf(t *testing.T) {
	home := t.TempDir()
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	id := MintDeterministic("hard-linked-lock", created)
	lockDir := filepath.Join(home, "lock", "runattempts")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(home, "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(sentinel, filepath.Join(lockDir, id+".lock")); err != nil {
		t.Skipf("hard-link fixture unavailable: %v", err)
	}
	if _, err := Open(home).MintWithID(id, "work_hard_link", "actor:test", "harness:test", created); err == nil {
		t.Fatal("RunAttempt mutation accepted a hard-linked lock leaf")
	}
	body, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "unchanged" {
		t.Fatalf("hard-linked sentinel changed: %q", body)
	}
}

func TestRunAttemptLockRejectsUnsafeModesAndOwnership(t *testing.T) {
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, home, id string)
	}{
		{name: "writable home", setup: func(t *testing.T, home, _ string) {
			t.Helper()
			if err := os.Chmod(home, 0o770); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "writable lock directory", setup: func(t *testing.T, home, _ string) {
			t.Helper()
			path := filepath.Join(home, "lock")
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "legacy lock leaf not private", setup: func(t *testing.T, home, _ string) {
			t.Helper()
			if err := os.Mkdir(filepath.Join(home, "lock"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "lock", "runattempts.lock")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "per-attempt directory not private", setup: func(t *testing.T, home, _ string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(home, "lock", "runattempts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(home, "lock", "runattempts"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "lock leaf not private", setup: func(t *testing.T, home, id string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(home, "lock", "runattempts"), 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "lock", "runattempts", id+".lock")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			id := MintDeterministic("unsafe-mode-"+test.name, created)
			test.setup(t, home, id)
			if _, err := Open(home).MintWithID(id, "work_unsafe_mode", "actor:test", "harness:test", created); err == nil {
				t.Fatal("RunAttempt mutation accepted unsafe lock custody")
			}
		})
	}

	home := t.TempDir()
	originalUID := runattemptCurrentUID
	runattemptCurrentUID = func() int { return originalUID() + 1 }
	defer func() { runattemptCurrentUID = originalUID }()
	id := MintDeterministic("wrong-owner", created)
	if _, err := Open(home).MintWithID(id, "work_wrong_owner", "actor:test", "harness:test", created); err == nil {
		t.Fatal("RunAttempt mutation accepted lock custody owned by a different uid")
	}
}

func TestRunAttemptLockRejectsLeafReplacementRace(t *testing.T) {
	home := t.TempDir()
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	id := MintDeterministic("lock-replacement-race", created)
	lockDir := filepath.Join(home, "lock", "runattempts")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, id+".lock")
	replacementPath := filepath.Join(lockDir, id+".replacement")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	originalHook := runattemptLockBeforeOpen
	swapped := false
	runattemptLockBeforeOpen = func(kind, path string) {
		if kind != "per-attempt" || swapped {
			return
		}
		swapped = true
		if err := os.Rename(path, path+".original"); err != nil {
			t.Fatalf("move validated lock leaf: %v", err)
		}
		if err := os.Symlink(filepath.Base(replacementPath), path); err != nil {
			t.Fatalf("replace validated lock leaf: %v", err)
		}
	}
	defer func() { runattemptLockBeforeOpen = originalHook }()

	if _, err := Open(home).MintWithID(id, "work_lock_race", "actor:test", "harness:test", created); err == nil {
		t.Fatal("RunAttempt mutation accepted a lock leaf replaced after validation")
	}
	if !swapped {
		t.Fatal("replacement-race hook did not run")
	}
	if _, err := os.Stat(filepath.Join(home, "run_attempts", id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record published after lock replacement race: %v", err)
	}
}

func TestRunAttemptLockSurfacesUnlockAndCloseAmbiguity(t *testing.T) {
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	t.Run("unlock", func(t *testing.T) {
		home := t.TempDir()
		id := MintDeterministic("unlock-failure", created)
		injected := errors.New("injected unlock failure")
		originalFlock := runattemptFlock
		failed := false
		runattemptFlock = func(fd int, operation int) error {
			if operation == syscall.LOCK_UN && !failed {
				failed = true
				actualErr := originalFlock(fd, operation)
				return errors.Join(actualErr, injected)
			}
			return originalFlock(fd, operation)
		}
		defer func() { runattemptFlock = originalFlock }()
		if _, err := Open(home).MintWithID(id, "work_unlock_failure", "actor:test", "harness:test", created); !errors.Is(err, injected) {
			t.Fatalf("MintWithID error = %v, want surfaced unlock ambiguity", err)
		}
	})

	t.Run("close", func(t *testing.T) {
		home := t.TempDir()
		id := MintDeterministic("close-failure", created)
		injected := errors.New("injected lock close failure")
		originalClose := runattemptCloseLockFile
		failed := false
		runattemptCloseLockFile = func(file *os.File) error {
			actualErr := originalClose(file)
			if !failed {
				failed = true
				return errors.Join(actualErr, injected)
			}
			return actualErr
		}
		defer func() { runattemptCloseLockFile = originalClose }()
		if _, err := Open(home).MintWithID(id, "work_close_failure", "actor:test", "harness:test", created); !errors.Is(err, injected) {
			t.Fatalf("MintWithID error = %v, want surfaced close ambiguity", err)
		}
	})
}

func TestRunAttemptLockAcceptsPinnedHomeSymlinkAlias(t *testing.T) {
	parent := t.TempDir()
	realHome := filepath.Join(parent, "real-home")
	otherHome := filepath.Join(parent, "other-home")
	aliasHome := filepath.Join(parent, "home-alias")
	if err := os.Mkdir(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(otherHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realHome, aliasHome); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	store := Open(aliasHome)
	if err := os.Remove(aliasHome); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherHome, aliasHome); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	if _, err := store.Mint("work_alias", "actor:test", "harness:test", created); err != nil {
		t.Fatalf("mint through pinned home alias: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(realHome, "run_attempts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("run attempts through alias = %d, want 1", len(entries))
	}
	if _, err := os.Stat(filepath.Join(otherHome, "run_attempts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retargeted alias received RunAttempt bytes: %v", err)
	}
}

func TestCanonicalAttemptLockKeyConvergesAcrossHomeAliases(t *testing.T) {
	parent := t.TempDir()
	realHome := filepath.Join(parent, "real-home")
	aliasHome := filepath.Join(parent, "home-alias")
	if err := os.Mkdir(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realHome, aliasHome); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	id := MintDeterministic("canonical-attempt-lock", time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC))
	realKey, err := canonicalAttemptLockKey(realHome, id)
	if err != nil {
		t.Fatal(err)
	}
	aliasKey, err := canonicalAttemptLockKey(aliasHome, id)
	if err != nil {
		t.Fatal(err)
	}
	if realKey != aliasKey {
		t.Fatalf("canonical lock keys differ: real=%q alias=%q", realKey, aliasKey)
	}
}
