package landlease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
)

type ProofDomainStatus struct {
	SchemaVersion string   `json:"schema_version,omitempty"`
	Generation    string   `json:"generation"`
	Phase         string   `json:"phase"`
	Homes         []string `json:"homes"`
}
type proofLegacyFence struct {
	SchemaVersion string `json:"schema_version"`
	Holder        struct {
		Layout     string `json:"layout"`
		Generation string `json:"generation"`
	} `json:"holder"`
}

func ProofDomainMigrationGeneration(backing string, homes []string) string {
	body, _ := json.Marshal(struct {
		Backing string
		Homes   []string
	}{backing, homes})
	sum := sha256.Sum256(append([]byte("compound.proof_domain.v0.2:"), body...))
	return hex.EncodeToString(sum[:])
}

func ReadProofDomainStatus(backing string) (ProofDomainStatus, error) {
	manifest, exists, err := readProofDomain(backing)
	if err != nil {
		return ProofDomainStatus{}, err
	}
	if !exists {
		path, _ := statePath(scopedRoot(backing, ProofResourceKey()))
		body, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return ProofDomainStatus{}, nil
		}
		if err != nil {
			return ProofDomainStatus{}, err
		}
		var header struct {
			SchemaVersion string `json:"schema_version"`
		}
		if err := json.Unmarshal(body, &header); err != nil {
			return ProofDomainStatus{}, err
		}
		if header.SchemaVersion != proofDomainSchema {
			return ProofDomainStatus{}, nil
		}
		var fence proofLegacyFence
		if err := json.Unmarshal(body, &fence); err != nil {
			return ProofDomainStatus{}, err
		}
		if !proofIdentityDigest(fence.Holder.Generation) {
			return ProofDomainStatus{}, fmt.Errorf("invalid proof fence generation")
		}
		if err := validateProofFence(backing, fence.Holder.Generation); err != nil {
			return ProofDomainStatus{}, err
		}
		return ProofDomainStatus{Generation: fence.Holder.Generation, Phase: "fenced"}, nil
	}
	return ProofDomainStatus{SchemaVersion: manifest.SchemaVersion, Generation: manifest.Generation, Phase: manifest.Phase, Homes: manifest.Homes}, nil
}

func proofFence(backing string, generation string) proofLegacyFence {
	fence := proofLegacyFence{SchemaVersion: proofDomainSchema}
	fence.Holder.Layout = "proof_classes"
	fence.Holder.Generation = generation
	return fence
}
func validateProofFence(backing, generation string) error {
	path, err := statePath(scopedRoot(backing, ProofResourceKey()))
	if err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("proof domain requires permanent historical endpoint fence: %w", err)
	}
	expected, _ := json.Marshal(proofFence(backing, generation))
	var got, want any
	if json.Unmarshal(body, &got) != nil || json.Unmarshal(expected, &want) != nil || !reflect.DeepEqual(got, want) {
		return fmt.Errorf("historical proof fence does not match domain")
	}
	return nil
}

func writeProofDomainJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".proof-domain-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

// MigrateProofDomain is a one-way quiescent cutover. The Work owner must
// authenticate a current all-writer pause and census every enrolled home.
// The shared historical endpoint is fenced, including omitted scoped homes.
// A never-enrolled independent legacy home is outside this backing domain.
func MigrateProofDomain(backing string, homes []string, validate func() error, fault func(string) error, target ...ProofDomainRevision) error {
	revision := ProofDomainLocalV2
	if len(target) > 1 {
		return fmt.Errorf("at most one proof domain target revision required")
	}
	if len(target) == 1 {
		revision = target[0]
	}
	if revision != ProofDomainLocalV2 && revision != ProofDomainFencedV3 {
		return fmt.Errorf("unknown proof domain target revision %q", revision)
	}
	if validate == nil || len(homes) == 0 || !sort.StringsAreSorted(homes) {
		return fmt.Errorf("sorted enrolled homes and owner validation required")
	}
	canonical, err := filepath.EvalSymlinks(backing)
	if err != nil || canonical != backing || !filepath.IsAbs(backing) {
		return fmt.Errorf("canonical existing backing root required")
	}
	prior := ""
	for _, h := range homes {
		canonical, err := filepath.EvalSymlinks(h)
		if err != nil || canonical != h || !filepath.IsAbs(h) || h == prior {
			return fmt.Errorf("canonical distinct enrolled homes required")
		}
		prior = h
	}
	generation := ProofDomainMigrationGeneration(backing, homes)
	queue, err := newProofQueue(backing, generation)
	if err != nil {
		return err
	}
	// Fixed cutover lock prevents competing generations. The queue domain lock
	// precedes home-layout and historical-class locks, matching admission order.
	return withLock(filepath.Join(backing, "landlease", "proof", "cutover"), func() error {
		return withLock(queue.root, func() error {
			manifest, exists, err := readProofDomain(backing)
			if err != nil {
				return err
			}
			if exists && (manifest.Generation != generation || !reflect.DeepEqual(manifest.Homes, homes)) {
				return fmt.Errorf("proof domain already bound to different enrollment")
			}
			if exists && manifest.SchemaVersion == proofDomainFencedSchema && revision != ProofDomainFencedV3 {
				return fmt.Errorf("proof domain revision cannot be downgraded")
			}
			if exists && manifest.Phase == "active" && manifest.SchemaVersion == string(revision) {
				return validateProofFence(backing, generation)
			}
			for _, h := range homes {
				layout, err := LayoutStatus(h)
				if err != nil {
					return err
				}
				if layout.Mode != "scoped" || layout.BackingRoot != backing {
					return fmt.Errorf("participating home is not bound to exact shared backing")
				}
				if err := ensureNoLiveLegacyBarriers(h); err != nil {
					return err
				}
			}
			oldRoot := scopedRoot(backing, ProofResourceKey())
			return withProofDomainQuiescence(backing, generation, func() error {
				return withLock(oldRoot, func() error {
					oldPath, err := statePath(oldRoot)
					if err != nil {
						return err
					}
					body, err := os.ReadFile(oldPath)
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
					if err == nil {
						if validateProofFence(backing, generation) != nil {
							return fmt.Errorf("historical proof record must be released before cutover, including expired records")
						}
						var old Lease
						if json.NewDecoder(bytes.NewReader(body)).Decode(&old) == nil {
							return fmt.Errorf("historical proof endpoint is not fenced")
						}
					}
					if err := ensureNoLiveLegacyBarriers(oldRoot); err != nil {
						return err
					}
					if err := validate(); err != nil {
						return err
					}
					// Fence the shared old endpoint first. If publication is interrupted
					// before the manifest, both old and upgraded clients fail closed there.
					if err := writeProofDomainJSON(oldPath, proofFence(backing, generation)); err != nil {
						return err
					}
					if fault != nil {
						if err := fault("proof-fenced"); err != nil {
							return err
						}
					}
					manifest = proofDomainManifest{SchemaVersion: string(revision), Generation: generation, Phase: "prepared", Homes: append([]string(nil), homes...)}
					if err := writeProofDomainJSON(proofDomainManifestPath(backing), manifest); err != nil {
						return err
					}
					if fault != nil {
						if err := fault("proof-prepared"); err != nil {
							return err
						}
					}
					// Recheck the pause immediately before activation; preparation is resumable.
					if err := validate(); err != nil {
						return err
					}
					manifest.Phase = "active"
					return writeProofDomainJSON(proofDomainManifestPath(backing), manifest)
				})
			})
		})
	})
}

// withProofDomainQuiescence shares the queue -> slot -> repository lock order.
// Even expired records require their original owner's exact recovery before a
// revision cutover. A callback cannot attest away a still-observed resource.
func withProofDomainQuiescence(backing, generation string, fn func() error) error {
	var roots []string
	for _, class := range []ProofCapacityClass{ProofCapacitySystemFull, ProofCapacityNativeConsumer} {
		for slot := 0; slot <= proofSlotMax; slot++ {
			roots = append(roots, proofSlotRoot(backing, generation, class, slot))
		}
	}
	roots = append(roots, proofRepositoryRoot(backing, generation))
	var visit func(int) error
	visit = func(i int) error {
		if i == len(roots) {
			return fn()
		}
		root := roots[i]
		return withLock(root, func() error {
			path, err := statePath(root)
			if err != nil {
				return err
			}
			_, exists, err := readLease(path)
			if err != nil {
				return err
			}
			if exists {
				return fmt.Errorf("proof domain cutover requires released slot records, including expired records")
			}
			if err := ensureNoLiveLegacyBarriers(root); err != nil {
				return err
			}
			if err := ensureNoUnresolvedRemoteFences(root); err != nil {
				return err
			}
			return visit(i + 1)
		})
	}
	return visit(0)
}
