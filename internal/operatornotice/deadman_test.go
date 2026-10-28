package operatornotice

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writePingURL(t *testing.T, homePath, url string, mode os.FileMode) {
	t.Helper()
	path := DeadmanPingURLPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(url+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // umask must not soften the fixture
		t.Fatal(err)
	}
}

type pingRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (p *pingRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (p *pingRecorder) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

func runDeadman(t *testing.T, homePath string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runDeadmanCLI(homePath, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestDeadmanNotConfiguredMakesNoRequest(t *testing.T) {
	homePath := t.TempDir()
	var rec pingRecorder
	rec.server(t)
	code, out, errOut := runDeadman(t, homePath, "ping")
	if code != 0 || strings.TrimSpace(out) != "deadman: not configured" {
		t.Fatalf("exit %d out=%q err=%q", code, out, errOut)
	}
	if code, _, _ := runDeadman(t, homePath, "ping", "--fail"); code != 0 {
		t.Fatalf("--fail without a file must also be a quiet no-op, got exit %d", code)
	}
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("unconfigured deadman made requests: %v", got)
	}
}

func TestDeadmanPingAndFail(t *testing.T) {
	homePath := t.TempDir()
	var rec pingRecorder
	srv := rec.server(t)
	writePingURL(t, homePath, srv.URL+"/ping/secret-uuid", 0o600)

	code, out, errOut := runDeadman(t, homePath, "ping")
	if code != 0 {
		t.Fatalf("ping exit %d: %s", code, errOut)
	}
	if strings.Contains(out+errOut, "secret-uuid") {
		t.Fatalf("ping output leaked the ping URL: %q %q", out, errOut)
	}
	code, _, errOut = runDeadman(t, homePath, "ping", "--fail")
	if code != 0 {
		t.Fatalf("ping --fail exit %d: %s", code, errOut)
	}
	got := rec.got()
	want := []string{"GET /ping/secret-uuid", "GET /ping/secret-uuid/fail"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestDeadmanRefusesWorldReadableURLFile(t *testing.T) {
	homePath := t.TempDir()
	var rec pingRecorder
	srv := rec.server(t)
	writePingURL(t, homePath, srv.URL+"/ping/secret-uuid", 0o644)
	code, _, errOut := runDeadman(t, homePath, "ping")
	if code == 0 || !strings.Contains(errOut, "world-readable") {
		t.Fatalf("world-readable ping file must be refused: exit %d err=%q", code, errOut)
	}
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("refused deadman still made requests: %v", got)
	}
}

func TestDeadmanServerErrorIsAFailure(t *testing.T) {
	homePath := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	writePingURL(t, homePath, srv.URL+"/ping/secret-uuid", 0o600)
	if code, _, errOut := runDeadman(t, homePath, "ping"); code == 0 || strings.Contains(errOut, "secret-uuid") {
		t.Fatalf("a non-2xx ping must fail without leaking the URL: exit %d err=%q", code, errOut)
	}
}
