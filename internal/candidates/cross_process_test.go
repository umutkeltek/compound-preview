package candidates

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const candidateHelperActionEnv = "COMPOUND_CANDIDATE_HELPER_ACTION"

type candidateHelperResult struct {
	output []byte
	err    error
}

// TestCandidateSubprocess is the process boundary used by the concurrency
// falsifiers below. A normal test run leaves the action unset and returns.
func TestCandidateSubprocess(t *testing.T) {
	action := os.Getenv(candidateHelperActionEnv)
	if action == "" {
		return
	}
	home := os.Getenv("COMPOUND_CANDIDATE_HELPER_HOME")
	id := os.Getenv("COMPOUND_CANDIDATE_HELPER_ID")
	marker := os.Getenv("COMPOUND_CANDIDATE_HELPER_MARKER")
	ready := os.Getenv("COMPOUND_CANDIDATE_HELPER_READY")
	if err := os.WriteFile(ready, []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("publish helper readiness: %v", err)
	}
	if start := os.Getenv("COMPOUND_CANDIDATE_HELPER_START"); start != "" {
		waitForCandidateHelperStart(t, start)
	}
	markerBytes, err := strconv.Atoi(os.Getenv("COMPOUND_CANDIDATE_HELPER_MARKER_BYTES"))
	if err != nil && os.Getenv("COMPOUND_CANDIDATE_HELPER_MARKER_BYTES") != "" {
		t.Fatalf("parse helper marker bytes: %v", err)
	}
	if markerBytes > 0 {
		marker = strings.Repeat(marker, markerBytes)
	}
	payload := struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Marker string `json:"marker"`
	}{ID: id, Kind: string(Memory), Marker: marker}

	switch action {
	case "write":
		if err := WriteCandidate(home, Memory, id, payload); err != nil {
			t.Fatalf("write candidate: %v", err)
		}
		fmt.Println("WROTE=true")
	case "create":
		created, err := CreateCandidateIfAbsent(home, Memory, id, payload)
		if err != nil {
			t.Fatalf("create candidate: %v", err)
		}
		fmt.Printf("CREATED=%t\n", created)
	case "write-loop":
		iterations, err := strconv.Atoi(os.Getenv("COMPOUND_CANDIDATE_HELPER_ITERATIONS"))
		if err != nil || iterations < 1 {
			t.Fatalf("parse helper iterations: %q: %v", os.Getenv("COMPOUND_CANDIDATE_HELPER_ITERATIONS"), err)
		}
		other := payload
		other.Marker = strings.Repeat("B", markerBytes)
		for i := 0; i < iterations; i++ {
			current := payload
			if i%2 == 1 {
				current = other
			}
			if err := WriteCandidate(home, Memory, id, current); err != nil {
				t.Fatalf("write candidate iteration %d: %v", i, err)
			}
		}
		fmt.Printf("WROTE_ITERATIONS=%d\n", iterations)
	default:
		t.Fatalf("unknown candidate helper action %q", action)
	}
}

func TestWriteCandidateAtomicReplacement(t *testing.T) {
	home := t.TempDir()
	const (
		id         = "atomic-replacement"
		markerSize = 2 << 20
	)
	type rec struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Marker string `json:"marker"`
	}
	oldPayload := rec{ID: id, Kind: string(Memory), Marker: strings.Repeat("A", markerSize)}
	newPayload := rec{ID: id, Kind: string(Memory), Marker: strings.Repeat("B", markerSize)}
	if err := WriteCandidate(home, Memory, id, oldPayload); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, InboxDir(Memory), id+".json")
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	wantOld := canonicalCandidateBody(t, oldPayload)
	wantNew := canonicalCandidateBody(t, newPayload)

	start := filepath.Join(t.TempDir(), "start")
	ready, done := startCandidateHelper(t, home, "write-loop", id, "A",
		"COMPOUND_CANDIDATE_HELPER_START="+start,
		"COMPOUND_CANDIDATE_HELPER_MARKER_BYTES="+strconv.Itoa(markerSize),
		"COMPOUND_CANDIDATE_HELPER_ITERATIONS=40",
	)
	waitForCandidateHelperReady(t, ready)
	if err := os.WriteFile(start, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var invalid []byte
	finished := false
	for !finished && invalid == nil {
		select {
		case result := <-done:
			requireCandidateHelperResult(t, "write-loop", result)
			finished = true
		default:
			body, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("read candidate during replacement: %v", err)
			}
			if !bytes.Equal(body, wantOld) && !bytes.Equal(body, wantNew) {
				invalid = append([]byte(nil), body...)
			}
		}
	}
	if !finished {
		waitForCandidateHelper(t, "write-loop", done)
	}
	if invalid != nil {
		var decoded rec
		t.Fatalf("reader observed torn replacement: bytes=%d valid_json=%t decode_err=%v", len(invalid), json.Valid(invalid), json.Unmarshal(invalid, &decoded))
	}
	// The helper loop proves readers never observe a torn replacement.
	// Comparing inodes across all iterations is filesystem-dependent:
	// ext4 may immediately reuse a freed inode. Use one controlled
	// replacement for the portable rename proof. Staging the temporary file
	// while the target still exists gives them distinct inode identities
	// before rename publishes the replacement.
	preLoopEnd, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	finalPayload := rec{ID: id, Kind: string(Memory), Marker: strings.Repeat("C", markerSize)}
	if err := WriteCandidate(home, Memory, id, finalPayload); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(preLoopEnd, after) {
		t.Fatal("replacement retained the authoritative inode; write was in-place rather than atomic rename publication")
	}
	if body, rerr := os.ReadFile(target); rerr != nil || !bytes.Equal(body, canonicalCandidateBody(t, finalPayload)) {
		t.Fatalf("final controlled replacement body mismatch: err=%v bytes=%d", rerr, len(body))
	}
	_ = before
}

func TestCreateCandidateIfAbsentCrossProcessExactlyOne(t *testing.T) {
	home := t.TempDir()
	const contenders = 12
	start := filepath.Join(t.TempDir(), "start")
	readyPaths := make([]string, 0, contenders)
	done := make([]<-chan candidateHelperResult, 0, contenders)
	wantMarkers := make(map[string]struct{}, contenders)
	for i := 0; i < contenders; i++ {
		marker := fmt.Sprintf("contender-%02d", i)
		wantMarkers[marker] = struct{}{}
		ready, result := startCandidateHelper(t, home, "create", "one-winner", marker,
			"COMPOUND_CANDIDATE_HELPER_START="+start,
		)
		readyPaths = append(readyPaths, ready)
		done = append(done, result)
	}
	for _, ready := range readyPaths {
		waitForCandidateHelperReady(t, ready)
	}
	if err := os.WriteFile(start, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	created := 0
	for i, resultCh := range done {
		result := candidateHelperResultWithin(t, fmt.Sprintf("create-%02d", i), resultCh)
		output := string(result.output)
		switch {
		case strings.Contains(output, "CREATED=true"):
			created++
		case strings.Contains(output, "CREATED=false"):
		default:
			t.Fatalf("create-%02d helper omitted created result:\n%s", i, output)
		}
	}
	if created != 1 {
		t.Fatalf("created winners = %d, want exactly 1", created)
	}

	type rec struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Marker string `json:"marker"`
	}
	var got rec
	if err := ReadCandidate(home, Memory, "one-winner", &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "one-winner" || got.Kind != string(Memory) {
		t.Fatalf("winner identity = %+v", got)
	}
	if _, ok := wantMarkers[got.Marker]; !ok {
		t.Fatalf("winner marker = %q, want one of the contenders", got.Marker)
	}
	target := filepath.Join(home, InboxDir(Memory), "one-winner.json")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := canonicalCandidateBody(t, got); !bytes.Equal(body, want) {
		t.Fatalf("winner body is not one complete canonical contender: got %q want %q", body, want)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".candidate-publish-") {
			t.Fatalf("candidate temp leaked after race: %s", entry.Name())
		}
	}
}

func TestWriteCandidateAndCreateCandidateIfAbsentShareLock(t *testing.T) {
	home := t.TempDir()
	lockPath := filepath.Join(home, "lock", "candidates.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		}
		_ = lockFile.Close()
	})

	writeReady, writeDone := startCandidateHelper(t, home, "write", "replace", "replacement")
	createReady, createDone := startCandidateHelper(t, home, "create", "create", "create-only")
	waitForCandidateHelperReady(t, writeReady)
	waitForCandidateHelperReady(t, createReady)

	early := make([]candidateHelperResult, 0, 2)
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	for len(early) == 0 {
		select {
		case result := <-writeDone:
			early = append(early, result)
		case result := <-createDone:
			early = append(early, result)
		case <-timer.C:
			goto release
		}
	}

release:
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	locked = false
	if len(early) > 0 {
		t.Fatalf("candidate publication completed while candidates.lock was held: %+v", early)
	}
	waitForCandidateHelper(t, "write", writeDone)
	waitForCandidateHelper(t, "create", createDone)
}

func TestWriteCandidateAndCreateCandidateIfAbsentRaceLeavesWholeBody(t *testing.T) {
	home := t.TempDir()
	const (
		id         = "write-create-race"
		markerSize = 512 << 10
	)
	start := filepath.Join(t.TempDir(), "start")
	writeReady, writeDone := startCandidateHelper(t, home, "write", id, "W",
		"COMPOUND_CANDIDATE_HELPER_START="+start,
		"COMPOUND_CANDIDATE_HELPER_MARKER_BYTES="+strconv.Itoa(markerSize),
	)
	createReady, createDone := startCandidateHelper(t, home, "create", id, "C",
		"COMPOUND_CANDIDATE_HELPER_START="+start,
		"COMPOUND_CANDIDATE_HELPER_MARKER_BYTES="+strconv.Itoa(markerSize),
	)
	waitForCandidateHelperReady(t, writeReady)
	waitForCandidateHelperReady(t, createReady)
	if err := os.WriteFile(start, []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForCandidateHelper(t, "write", writeDone)
	createResult := candidateHelperResultWithin(t, "create", createDone)
	if output := string(createResult.output); !strings.Contains(output, "CREATED=true") && !strings.Contains(output, "CREATED=false") {
		t.Fatalf("create helper omitted result:\n%s", output)
	}

	type rec struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Marker string `json:"marker"`
	}
	want := rec{ID: id, Kind: string(Memory), Marker: strings.Repeat("W", markerSize)}
	target := filepath.Join(home, InboxDir(Memory), id+".json")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if canonical := canonicalCandidateBody(t, want); !bytes.Equal(body, canonical) {
		t.Fatalf("write/create race left a non-writer or torn body: bytes=%d valid_json=%t", len(body), json.Valid(body))
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".candidate-publish-") {
			t.Fatalf("candidate temp was mistaken for authority after mixed race: %s", entry.Name())
		}
	}
}

func TestCandidateWriterDoesNotAcquireMemLock(t *testing.T) {
	home := t.TempDir()
	lockPath := filepath.Join(home, "lock", "mem.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		}
		_ = lockFile.Close()
	})

	ready, done := startCandidateHelper(t, home, "write", "mem-lock-independent", "candidate")
	waitForCandidateHelperReady(t, ready)
	waitForCandidateHelper(t, "write while mem.lock held", done)
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	locked = false

	target := filepath.Join(home, InboxDir(Memory), "mem-lock-independent.json")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("candidate writer did not publish while mem.lock was held: %v", err)
	}
}

func startCandidateHelper(t *testing.T, home, action, id, marker string, extraEnv ...string) (string, <-chan candidateHelperResult) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCandidateSubprocess$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		candidateHelperActionEnv+"="+action,
		"COMPOUND_CANDIDATE_HELPER_HOME="+home,
		"COMPOUND_CANDIDATE_HELPER_ID="+id,
		"COMPOUND_CANDIDATE_HELPER_MARKER="+marker,
		"COMPOUND_CANDIDATE_HELPER_READY="+ready,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s helper: %v", action, err)
	}
	done := make(chan candidateHelperResult, 1)
	go func() {
		err := cmd.Wait()
		done <- candidateHelperResult{output: output.Bytes(), err: err}
	}()
	return ready, done
}

func waitForCandidateHelperStart(t *testing.T, path string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect helper start signal: %v", err)
		}
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for helper start at %s", path)
		case <-ticker.C:
		}
	}
}

func waitForCandidateHelperReady(t *testing.T, path string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect helper readiness: %v", err)
		}
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for helper readiness at %s", path)
		case <-ticker.C:
		}
	}
}

func waitForCandidateHelper(t *testing.T, name string, done <-chan candidateHelperResult) {
	t.Helper()
	_ = candidateHelperResultWithin(t, name, done)
}

func candidateHelperResultWithin(t *testing.T, name string, done <-chan candidateHelperResult) candidateHelperResult {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case result := <-done:
		requireCandidateHelperResult(t, name, result)
		return result
	case <-timer.C:
		t.Fatalf("timed out waiting for %s helper", name)
	}
	return candidateHelperResult{}
}

func requireCandidateHelperResult(t *testing.T, name string, result candidateHelperResult) {
	t.Helper()
	if result.err != nil {
		t.Fatalf("%s helper failed: %v\n%s", name, result.err, result.output)
	}
}

func canonicalCandidateBody(t *testing.T, payload any) []byte {
	t.Helper()
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(body, '\n')
}
