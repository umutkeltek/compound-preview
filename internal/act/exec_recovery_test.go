package act

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/finding"
	"compound/internal/policy"
)

func TestExecTransportLocalRecordFailureNeverReposts(t *testing.T) {
	homePath := t.TempDir()
	marker := filepath.Join(homePath, "invocations")
	script := filepath.Join(homePath, "post-transport")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'called\\n' >> \"$1\"\nprintf '{\"id\":\"synthetic-post\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTransportConfig(t, homePath, "fixture", []string{script, marker}, 30)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ai := execTransportIntent(t, "work_transport_record_failure", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
	original := recordEnvelopeFn
	t.Cleanup(func() { recordEnvelopeFn = original })
	recordEnvelopeFn = func(string, string, any) error { return errors.New("injected local record failure") }
	_, err = Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("reviewed package"), fixedNow())
	if err == nil {
		t.Fatal("record failure was hidden")
	}
	called, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(called) != "called\n" {
		t.Fatalf("local failure called outbound transport again: %q", called)
	}
	var uncertain interface{ MutationMayHaveOccurred() bool }
	if !errors.As(err, &uncertain) || !uncertain.MutationMayHaveOccurred() {
		t.Fatalf("unrecorded external effect is not typed uncertain: %v", err)
	}
	var recovery *RecoveryRequiredError
	if !errors.As(err, &recovery) || recovery.ActionIntentID != ai.ID || recovery.PostImageRef == "" {
		t.Fatalf("original action and response reference unavailable to caller: %v", err)
	}
	findings, findErr := finding.List(homePath, "act-recovery")
	if findErr != nil || len(findings) != 1 {
		t.Fatalf("missing durable recovery evidence: %v %v", findings, findErr)
	}
	if len(findings[0].EvidenceRefs) < 2 {
		t.Fatalf("successful external response missing from recovery evidence: %+v", findings[0])
	}
}

func TestExecTransportTruncatedResponseCannotCertifyPublication(t *testing.T) {
	homePath := t.TempDir()
	script := filepath.Join(homePath, "oversize-transport")
	// A complete JSON prefix followed by padding and trailing junk must not
	// become valid publication evidence merely because the buffer is bounded.
	response := `{"id":"synthetic-post","status":"published"}` + strings.Repeat(" ", maxExecTransportOutput) + "junk"
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '"+response+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTransportConfig(t, homePath, "fixture", []string{script}, 30)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ai := execTransportIntent(t, "work_transport_truncated", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
	_, err = Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("reviewed package"), fixedNow())
	var recovery *RecoveryRequiredError
	if !errors.As(err, &recovery) || !strings.Contains(err.Error(), "response exceeded") {
		t.Fatalf("truncated output accepted: %v", err)
	}
	if receiptCount(t, homePath) != 0 {
		t.Fatal("truncated output minted successful receipt")
	}
}

func TestExecTransportPartialResponseRequiresReconciliation(t *testing.T) {
	homePath := t.TempDir()
	script := filepath.Join(homePath, "uncertain-transport")
	response := `{"id":"synthetic-job","status":"processing"}`
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '"+response+"'\nprintf 'connection lost' >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTransportConfig(t, homePath, "fixture", []string{script}, 30)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ai := execTransportIntent(t, "work_transport_partial", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
	_, err = Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("reviewed package"), fixedNow())
	var uncertain interface{ MutationMayHaveOccurred() bool }
	if !errors.As(err, &uncertain) || !uncertain.MutationMayHaveOccurred() {
		t.Fatalf("started transport treated as side-effect-free refusal: %v", err)
	}
	findings, findErr := finding.List(homePath, "act-recovery")
	if findErr != nil || len(findings) != 1 {
		t.Fatalf("missing uncertainty finding: %v %v", findings, findErr)
	}
	found := false
	for _, ref := range findings[0].EvidenceRefs {
		body, getErr := store.Get(ref)
		if getErr == nil && string(body) == response {
			found = true
		}
	}
	if !found {
		t.Fatal("partial stdout was discarded or mixed with stderr")
	}
	if receiptCount(t, homePath) != 0 {
		t.Fatal("uncertain transport claimed a successful receipt")
	}
}

func TestExecTransportStderrCannotContaminateObservation(t *testing.T) {
	homePath := t.TempDir()
	script := filepath.Join(homePath, "diagnostic-transport")
	response := `{"id":"synthetic-post","status":"published"}`
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'diagnostic only\\n' >&2\nprintf '%s' '"+response+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTransportConfig(t, homePath, "fixture", []string{script}, 30)
	store, err := NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	ai := execTransportIntent(t, "work_transport_stderr", "fixture")
	pd := policy.Evaluate(ai, execTransportGrantConfig(ai), fixedNow())
	result, err := Execute(store, NewExecTransportAdapter("fixture", homePath), ai, pd, []byte("reviewed package"), fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.PostImage) != response {
		t.Fatalf("stdout contract contaminated: %q", result.PostImage)
	}
}

func TestExecTransportStartFailureIsNotExternalUncertainty(t *testing.T) {
	homePath := t.TempDir()
	err := runExecTransport(t, homePath, []string{filepath.Join(homePath, "absent-transport")})
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("start failure: %v", err)
	}
	var uncertain interface{ MutationMayHaveOccurred() bool }
	if errors.As(err, &uncertain) && uncertain.MutationMayHaveOccurred() {
		t.Fatalf("pre-start failure called uncertain: %v", err)
	}
	findings, findErr := finding.List(homePath, "act-recovery")
	if findErr != nil || len(findings) != 0 {
		t.Fatalf("pre-start failure produced recovery finding: %v %v", findings, findErr)
	}
}
