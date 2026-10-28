package harness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenCodeSandboxAllowsOnlySystemResolverSocket(t *testing.T) {
	const probeEnv = "COMPOUND_TEST_OPENCODE_UNIX_SOCKET"
	if path := os.Getenv(probeEnv); path != "" {
		conn, err := net.DialTimeout("unix", path, 2*time.Second)
		if err != nil {
			if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
				fmt.Print("denied")
				os.Exit(42)
			}
			fmt.Printf("unexpected socket failure: %v", err)
			os.Exit(43)
		}
		_ = conn.Close()
		fmt.Print("connected")
		os.Exit(0)
	}
	if runtime.GOOS != "darwin" {
		t.Skip("native OpenCode sandbox requires macOS")
	}
	const resolver = "/private/var/run/mDNSResponder"
	info, err := os.Stat(resolver)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("system resolver socket unavailable: %v", err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		t.Fatal("system resolver socket is not root-owned")
	}
	// Keep Unix paths below the platform length limit. This is an owned empty
	// fixture root; no auth, native config, DNS request, or provider call is used.
	root, err := os.MkdirTemp("/private/tmp", "oc-resolver-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove fixture root: %v", err)
		}
	})
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(root, "unrelated.sock")
	listener, err := net.Listen("unix", unrelated)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	profile := (&OpenCodeAdapter{}).sandboxProfile(&openCodeLaunch{Root: root})
	probe := func(t *testing.T, path, sandbox string) (string, error) {
		t.Helper()
		// Establish endpoint readiness outside the sandbox first. A missing or
		// dead socket must not masquerade as successful sandbox enforcement.
		conn, err := net.DialTimeout("unix", path, 2*time.Second)
		if err != nil {
			t.Fatalf("unsandboxed socket control: %v", err)
		}
		_ = conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", sandbox,
			binary, "-test.run=^TestOpenCodeSandboxAllowsOnlySystemResolverSocket$")
		cmd.Env = []string{"PATH=/usr/bin:/bin", probeEnv + "=" + path}
		output, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("socket probe deadline: %v", ctx.Err())
		}
		return strings.TrimSpace(string(output)), err
	}
	t.Run("system_resolver", func(t *testing.T) {
		output, err := probe(t, resolver, profile)
		if err != nil || output != "connected" {
			t.Fatalf("system resolver denied by native sandbox: %v: %s", err, output)
		}
	})
	t.Run("unrelated_socket", func(t *testing.T) {
		// A fixture-only exact exception must connect. This proves the negative
		// assertion observes network denial, rather than a file fence or dead socket.
		reference := profile + fmt.Sprintf(` (allow network-outbound (literal %q))`, unrelated)
		if output, err := probe(t, unrelated, reference); err != nil || output != "connected" {
			t.Fatalf("unrelated socket allowed control: %v: %s", err, output)
		}
		output, err := probe(t, unrelated, profile)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 42 || output != "denied" {
			t.Fatalf("unrelated Unix socket was not denied: %v: %s", err, output)
		}
	})
}
