package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
)

type homeFixtureScope struct {
	homePath, workID, workspace string
	source                      gitrepository.Snapshot
}
type homeFixtureScopeKey struct{}
type HomeFixtureCommand func(context.Context, string, ...string) *exec.Cmd

// Only the admitted proof adapter supplies this context after reserving a fresh
// run. CI, development and unsupported platforms keep fresh ordinary tests.
func ContextWithHomeFixtureReuse(ctx context.Context, homePath, workID string, source *gitrepository.Snapshot, workspace ...string) context.Context {
	if source == nil {
		return ctx
	}
	scope := homeFixtureScope{homePath: homePath, workID: workID, source: *source}
	if len(workspace) == 1 {
		scope.workspace = workspace[0]
	}
	return context.WithValue(ctx, homeFixtureScopeKey{}, scope)
}

var homeFixtureNow = time.Now

var homeFixtureCacheBase = defaultHomeFixtureCacheBase

// RunHomeFixtureGate executes or reuses only the restricted home source gate.
// The script prepares observed inputs; Store authenticates the proof origin.
func RunHomeFixtureGate(ctx context.Context, repo string, step ProofCheckStep, env []string, command HomeFixtureCommand) ([]byte, *HomeFixtureEvidence, error) {
	fresh := func(reason string) ([]byte, *HomeFixtureEvidence, error) {
		cmd := command(ctx, step.Command[0], step.Command[1:]...)
		cmd.Dir = repo
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if reason != "" {
			output = append([]byte("home fixture reuse unavailable: "+reason+"; executed fresh\n"), output...)
		}
		return output, nil, err
	}
	scope, ok := ctx.Value(homeFixtureScopeKey{}).(homeFixtureScope)
	if !ok {
		return fresh("")
	}
	if runtime.GOOS != "darwin" {
		return fresh("platform")
	}
	if !validHomeFixtureGate(step, &scope.source) {
		return nil, nil, fmt.Errorf("home fixture gate does not match admitted source")
	}
	if strings.TrimSpace(os.Getenv("GOFLAGS")) != "" {
		return fresh("nondefault GOFLAGS")
	}
	for _, binary := range []string{"/usr/bin/python3", "/usr/bin/sandbox-exec"} {
		if _, err := os.Stat(binary); err != nil {
			return fresh("missing system fixture tool")
		}
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return nil, nil, err
	}
	base, err := homeFixtureCacheBase()
	if err != nil {
		return fresh("shared fixture parent unavailable: " + err.Error())
	}
	identity := sha256.Sum256([]byte(scope.homePath + "\x00" + scope.workID + "\x00" + scope.source.RepositoryFingerprint))
	identityText := hex.EncodeToString(identity[:])
	stage := filepath.Join(base, identityText[:24])
	helper := filepath.Join(repo, "scripts", "testbattery", "home_fixture.py")
	invoke := func(action string, extra ...string) ([]byte, []byte, error) {
		args := []string{"-I", helper, action, "--stage", stage}
		args = append(args, extra...)
		cmd := command(ctx, "/usr/bin/python3", args...)
		// The native factory attaches exactly the content-free execution barriers.
		// Python closes every unrelated inherited descriptor in every subprocess.
		cmd.Args = append(cmd.Args, "--barrier-count", strconv.Itoa(len(cmd.ExtraFiles)))
		cmd.Dir = repo
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}
	prepared, diagnostic, err := invoke("prepare", "--identity", identityText, "--repo", repo, "--source-head", scope.source.HeadSHA, "--go", goBinary)
	if err != nil {
		if homeFixtureUnavailable(ctx, err) {
			return fresh(strings.TrimSpace(string(diagnostic)))
		}
		return append(diagnostic, prepared...), nil, err
	}
	var inputs HomeFixtureInputs
	decoder := json.NewDecoder(bytes.NewReader(prepared))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inputs); err != nil {
		return prepared, nil, fmt.Errorf("home fixture preparation protocol: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return prepared, nil, fmt.Errorf("trailing home fixture protocol output")
	}
	if inputs.StageDirectory != stage {
		return prepared, nil, fmt.Errorf("home fixture stage mismatch")
	}
	evidence, err := NewHomeFixtureEvidence(inputs)
	if err != nil {
		return prepared, nil, err
	}
	store, err := Open(scope.homePath)
	if err != nil {
		return nil, nil, err
	}
	selected, err := store.ResolveHomeFixtureReuse(scope.workID, scope.source, step, inputs, homeFixtureNow())
	if err != nil {
		return nil, nil, err
	}
	encoded, _ := json.Marshal(inputs)
	finish := func(output []byte, evidence *HomeFixtureEvidence) ([]byte, *HomeFixtureEvidence, error) {
		// Retire only this Work's acceleration payload after its target home gate
		// passes, while the admitted attempt and barrier are still live. Subsequent
		// runtime gates remain fresh and still decide the overall proof result.
		w, lookupErr := store.Get(scope.workID)
		if lookupErr != nil {
			return output, nil, lookupErr
		}
		if w.GitDelivery != nil && sameHomeFixtureWorkspace(scope.workspace, w.GitDelivery.TargetWorktreeRoot) {
			retired, diagnostic, cleanupErr := invoke("retire", "--identity", identityText, "--expected-inputs", string(encoded))
			output = append(output, retired...)
			if cleanupErr != nil {
				output = append(output, []byte(fmt.Sprintf("home fixture payload retained for exact cleanup: %v %s\n", cleanupErr, diagnostic))...)
			}
		}
		return output, evidence, nil
	}

	fallback := func(reason string) ([]byte, *HomeFixtureEvidence, error) {
		output, _, err := fresh(reason)
		if err != nil {
			return output, nil, err
		}
		return finish(output, nil)
	}
	if selected != nil {
		output, diagnostic, err := invoke("verify", "--expected-inputs", string(encoded))
		if err != nil {
			if homeFixtureUnavailable(ctx, err) {
				return fallback(strings.TrimSpace(string(diagnostic)))
			}
			return append(diagnostic, output...), nil, err
		}
		return finish([]byte(fmt.Sprintf("home source proof reused from %s; direct execution at %s; runtime gates remain fresh\n", selected.OriginVerificationRef, selected.OriginGeneratedAt)), selected)
	}
	output, diagnostic, err := invoke("execute", "--expected-inputs", string(encoded))
	output = append(output, diagnostic...)
	if err != nil {
		// Exit 76 is emitted only after the complete restricted suite passed;
		// observable ancestor drift prevents certification, not fresh acceptance.
		var executed *exec.ExitError
		if ctx.Err() == nil && errors.As(err, &executed) && executed.ExitCode() == 76 {
			return finish(output, nil)
		}
		if homeFixtureUnavailable(ctx, err) {
			return fallback(strings.TrimSpace(string(diagnostic)))
		}
		return output, nil, err
	}
	return finish(output, evidence)
}

func homeFixtureUnavailable(ctx context.Context, err error) bool {
	var exit *exec.ExitError
	return ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 75
}

func sameHomeFixtureWorkspace(admitted, target string) bool {
	if admitted == "" || target == "" {
		return false
	}
	a, err := filepath.EvalSymlinks(admitted)
	if err != nil {
		return false
	}
	b, err := filepath.EvalSymlinks(target)
	return err == nil && a == b
}
