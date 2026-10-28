package operatornotice

// Dead-man switch: an off-box check (a healthchecks.io-style ping URL) that
// alerts when the machine STOPS checking in. The autonomy heartbeat pings on a
// green tick and pings <url>/fail on a red one; a heartbeat that never runs at
// all is what the external service notices. This is the only alarm that still
// works when the whole machine has gone quiet, which no local doctor can say.
//
// The URL is the whole credential (anyone holding it can ping), so it lives in
// one file, mode 0600, and is never printed.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const deadmanPingURLFile = "healthchecks-ping-url"

// deadmanTimeout bounds one ping. The heartbeat must never stall on it.
const deadmanTimeout = 10 * time.Second

var deadmanClient = &http.Client{Timeout: deadmanTimeout}

// DeadmanPingURLPath is the one-line ping URL file under a home.
func DeadmanPingURLPath(homePath string) string {
	return filepath.Join(homePath, "config", deadmanPingURLFile)
}

// errDeadmanNotConfigured is the absent-file answer: no ping, no network.
var errDeadmanNotConfigured = errors.New("deadman: not configured")

// readDeadmanURL loads and validates the ping URL. It refuses a file other
// users can read, a file that is not exactly one http(s) URL, and never puts
// the URL itself in an error.
func readDeadmanURL(homePath string) (*url.URL, error) {
	path := DeadmanPingURLPath(homePath)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errDeadmanNotConfigured
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o004 != 0 {
		return nil, fmt.Errorf("%s is world-readable (mode %04o); the URL is a credential: chmod 600 it", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" {
		return nil, fmt.Errorf("%s is empty", path)
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return nil, fmt.Errorf("%s must hold exactly one URL", path)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%s does not hold an http(s) URL", path)
	}
	return u, nil
}

// deadmanState is the doctor word for the ping configuration.
func deadmanState(homePath string) string {
	_, err := readDeadmanURL(homePath)
	switch {
	case err == nil:
		return "configured"
	case errors.Is(err, errDeadmanNotConfigured):
		return "not configured"
	default:
		return "misconfigured: " + err.Error()
	}
}

// deadmanPing GETs the ping URL (or <url>/fail) and returns a one-line status
// that never contains the URL.
func deadmanPing(homePath string, fail bool) (string, error) {
	u, err := readDeadmanURL(homePath)
	if err != nil {
		return "", err
	}
	target := *u
	signal := "ping"
	if fail {
		target.Path = strings.TrimRight(target.Path, "/") + "/fail"
		target.RawPath = ""
		signal = "fail"
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadmanTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("deadman: build %s request failed", signal)
	}
	resp, err := deadmanClient.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // *url.Error embeds the URL; keep only the cause
		}
		return "", fmt.Errorf("deadman: %s failed: %v", signal, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("deadman: %s returned HTTP %d", signal, resp.StatusCode)
	}
	return fmt.Sprintf("deadman: %s sent (HTTP %d)", signal, resp.StatusCode), nil
}

const deadmanUsage = "usage: compound notice deadman ping [--fail] [--home P]"

// runDeadmanCLI is `compound notice deadman ping [--fail]`. Exit 0 when the
// ping was accepted or nothing is configured; 1 when a configured ping could
// not be delivered or the URL file is refused; 2 on usage.
func runDeadmanCLI(homePath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "ping" {
		fmt.Fprintln(stderr, deadmanUsage)
		return 2
	}
	fail := false
	for _, a := range args[1:] {
		if a != "--fail" {
			fmt.Fprintln(stderr, deadmanUsage)
			return 2
		}
		fail = true
	}
	status, err := deadmanPing(homePath, fail)
	if errors.Is(err, errDeadmanNotConfigured) {
		fmt.Fprintln(stdout, err.Error())
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, status)
	return 0
}
