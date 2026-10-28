package cli

// test_helpers_test.go: shared stdio capture and exit-code capture for the
// cli package's tests. Lives in cli_test (not cli) so it can be referenced
// from every test file in the package.

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// captureStdoutStderr redirects os.Stdout and os.Stderr to in-memory
// buffers for the duration of fn, returning the captured bytes. Errors
// from fn are NOT captured — callers handle them via runGameCmd.
func captureStdoutStderr(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr
	outDone := make(chan struct{})
	errDone := make(chan struct{})
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, rOut)
		stdout = b.String()
		close(outDone)
	}()
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, rErr)
		stderr = b.String()
		close(errDone)
	}()
	fn()
	_ = wOut.Close()
	_ = wErr.Close()
	<-outDone
	<-errDone
	os.Stdout = origOut
	os.Stderr = origErr
	return stdout, stderr
}

// runGameCmd runs fn and returns its exit code. Does NOT call os.Exit, so
// tests stay in-process.
func runGameCmd(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	var code int
	stdout, stderr := captureStdoutStderr(t, func() {
		code = fn()
	})
	if stderr != "" {
		t.Logf("stderr: %s", stderr)
	}
	return code, stdout
}
