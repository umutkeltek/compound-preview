package home

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newInitializedAdmissionHome(t *testing.T) string {
	t.Helper()
	homePath := filepath.Join(t.TempDir(), "home")
	if err := Init(homePath, homeKindTest, time.Unix(1, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	return homePath
}

func TestOpenDescendantRootRequiresPreScaffoldedAuthorityWithoutFootprint(t *testing.T) {
	homePath := newInitializedAdmissionHome(t)
	workID := "work_missing_descendant_root"
	relative := "run_attempts"
	rootPath := filepath.Join(homePath, relative)
	if err := os.Remove(rootPath); err != nil {
		t.Fatal(err)
	}

	var openErr error
	if err := WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
		var root *AdmittedDescendantRoot
		root, openErr = guard.OpenDescendantRoot(homePath, workID, relative, "run attempts", 0o755)
		if root != nil {
			return root.Close()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if openErr == nil || !strings.Contains(openErr.Error(), "must be pre-scaffolded") {
		t.Fatalf("missing descendant root error = %v, want pre-scaffolded refusal", openErr)
	}
	if _, err := os.Lstat(rootPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing descendant-root refusal materialized authority: %v", err)
	}
}

func TestOpenDescendantRootAcceptsStablePreScaffoldedAuthority(t *testing.T) {
	homePath := newInitializedAdmissionHome(t)
	workID := "work_stable_descendant_root"
	if err := WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
		root, err := guard.OpenDescendantRoot(homePath, workID, "run_attempts", "run attempts", 0o755)
		if err != nil {
			return err
		}
		defer root.Close()
		return root.Validate("stable pre-scaffolded root")
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdmittedDescendantRootRemovalRetainsWorkAdmissionAcrossIO(t *testing.T) {
	homePath := newInitializedAdmissionHome(t)
	workID := "work_descendant_retained_removal"
	leaf := "retained-removal.json"
	wantBody := []byte("retained removal\n")
	if err := os.WriteFile(filepath.Join(homePath, "run_attempts", leaf), wantBody, 0o600); err != nil {
		t.Fatal(err)
	}

	guardReady := make(chan *WorkAdmissionGuard, 1)
	operationEntered := make(chan struct{})
	releaseOperation := make(chan struct{})
	operationDone := make(chan error, 1)
	callbackReturned := make(chan struct{})
	outerDone := make(chan error, 1)
	defer func() {
		select {
		case <-releaseOperation:
		default:
			close(releaseOperation)
		}
	}()

	go func() {
		outerDone <- WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
			guardReady <- guard
			root, err := guard.OpenDescendantRoot(homePath, workID, "run_attempts", "run attempts", 0o755)
			if err != nil {
				return err
			}
			go func() {
				removeErr := root.RecoverRegularFileRemovalValidated(leaf, func(body []byte) error {
					if !bytes.Equal(body, wantBody) {
						return fmt.Errorf("validated body = %q, want %q", body, wantBody)
					}
					close(operationEntered)
					<-releaseOperation
					return nil
				})
				operationDone <- errors.Join(removeErr, root.Close())
			}()
			<-operationEntered
			close(callbackReturned)
			return nil
		})
	}()

	guard := <-guardReady
	<-callbackReturned
	waitForWorkAdmissionGuardInvalidation(t, guard, 1)
	select {
	case err := <-outerDone:
		t.Fatalf("outer admission returned during descriptor-relative removal: %v", err)
	default:
	}
	contenderEntered := false
	acquired, err := TryWithWorkAdmissionGuard(homePath, workID, func(*WorkAdmissionGuard) error {
		contenderEntered = true
		return nil
	})
	if err != nil {
		t.Fatalf("contender during descriptor-relative removal: %v", err)
	}
	if acquired || contenderEntered {
		t.Fatalf("contender bypassed descriptor-relative removal: acquired=%v entered=%v", acquired, contenderEntered)
	}

	close(releaseOperation)
	if err := <-operationDone; err != nil {
		t.Fatalf("descriptor-relative removal: %v", err)
	}
	if err := <-outerDone; err != nil {
		t.Fatalf("outer admission after descriptor-relative removal: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(homePath, "run_attempts", leaf)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed leaf remains after retained operation: %v", err)
	}
}

func TestAdmittedDescendantRootCloseWaitsForActiveOperation(t *testing.T) {
	homePath := newInitializedAdmissionHome(t)
	workID := "work_descendant_concurrent_close"
	leaf := "concurrent-close.json"
	if err := os.WriteFile(filepath.Join(homePath, "run_attempts", leaf), []byte("close fencing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
		root, err := guard.OpenDescendantRoot(homePath, workID, "run_attempts", "run attempts", 0o755)
		if err != nil {
			return err
		}
		operationEntered := make(chan struct{})
		releaseOperation := make(chan struct{})
		operationDone := make(chan error, 1)
		go func() {
			operationDone <- root.RecoverRegularFileRemovalValidated(leaf, func([]byte) error {
				close(operationEntered)
				<-releaseOperation
				return nil
			})
		}()
		<-operationEntered

		closeStarted := make(chan struct{})
		closeDone := make(chan error, 1)
		go func() {
			close(closeStarted)
			closeDone <- root.Close()
		}()
		<-closeStarted
		closeReturnedEarly := false
		var closeErr error
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case closeErr = <-closeDone:
			closeReturnedEarly = true
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		close(releaseOperation)
		operationErr := <-operationDone
		if !closeReturnedEarly {
			closeErr = <-closeDone
		}
		if closeReturnedEarly {
			return errors.Join(
				operationErr,
				closeErr,
				errors.New("admitted descendant root closed while an operation was active"),
			)
		}
		return errors.Join(operationErr, closeErr)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdmittedDescendantRootOperationPanicDrainsAdmissionAndReleasesClose(t *testing.T) {
	homePath := newInitializedAdmissionHome(t)
	workID := "work_descendant_operation_panic"
	leaf := "panic-removal.json"
	if err := os.WriteFile(filepath.Join(homePath, "run_attempts", leaf), []byte("panic fencing\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	guardReady := make(chan *WorkAdmissionGuard, 1)
	operationEntered := make(chan struct{})
	releaseOperation := make(chan struct{})
	operationPanicked := make(chan any, 1)
	rootClosed := make(chan error, 1)
	callbackReturned := make(chan struct{})
	outerDone := make(chan error, 1)
	defer func() {
		select {
		case <-releaseOperation:
		default:
			close(releaseOperation)
		}
	}()

	go func() {
		outerDone <- WithWorkAdmissionGuard(homePath, workID, func(guard *WorkAdmissionGuard) error {
			guardReady <- guard
			root, err := guard.OpenDescendantRoot(homePath, workID, "run_attempts", "run attempts", 0o755)
			if err != nil {
				return err
			}
			go func() {
				func() {
					defer func() { operationPanicked <- recover() }()
					_ = root.RecoverRegularFileRemovalValidated(leaf, func([]byte) error {
						close(operationEntered)
						<-releaseOperation
						panic("synthetic admitted operation panic")
					})
				}()
				rootClosed <- root.Close()
			}()
			<-operationEntered
			close(callbackReturned)
			return nil
		})
	}()

	guard := <-guardReady
	<-callbackReturned
	waitForWorkAdmissionGuardInvalidation(t, guard, 1)
	select {
	case err := <-outerDone:
		t.Fatalf("outer admission returned before panicking descendant operation drained: %v", err)
	default:
	}
	close(releaseOperation)
	select {
	case recovered := <-operationPanicked:
		if recovered != "synthetic admitted operation panic" {
			t.Fatalf("recovered panic = %v, want synthetic admitted operation panic", recovered)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for descendant-operation panic")
	}
	if err := <-rootClosed; err != nil {
		t.Fatalf("close after descendant-operation panic: %v", err)
	}
	if err := <-outerDone; err != nil {
		t.Fatalf("outer admission after descendant-operation panic: %v", err)
	}
}
