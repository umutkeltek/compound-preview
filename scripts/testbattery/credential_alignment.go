package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// This is a frozen synthetic test prerequisite, not a reproducible source
// build. It contains Linux PG17, Python, sudo and a root-owned Docker CLI.
// Mounted PostgreSQL data and the Docker socket were excluded from the freeze.
// Every test copies its current sealed source into a new disposable container.
const credentialAlignmentPrerequisiteImage = "sha256:e9851137192b495dcc67f03d032446a0bfeca905351b4065ba214173dac9a5fa"
const credentialAlignmentDockerHost = "unix:///var/run/docker.sock"
const credentialAlignmentReportLimit = 4 << 20
const credentialAlignmentProofMode = "credential-password-alignment-native"

var credentialAlignmentRequiredTests = []string{
	"TestCredentialPasswordAlignmentActualLinuxOwnerThenUnchangedVerifier",
	"TestCredentialPasswordAlignmentActualCommercialSevenThenUnchangedVerifier",
	"TestCredentialPasswordAlignmentActualCommercialSevenStoppedContainers",
	"TestCredentialPasswordAlignmentActualCommercialSevenStoppedContainers/auth",
	"TestCredentialPasswordAlignmentActualCommercialSevenStoppedContainers/cloudflared",
	"TestCredentialPasswordAlignmentProgramActualLinuxPrimitiveConsumer",
	"TestCredentialPasswordAlignmentActualCommitAckLossAndRestoreAckLoss",
	"TestCredentialPasswordAlignmentActualTamperedOldVerifierSwapHolds",
	"TestCredentialPasswordAlignmentActualTamperedOldVerifierSwapHolds/rewrite_request_false",
	"TestCredentialPasswordAlignmentActualTamperedOldVerifierSwapHolds/rewrite_request_true",
	"TestCredentialPasswordAlignmentActualPrecommitSIGKILLMatrix",
	"TestCredentialPasswordAlignmentActualPrecommitSIGKILLMatrix/before_preimage",
	"TestCredentialPasswordAlignmentActualPrecommitSIGKILLMatrix/after_preimage",
	"TestCredentialPasswordAlignmentActualPrecommitSIGKILLMatrix/after_first_alter",
	"TestCredentialPasswordAlignmentActualPrecommitSIGKILLMatrix/after_second_alter",
	"TestCredentialPasswordAlignmentActualPrecommitSIGKILLMatrix/after_postimage_before_commit",
}

type credentialAlignmentCommandRunner func(context.Context, []string, []string, io.Writer, io.Writer) error

func credentialAlignmentCommand(ctx context.Context, args, environment []string, stdout, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Inherit the canonical gate's process group and execution barrier, like runGo.
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env, cmd.Stdout, cmd.Stderr = environment, stdout, stderr
	return cmd.Run()
}

func credentialAlignmentEnvironment(container string) []string {
	// No ambient DSN, provider credential, Docker endpoint or fixture override
	// reaches the suite. Docker is restricted to the local Unix socket.
	environment := []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOENV=off", "GOFLAGS=", "LC_ALL=C", "DOCKER_HOST=" + credentialAlignmentDockerHost}
	for _, key := range []string{"PATH", "HOME", "GOCACHE", "GOMODCACHE", "GOTMPDIR", "TMPDIR"} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	if container != "" {
		environment = append(environment, "COMPOUND_ALIGNMENT_NATIVE_CONTAINER="+container, "COMPOUND_ALIGNMENT_ALLOW_FIXTURE=1")
	}
	return environment
}

func credentialAlignmentDocker(args ...string) []string {
	return append([]string{"docker", "--host", credentialAlignmentDockerHost}, args...)
}

func runCredentialAlignmentProof(ctx context.Context, run credentialAlignmentCommandRunner, stdout, stderr io.Writer) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	environment := credentialAlignmentEnvironment("")
	var image bytes.Buffer
	if err := run(ctx, credentialAlignmentDocker("image", "inspect", "--format", "{{.Id}}", credentialAlignmentPrerequisiteImage), environment, &image, stderr); err != nil || strings.TrimSpace(image.String()) != credentialAlignmentPrerequisiteImage {
		return errors.New("frozen credential-alignment prerequisite image absent or different; proof never pulls or installs prerequisites")
	}
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes[:])
	name := "compound-credential-proof-" + token
	workLabel := "work_fixture_credential_alignment_" + token
	containerID := ""
	// A start failure may still have created our exact named container. Cleanup
	// resolves only that unique name and verifies both labels before any removal.
	defer func() {
		result = errors.Join(result, cleanupCredentialAlignmentFixture(context.WithoutCancel(ctx), run, environment, name, token, workLabel, containerID, stderr))
	}()
	var started bytes.Buffer
	args := credentialAlignmentDocker("run", "--pull", "never", "--detach", "--name", name,
		"--label", "compound.credential-alignment-fixture="+token,
		"--label", "compound.work="+workLabel,
		"--network", "none", "--hostname", name, "--add-host", name+":127.0.0.1",
		"--memory", "512m", "--cpus", "2", "--pids-limit", "256",
		"--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock,readonly",
		"--env", "POSTGRES_HOST_AUTH_METHOD=trust", credentialAlignmentPrerequisiteImage)
	if err := run(ctx, args, environment, &started, stderr); err != nil {
		return fmt.Errorf("create owned credential-alignment fixture: %w", err)
	}
	containerID = strings.TrimSpace(started.String())
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(containerID) {
		containerID = ""
		return errors.New("credential-alignment fixture returned no exact container identity")
	}
	if err := inspectCredentialAlignmentFixture(ctx, run, environment, name, token, workLabel, containerID, stderr); err != nil {
		return err
	}
	readyUntil := time.Now().Add(30 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The image entrypoint's temporary initialization server is Unix-only.
		// Require the final TCP listener before any native authentication case.
		if err := run(ctx, credentialAlignmentDocker("exec", containerID, "/usr/lib/postgresql/17/bin/pg_isready", "-h", "127.0.0.1", "-p", "5432", "-U", "postgres"), environment, io.Discard, io.Discard); err == nil {
			break
		}
		if !time.Now().Before(readyUntil) {
			return errors.New("credential-alignment fixture PostgreSQL17 readiness failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	var topLevel []string
	for _, test := range credentialAlignmentRequiredTests {
		if !strings.Contains(test, "/") {
			topLevel = append(topLevel, test)
		}
	}
	command := []string{"go", "test", "-count=1", "-timeout=5m", "-json", "./internal/act", "-run", "^(" + strings.Join(topLevel, "|") + ")$"}
	var report credentialAlignmentReport
	runErr := run(ctx, command, credentialAlignmentEnvironment(name), &report, stderr)
	if _, err := stdout.Write(report.Bytes()); err != nil {
		return errors.Join(runErr, err)
	}
	if err := errors.Join(runErr, report.err, validateCredentialAlignmentReport(report.Bytes())); err != nil {
		return fmt.Errorf("required native credential-alignment consumers failed: %w", err)
	}
	_, err := fmt.Fprintf(stdout, "credential-alignment-native: passed=%d skipped=0 frozen-prerequisite=%s\n", len(credentialAlignmentRequiredTests), credentialAlignmentPrerequisiteImage)
	return err
}

const credentialAlignmentInspectFormat = `{{.Id}} {{.State.Running}} {{index .Config.Labels "compound.credential-alignment-fixture"}} {{index .Config.Labels "compound.work"}} {{len .HostConfig.PortBindings}} {{.HostConfig.NetworkMode}}`

func inspectCredentialAlignmentFixture(ctx context.Context, run credentialAlignmentCommandRunner, environment []string, name, token, workLabel, containerID string, stderr io.Writer) error {
	var metadata bytes.Buffer
	if err := run(ctx, credentialAlignmentDocker("container", "inspect", "--format", credentialAlignmentInspectFormat, name), environment, &metadata, stderr); err != nil {
		return fmt.Errorf("owned credential-alignment fixture unavailable: %w", err)
	}
	if strings.TrimSpace(metadata.String()) != containerID+" true "+token+" "+workLabel+" 0 none" {
		return errors.New("credential-alignment fixture identity, ownership or network fence differs")
	}
	return nil
}

func cleanupCredentialAlignmentFixture(ctx context.Context, run credentialAlignmentCommandRunner, environment []string, name, token, workLabel, containerID string, stderr io.Writer) error {
	var metadata bytes.Buffer
	if err := run(ctx, credentialAlignmentDocker("container", "inspect", "--format", credentialAlignmentInspectFormat, name), environment, &metadata, io.Discard); err != nil {
		return errors.New("owned fixture cleanup could not verify exact ownership; no removal attempted")
	}
	fields := strings.Fields(metadata.String())
	if len(fields) != 6 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fields[0]) || fields[2] != token || fields[3] != workLabel || fields[4] != "0" || fields[5] != "none" || (containerID != "" && fields[0] != containerID) {
		return errors.New("owned fixture cleanup refused changed identity or ownership; no removal attempted")
	}
	return run(ctx, credentialAlignmentDocker("container", "rm", "--force", "--volumes", fields[0]), environment, io.Discard, stderr)
}

type credentialAlignmentReport struct {
	bytes.Buffer
	err error
}

func (r *credentialAlignmentReport) Write(body []byte) (int, error) {
	if r.Len()+len(body) > credentialAlignmentReportLimit {
		r.err = errors.New("native credential-alignment report exceeds its fixed bound")
		return 0, r.err
	}
	return r.Buffer.Write(body)
}

func validateCredentialAlignmentReport(body []byte) error {
	if len(body) == 0 || len(body) > credentialAlignmentReportLimit {
		return errors.New("native credential-alignment report absent or oversized")
	}
	states := make(map[string]string, len(credentialAlignmentRequiredTests))
	for _, test := range credentialAlignmentRequiredTests {
		states[test] = ""
	}
	packageState := ""
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var event struct{ Action, Package, Test, Output string }
		if !uniqueCredentialAlignmentEvent(scanner.Bytes()) || json.Unmarshal(scanner.Bytes(), &event) != nil || event.Package != "compound/internal/act" {
			return errors.New("malformed or foreign native credential-alignment event")
		}
		if event.Action == "output" {
			if strings.Contains(event.Output, "(cached)") {
				return errors.New("cached native credential-alignment evidence")
			}
			continue
		}
		if event.Test == "" {
			if event.Action == "start" && packageState == "" {
				packageState = "running"
				continue
			}
			if event.Action == "pass" && packageState == "running" {
				for _, state := range states {
					if state != "pass" {
						return errors.New("required native credential-alignment case omitted or unfinished")
					}
				}
				packageState = "pass"
				continue
			}
			return errors.New("native credential-alignment package failed, skipped, duplicated or unfinished")
		}
		state, required := states[event.Test]
		if !required || packageState != "running" {
			return errors.New("unexpected native credential-alignment test or ordering")
		}
		if event.Action == "run" && state == "" {
			states[event.Test] = "running"
		} else if event.Action == "pass" && state == "running" {
			states[event.Test] = "pass"
		} else {
			return errors.New("native credential-alignment test failed, skipped, duplicated or unordered")
		}
	}
	if scanner.Err() != nil || packageState != "pass" {
		return errors.New("native credential-alignment report incomplete")
	}
	return nil
}

func uniqueCredentialAlignmentEvent(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		key = strings.ToLower(key)
		if err != nil || !ok || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return false
	}
	var extra json.RawMessage
	return decoder.Decode(&extra) == io.EOF
}
