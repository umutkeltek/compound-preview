package release

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the actual source installer and filesystem operations. Only the Go
// builder/CLI are synthetic, allowing failures after publication without a new
// full repository build per case. The CLI package also tests real clean installs.
func TestSourceInstallerRestoresEnabledArtifactSet(t *testing.T) {
	for _, fail := range []string{"breadth_build", "breadth_probe", "helper_validation", "home_doctor", "refresh_preflight", "home_init", "runtime_doctor", "refresh", "rename:compound-breadth-engine", "rename:os", "rename:heartbeat-promote-stage.sh", "rename:compound-autonomy-heartbeat.sh", "rename:attention-letter.py", "term"} {
		for _, prior := range []bool{false, true} {
			t.Run(fail+map[bool]string{false: "/fresh", true: "/upgrade"}[prior], func(t *testing.T) {
				f := sourceInstallFixture(t)
				before := map[string][]byte{}
				for _, path := range f.targets {
					if prior {
						body := []byte("previous " + filepath.Base(path) + "\n")
						if filepath.Base(path) == "os" {
							body = []byte("#!/bin/sh\n# shim only operating-system/umut-os/exec-lane/os.ts\n")
						}
						if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, body, 0711); err != nil {
							t.Fatal(err)
						}
						before[path] = body
					}
				}
				if fail == "helper_validation" {
					if err := os.WriteFile(filepath.Join(f.repo, "scripts", "attention-letter.py"), []byte("def broken(\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				out, err := f.run(fail)
				if err == nil {
					t.Fatalf("injected %s reported success:\n%s", fail, out)
				}
				if strings.Contains(string(out), "activation: complete") {
					t.Fatalf("failure claimed complete activation:\n%s", out)
				}
				for _, path := range f.targets {
					body, readErr := os.ReadFile(path)
					if !prior {
						if _, err := os.Lstat(path); !os.IsNotExist(err) {
							t.Errorf("fresh failure left published %s: %v", path, err)
						}
						continue
					}
					if readErr != nil || string(body) != string(before[path]) {
						t.Errorf("prior bytes not restored for %s: %v\n%s", path, readErr, out)
					}
					if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0711 {
						t.Errorf("prior mode not restored for %s: %v", path, err)
					}
				}
			})
		}
	}
}

func TestSourceInstallerRefusesRefreshBeforeActivation(t *testing.T) {
	f := sourceInstallFixture(t)
	for _, path := range f.targets {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		body := "previous " + filepath.Base(path) + "\n"
		if filepath.Base(path) == "os" {
			body = "#!/bin/sh\n# shim only operating-system/umut-os/exec-lane/os.ts\n"
		}
		if err := os.WriteFile(path, []byte(body), 0711); err != nil {
			t.Fatal(err)
		}
	}
	out, err := f.run("refresh_preflight")
	if err == nil || !strings.Contains(string(out), "pre-publication refresh preflight FAILED") || strings.Contains(string(out), "activation incomplete") {
		t.Fatalf("refusal did not happen before activation: %v\n%s", err, out)
	}
	calls, err := os.ReadFile(filepath.Join(f.repo, "cli-calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "harness refresh-lane --preflight") || strings.Contains(string(calls), "home init") || strings.Contains(string(calls), "runtime doctor") {
		t.Fatalf("installer activated after failed eligibility check:\n%s", calls)
	}
}

func TestSourceInstallerPublishesAndActivatesEnabledArtifactSet(t *testing.T) {
	f := sourceInstallFixture(t)
	out, err := f.run("")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, path := range f.targets {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing installed artifact %s: %v", path, err)
		}
	}
	if !strings.Contains(string(out), "publication: complete") || !strings.Contains(string(out), "activation: complete") {
		t.Fatalf("missing truthful phase results:\n%s", out)
	}
}

func TestSourceInstallerRetainsFailedRollbackAndRefusesRetry(t *testing.T) {
	f := sourceInstallFixture(t)
	old := filepath.Join(f.bin, "compound")
	if err := os.WriteFile(old, []byte("previous binary\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := f.run("rollback")
	if err == nil || !strings.Contains(string(out), "rollback INCOMPLETE") {
		t.Fatalf("missing rollback failure: %v\n%s", err, out)
	}
	retained, err := filepath.Glob(filepath.Join(f.bin, ".compound-source-stage.*", "previous", "compound"))
	if err != nil || len(retained) != 1 {
		t.Fatalf("missing retained preimage: %v %v", retained, err)
	}
	if body, err := os.ReadFile(retained[0]); err != nil || string(body) != "previous binary\n" {
		t.Fatalf("preimage lost: %v %q", err, body)
	}
	out, err = f.run("")
	if err == nil || !strings.Contains(string(out), "interrupted source installation retained") {
		t.Fatalf("unsafe retry accepted: %v\n%s", err, out)
	}
}

func TestSourceInstallerRefusesPendingReleaseRecovery(t *testing.T) {
	f := sourceInstallFixture(t)
	state := filepath.Join(f.bin, ".compound-install-state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "owner"), []byte("retained release owner\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run("")
	if err == nil || !strings.Contains(string(out), "pending release recovery state") {
		t.Fatalf("release recovery was ignored: %v\n%s", err, out)
	}
	if body, err := os.ReadFile(filepath.Join(state, "owner")); err != nil || string(body) != "retained release owner\n" {
		t.Fatal("source installer changed release recovery evidence")
	}
}

func TestSourceInstallerRestoresExistingAliasSymlink(t *testing.T) {
	f := sourceInstallFixture(t)
	if err := os.Symlink("compound", filepath.Join(f.bin, "os")); err != nil {
		t.Fatal(err)
	}
	out, err := f.run("refresh")
	if err == nil {
		t.Fatalf("refresh failure accepted:\n%s", out)
	}
	if target, err := os.Readlink(filepath.Join(f.bin, "os")); err != nil || target != "compound" {
		t.Fatalf("prior symlink was not restored: %q %v", target, err)
	}
}

func TestSourceInstallerHonorsReleaseKernelLock(t *testing.T) {
	f := sourceInstallFixture(t)
	state := filepath.Join(f.bin, ".compound-install-state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(state, "lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	locking := "flock -n 9"
	if runtime.GOOS == "darwin" {
		locking = "lockf -s -t 0 9"
	}
	holder := exec.Command("sh", "-c", "exec 9>\"$1\"; "+locking+" || exit 1; echo locked; read release", "holder", lock)
	input, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); holder.Wait() }()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("lock holder failed: %q %v", line, err)
	}
	out, err := f.run("")
	if err == nil || !strings.Contains(string(out), "another installer holds") {
		t.Fatalf("source install ignored release lock: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(f.bin, "compound")); !os.IsNotExist(err) {
		t.Fatal("contending installer published a binary")
	}
}

func TestSourceInstallerPreservesInterruptedPreimage(t *testing.T) {
	f := sourceInstallFixture(t)
	if err := os.WriteFile(filepath.Join(f.bin, "compound"), []byte("previous binary\n"), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := f.run("kill")
	if err == nil {
		t.Fatalf("killed installer succeeded:\n%s", out)
	}
	retained, err := filepath.Glob(filepath.Join(f.bin, ".compound-source-stage.*", "previous", "compound"))
	if err != nil || len(retained) != 1 {
		t.Fatalf("killed install lost retained preimage: %v %v", retained, err)
	}
	if body, err := os.ReadFile(retained[0]); err != nil || string(body) != "previous binary\n" {
		t.Fatalf("interrupted preimage changed: %v %q", err, body)
	}
	out, err = f.run("")
	if err == nil || !strings.Contains(string(out), "interrupted source installation retained") {
		t.Fatalf("interrupted install was overwritten: %v\n%s", err, out)
	}
}

type sourceInstallerFixture struct {
	repo, bin, home, fakeBin string
	targets                  []string
}

func sourceInstallFixture(t *testing.T) sourceInstallerFixture {
	t.Helper()
	root := t.TempDir()
	f := sourceInstallerFixture{repo: filepath.Join(root, "repo"), bin: filepath.Join(root, "bin"), home: filepath.Join(root, "home"), fakeBin: filepath.Join(root, "tools")}
	for _, dir := range []string{f.repo, f.bin, f.fakeBin, filepath.Join(f.home, "config"), filepath.Join(f.repo, "scripts", "_lib")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "install.sh"), body, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"scripts/sync-ceremony-overlays.sh":       "#!/bin/sh\nexit 0\n",
		"scripts/compound-autonomy-heartbeat.sh":  "#!/bin/sh\n. \"$(dirname \"$0\")/_lib/heartbeat-promote-stage.sh\"\n",
		"scripts/_lib/heartbeat-promote-stage.sh": "promote_driver_stage_decide() { :; }\n",
		"scripts/attention-letter.py":             "print('synthetic')\n",
		"synthetic-cli":                           "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$FIXTURE_REPO/cli-calls\"\ncase \"$1 $2\" in\n 'home init') [ \"$INSTALL_FAIL\" != home_init ] ;;\n 'home doctor') [ \"$INSTALL_FAIL\" != home_doctor ] ;;\n 'runtime doctor') [ \"$INSTALL_FAIL\" != runtime_doctor ] ;;\n 'harness refresh-lane') case \" $* \" in *' --preflight '*) [ \"$INSTALL_FAIL\" != refresh_preflight ] ;; *) [ \"$INSTALL_FAIL\" != refresh ] ;; esac ;;\n *) exit 0 ;;\nesac\n",
	}
	files["synthetic-breadth"] = `#!/bin/sh
[ "$INSTALL_FAIL" != breadth_probe ] || exit 73
sed -n 's/.*"request_id":"\([^"]*\)".*/{"ok":true,"schema_version":1,"request_id":"\1","name":"compound-breadth","breadth":true,"ready":false}/p'
`
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(f.repo, rel), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	goShim := `#!/bin/sh
set -eu
case "$1" in
 run) echo 'install verification ok'; exit 0 ;;
 build)
  out=
  while [ "$#" -gt 0 ]; do
   if [ "$1" = -o ]; then shift; out=$1; fi
   target=$1; shift
  done
  if [ "$target" = ./cmd/compound-breadth-engine ] && [ "$INSTALL_FAIL" = breadth_build ]; then exit 71; fi
  if [ "$target" = ./cmd/compound-breadth-engine ]; then
   cp "$FIXTURE_REPO/synthetic-breadth" "$out"
  else
   cp "$FIXTURE_REPO/synthetic-cli" "$out"
  fi
  chmod 0755 "$out"
  ;;
 *) exit 64 ;;
esac
`
	if err := os.WriteFile(filepath.Join(f.fakeBin, "go"), []byte(goShim), 0700); err != nil {
		t.Fatal(err)
	}
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	mvShim := `#!/bin/sh
set -eu
for arg; do target=$arg; done
if [ "$INSTALL_FAIL" = "rename:$(basename "$target")" ] && [ ! -f "$FIXTURE_REPO/rename-failed" ]; then
 : > "$FIXTURE_REPO/rename-failed"
 exit 72
fi
if [ "$INSTALL_FAIL" = rollback ]; then
 if [ "$(basename "$target")" = compound-breadth-engine ]; then : > "$FIXTURE_REPO/rename-failed"; exit 72; fi
 if [ -f "$FIXTURE_REPO/rename-failed" ]; then exit 72; fi
fi
if { [ "$INSTALL_FAIL" = term ] || [ "$INSTALL_FAIL" = kill ]; } && [ "$(basename "$target")" = compound ] && [ ! -f "$FIXTURE_REPO/rename-failed" ]; then
 : > "$FIXTURE_REPO/rename-failed"
 '` + realMV + `' "$@"
 if [ "$INSTALL_FAIL" = kill ]; then kill -KILL "$PPID"; else kill -TERM "$PPID"; fi
 exit 0
fi
exec '` + realMV + `' "$@"
`
	if err := os.WriteFile(filepath.Join(f.fakeBin, "mv"), []byte(mvShim), 0700); err != nil {
		t.Fatal(err)
	}
	f.targets = []string{filepath.Join(f.bin, "compound"), filepath.Join(f.bin, "compound-breadth-engine"), filepath.Join(f.bin, "os"), filepath.Join(f.bin, "_lib", "heartbeat-promote-stage.sh"), filepath.Join(f.bin, "compound-autonomy-heartbeat.sh"), filepath.Join(f.home, "scripts", "attention-letter.py")}
	return f
}

func (f sourceInstallerFixture) run(fail string) ([]byte, error) {
	cmd := exec.Command("sh", filepath.Join(f.repo, "install.sh"))
	cmd.Dir = f.repo
	cmd.Env = append(os.Environ(), "HOME="+f.home, "COMPOUND_HOME="+f.home, "GOBIN="+f.bin, "PATH="+f.fakeBin+":"+os.Getenv("PATH"), "FIXTURE_REPO="+f.repo, "INSTALL_FAIL="+fail, "COMPOUND_INSTALL_OS_ALIAS=1", "COMPOUND_INSTALL_AUTONOMY_LAB=1")
	return cmd.CombinedOutput()
}
