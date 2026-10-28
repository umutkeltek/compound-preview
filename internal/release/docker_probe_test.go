package release

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerProbeTimeout bounds the daemon probe. A daemon that is still starting
// can hold `docker info` open; past this the host cannot run the container
// probe in test time, which is the same missing capability as no daemon.
const dockerProbeTimeout = 15 * time.Second

// dockerDaemonUnavailable names the missing host capability that keeps a test
// from running containers, or returns "" when a daemon answers. A docker CLI on
// PATH is not enough: a client-only install (a package-manager docker CLI with
// no engine running) answers LookPath and then fails every `docker run` with
// "failed to connect to docker API", which reads as a broken archive rather
// than a host without Docker.
func dockerDaemonUnavailable() string {
	cli, err := exec.LookPath("docker")
	if err != nil {
		return "docker CLI not on PATH"
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, cli, "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	detail := strings.TrimSpace(string(out))
	if i := strings.IndexByte(detail, '\n'); i >= 0 {
		detail = detail[:i]
	}
	switch {
	case ctx.Err() != nil:
		return "docker daemon did not answer `docker info` within " + dockerProbeTimeout.String()
	case err != nil:
		return "docker daemon unreachable (`docker info`: " + err.Error() + ": " + detail + ")"
	case detail == "":
		return "docker daemon reported no server version"
	}
	return ""
}

// requireDockerDaemon skips only when the host lacks a reachable Docker
// daemon, and says which capability is missing. Once the daemon answers, the
// calling test runs in full and a broken artifact still fails it.
func requireDockerDaemon(t *testing.T) {
	t.Helper()
	if missing := dockerDaemonUnavailable(); missing != "" {
		t.Skip("host capability missing: " + missing)
	}
}

// The probe must tell "no CLI", "CLI without a daemon" and "daemon answers"
// apart. A probe that only looks at PATH turns a client-only host into a red
// release test; a probe that always reports missing would silently retire
// the GNU-tar portability check on hosts that can run it.
func TestDockerDaemonProbeSeparatesCLIFromDaemon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string // "" means no docker on PATH
		want   string // substring of the reason; "" means available
	}{
		{name: "no_cli", want: "docker CLI not on PATH"},
		{
			name:   "cli_without_daemon",
			script: "#!/bin/sh\necho 'failed to connect to docker API at unix:///nonexistent.invalid/docker.sock' >&2\nexit 1\n",
			want:   "docker daemon unreachable",
		},
		{
			name:   "daemon_without_version",
			script: "#!/bin/sh\nexit 0\n",
			want:   "no server version",
		},
		{
			name:   "daemon_answers",
			script: "#!/bin/sh\ntest \"$1 $2 $3\" = 'info --format {{.ServerVersion}}' || exit 64\necho 29.0.0\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.script != "" {
				writeExecutable(t, filepath.Join(dir, "docker"), tc.script)
			}
			t.Setenv("PATH", dir)
			got := dockerDaemonUnavailable()
			if tc.want == "" {
				if got != "" {
					t.Fatalf("probe reported %q for a daemon that answers", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("probe reason = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}
