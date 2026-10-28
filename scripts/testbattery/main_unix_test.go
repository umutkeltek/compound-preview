//go:build darwin || linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Real children test signals, streams, environment and process-group ownership.
func TestBatteryRealChildrenInheritProofGroupAndEnvironment(t *testing.T) {
	for _, mode := range []string{"success", "first-failure", "release-failure", "home-failure", "signal", "release-signal", "list-failure"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "calls")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$COMPOUND_TEST_BATTERY_CALLS"
case "$1" in
list)
    printf 'compound/internal/work\ncompound/internal/home\ncompound/internal/release\n'
  printf 'inventory stderr\n' >&2
  if [ "$COMPOUND_TEST_BATTERY_MODE" = list-failure ]; then exit 1; fi
  ;;
test)
  printf 'child stdout %s %s\n' "$GOTOOLCHAIN" "$GOCACHE"
  printf 'child stderr\n' >&2
  printf 'shared-pgid='
  ps -o pgid= -p "$$"
  case "$*" in
  *compound/internal/work)
    if [ "$COMPOUND_TEST_BATTERY_MODE" = first-failure ]; then exit 1; fi
    if [ "$COMPOUND_TEST_BATTERY_MODE" = signal ]; then kill -TERM "$$"; fi
    ;;
      *compound/internal/release)
        if [ "$COMPOUND_TEST_BATTERY_MODE" = release-failure ]; then exit 1; fi
        if [ "$COMPOUND_TEST_BATTERY_MODE" = release-signal ]; then kill -TERM "$$"; fi
        ;;
      *compound/internal/home)
    if [ "$COMPOUND_TEST_BATTERY_MODE" = home-failure ]; then exit 1; fi
    ;;
  esac
  ;;
*) exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("COMPOUND_TEST_BATTERY_CALLS", logPath)
			t.Setenv("COMPOUND_TEST_BATTERY_MODE", mode)
			t.Setenv("GOTOOLCHAIN", "local")
			t.Setenv("GOCACHE", filepath.Join(dir, "proof-cache"))
			var out, stderr bytes.Buffer
			err := runBattery(t.Context(), runGo, &out, &stderr)
			if (err != nil) != (mode != "success") {
				t.Fatalf("mode %s: %v", mode, err)
			}
			data, readErr := os.ReadFile(logPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := []string{"list ./..."}
			if mode != "list-failure" {
				want = append(want, "test -count=1 -timeout=40m compound/internal/work")
			}
			if mode != "list-failure" && mode != "signal" {
				want = append(want, "test -count=1 -timeout=40m compound/internal/release")
				if mode != "release-signal" {
					want = append(want, "test -count=1 -timeout=40m compound/internal/home")
				}
			}
			if string(data) != strings.Join(want, "\n")+"\n" {
				t.Fatalf("child launches = %s; want %v", data, want)
			}
			if !strings.Contains(stderr.String(), "inventory stderr") {
				t.Fatalf("inventory stderr lost: %s", &stderr)
			}
			groups := 0
			for _, line := range strings.Split(out.String(), "\n") {
				if group, ok := strings.CutPrefix(line, "shared-pgid="); ok {
					got, err := strconv.Atoi(strings.TrimSpace(group))
					if err != nil || got != syscall.Getpgrp() {
						t.Fatalf("child group %q differs from parent %d: %v", group, syscall.Getpgrp(), err)
					}
					groups++
				}
			}
			if groups != len(want)-1 {
				t.Fatalf("group checks=%d; want %d; output=%s", groups, len(want)-1, &out)
			}
			if groups > 0 && (!strings.Contains(out.String(), fmt.Sprintf("child stdout local %s", filepath.Join(dir, "proof-cache"))) || !strings.Contains(stderr.String(), "child stderr")) {
				t.Fatalf("child environment/streams lost: out=%s err=%s", &out, &stderr)
			}
		})
	}
}

func TestRunGoRealStartFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var output bytes.Buffer
	if err := runGo(t.Context(), []string{"list", "./..."}, &output, &output); err == nil || output.Len() != 0 {
		t.Fatalf("missing Go executable: error=%v output=%q", err, &output)
	}
}
