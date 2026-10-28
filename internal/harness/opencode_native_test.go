package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"compound/internal/runattempt"
)

// Opt-in proof uses the installed executable with synthetic local TLS and a
// temporary synthetic auth source. It never reads native saved credentials.
func TestOpenCodeInstalledNativeSyntheticControls(t *testing.T) {
	command := os.Getenv("COMPOUND_OPENCODE_NATIVE_TEST_BIN")
	if command == "" || runtime.GOOS != "darwin" {
		t.Skip("set COMPOUND_OPENCODE_NATIVE_TEST_BIN for installed-native synthetic proof")
	}
	digest, err := ompFileSHA256(command)
	if err != nil {
		t.Fatal("native executable digest unavailable")
	}
	t.Logf("installed_native_sha256=%s", digest)
	for _, status := range []int{200, 401, 500, 429} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				var request map[string]any
				if json.Unmarshal(body, &request) != nil {
					t.Error("invalid synthetic provider request")
				}
				if request["model"] != "control" {
					t.Errorf("request model=%v", request["model"])
				}
				if tools, ok := request["tools"].([]any); ok && len(tools) != 0 {
					t.Error("native request exposed tools")
				}
				if request["max_tokens"] != float64(800) {
					t.Errorf("request output cap=%v", request["max_tokens"])
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-selected-credential" {
					t.Error("selected synthetic auth was not used")
				}
				if r.URL.Path != "/v1/chat/completions" {
					t.Errorf("unexpected request path=%s", r.URL.Path)
				}
				if status != 200 {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("retry-after-ms", "0")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"synthetic provider response","type":"synthetic_error"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"control\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"CONTROL_OK\"},\"finish_reason\":null}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"control\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			_, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			a, in := openCodeFixtureCommand(t, server.URL+"/v1", command)
			a.testNetworkPort = port
			a.testCAPath = filepath.Join(t.TempDir(), "synthetic-ca.pem")
			if err := os.WriteFile(a.testCAPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}
			in.Prompt = "Return exactly CONTROL_OK. Do not request tools."
			before, _ := os.ReadFile(a.authPath)
			start := time.Now()
			out, err := a.Run(context.Background(), in)
			elapsed := time.Since(start)
			mu.Lock()
			count := requests
			mu.Unlock()
			want := 1
			if status == 500 || status == 429 {
				want = 6
			}
			if count != want {
				t.Errorf("native status%d requests=%d expected%d; SDK0 and native session retries are distinct", status, count, want)
			}
			if out.TimedOut || elapsed > 90*time.Second || (out.RunAttempt.Status == runattempt.StatusSucceeded) != (status == 200) {
				t.Errorf("native lifecycle status=%s timeout=%v elapsed=%s err=%v", out.RunAttempt.Status, out.TimedOut, elapsed, err)
			}
			transcript, readErr := os.ReadFile(out.TranscriptPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if count != want {
				t.Logf("synthetic diagnostic transcript: %s", transcript)
			}
			if status == 200 && !strings.Contains(string(transcript), "CONTROL_OK") {
				t.Error("normal text missing")
			}
			if strings.Contains(string(transcript), "synthetic-selected-credential") {
				t.Error("selected key persisted")
			}
			after, _ := os.ReadFile(a.authPath)
			if string(before) != string(after) {
				t.Error("synthetic source auth changed")
			}
			for _, ref := range out.RunAttempt.ProofRefs {
				if strings.Contains(ref, "synthetic-selected-credential") {
					t.Error("selected key persisted in metadata")
				}
			}
			digest := sha256.Sum256(transcript)
			t.Log(fmt.Sprintf("native synthetic status=%d requests=%d compound_retries=0 sdk_retries=0 native_session_retries=%d exit=%d attempt=%s status=%s elapsed=%s transcript_sha256=%s", status, count, count-1, out.ExitCode, out.RunAttempt.ID, out.RunAttempt.Status, elapsed, hex.EncodeToString(digest[:])))
		})
	}
}
