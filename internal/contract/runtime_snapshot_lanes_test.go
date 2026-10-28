package contract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"compound/internal/breadth"
	"compound/internal/capgraph"
)

const runtimeSnapshotLaneTestTimeout = 2 * time.Second

func awaitRuntimeSnapshotLaneValue[T any](t *testing.T, name string, values <-chan T) T {
	t.Helper()
	timer := time.NewTimer(runtimeSnapshotLaneTestTimeout)
	defer timer.Stop()
	select {
	case value, ok := <-values:
		if !ok {
			t.Fatalf("%s closed before delivering a value", name)
		}
		return value
	case <-timer.C:
		t.Fatalf("timed out after %s waiting for %s", runtimeSnapshotLaneTestTimeout, name)
		var zero T
		return zero
	}
}

func awaitRuntimeSnapshotLaneSignal(t *testing.T, name string, signal <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(runtimeSnapshotLaneTestTimeout)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timed out after %s waiting for %s", runtimeSnapshotLaneTestTimeout, name)
	}
}

func closeRuntimeSnapshotLaneSignalOnce(signal chan struct{}) func() {
	var once sync.Once
	return func() {
		once.Do(func() { close(signal) })
	}
}

func waitForRuntimeSnapshotLanePeer(name string, signal <-chan struct{}) error {
	timer := time.NewTimer(2 * runtimeSnapshotLaneTestTimeout)
	defer timer.Stop()
	select {
	case <-signal:
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out waiting for test peer %s", name)
	}
}

func TestJoinRuntimeSnapshotLanesActuallyOverlaps(t *testing.T) {
	leftStarted := make(chan struct{})
	rightStarted := make(chan struct{})
	release := make(chan struct{})
	signalLeftStarted := closeRuntimeSnapshotLaneSignalOnce(leftStarted)
	signalRightStarted := closeRuntimeSnapshotLaneSignalOnce(rightStarted)
	releaseLanes := closeRuntimeSnapshotLaneSignalOnce(release)
	t.Cleanup(releaseLanes)
	type joinedResult struct {
		left  int
		right string
		err   error
	}
	done := make(chan joinedResult, 1)
	go func() {
		left, right, err := joinRuntimeSnapshotLanes(
			func() (int, error) {
				signalLeftStarted()
				if err := waitForRuntimeSnapshotLanePeer("lane release", release); err != nil {
					return 0, err
				}
				return 7, nil
			},
			func() (string, error) {
				signalRightStarted()
				if err := waitForRuntimeSnapshotLanePeer("lane release", release); err != nil {
					return "", err
				}
				return "memory", nil
			},
		)
		done <- joinedResult{left: left, right: right, err: err}
	}()
	for name, started := range map[string]<-chan struct{}{"capability": leftStarted, "memory": rightStarted} {
		awaitRuntimeSnapshotLaneSignal(t, name+" lane start", started)
	}
	select {
	case result := <-done:
		t.Fatalf("joined helper returned before both lanes released: %+v", result)
	default:
	}
	releaseLanes()
	result := awaitRuntimeSnapshotLaneValue(t, "joined runtime lanes", done)
	if result.err != nil || result.left != 7 || result.right != "memory" {
		t.Fatalf("joined result = %+v", result)
	}
}

func TestJoinRuntimeSnapshotLanesWaitsForPeerOnError(t *testing.T) {
	wantErr := errors.New("capability failed")
	rightStarted := make(chan struct{})
	releaseRight := make(chan struct{})
	rightFinished := make(chan struct{})
	signalRightStarted := closeRuntimeSnapshotLaneSignalOnce(rightStarted)
	releaseRightLane := closeRuntimeSnapshotLaneSignalOnce(releaseRight)
	signalRightFinished := closeRuntimeSnapshotLaneSignalOnce(rightFinished)
	t.Cleanup(signalRightStarted)
	t.Cleanup(releaseRightLane)
	done := make(chan error, 1)
	go func() {
		_, _, err := joinRuntimeSnapshotLanes(
			func() (int, error) {
				if err := waitForRuntimeSnapshotLanePeer("right lane start", rightStarted); err != nil {
					return 0, err
				}
				return 0, wantErr
			},
			func() (string, error) {
				signalRightStarted()
				if err := waitForRuntimeSnapshotLanePeer("right lane release", releaseRight); err != nil {
					return "", err
				}
				signalRightFinished()
				return "late activity complete", nil
			},
		)
		done <- err
	}()
	awaitRuntimeSnapshotLaneSignal(t, "right lane start", rightStarted)
	select {
	case err := <-done:
		t.Fatalf("error path returned while peer could still mutate: %v", err)
	default:
	}
	releaseRightLane()
	err := awaitRuntimeSnapshotLaneValue(t, "joined error after peer completion", done)
	if !errors.Is(err, wantErr) {
		t.Fatalf("joined error = %v, want %v", err, wantErr)
	}
	awaitRuntimeSnapshotLaneSignal(t, "right lane completion", rightFinished)
}

func TestRuntimeSnapshotRejectsInvalidHooksBeforeStartingCacheLanes(t *testing.T) {
	home := t.TempDir()
	graph, err := capgraph.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Add(capgraph.Node{
		ID: "skill:must-not-run", Kind: "skill", Name: "must not run",
		Triggers: []string{"cache activity"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(InstalledHooksPath(home)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(InstalledHooksPath(home), []byte("schema_version: wrong\nhooks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	embedder := &runtimeSemanticEmbedder{task: "cache activity"}
	priorResolver := breadthEmbedderResolver
	breadthEmbedderResolver = func(string) (breadth.EmbedderAdapter, bool) { return embedder, true }
	t.Cleanup(func() { breadthEmbedderResolver = priorResolver })

	_, err = loadRuntimeSnapshot(Input{
		Harness: "codex", Cwd: "/repo/compound", Task: embedder.task,
		RepoKind: "control-plane", RepoKindVia: "fixture", Mode: "execute",
	}, DefaultPolicy(), home, nil)
	if err == nil {
		t.Fatal("invalid installed-hook authority unexpectedly materialized")
	}
	if embedder.taskCalls != 0 {
		t.Fatalf("cache lane embedded the task %d time(s) before hook validation rejected the request", embedder.taskCalls)
	}
}
