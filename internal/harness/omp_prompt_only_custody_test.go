package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	homepkg "compound/internal/home"
)

func TestValidateOMPNativeProofCustodyAcceptsCurrentOwnedProofReadOnly(t *testing.T) {
	fixture, artifacts, proof := newOMPNativeCustodyProofFixture(t)
	before := snapshotOMPNativeCustodyTree(t, fixture.root)

	if err := validateOMPNativeProofCustodyAtProjectionRoot(
		fixture.home,
		fixture.adapter.projectionRoot,
		proof,
	); err != nil {
		t.Fatalf("current producer proof rejected: %v", err)
	}

	after := snapshotOMPNativeCustodyTree(t, fixture.root)
	if before != after {
		t.Fatal("custody validation mutated the fixture tree")
	}

	missingHome := filepath.Join(t.TempDir(), "missing-home")
	if err := validateOMPNativeProofCustodyAtProjectionRoot(
		missingHome,
		fixture.adapter.projectionRoot,
		proof,
	); err != errOMPNativeProofCustody {
		t.Fatalf("missing home error = %v", err)
	}
	if _, err := os.Stat(missingHome); !os.IsNotExist(err) {
		t.Fatalf("read-only validator created missing home: %v", err)
	}

	_ = artifacts
}

func TestValidateOMPNativeProofCustodyRejectsDrift(t *testing.T) {
	fixture, artifacts, proof := newOMPNativeCustodyProofFixture(t)
	captureBody, err := artifacts.GetPrivate(
		homepkg.ArtifactRefPrefix + proof.PrivateCustodySHA256["capture"],
	)
	if err != nil {
		t.Fatal(err)
	}
	reportBody, err := artifacts.Get(proof.EvidenceRefs[1])
	if err != nil {
		t.Fatal(err)
	}

	t.Run("corrupt public report", func(t *testing.T) {
		corrupt := append(append([]byte(nil), reportBody...), 0xff)
		ref, putErr := artifacts.Put(corrupt)
		if putErr != nil {
			t.Fatal(putErr)
		}
		got := cloneOMPNativeCustodyProof(proof)
		sum := sha256.Sum256(corrupt)
		got.ReportSHA256 = hex.EncodeToString(sum[:])
		got.EvidenceRefs[1] = ref
		assertOMPNativeCustodyRejected(t, fixture, got, "corrupt-report-canary")
	})

	t.Run("foreign config semantics", func(t *testing.T) {
		envelope, decodeErr := decodeOMPPrivateEnvelope(captureBody)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(envelope.Config, &fields) != nil {
			t.Fatal("decode genuine config")
		}
		fields["route"] = json.RawMessage(`"foreign-route-canary"`)
		envelope.Config, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		foreignCapture, marshalErr := json.Marshal(envelope)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		ref, putErr := artifacts.PutPrivate(foreignCapture)
		if putErr != nil {
			t.Fatal(putErr)
		}
		got := cloneOMPNativeCustodyProof(proof)
		got.PrivateCustodySHA256["capture"] = strings.TrimPrefix(ref, homepkg.ArtifactRefPrefix)
		assertOMPNativeCustodyRejected(t, fixture, got, "foreign-route-canary")
	})

	t.Run("current owner projection drift", func(t *testing.T) {
		projection, loadErr := loadOMPOwnerProjectionFrom(
			fixture.adapter.projectionRoot,
			fixture.adapter.profileID,
		)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		projection.Profile.Route = "current-owner-canary"
		body, marshalErr := json.Marshal(projection)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		root := t.TempDir()
		if writeErr := os.WriteFile(
			filepath.Join(root, fixture.adapter.profileID+".json"),
			body,
			0o600,
		); writeErr != nil {
			t.Fatal(writeErr)
		}
		gotErr := validateOMPNativeProofCustodyAtProjectionRoot(fixture.home, root, proof)
		if gotErr != errOMPNativeProofCustody ||
			strings.Contains(gotErr.Error(), "current-owner-canary") {
			t.Fatalf("current owner drift error = %v", gotErr)
		}
	})

	t.Run("private raw binding drift", func(t *testing.T) {
		raw, getErr := artifacts.GetPrivate(
			homepkg.ArtifactRefPrefix + proof.PrivateCustodySHA256["executable"],
		)
		if getErr != nil {
			t.Fatal(getErr)
		}
		ref, putErr := artifacts.PutPrivate(append(raw, []byte("raw-binding-canary")...))
		if putErr != nil {
			t.Fatal(putErr)
		}
		got := cloneOMPNativeCustodyProof(proof)
		got.PrivateCustodySHA256["executable"] = strings.TrimPrefix(ref, homepkg.ArtifactRefPrefix)
		assertOMPNativeCustodyRejected(t, fixture, got, "raw-binding-canary")
	})

	t.Run("recipe drift", func(t *testing.T) {
		ref, putErr := artifacts.Put([]byte(ompNativeVerifierRecipe + "\nrecipe-drift-canary"))
		if putErr != nil {
			t.Fatal(putErr)
		}
		got := cloneOMPNativeCustodyProof(proof)
		got.EvidenceRefs[0] = ref
		assertOMPNativeCustodyRejected(t, fixture, got, "recipe-drift-canary")
	})
}

func TestDecodeOMPNativeCustodyConfigIsExactAndContentFree(t *testing.T) {
	_, artifacts, proof := newOMPNativeCustodyProofFixture(t)
	captureBody, err := artifacts.GetPrivate(
		homepkg.ArtifactRefPrefix + proof.PrivateCustodySHA256["capture"],
	)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := decodeOMPPrivateEnvelope(captureBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeOMPNativeCustodyConfig(envelope.Config); err != nil {
		t.Fatalf("genuine config rejected: %v", err)
	}

	var base map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Config, &base); err != nil {
		t.Fatal(err)
	}
	marshalMutation := func(mutate func(map[string]json.RawMessage)) []byte {
		fields := make(map[string]json.RawMessage, len(base)+1)
		for key, value := range base {
			fields[key] = append(json.RawMessage(nil), value...)
		}
		mutate(fields)
		body, marshalErr := json.Marshal(fields)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return body
	}
	unknown := marshalMutation(func(fields map[string]json.RawMessage) {
		fields["unknown_config_canary"] = json.RawMessage(`"value"`)
	})
	missing := marshalMutation(func(fields map[string]json.RawMessage) {
		delete(fields, "route")
	})
	caseVariant := marshalMutation(func(fields map[string]json.RawMessage) {
		fields["Route"] = fields["route"]
		delete(fields, "route")
	})
	nestedUnknown := marshalMutation(func(fields map[string]json.RawMessage) {
		var binding map[string]json.RawMessage
		if json.Unmarshal(fields["interpreter"], &binding) != nil {
			t.Fatal("decode genuine interpreter binding")
		}
		binding["unknown_binding_canary"] = json.RawMessage(`"value"`)
		fields["interpreter"], _ = json.Marshal(binding)
	})
	duplicateAlias := append(
		[]byte(`{"\\u0072oute":"duplicate-alias-canary",`),
		bytes.TrimPrefix(envelope.Config, []byte("{"))...,
	)
	nestedDuplicateAlias := bytes.Replace(
		envelope.Config,
		[]byte(`"interpreter":{`),
		[]byte(`"interpreter":{"\\u0070ath":"duplicate-binding-canary",`),
		1,
	)

	for name, body := range map[string][]byte{
		"unknown":                unknown,
		"missing":                missing,
		"case variant":           caseVariant,
		"duplicate alias":        duplicateAlias,
		"nested unknown":         nestedUnknown,
		"nested duplicate alias": nestedDuplicateAlias,
	} {
		t.Run(name, func(t *testing.T) {
			_, gotErr := decodeOMPNativeCustodyConfig(body)
			if gotErr != errOMPNativeProofCustody {
				t.Fatalf("error = %v", gotErr)
			}
			if strings.Contains(gotErr.Error(), "canary") {
				t.Fatalf("private input escaped error: %v", gotErr)
			}
		})
	}
}

func newOMPNativeCustodyProofFixture(
	t *testing.T,
) (ompVerifierFixture, *homepkg.ArtifactStore, OMPNativePublicProof) {
	t.Helper()
	fixture := newOMPVerifierFixture(t, "")
	canonicalHome, canonicalErr := filepath.EvalSymlinks(fixture.home)
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	fixture.home = canonicalHome
	out, err := fixture.adapter.Run(context.Background(), fixture.input)
	if err != nil {
		t.Fatalf("produce genuine OMP proof: %v", err)
	}
	artifacts, err := homepkg.NewArtifactStore(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	proofBody, err := artifacts.Get(out.RunAttempt.ProofRefs[len(out.RunAttempt.ProofRefs)-1])
	if err != nil {
		t.Fatal(err)
	}
	proof, err := DecodeOMPNativePublicProof(proofBody)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, artifacts, proof
}

func cloneOMPNativeCustodyProof(proof OMPNativePublicProof) OMPNativePublicProof {
	cloned := proof
	cloned.EvidenceRefs = append([]string(nil), proof.EvidenceRefs...)
	cloned.PrivateCustodySHA256 = make(map[string]string, len(proof.PrivateCustodySHA256))
	for role, digest := range proof.PrivateCustodySHA256 {
		cloned.PrivateCustodySHA256[role] = digest
	}
	return cloned
}

func assertOMPNativeCustodyRejected(
	t *testing.T,
	fixture ompVerifierFixture,
	proof OMPNativePublicProof,
	canary string,
) {
	t.Helper()
	err := validateOMPNativeProofCustodyAtProjectionRoot(
		fixture.home,
		fixture.adapter.projectionRoot,
		proof,
	)
	if err != errOMPNativeProofCustody {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("private input escaped error: %v", err)
	}
}

func snapshotOMPNativeCustodyTree(t *testing.T, root string) string {
	t.Helper()
	var rows []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		row := fmt.Sprintf("%s|%s|%d", relative, info.Mode(), info.Size())
		switch {
		case info.Mode().IsRegular():
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(body)
			row += "|" + hex.EncodeToString(sum[:])
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			row += "|" + target
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

func TestValidateOMPNativeProofCustodyReturnsExactCurrentOwner(t *testing.T) {
	fixture, _, proof := newOMPNativeCustodyProofFixture(t)
	got, err := validateOMPNativeProofCustodyAtRoot(fixture.home, fixture.adapter.projectionRoot, proof)
	if err != nil {
		t.Fatal(err)
	}
	want, err := loadOMPOwnerProjectionFrom(fixture.adapter.projectionRoot, fixture.adapter.profileID)
	if err != nil {
		t.Fatal(err)
	}
	gotBody, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantBody, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatal("custody returned an unbound owner projection")
	}
}
