package gitrepository

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

// ReferenceExpectation is one exact ref value that must remain locked while a
// caller persists an authority transition.
type ReferenceExpectation struct {
	Ref string
	SHA string
}

// ReferenceTransaction is a prepared, verify-only Git reference transaction.
// Prepare holds Git's native ref locks until Commit or Abort.
type ReferenceTransaction interface {
	Commit() error
	Abort() error
}

type preparedReferenceTransaction struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	stderr   *bytes.Buffer
	finished bool
}

// PrepareReferenceTransaction verifies every expected ref through `git
// update-ref --stdin`, prepares the transaction, and returns while Git keeps
// its native ref locks held. No ref values are changed by this verify-only
// transaction.
func PrepareReferenceTransaction(repo string, expectations []ReferenceExpectation) (ReferenceTransaction, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil, fmt.Errorf("repository path required")
	}
	if len(expectations) == 0 {
		return nil, fmt.Errorf("at least one Git reference expectation is required")
	}
	expectations = append([]ReferenceExpectation{}, expectations...)
	sort.Slice(expectations, func(i, j int) bool { return expectations[i].Ref < expectations[j].Ref })
	seen := map[string]bool{}
	for _, expected := range expectations {
		if expected.Ref = strings.TrimSpace(expected.Ref); expected.Ref == "" || !strings.HasPrefix(expected.Ref, "refs/") {
			return nil, fmt.Errorf("canonical Git ref required, got %q", expected.Ref)
		}
		if seen[expected.Ref] {
			return nil, fmt.Errorf("duplicate Git ref expectation %s", expected.Ref)
		}
		seen[expected.Ref] = true
		if !validObjectID(strings.TrimSpace(expected.SHA)) {
			return nil, fmt.Errorf("full lowercase Git object id required for %s", expected.Ref)
		}
		if out, err := gitCommandWithoutHooks(repo, "check-ref-format", expected.Ref).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("invalid Git ref %s: %w: %s", expected.Ref, err, strings.TrimSpace(string(out)))
		}
	}

	stderr := &bytes.Buffer{}
	cmd := gitCommandWithoutHooks(repo, "update-ref", "--stdin")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	tx := &preparedReferenceTransaction{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdoutPipe),
		stderr: stderr,
	}
	if err := tx.control("start", "start: ok"); err != nil {
		return nil, tx.fail(err)
	}
	var body strings.Builder
	for _, expected := range expectations {
		fmt.Fprintf(&body, "verify %s %s\n", expected.Ref, strings.TrimSpace(expected.SHA))
	}
	body.WriteString("prepare\n")
	if _, err := io.WriteString(tx.stdin, body.String()); err != nil {
		return nil, tx.fail(err)
	}
	if err := tx.readControl("prepare: ok"); err != nil {
		return nil, tx.fail(err)
	}
	return tx, nil
}

func gitCommandWithoutHooks(repo string, args ...string) *exec.Cmd {
	gitArgs := []string{"-c", "core.hooksPath=" + os.DevNull, "-C", repo}
	return exec.Command("git", append(gitArgs, args...)...)
}

func (tx *preparedReferenceTransaction) Commit() error {
	return tx.finish("commit", "commit: ok")
}

func (tx *preparedReferenceTransaction) Abort() error {
	return tx.finish("abort", "abort: ok")
}

func (tx *preparedReferenceTransaction) control(command, want string) error {
	if _, err := io.WriteString(tx.stdin, command+"\n"); err != nil {
		return err
	}
	return tx.readControl(want)
}

func (tx *preparedReferenceTransaction) readControl(want string) error {
	line, err := tx.stdout.ReadString('\n')
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(line); got != want {
		return fmt.Errorf("Git reference transaction response %q, want %q", got, want)
	}
	return nil
}

func (tx *preparedReferenceTransaction) finish(command, want string) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.finished {
		return nil
	}
	tx.finished = true
	controlErr := tx.control(command, want)
	closeErr := tx.stdin.Close()
	waitErr := tx.cmd.Wait()
	if controlErr != nil {
		return tx.commandError(command, controlErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if waitErr != nil {
		return tx.commandError(command, waitErr)
	}
	return nil
}

func (tx *preparedReferenceTransaction) fail(cause error) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if !tx.finished {
		tx.finished = true
		_ = tx.stdin.Close()
		waitErr := tx.cmd.Wait()
		if cause == nil {
			cause = waitErr
		}
	}
	return tx.commandError("prepare", cause)
}

func (tx *preparedReferenceTransaction) commandError(stage string, cause error) error {
	message := strings.TrimSpace(tx.stderr.String())
	if message == "" && cause != nil && !errors.Is(cause, io.EOF) {
		message = cause.Error()
	}
	if message == "" {
		message = "Git process ended without a response"
	}
	return fmt.Errorf("%s Git reference transaction: %s", stage, message)
}
