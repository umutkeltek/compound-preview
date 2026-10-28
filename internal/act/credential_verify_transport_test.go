package act

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCredentialVerificationTransportAcceptsOnlyOneFiniteFailureMarker(t *testing.T) {
	const marker = "credential_verify:refused:source_format\n"
	const secret = "FIXTURE_SECRET_STDERR_NEVER_PERSIST"
	for _, tc := range []struct {
		name   string
		chunks []string
		failed bool
		stage  string
	}{
		{"exact", []string{marker}, true, "source_format"},
		{"split", []string{"credential_verify:refused:source_", "format\n"}, true, "source_format"},
		{"duplicate", []string{marker, marker}, true, ""},
		{"unknown", []string{"credential_verify:refused:unknown\n"}, true, ""},
		{"oversize", []string{strings.Repeat(secret, 10000)}, true, ""},
		{"raw-secret", []string{secret}, true, ""},
		{"raw-plus-marker", []string{secret, marker}, true, ""},
		{"missing-newline", []string{strings.TrimSuffix(marker, "\n")}, true, ""},
		{"crlf", []string{strings.ReplaceAll(marker, "\n", "\r\n")}, true, ""},
		{"leading-space", []string{" " + marker}, true, ""},
		{"contradictory-success", []string{marker}, false, ""},
		{"success-raw-stderr", []string{secret}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			close(done)
			child := &credentialVerifyNativeChild{done: done, failed: tc.failed}
			// A failed process cannot offer even an otherwise valid-looking observation.
			child.stdout.data = []byte(`{"schema_version":"compound.credential_verification_observation.v0.1"}`)
			for _, chunk := range tc.chunks {
				if n, err := child.stderr.Write([]byte(chunk)); err != nil || n != len(chunk) {
					t.Fatal("stderr was not drained")
				}
			}
			raw, err := child.Wait()
			if err == nil || raw != nil || strings.Contains(err.Error(), secret) {
				t.Fatal("failed output escaped")
			}
			if credentialVerifySafeFailureStage(err) != tc.stage {
				t.Fatalf("stage = %q, want %q", credentialVerifySafeFailureStage(err), tc.stage)
			}
			if len(child.stderr.data) > 96 {
				t.Fatal("stderr memory bound exceeded")
			}
		})
	}
	for _, stage := range []string{"request_validation", "executable", "host_identity", "protected_file", "source_format", "pre_catalog", "app_existing", "app_wrong", "migrator_existing", "migrator_wrong", "post_catalog", "postchecks", "deadline", "cleanup"} {
		var output credentialVerifyStageOutput
		_, _ = output.Write([]byte("credential_verify:refused:" + stage + "\n"))
		actual, present := output.result()
		if !present || actual != stage {
			t.Fatalf("known stage %s refused", stage)
		}
	}
	if credentialVerifySafeFailureStage(credentialVerifyStageError{stage: "cleanup"}) != "" || credentialVerifySafeFailureStage(errors.New(secret)) != "" {
		t.Fatal("cleanup or raw diagnostic became stage metadata")
	}
}

func credentialKnownHostsFixture(t *testing.T) (*credentialFixture, []byte) {
	t.Helper()
	f := newCredentialFixture(t)
	key := make([]byte, 4+len("ssh-ed25519")+4+32)
	binary.BigEndian.PutUint32(key[:4], uint32(len("ssh-ed25519")))
	copy(key[4:], "ssh-ed25519")
	offset := 4 + len("ssh-ed25519")
	binary.BigEndian.PutUint32(key[offset:offset+4], 32)
	copy(key[offset+4:], strings.Repeat("x", 32))
	raw := []byte(f.request.SSH.Alias + " ssh-ed25519 " + base64.StdEncoding.EncodeToString(key) + " fixture-only\n")
	whole := sha256.Sum256(raw)
	f.request.SSH.KnownHostsSHA256 = hex.EncodeToString(whole[:])
	fp := sha256.Sum256(key)
	f.request.SSH.HostKeySHA256 = "SHA256:" + base64.RawStdEncoding.EncodeToString(fp[:])
	return f, raw
}

func TestCredentialVerificationTransportRequiresOneExactPinnedAliasHostkey(t *testing.T) {
	f, raw := credentialKnownHostsFixture(t)
	if e := verifyCredentialKnownHosts(raw, f.request); e != nil {
		t.Fatal(e)
	}
	for _, which := range []string{"wholefile", "fingerprint", "duplicate", "revoked", "address_only", "wildcard", "key_type"} {
		t.Run(which, func(t *testing.T) {
			r := f.request
			b := append([]byte(nil), raw...)
			switch which {
			case "wholefile":
				b = append(b, '\n')
			case "fingerprint":
				r.SSH.HostKeySHA256 = "SHA256:" + strings.Repeat("A", 43)
			case "duplicate":
				b = append(b, b...)
			case "revoked":
				b = append([]byte("@revoked "), b...)
			case "address_only":
				b = []byte(strings.ReplaceAll(string(b), r.SSH.Alias, r.Resource.Address))
			case "wildcard":
				b = []byte(strings.ReplaceAll(string(b), r.SSH.Alias, "*.fixture.invalid"))
			case "key_type":
				b = []byte(strings.Replace(string(b), " ssh-ed25519 ", " ssh-rsa ", 1))
			}
			if which != "wholefile" {
				sum := sha256.Sum256(b)
				r.SSH.KnownHostsSHA256 = hex.EncodeToString(sum[:])
			}
			if e := verifyCredentialKnownHosts(b, r); e == nil {
				t.Fatalf("%s hostkey accepted", which)
			}
		})
	}
}

func TestCredentialVerificationTransportHasClosedSSHRouteAndNoAmbientAuthority(t *testing.T) {
	f := newCredentialFixture(t)
	args := credentialVerifySSHArgs(f.request)
	joined := strings.Join(args, "\n")
	for _, required := range []string{"/dev/null", "HostName=" + f.request.Resource.Address, "HostKeyAlias=" + f.request.SSH.Alias, "StrictHostKeyChecking=yes", "IdentityAgent=none", "ForwardAgent=no", "ControlMaster=no", "ControlPath=none", "ProxyCommand=none", "ProxyJump=none", "PermitLocalCommand=no", "PasswordAuthentication=no", "KbdInteractiveAuthentication=no", "GlobalKnownHostsFile=/dev/null"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %s", required)
		}
	}
	command := args[len(args)-1]
	if !strings.HasPrefix(command, "/usr/bin/sudo -n -- /usr/bin/python3 -I -B -c ") || !strings.HasSuffix(command, credentialVerifyShellQuote(CredentialVerificationProgramSHA256())) {
		t.Fatal("remote program/interpreter/hash no longer fixed")
	}
	if args[len(args)-2] != f.request.SSH.Alias || !strings.Contains(command, "signal.setitimer") {
		t.Fatal("closed program route changed")
	}
	if credentialVerifyShellQuote("a'b") != "'a'\\''b'" {
		t.Fatal("fixed source shell quoting changed")
	}
}

func TestCredentialVerificationTransportCancelledOrUnownedRequestsNeverReadOrStart(t *testing.T) {
	f := newCredentialFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, which := range []string{"cancelled", "past_deadline", "missing_guard"} {
		t.Run(which, func(t *testing.T) {
			useCtx := context.Background()
			deadline := time.Now().Add(time.Second)
			guard := credentialVerifyAdmissionGuard(func(func() error) error { t.Fatal("native admission reached"); return nil })
			if which == "cancelled" {
				useCtx = ctx
			}
			if which == "past_deadline" {
				deadline = time.Now().Add(-time.Second)
			}
			if which == "missing_guard" {
				guard = nil
			}
			child, e := (credentialVerifyNativeTransport{}).Start(useCtx, f.request, deadline, guard)
			if e == nil || child != nil {
				t.Fatal("inadmissible native transport accepted")
			}
		})
	}
}

func TestCredentialVerificationTransportBoundsStdoutAndNeverReturnsDiagnostics(t *testing.T) {
	var b credentialVerifyLimitedOutput
	sentinel := "FIXTURE_REMOTE_DIAGNOSTIC"
	input := []byte(strings.Repeat(sentinel, 4000))
	if n, e := b.Write(input); e != nil || n != len(input) {
		t.Fatal("bounded discard did not drain")
	}
	if !b.overflow || len(b.data) != 32769 {
		t.Fatal("output memory bound changed")
	}
	done := make(chan struct{})
	close(done)
	child := &credentialVerifyNativeChild{done: done}
	child.stdout.data = append([]byte(nil), b.data...)
	child.stdout.overflow = b.overflow
	raw, e := child.Wait()
	if e == nil || raw != nil || strings.Contains(e.Error(), sentinel) {
		t.Fatal("raw overflow escaped")
	}
	child = &credentialVerifyNativeChild{done: done, failed: true}
	if raw, e = child.Wait(); e == nil || raw != nil {
		t.Fatal("native diagnostic became success")
	}
	if e = child.Stop(context.Background()); e != nil {
		t.Fatal("already reaped child was not accepted as cleaned")
	}
}

func TestCredentialVerificationKnownHostsRejectsWholeFileTrustExpansion(t *testing.T) {
	for _, host := range []string{"*", "*.fixture.invalid", "target.?ixture.invalid", "!unrelated.fixture.invalid",
		"other.fixture.invalid,target.fixture.invalid", "[target.fixture.invalid]:22", "Target.fixture.invalid",
		"@cert-authority", "unrelated..fixture.invalid", "unrelated.fixture.invalid.", "-unrelated.fixture.invalid", "unrelated_.fixture.invalid"} {
		t.Run(host, func(t *testing.T) {
			f, exact := credentialKnownHostsFixture(t)
			fields := strings.Fields(string(exact))
			raw := append(append([]byte(nil), exact...), []byte(host+" "+fields[1]+" "+fields[2]+" synthetic-other-row\n")...)
			sum := sha256.Sum256(raw)
			f.request.SSH.KnownHostsSHA256 = hex.EncodeToString(sum[:])
			if err := verifyCredentialKnownHosts(raw, f.request); err == nil {
				t.Fatal("whole-file grammar admitted ambiguous trust row")
			}
		})
	}
	f, exact := credentialKnownHostsFixture(t)
	salt := []byte(strings.Repeat("s", 20))
	mac := hmac.New(sha1.New, salt)
	_, _ = mac.Write([]byte(f.request.SSH.Alias))
	hashed := "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	fields := strings.Fields(string(exact))
	raw := append(append([]byte(nil), exact...), []byte(hashed+" "+fields[1]+" "+fields[2]+" synthetic-hashed-row\n")...)
	sum := sha256.Sum256(raw)
	f.request.SSH.KnownHostsSHA256 = hex.EncodeToString(sum[:])
	if err := verifyCredentialKnownHosts(raw, f.request); err == nil {
		t.Fatal("canonical twenty-byte-salt hashed host admitted")
	}
}

func TestCredentialVerificationKnownHostsAllowsUnrelatedCanonicalLiteralsOnlyWithValidKeys(t *testing.T) {
	f, exact := credentialKnownHostsFixture(t)
	fields := strings.Fields(string(exact))
	other := "unrelated.fixture.invalid " + fields[1] + " " + fields[2] + " synthetic-other-row\n"
	for _, which := range []string{"valid", "truncated", "trailing_wire_data", "unknown_key", "missing_key"} {
		t.Run(which, func(t *testing.T) {
			row := other
			switch which {
			case "truncated":
				key, _ := base64.StdEncoding.DecodeString(fields[2])
				row = strings.Replace(row, fields[2], base64.StdEncoding.EncodeToString(key[:len(key)-1]), 1)
			case "trailing_wire_data":
				key, _ := base64.StdEncoding.DecodeString(fields[2])
				row = strings.Replace(row, fields[2], base64.StdEncoding.EncodeToString(append(key, 0)), 1)
			case "unknown_key":
				row = strings.Replace(row, fields[1], "ssh-unknown", 1)
			case "missing_key":
				row = "unrelated.fixture.invalid ssh-ed25519\n"
			}
			raw := append(append([]byte(nil), exact...), []byte(row)...)
			sum := sha256.Sum256(raw)
			r := f.request
			r.SSH.KnownHostsSHA256 = hex.EncodeToString(sum[:])
			err := verifyCredentialKnownHosts(raw, r)
			if (err == nil) != (which == "valid") {
				t.Fatalf("unrelated literal key validation: case=%s err=%v", which, err)
			}
		})
	}
}
