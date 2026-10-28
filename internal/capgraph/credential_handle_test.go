package capgraph

import (
	"strings"
	"testing"
)

// These tests are not about parsing. They are about the guarantee: a credential
// VALUE must not be writable into a Compound record, because the record it would
// land in is append-only and is joined into orient offers -- so a secret
// accepted here is a secret in an agent's context that cannot be unwritten.

func TestCredentialHandleRefusesPastedSecretValues(t *testing.T) {
	// Each of these has the shape of a real credential. Any one being ACCEPTED
	// means the pointer-not-value guarantee is decorative.
	// Fixtures are ASSEMBLED, never written literally. A test file full of
	// credential-shaped literals trips the repo's own redact push gate on every
	// push, and a gate that always fires is a gate that gets bypassed. Assembling
	// them costs nothing and keeps the guard honest for real leaks.
	lower := "abcdefghijklmnopqrstuvwxyz"
	valueShaped := map[string]string{
		"cloud access key id": "home-secret://" + "AKIA" + "IOSFODNN7EXAMPLE",
		"cloud session key":   "home-secret://" + "ASIA" + "Y34FZKBOKMUTVV7A",
		"vendor api key":      "home-secret://" + "sk-" + "proj-" + lower + "012345",
		"forge pat":           "home-secret://" + "ghp" + "_" + lower + "0123456789",
		"forge oauth token":   "home-secret://" + "gho" + "_" + lower + "0123456789",
		"chat platform token": "home-secret://" + "xoxb" + "-" + "AAAABBBBCC" + "-abcdefghij",
		"pem private key":     "home-secret://" + "-----BEGIN " + "RSA PRIVATE KEY-----",
		"json web token":      "home-secret://" + "eyJ" + "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" + "." + "eyJzdWIiOiIxMjM0NQ",
		"generic opaque blob": "home-secret://" + strings.Repeat("QWERTYuiop", 5),
	}
	for name, ref := range valueShaped {
		t.Run(name, func(t *testing.T) {
			err := ValidateCredentialHandle(CredentialHandle{
				Ref: ref, Custodian: "example-store", EnvName: "SOME_KEY",
			})
			if err == nil {
				t.Fatalf("ACCEPTED a credential value as a pointer (%s): %q\n"+
					"this record is append-only and feeds orient, so this would be a secret in agent context", name, ref)
			}
		})
	}
}

func TestCredentialHandleRefusesSecretsInEveryField(t *testing.T) {
	// Guarding only the ref would be guarding the tidy mistake. Someone pasting
	// a key is as likely to drop it in the env-name or custodian slot.
	cases := []struct {
		name string
		h    CredentialHandle
	}{
		{"in env name", CredentialHandle{
			Ref: "home-secret://lane/agent.env", Custodian: "compound",
			EnvName: "sk-" + "proj-" + "abcdefghijklmnopqrstuvwxyz012345"}},
		{"in custodian", CredentialHandle{
			Ref: "home-secret://lane/agent.env", Custodian: "AKIA" + "IOSFODNN7EXAMPLE", EnvName: "AGENT_KEY"}},
		{"in broker argv", CredentialHandle{
			Ref: "home-secret://lane/agent.env", Custodian: "compound", EnvName: "AGENT_KEY",
			Broker: "inject " + "AKIA" + "IOSFODNN7EXAMPLE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCredentialHandle(tc.h); err == nil {
				t.Fatalf("ACCEPTED a credential value in a non-ref field: %+v", tc.h)
			}
		})
	}
}

func TestCredentialHandleRefusesUnknownInjectionKind(t *testing.T) {
	// An unrecognized kind means nobody has decided how the value reaches the
	// child. Guessing is how a credential gets copied somewhere it should not be.
	err := ValidateCredentialHandle(CredentialHandle{
		Ref: "mechanism-of-the-week://prod/key", Custodian: "somewhere", EnvName: "K",
	})
	if err == nil {
		t.Fatal("ACCEPTED an unknown injection kind; the kind set must stay closed")
	}
}

func TestCredentialHandleRefusesAnUnnamedCustodian(t *testing.T) {
	// "Which store holds this?" being unanswerable is the exact failure this
	// type exists to fix, so a handle that cannot answer it is not a handle.
	err := ValidateCredentialHandle(CredentialHandle{Ref: "selector://some-profile", EnvName: "PROFILE_VAR"})
	if err == nil {
		t.Fatal("ACCEPTED a handle with no custodian; the sprawl it fixes is precisely not knowing who holds what")
	}
}

func TestBrokeredKindRequiresAnEnvName(t *testing.T) {
	// With no target variable the broker has nowhere to put the value, so the
	// child silently runs unauthenticated -- surfacing as a confusing 401 far
	// from the cause rather than as a refusal at the door.
	err := ValidateCredentialHandle(CredentialHandle{Ref: "brokered://tenant-name", Custodian: "gateway"})
	if err == nil {
		t.Fatal("ACCEPTED a brokered handle with no env name; the child would run unauthenticated")
	}
}

func TestContainerKindLocatorMustNameItsVariable(t *testing.T) {
	// A dotenv holds many secrets. A locator naming only the file says "one of
	// the secrets in here", which is not an address.
	if err := ValidateCredentialHandle(CredentialHandle{
		Ref: "env-file://~/example/.env", Custodian: "operator", EnvName: "K",
	}); err == nil {
		t.Fatal("ACCEPTED an env-file locator with no variable; that addresses a file, not a credential")
	}
	if err := ValidateCredentialHandle(CredentialHandle{
		Ref: "env-file://~/example/.env#EXAMPLE_API_KEY", Custodian: "operator", EnvName: "EXAMPLE_API_KEY",
	}); err != nil {
		t.Fatalf("REFUSED a well-formed env-file locator: %v", err)
	}
}

func TestHandleFieldsWithoutARefAreRefused(t *testing.T) {
	for _, h := range []CredentialHandle{
		{EnvName: "SOME_KEY"},
		{Custodian: "somewhere"},
		{Broker: "some-tool exec --"},
	} {
		if err := ValidateCredentialHandle(h); err == nil {
			t.Fatalf("ACCEPTED handle fields with no credential_ref: %+v", h)
		}
	}
}

func TestLegitimatePointersAreAccepted(t *testing.T) {
	// A guard that also refuses the real thing is worthless. One example per
	// supported mechanism.
	ok := []CredentialHandle{
		{}, // no credential needed: the common case
		{Ref: "selector://example-profile", Custodian: "cloud-cli", EnvName: "EXAMPLE_PROFILE"},
		{Ref: "brokered://example-tenant", Custodian: "gateway", EnvName: "EXAMPLE_API_KEY",
			Broker: "example-cli tenant exec --tenant example-tenant --"},
		{Ref: "env-file://~/example/.env#EXAMPLE_API_KEY", Custodian: "operator", EnvName: "EXAMPLE_API_KEY"},
		{Ref: "home-secret://lane/agent.env", Custodian: "compound", EnvName: "LANE_AGENT_TOKEN"},
	}
	for _, h := range ok {
		if err := ValidateCredentialHandle(h); err != nil {
			t.Fatalf("REFUSED a legitimate pointer %+v: %v", h, err)
		}
	}
}

func TestValueTransitIsAnswerablePerHandle(t *testing.T) {
	// An operator auditing exposure needs "does the secret pass through a
	// process Compound started" answerable per surface. It is a property of the
	// mechanism, so it cannot drift from how injection actually works.
	selectorTransits, known := CredentialValueTransits(CredentialHandle{Ref: "selector://example-profile"})
	if !known {
		t.Fatal("selector kind reported unknown")
	}
	if selectorTransits {
		t.Fatal("selector kind claims the value transits Compound; it sets a selector and the tool reads its own credential")
	}
	brokeredTransits, known := CredentialValueTransits(CredentialHandle{Ref: "brokered://example-tenant"})
	if !known || !brokeredTransits {
		t.Fatalf("brokered kind must report a transiting value, got transits=%v known=%v", brokeredTransits, known)
	}
	if _, known := CredentialValueTransits(CredentialHandle{}); known {
		t.Fatal("an empty handle reported a known transit answer")
	}
}

func TestOnlyOneKindPlacesCustodyInThisHome(t *testing.T) {
	// The design commitment is that this home custodies ONLY what nothing else
	// does. If a second kind ever reads from the home's own secrets root, the
	// extra store has arrived and this test is the alarm.
	var homeOwned []string
	for name, kind := range CredentialKinds() {
		if strings.Contains(kind.Detail, "COMPOUND_HOME/secrets") {
			homeOwned = append(homeOwned, name)
		}
	}
	if len(homeOwned) != 1 || homeOwned[0] != "home-secret" {
		t.Fatalf("kinds reading this home's secrets root = %v, want exactly [home-secret]; "+
			"a second one means the home quietly became another credential store", homeOwned)
	}
}

func TestConnectionSurfaceWriteDoorRefusesACredentialValue(t *testing.T) {
	// Validating the type is worth nothing if the door that persists it does not
	// call the validator. This log is append-only and feeds orient, so the door
	// is the last place a value can be stopped.
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, err = store.AddConnectionSurface(ConnectionSurface{
		NodeID: "gateway:example",
		CredentialHandle: CredentialHandle{
			Ref:       "home-secret://" + "sk-" + "proj-" + "abcdefghijklmnopqrstuvwxyz012345",
			Custodian: "compound", EnvName: "EXAMPLE_API_KEY",
		},
	})
	if err == nil {
		t.Fatal("write door PERSISTED a credential value; the validator is not wired into AddConnectionSurface")
	}

	// And the door must still accept a legitimate pointer, or the guard has
	// simply broken the feature.
	if _, err := store.AddConnectionSurface(ConnectionSurface{
		NodeID: "gateway:example",
		CredentialHandle: CredentialHandle{
			Ref: "brokered://example-tenant", Custodian: "gateway", EnvName: "EXAMPLE_API_KEY",
		},
	}); err != nil {
		t.Fatalf("write door REFUSED a legitimate pointer: %v", err)
	}
}
