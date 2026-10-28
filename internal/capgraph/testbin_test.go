package capgraph_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// capgraphCompoundTestBinary builds ./cmd/compound once for the lifetime of
// this test binary and returns the path to the resulting executable.
// Callers exec that binary directly instead of shelling out through
// `go run ./cmd/compound`, which recompiles and relinks the whole binary on
// every single invocation. The same build-once-and-exec pattern is used in
// cmd/compound/testmain_test.go; this is a small package-local helper
// rather than a shared internal/ package so it does not mint a new
// architecture-classification unit for one call site.
var (
	capgraphTestBinOnce  sync.Once
	capgraphTestBinDir   string
	capgraphTestBinPath  string
	capgraphTestBinBuild string
)

// TestMain builds nothing eagerly; it only guarantees the temp directory
// used by capgraphCompoundTestBinary is cleaned up once every test in this
// binary has finished running.
func TestMain(m *testing.M) {
	code := m.Run()
	if capgraphTestBinDir != "" {
		_ = os.RemoveAll(capgraphTestBinDir)
	}
	os.Exit(code)
}

func capgraphCompoundTestBinary(t *testing.T) string {
	t.Helper()
	capgraphTestBinOnce.Do(func() {
		wd, err := os.Getwd()
		if err != nil {
			capgraphTestBinBuild = fmt.Sprintf("os.Getwd: %v", err)
			return
		}
		// This file lives at internal/capgraph, so the repo root is two
		// levels up.
		repoRoot, err := filepath.Abs(filepath.Join(wd, "..", ".."))
		if err != nil {
			capgraphTestBinBuild = fmt.Sprintf("resolve repo root: %v", err)
			return
		}
		dir, err := os.MkdirTemp("", "capgraph-compound-bin-")
		if err != nil {
			capgraphTestBinBuild = fmt.Sprintf("MkdirTemp: %v", err)
			return
		}
		binPath := filepath.Join(dir, "compound")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/compound")
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		out, buildErr := cmd.CombinedOutput()
		if buildErr != nil {
			capgraphTestBinBuild = fmt.Sprintf("go build ./cmd/compound failed: %v\n%s", buildErr, out)
			return
		}
		capgraphTestBinDir = dir
		capgraphTestBinPath = binPath
	})
	if capgraphTestBinBuild != "" {
		t.Fatalf("%s", capgraphTestBinBuild)
	}
	return capgraphTestBinPath
}
