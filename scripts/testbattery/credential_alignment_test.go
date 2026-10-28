package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCredentialAlignmentInventoryConsumesEveryActualNativeOwnerTest(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "internal", "act", "credential_password_align_native_test.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]bool{}
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && (strings.HasPrefix(function.Name.Name, "TestCredentialPasswordAlignmentActual") || function.Name.Name == "TestCredentialPasswordAlignmentProgramActualLinuxPrimitiveConsumer") {
			actual[function.Name.Name] = true
		}
	}
	if len(actual) == 0 {
		t.Fatal("source owns no actual native credential consumers")
	}
	for _, required := range credentialAlignmentRequiredTests {
		if strings.Contains(required, "/") {
			continue
		}
		if !actual[required] {
			t.Fatal("native gate names an absent source consumer", required)
		}
		delete(actual, required)
	}
	if len(actual) != 0 {
		t.Fatal("actual native owner cases are absent from the required release inventory", actual)
	}
}

func credentialAlignmentFixtureReport() []byte {
	var report bytes.Buffer
	write := func(action, test string) {
		body, _ := json.Marshal(map[string]string{"Action": action, "Package": "compound/internal/act", "Test": test})
		report.Write(body)
		report.WriteByte('\n')
	}
	write("start", "")
	for _, test := range credentialAlignmentRequiredTests {
		if strings.Contains(test, "/") {
			continue
		}
		write("run", test)
		for _, child := range credentialAlignmentRequiredTests {
			if strings.HasPrefix(child, test+"/") {
				write("run", child)
				write("pass", child)
			}
		}
		write("pass", test)
	}
	write("pass", "")
	return report.Bytes()
}

func TestCredentialAlignmentNativeReportRejectsMissingSkippedFailedAndAmbiguousEvidence(t *testing.T) {
	good := credentialAlignmentFixtureReport()
	if err := validateCredentialAlignmentReport(good); err != nil {
		t.Fatal(err)
	}
	first := credentialAlignmentRequiredTests[0]
	for name, body := range map[string][]byte{
		"absent":       nil,
		"unfinished":   good[:bytes.LastIndex(bytes.TrimSuffix(good, []byte("\n")), []byte("\n"))+1],
		"skipped":      bytes.Replace(good, []byte(`"Action":"pass"`), []byte(`"Action":"skip"`), 1),
		"failed":       bytes.Replace(good, []byte(`"Action":"pass"`), []byte(`"Action":"fail"`), 1),
		"foreign":      bytes.ReplaceAll(good, []byte("compound/internal/act"), []byte("example.invalid/foreign")),
		"cached":       append([]byte("{\"Action\":\"output\",\"Package\":\"compound/internal/act\",\"Output\":\"ok (cached)\"}\n"), good...),
		"duplicateKey": bytes.Replace(good, []byte(`"Action":"run"`), []byte(`"Action":"run","action":"pass"`), 1),
		"duplicateRun": bytes.Replace(good, []byte(`"Action":"pass"`), []byte(`"Action":"run"`), 1),
		"malformed":    append(slices.Clone(good), '{'),
		"oversized":    bytes.Repeat([]byte(" "), credentialAlignmentReportLimit+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateCredentialAlignmentReport(body); err == nil {
				t.Fatal("false-green native evidence accepted")
			}
		})
	}
	var omitted bytes.Buffer
	for _, line := range bytes.Split(good, []byte("\n")) {
		if len(line) != 0 && !bytes.Contains(line, []byte(first)) {
			omitted.Write(line)
			omitted.WriteByte('\n')
		}
	}
	if err := validateCredentialAlignmentReport(omitted.Bytes()); err == nil {
		t.Fatal("omitted real repair consumer accepted")
	}
	var bounded credentialAlignmentReport
	if _, err := bounded.Write(bytes.Repeat([]byte("x"), credentialAlignmentReportLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Write([]byte("x")); err == nil || bounded.Len() != credentialAlignmentReportLimit {
		t.Fatal("report bound was not enforced")
	}
}

type credentialAlignmentRunnerFixture struct {
	t                  *testing.T
	name, token, label string
	missingImage       bool
	wrongImage         bool
	wrongCleanupOwner  bool
	goFailure          bool
	started, removed   bool
	goEnvironment      []string
}

func (f *credentialAlignmentRunnerFixture) run(_ context.Context, args, environment []string, stdout, _ io.Writer) error {
	f.t.Helper()
	if args[0] == "go" {
		f.goEnvironment = slices.Clone(environment)
		if !slices.Contains(args, "-count=1") || !slices.Contains(args, "-json") || !slices.Contains(args, "./internal/act") {
			f.t.Fatal("native Go inventory lost freshness or its actual production seam", args)
		}
		_, _ = stdout.Write(credentialAlignmentFixtureReport())
		if f.goFailure {
			return errors.New("synthetic native process failure")
		}
		return nil
	}
	if len(args) < 5 || args[0] != "docker" || args[1] != "--host" || args[2] != credentialAlignmentDockerHost {
		f.t.Fatal("fixture escaped the fixed local Docker endpoint", args)
	}
	switch args[3] {
	case "image":
		if f.missingImage {
			return errors.New("synthetic missing prerequisite")
		}
		image := credentialAlignmentPrerequisiteImage
		if f.wrongImage {
			image = "sha256:" + strings.Repeat("0", 64)
		}
		_, _ = fmt.Fprintln(stdout, image)
	case "run":
		f.started = true
		for i, arg := range args {
			if arg == "--name" {
				f.name = args[i+1]
			}
			if strings.HasPrefix(arg, "compound.credential-alignment-fixture=") {
				f.token = strings.TrimPrefix(arg, "compound.credential-alignment-fixture=")
			}
			if strings.HasPrefix(arg, "compound.work=") {
				f.label = strings.TrimPrefix(arg, "compound.work=")
			}
		}
		if !slices.Contains(args, "none") || !slices.Contains(args, "--pull") || !slices.Contains(args, "never") || slices.Contains(args, "--publish") || !slices.Contains(args, "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock,readonly") {
			f.t.Fatal("fixture lost no-port/network/socket fences", args)
		}
		hostname, alias := slices.Index(args, "--hostname"), slices.Index(args, "--add-host")
		if hostname < 0 || alias < 0 || args[hostname+1] != f.name || args[alias+1] != f.name+":127.0.0.1" {
			f.t.Fatal("network-isolated fixture cannot resolve its exact own hostname", args)
		}
		_, _ = fmt.Fprintln(stdout, strings.Repeat("a", 64))
	case "exec":
		if !slices.Contains(args, "-h") || !slices.Contains(args, "127.0.0.1") || !slices.Contains(args, "5432") {
			f.t.Fatal("temporary Unix-only initialization server could count as ready", args)
		}
		return nil
	case "container":
		if args[4] == "inspect" {
			owner := f.token
			if f.wrongCleanupOwner && len(f.goEnvironment) > 0 {
				owner = "foreign-owner"
			}
			_, _ = fmt.Fprintln(stdout, strings.Repeat("a", 64), "true", owner, f.label, "0 none")
		} else if args[4] == "rm" {
			if args[len(args)-1] != strings.Repeat("a", 64) || !slices.Contains(args, "--volumes") {
				f.t.Fatal("cleanup targeted an unverified container or leaked its volume", args)
			}
			f.removed = true
		} else {
			f.t.Fatal("unexpected fixture operation", args)
		}
	default:
		f.t.Fatal("unexpected fixture operation", args)
	}
	return nil
}

func TestCredentialAlignmentProofUsesFreshOwnedFixtureAndCleansOnFailure(t *testing.T) {
	t.Setenv("DATABASE_URL", "synthetic-provider-secret")
	t.Setenv("DOCKER_HOST", "ssh://foreign.invalid")
	t.Setenv("DOCKER_CONTEXT", "foreign-context")
	t.Setenv("COMPOUND_ALIGNMENT_NATIVE_CONTAINER", "foreign-fixture")
	for _, failed := range []bool{false, true} {
		f := &credentialAlignmentRunnerFixture{t: t, goFailure: failed}
		var output bytes.Buffer
		err := runCredentialAlignmentProof(t.Context(), f.run, &output, io.Discard)
		if (err != nil) != failed || !f.started || !f.removed {
			t.Fatal("native verdict or exact owned cleanup was lost", err)
		}
		if !strings.HasPrefix(f.name, "compound-credential-proof-") || !slices.Contains(f.goEnvironment, "COMPOUND_ALIGNMENT_NATIVE_CONTAINER="+f.name) || !slices.Contains(f.goEnvironment, "COMPOUND_ALIGNMENT_ALLOW_FIXTURE=1") || !slices.Contains(f.goEnvironment, "DOCKER_HOST="+credentialAlignmentDockerHost) {
			t.Fatal("closed native fixture selection was not supplied")
		}
		for _, value := range f.goEnvironment {
			if strings.Contains(value, "foreign") || strings.Contains(value, "synthetic-provider-secret") || strings.HasPrefix(value, "DOCKER_CONTEXT=") {
				t.Fatal("ambient endpoint, credential or fixture selection escaped into proof")
			}
		}
	}
}

func TestCredentialAlignmentProofFailsMissingPrerequisitesAndRefusesForeignCleanup(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		f := &credentialAlignmentRunnerFixture{t: t, missingImage: !wrong, wrongImage: wrong}
		if err := runCredentialAlignmentProof(t.Context(), f.run, io.Discard, io.Discard); err == nil || f.started || f.removed {
			t.Fatal("missing/different prerequisite caused a skip or fixture start")
		}
	}
	f := &credentialAlignmentRunnerFixture{t: t, wrongCleanupOwner: true}
	if err := runCredentialAlignmentProof(t.Context(), f.run, io.Discard, io.Discard); err == nil || f.removed {
		t.Fatal("changed fixture ownership allowed cleanup")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := runCredentialAlignmentProof(ctx, func(context.Context, []string, []string, io.Writer, io.Writer) error { called = true; return nil }, io.Discard, io.Discard)
	if err == nil || called {
		t.Fatal("cancelled proof launched a command")
	}
	if err := runBoundSourceGate(t.Context(), credentialAlignmentProofMode, "", io.Discard, io.Discard); err == nil {
		t.Fatal("native proof bypassed the exact sealed source-head requirement")
	}
}
