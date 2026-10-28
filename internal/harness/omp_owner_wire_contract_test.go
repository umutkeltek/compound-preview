package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Mirror Switchback's wire representation, not Compound's normalized struct:
// requested_model is an alias; request_model is the canonical request identity.
func prefixOMPOwnerWireDigests(p *OMPOwnerProjection) {
	for _, digest := range []*string{
		&p.PromptOnly.Wrapper.SHA256, &p.PromptOnly.Wrapper.ExpectedSHA256,
		&p.PromptOnly.ModelsConfig.SHA256, &p.PromptOnly.ModelsConfig.ExpectedSHA256,
		&p.PromptOnly.Executable.SHA256,
	} {
		*digest = "sha256:" + *digest
	}
}

func writeOMPOwnerWireFixture(t *testing.T, root string, p OMPOwnerProjection) {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, p.Profile.ID+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOMPOwnerWireContract(t *testing.T) {
	for _, tc := range []struct {
		name            string
		alias, prefixed bool
	}{
		{"legacy_bare", false, false},
		{"distinct_model_alias", true, false},
		{"prefixed_digests", false, true},
		{"actual_owner_shape", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := validOMPOwnerProjection(t)
			if tc.alias {
				p.Profile.RequestedModel = "omp-model"
			}
			want := p
			if tc.prefixed {
				prefixOMPOwnerWireDigests(&p)
			}
			root := t.TempDir()
			writeOMPOwnerWireFixture(t, root, p)
			got, err := loadOMPOwnerProjectionFrom(root, p.Profile.ID)
			if err != nil {
				t.Fatalf("owner wire contract rejected: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("normalization changed meaning or left prefixed binding hashes")
			}
		})
	}
}

func TestOMPOwnerWireRejectsMalformedBindings(t *testing.T) {
	for name, mutate := range map[string]func(*OMPOwnerProjection){
		"uppercase_algorithm": func(p *OMPOwnerProjection) { p.PromptOnly.Executable.SHA256 = "SHA256:" + strings.Repeat("a", 64) },
		"uppercase_hex":       func(p *OMPOwnerProjection) { p.PromptOnly.Executable.SHA256 = "sha256:" + strings.Repeat("A", 64) },
		"repeated_prefix": func(p *OMPOwnerProjection) {
			p.PromptOnly.Executable.SHA256 = "sha256:" + p.PromptOnly.Executable.SHA256
		},
		"wrong_algorithm":  func(p *OMPOwnerProjection) { p.PromptOnly.Executable.SHA256 = "sha512:" + strings.Repeat("a", 64) },
		"short_digest":     func(p *OMPOwnerProjection) { p.PromptOnly.Executable.SHA256 = "sha256:a" },
		"whitespace":       func(p *OMPOwnerProjection) { p.PromptOnly.Executable.SHA256 += " " },
		"wrapper_mismatch": func(p *OMPOwnerProjection) { p.PromptOnly.Wrapper.ExpectedSHA256 = "sha256:" + strings.Repeat("b", 64) },
		"models_mismatch": func(p *OMPOwnerProjection) {
			p.PromptOnly.ModelsConfig.ExpectedSHA256 = "sha256:" + strings.Repeat("b", 64)
		},
		"alias_used_as_request": func(p *OMPOwnerProjection) {
			p.PromptOnly.FixedArgv[2] = "--model=switchback/" + p.Profile.RequestedModel
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := validOMPOwnerProjection(t)
			p.Profile.RequestedModel = "omp-model"
			prefixOMPOwnerWireDigests(&p)
			mutate(&p)
			root := t.TempDir()
			writeOMPOwnerWireFixture(t, root, p)
			if _, err := loadOMPOwnerProjectionFrom(root, p.Profile.ID); err == nil {
				t.Fatal("malformed owner binding accepted")
			}
		})
	}
}

func TestOMPOwnerWireAdaptersPreflight(t *testing.T) {
	t.Run("native_verifier", func(t *testing.T) {
		f := newOMPVerifierFixture(t, "")
		p, err := loadOMPOwnerProjectionFrom(f.adapter.projectionRoot, f.adapter.profileID)
		if err != nil {
			t.Fatal(err)
		}
		p.Profile.RequestedModel = "omp-model"
		prefixOMPOwnerWireDigests(&p)
		writeOMPOwnerWireFixture(t, f.adapter.projectionRoot, p)
		prepared, err := f.adapter.Preflight(context.Background(), f.input)
		if err != nil {
			t.Fatalf("real owner shape rejected before native launch: %v", err)
		}
		if err := f.adapter.revalidateLaunch(prepared.ompNativeVerifier); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.wrapper, []byte("drift"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := f.adapter.revalidateLaunch(prepared.ompNativeVerifier); err == nil {
			t.Fatal("normalization masked actual wrapper byte drift")
		}
	})
	t.Run("prompt_only_consumer", func(t *testing.T) {
		f := newOMPPromptOnlyFixture(t)
		p := f.projection
		p.Profile.RequestedModel = "omp-model"
		prefixOMPOwnerWireDigests(&p)
		writeOMPOwnerWireFixture(t, f.adapter.projectionRoot, p)
		prepared, err := f.adapter.Preflight(context.Background(), f.input)
		if err != nil {
			t.Fatalf("real owner shape rejected before consumer launch: %v", err)
		}
		if err := f.adapter.revalidateLaunch(prepared.ompPromptOnly); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.wrapperPath, []byte("drift"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := f.adapter.revalidateLaunch(prepared.ompPromptOnly); err == nil {
			t.Fatal("normalization masked actual wrapper byte drift")
		}
	})
}

func TestOMPOwnerWireCustodyUsesCanonicalModel(t *testing.T) {
	f, _, proof := newOMPNativeCustodyProofFixture(t)
	p, err := loadOMPOwnerProjectionFrom(f.adapter.projectionRoot, f.adapter.profileID)
	if err != nil {
		t.Fatal(err)
	}
	p.Profile.RequestedModel = "omp-model"
	prefixOMPOwnerWireDigests(&p)
	writeOMPOwnerWireFixture(t, f.adapter.projectionRoot, p)
	if err := validateOMPNativeProofCustodyAtProjectionRoot(f.home, f.adapter.projectionRoot, proof); err != nil {
		t.Fatalf("owner wire formatting and distinct alias rejected genuine custody: %v", err)
	}
	p.Profile.RequestModel = "test/other-model"
	p.PromptOnly.FixedArgv[2] = "--model=switchback/test/other-model"
	writeOMPOwnerWireFixture(t, f.adapter.projectionRoot, p)
	if err := validateOMPNativeProofCustodyAtProjectionRoot(f.home, f.adapter.projectionRoot, proof); err == nil {
		t.Fatal("changed canonical request model accepted by custody replay")
	}
}
