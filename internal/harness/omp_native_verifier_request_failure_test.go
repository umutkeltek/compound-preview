package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	homepkg "compound/internal/home"
	"compound/internal/runattempt"
)

func TestOMPVerifierContainsMalformedHTTPRequest(t *testing.T) {
	interpreter, err := resolveOMPVerifierInterpreter("")
	if err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct{ name, payload string }{
		{"invalid_json", "'{PRIVATE_REQUEST_CANARY'"},
		{"null_object", "'null'"},
		{"array_object", "'[]'"},
		{"oversized_body", "'PRIVATE_REQUEST_CANARY'+'x'.repeat(2*1024*1024)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Reuse the verifier interpreter instead of charging Python/urllib startup
			// to this failure-path deadline. The client stays alive after disconnect:
			// the handler must kill it; natural exit cannot make this test pass.
			native := fmt.Sprintf("#!%s\nconst f=require('node:fs'),h=require('node:http'),p=require('node:path');\nconst endpoint=f.readFileSync(p.join(process.env.PI_CODING_AGENT_DIR,'models.yml'),'utf8').trim().split('=',2)[1];\nconst req=h.request(endpoint+'/v1/chat/completions',{method:'POST',headers:{'content-type':'application/json'}},res=>res.resume());\nreq.on('error',()=>{});req.end(%s);setInterval(()=>{},1000);\n", interpreter.Path, tc.payload)
			f := newOMPVerifierFixture(t, native)
			f.input.Timeout = 5 * time.Second
			out, err := f.adapter.Run(context.Background(), f.input)
			if err != nil {
				t.Fatal(err)
			}
			if out.RunAttempt.Status != runattempt.StatusFailed || out.TimedOut {
				t.Fatalf("request failure must settle without outer timeout: status=%s timeout=%v", out.RunAttempt.Status, out.TimedOut)
			}
			if got := string(mustReadTestFile(t, out.TranscriptPath)); got != "stderr: OMP_NATIVE_PROBE_FAILED\n" {
				t.Fatalf("asynchronous request failure escaped stable public code (bytes=%d)", len(got))
			}
			digest := ""
			for _, ref := range out.RunAttempt.ProofRefs {
				if strings.HasPrefix(ref, homepkg.ArtifactRefPrefix) {
					t.Fatal("invalid request certified")
				}
				if strings.HasPrefix(ref, "failed_private_capture_sha256:") {
					if digest != "" {
						t.Fatal("duplicate failure capture")
					}
					digest = strings.TrimPrefix(ref, "failed_private_capture_sha256:")
				}
			}
			if len(digest) != 64 {
				t.Fatal("failure lost private capture")
			}
			artifacts, err := homepkg.NewArtifactStore(f.home)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := artifacts.GetPrivate(homepkg.ArtifactRefPrefix + digest)
			if err != nil {
				t.Fatal(err)
			}
			var failure struct {
				Captured string `json:"captured_base64"`
			}
			if err := json.Unmarshal(raw, &failure); err != nil {
				t.Fatal(err)
			}
			captured, err := base64.StdEncoding.DecodeString(failure.Captured)
			if err != nil {
				t.Fatal(err)
			}
			var detail struct {
				Schema string                         `json:"schema_version"`
				Error  struct{ Code, Message string } `json:"error"`
			}
			if err := json.Unmarshal(captured, &detail); err != nil {
				t.Fatalf("handler did not preserve typed private error: %v", err)
			}
			if detail.Schema != "compound.omp_native_verifier_private.v0.1" || detail.Error.Code != "OMP_NATIVE_PROBE_FAILED" || detail.Error.Message != "OMP_PROBE_REQUEST_INVALID" {
				t.Fatalf("request error did not reach normal private failure path: %+v", detail)
			}
		})
	}
}
