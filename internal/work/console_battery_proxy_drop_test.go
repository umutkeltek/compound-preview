package work

import (
	"os"
	"reflect"
	"testing"
)

// A battery launched from an agent shell inherits its loopback model-gateway proxy;
// every bun console gate then fast-failed proxy_env_blocks_bun. Dropping the loopback
// variables must clear that reason and leave a non-loopback proxy untouched.
func TestDropInheritedLoopbackProxyClearsTheConsoleFastFail(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(repo+"/console/node_modules", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("https_proxy", "http://[::1]:9/")
	t.Setenv("HTTP_PROXY", "http://proxy.example.invalid:8080")
	step := ProofCheckStep{Name: "console-smoke", Dir: "console", Command: []string{"bun", "test"}}
	if got := ConsoleBatteryFastFailReason(repo, step); got != "proxy_env_blocks_bun" {
		t.Fatalf("precondition: fast-fail reason = %q, want proxy_env_blocks_bun", got)
	}

	dropped := DropInheritedLoopbackProxy()
	if want := []string{"HTTPS_PROXY", "https_proxy"}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped %v, want %v", dropped, want)
	}
	if got := ConsoleBatteryFastFailReason(repo, step); got != "" {
		t.Fatalf("fast-fail reason after drop = %q, want none", got)
	}
	if got := os.Getenv("HTTP_PROXY"); got != "http://proxy.example.invalid:8080" {
		t.Fatalf("non-loopback proxy changed to %q", got)
	}
}
