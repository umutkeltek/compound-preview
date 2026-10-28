package work

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Required consumers execute in the existing sealed checkout and inherit the existing
// cancellation/process-group barrier. This is not a sandbox for untrusted repository code.
func RunCanonicalConsumer(ctx context.Context, repo string, step ProofCheckStep, headRefs []string, command func(context.Context, string, ...string) *exec.Cmd, resolver ProofRuntimeResolver) ([]byte, *CanonicalConsumerEvidence, error) {
	if command == nil || resolver == nil {
		return nil, nil, fmt.Errorf("consumer execution adapters required")
	}
	head := "HEAD"
	if len(headRefs) > 0 {
		head = headRefs[len(headRefs)-1]
	}
	consumer, body, contractPath, err := readCanonicalConsumerAtPathContext(ctx, repo, head, step)
	if err != nil {
		return nil, nil, err
	}
	evidence := &CanonicalConsumerEvidence{Contract: body}
	if contractPath != CanonicalConsumerContractPath {
		evidence.ContractPath = contractPath
	}
	if consumer.ReportKind == "node_test_junit" {
		evidence.SourceRoot, err = filepath.EvalSymlinks(repo)
		if err != nil {
			return nil, evidence, err
		}
		evidence.SourceRoot, err = filepath.Abs(evidence.SourceRoot)
		if err != nil {
			return nil, evidence, err
		}
	}
	cwd, err := consumerContainedDirectory(repo, consumer.Dir)
	if err != nil {
		return nil, evidence, err
	}
	tools := make(map[string]string, len(consumer.RequiredTools)+2)
	requiredTools := append([]string(nil), consumer.RequiredTools...)
	if consumer.ReportKind == "xcresult" {
		requiredTools = append(requiredTools, "xcodebuild", "xcrun")
	}
	for _, tool := range requiredTools {
		resolved, err := ProofRuntimeExecutable(ctx, tool, resolver)
		if err != nil {
			return nil, evidence, err
		}
		executable, err := exec.LookPath(resolved)
		if err != nil {
			return nil, evidence, fmt.Errorf("required consumer tool %s unavailable: %w", tool, err)
		}
		tools[tool] = executable
	}
	// Validate declared external inputs before launching any product process.
	external := make([]string, 0, len(consumer.RequiredEnvironment))
	for _, name := range consumer.RequiredEnvironment {
		value := os.Getenv(name)
		info, err := os.Stat(value)
		if value == "" || !filepath.IsAbs(value) || err != nil || !info.IsDir() {
			return nil, evidence, fmt.Errorf("required consumer directory %s is missing or not absolute", name)
		}
		if name == "PG_BIN_DIR" {
			for _, binary := range []string{"initdb", "pg_ctl"} {
				info, err := os.Stat(filepath.Join(value, binary))
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
					return nil, evidence, fmt.Errorf("PG_BIN_DIR requires executable %s", binary)
				}
			}
		}
		external = append(external, name+"="+value)
	}
	toolPath := os.Getenv("PATH")
	for _, directory := range filepath.SplitList(toolPath) {
		if directory == "" || !filepath.IsAbs(directory) {
			return nil, evidence, fmt.Errorf("consumer PATH requires absolute, nonempty entries")
		}
	}
	if toolPath == "" {
		return nil, evidence, fmt.Errorf("consumer PATH is empty")
	}
	tmp, err := os.MkdirTemp("", "compound-consumer-*")
	if err != nil {
		return nil, evidence, err
	}
	defer os.RemoveAll(tmp)
	childHome := os.Getenv("HOME")
	if consumer.ReportKind == "bun_junit" || consumer.ReportKind == "node_test_junit" || consumer.ReportKind == "pytest_junit" {
		childHome = filepath.Join(tmp, "home")
		if err := os.Mkdir(childHome, 0o700); err != nil {
			return nil, evidence, err
		}
	}
	reportName := "result.jsonl"
	if consumer.ReportKind == "xcresult" {
		reportName = "result.xcresult"
	} else if consumer.ReportKind == "bun_junit" || consumer.ReportKind == "node_test_junit" || consumer.ReportKind == "pytest_junit" {
		reportName = "result.junit.xml"
	}
	reportPath := filepath.Join(tmp, reportName)
	env := []string{
		"PATH=" + toolPath, "HOME=" + childHome, "TMPDIR=" + tmp, "LANG=C", "LC_ALL=C",
		"GOPROXY=off", "GOSUMDB=off", "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off",
		"GOCACHE=" + filepath.Join(tmp, "go-cache"), "GOMODCACHE=" + filepath.Join(tmp, "go-modules"),
		"GOPATH=" + filepath.Join(tmp, "go-path"), "COMPOUND_PROOF_REPORT=" + reportPath,
	}
	if consumer.ReportKind == "pytest_junit" {
		env = append(env, "UV_CACHE_DIR="+filepath.Join(tmp, "uv-cache"), "UV_PROJECT_ENVIRONMENT="+filepath.Join(tmp, "venv"), "UV_OFFLINE=1", "UV_FROZEN=1", "UV_NO_CONFIG=1", "UV_PYTHON_DOWNLOADS=never", "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1", "PYTEST_ADDOPTS=--runxfail -o junit_family=xunit1")
	}
	// exec.Cmd deduplicates environment entries, retaining the last declared cache value.
	env = append(env, external...)
	if consumer.ReportKind == "node_test_junit" {
		// Isolating HOME alone leaves npm's installation-prefix global config
		// readable. Both config paths and the writable cache are runner-owned.
		userConfig, globalConfig := filepath.Join(tmp, "npm-user.config"), filepath.Join(tmp, "npm-global.config")
		for _, file := range []string{userConfig, globalConfig} {
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				return nil, evidence, err
			}
		}
		env = append(env, "NPM_CONFIG_USERCONFIG="+userConfig, "NPM_CONFIG_GLOBALCONFIG="+globalConfig,
			"NPM_CONFIG_CACHE="+filepath.Join(tmp, "npm-cache"), "NPM_CONFIG_OFFLINE=true", "NPM_CONFIG_IGNORE_SCRIPTS=true",
			"NPM_CONFIG_AUDIT=false", "NPM_CONFIG_FUND=false", "NPM_CONFIG_FETCH_RETRIES=0")
	}
	if _, err := os.Lstat(reportPath); !os.IsNotExist(err) {
		return nil, evidence, fmt.Errorf("native report target must be fresh")
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(consumer.TimeoutSeconds)*time.Second)
	defer cancel()
	if consumer.ReportKind == "xcresult" {
		if err := preflightConsumerXcode(runCtx, cwd, env, tools, command); err != nil {
			return nil, evidence, err
		}
	}
	cmd := command(runCtx, tools[consumer.Command[0]], consumer.Command[1:]...)
	cmd.Dir, cmd.Env = cwd, env
	// This report-custody fence is additional to the reservation's inherited
	// execution-barrier FD. Capture only after ordinary background descendants
	// finish; the direct command exiting is not proof its writers are finished.
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, evidence, err
	}
	defer reader.Close()
	defer writer.Close()
	cmd.ExtraFiles = append(cmd.ExtraFiles, writer)
	output, runErr := cmd.CombinedOutput()
	runErr = errors.Join(runErr, writer.Close())
	runErr = errors.Join(runErr, awaitConsumerWriters(runCtx, cmd, reader))
	if consumer.ReportKind == "go_test_json" || consumer.ReportKind == "bun_junit" || consumer.ReportKind == "node_test_junit" || consumer.ReportKind == "pytest_junit" {
		evidence.Report, err = readConsumerReport(tmp, reportName)
	} else {
		if err = validateConsumerResultBundle(reportPath); err == nil {
			for _, query := range []string{"summary", "tests"} {
				native := command(runCtx, tools["xcrun"], "xcresulttool", "get", "test-results", query, "--path", reportPath, "--compact")
				native.Dir, native.Env = cwd, env
				nativeOutput, queryErr := native.Output()
				if queryErr != nil {
					err = fmt.Errorf("xcresulttool %s: %w", query, queryErr)
					break
				}
				if len(nativeOutput) > CanonicalConsumerReportLimit {
					err = fmt.Errorf("Xcode native report exceeds limit")
					break
				}
				if query == "summary" {
					evidence.Report = nativeOutput
				} else {
					evidence.Tests = nativeOutput
				}
			}
		}
	}
	if err != nil {
		runErr = errors.Join(runErr, err)
	}
	step.ConsumerEvidence = evidence
	if err := ValidateCanonicalConsumerEvidence(step); err != nil {
		runErr = errors.Join(runErr, err)
	}
	// Generated outputs are allowed; modifications to tracked test/source bytes are not.
	if runErr == nil {
		check := command(runCtx, "git", "diff", "--exit-code", "--no-ext-diff", head, "--")
		check.Dir, check.Env = repo, env
		if diff, err := check.CombinedOutput(); err != nil {
			output = append(output, diff...)
			runErr = fmt.Errorf("consumer changed sealed tracked source: %w", err)
		}
	}
	if runCtx.Err() != nil {
		runErr = errors.Join(runErr, runCtx.Err())
	}
	if runErr != nil {
		output = append(output, []byte("\nconsumer proof: "+runErr.Error()+"\n")...)
	}
	return output, evidence, runErr
}

// Probe the selected toolchain with the product's exact environment before its
// source-owned command can launch. These queries never install or select Xcode,
// build a target, or boot a simulator. Destination selection stays in the recipe.
func preflightConsumerXcode(ctx context.Context, cwd string, env []string, tools map[string]string, command func(context.Context, string, ...string) *exec.Cmd) error {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	probe := func(tool string, args ...string) ([]byte, error) {
		cmd := command(probeCtx, tools[tool], args...)
		cmd.Dir, cmd.Env = cwd, env
		// Native tools may emit successful diagnostics on stderr. Only stdout
		// carries the selected path, version, or query help we validate here.
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("Xcode consumer preflight %s %s: %w", tool, strings.Join(args, " "), errors.Join(err, probeCtx.Err()))
		}
		if len(output) == 0 || len(output) > 64<<10 {
			return nil, fmt.Errorf("Xcode consumer preflight %s requires nonempty bounded output", tool)
		}
		return output, nil
	}
	for _, tool := range []string{"xcodebuild", "xcresulttool"} {
		output, err := probe("xcrun", "--find", tool)
		if err != nil {
			return err
		}
		path := strings.TrimSpace(string(output))
		info, err := os.Stat(path)
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("Xcode consumer preflight requires selected executable %s; use an existing full Xcode toolchain", tool)
		}
	}
	version, err := probe("xcodebuild", "-version")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(version), "Xcode ") || !strings.Contains(string(version), "\nBuild version ") {
		return fmt.Errorf("Xcode consumer preflight requires a full Xcode version and build")
	}
	for _, query := range []string{"summary", "tests"} {
		if _, err := probe("xcrun", "xcresulttool", "get", "test-results", query, "--help"); err != nil {
			return err
		}
	}
	return nil
}

func awaitConsumerWriters(ctx context.Context, cmd *exec.Cmd, reader *os.File) error {
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, reader); done <- err }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Cmd.Wait may already have returned. Kill the original group explicitly;
		// the outer reservation barrier still guards any escaped descendants.
		var killErr error
		if cmd.Process != nil {
			killErr = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(killErr, syscall.ESRCH) {
				killErr = nil
			}
		}
		_ = reader.Close()
		<-done
		return errors.Join(ctx.Err(), killErr)
	}
}

func consumerContainedDirectory(repo, relative string) (string, error) {
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return "", fmt.Errorf("consumer cwd: %w", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("consumer cwd escapes sealed repository")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("consumer cwd is not a directory")
	}
	return dir, nil
}

func readConsumerReport(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open fresh native report: %w", err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() == 0 || before.Size() > CanonicalConsumerReportLimit {
		return nil, fmt.Errorf("native report must be a nonempty bounded regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, CanonicalConsumerReportLimit+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if len(body) > CanonicalConsumerReportLimit || int64(len(body)) != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("native report changed during custody read")
	}
	return body, nil
}

func validateConsumerResultBundle(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("fresh xcresult bundle required: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("xcresult must be a fresh regular directory")
	}
	return filepath.WalkDir(path, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return fmt.Errorf("xcresult contains a symlink or special file")
		}
		return nil
	})
}
