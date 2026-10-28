package landlease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ResourceKey names one coordination domain, independently of a WorkEnvelope or
// checkout path. Canonical full proof remains one resource per backing root.
type ResourceKey struct {
	Kind                  string
	RepositoryFingerprint string
	TargetRef             string
}

const (
	ResourceProof   = "canonical_full_proof"
	ResourceLanding = "landing"
	layoutSchema    = "compound.land_lease_layout.v0.1"
)

func ProofResourceKey() ResourceKey { return ResourceKey{Kind: ResourceProof} }

func LandingResourceKey(fingerprint, targetRef string) (ResourceKey, error) {
	key := ResourceKey{Kind: ResourceLanding, RepositoryFingerprint: strings.TrimSpace(fingerprint), TargetRef: strings.TrimSpace(targetRef)}
	return key, key.validate()
}

func (k ResourceKey) validate() error {
	switch k.Kind {
	case ResourceProof:
		if k.RepositoryFingerprint != "" || k.TargetRef != "" {
			return fmt.Errorf("proof resource cannot carry repository or ref")
		}
	case ResourceLanding:
		if len(k.RepositoryFingerprint) != 64 {
			return fmt.Errorf("landing resource requires 64-character repository fingerprint")
		}
		for _, c := range k.RepositoryFingerprint {
			if c < '0' || c > '9' {
				if c < 'a' || c > 'f' {
					return fmt.Errorf("landing repository fingerprint must be lowercase hex")
				}
			}
		}
		if !strings.HasPrefix(k.TargetRef, "refs/heads/") || len(k.TargetRef) <= len("refs/heads/") ||
			strings.Contains(k.TargetRef, "..") || strings.ContainsAny(k.TargetRef, " \\~^:?*[]") ||
			strings.HasSuffix(k.TargetRef, "/") || strings.HasSuffix(k.TargetRef, ".") ||
			strings.Contains(k.TargetRef, "//") || strings.Contains(k.TargetRef, "@{") {
			return fmt.Errorf("landing target ref must be a canonical local branch ref")
		}
		for _, part := range strings.Split(strings.TrimPrefix(k.TargetRef, "refs/heads/"), "/") {
			if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
				return fmt.Errorf("landing target ref has invalid branch component")
			}
		}
		for _, c := range k.TargetRef {
			if c < 0x20 || c == 0x7f {
				return fmt.Errorf("landing target ref contains control character")
			}
		}
	default:
		return fmt.Errorf("unsupported landlease resource kind %q", k.Kind)
	}
	return nil
}

type layoutMarker struct {
	SchemaVersion string `json:"schema_version"`
	Holder        struct {
		Layout      string `json:"layout"`
		BackingRoot string `json:"backing_root"`
	} `json:"holder"`
}

// Resource is a bound lease handle. The caller supplies identity once; every
// operation rechecks the home's layout. No caller can redirect scoped storage.
type Resource struct {
	home           string
	key            ResourceKey
	initErr        error
	proofIdentity  *ProofCoordinationIdentity
	proofAdmission *proofAdmissionGuard
}

// ProofResource binds the one canonical full-proof resource without an error
// branch at every work call site. Invalid home still fails on every operation.
func ProofResource(home string) *Resource {
	r, err := OpenResource(home, ProofResourceKey())
	if err != nil {
		return &Resource{home: home, key: ProofResourceKey(), initErr: err}
	}
	return r
}

func OpenResource(home string, key ResourceKey) (*Resource, error) {
	if strings.TrimSpace(home) == "" {
		return nil, fmt.Errorf("COMPOUND_HOME required")
	}
	if err := key.validate(); err != nil {
		return nil, err
	}
	return &Resource{home: home, key: key}, nil
}

func strictLayout(data []byte) (layoutMarker, error) {
	var marker layoutMarker
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return marker, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return marker, fmt.Errorf("layout marker has trailing JSON")
	}
	if marker.SchemaVersion != layoutSchema || marker.Holder.Layout != "scoped" ||
		!filepath.IsAbs(marker.Holder.BackingRoot) ||
		filepath.Clean(marker.Holder.BackingRoot) != marker.Holder.BackingRoot {
		return marker, fmt.Errorf("invalid scoped landlease layout marker")
	}
	return marker, nil
}

// selectedRootLocked must be called under the legacy flock. An object-valued
// holder makes the marker unmarshalable by the old Lease reader, even though it
// is valid JSON. A merely new schema_version would be ignored by old binaries.
func selectedRootLocked(home string, key ResourceKey) (string, bool, error) {
	path, err := statePath(home)
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return home, false, nil
	}
	if err != nil {
		return "", false, err
	}
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return "", false, fmt.Errorf("parse land-lease state %s layout: %w", path, err)
	}
	if header.SchemaVersion == layoutSchema {
		marker, err := strictLayout(data)
		if err != nil {
			return "", false, err
		}
		return scopedRoot(marker.Holder.BackingRoot, key), true, nil
	}
	lease, exists, err := readLease(path)
	if err != nil {
		return "", false, err
	}
	if exists && lease.SchemaVersion != SchemaVersion {
		return "", false, fmt.Errorf("unsupported legacy landlease schema %q", lease.SchemaVersion)
	}
	return home, false, nil
}

// LayoutStatus reports the selected storage generation without creating state.
type LayoutStatusResult struct {
	Mode            string `json:"mode"`
	BackingRoot     string `json:"backing_root,omitempty"`
	ProofGeneration string `json:"proof_generation,omitempty"`
}

func LayoutStatus(home string) (LayoutStatusResult, error) {
	if strings.TrimSpace(home) == "" {
		return LayoutStatusResult{}, fmt.Errorf("COMPOUND_HOME required")
	}
	lockPath := filepath.Join(home, lockRelPath)
	file, err := os.Open(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		path, pathErr := statePath(home)
		if pathErr != nil {
			return LayoutStatusResult{}, pathErr
		}
		if _, statErr := os.Lstat(path); statErr == nil {
			return LayoutStatusResult{}, fmt.Errorf("landlease state exists without legacy lock")
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return LayoutStatusResult{}, statErr
		}
		return LayoutStatusResult{Mode: "legacy"}, nil
	}
	if err != nil {
		return LayoutStatusResult{}, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		return LayoutStatusResult{}, err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	path, err := statePath(home)
	if err != nil {
		return LayoutStatusResult{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return LayoutStatusResult{Mode: "legacy"}, nil
	}
	if err != nil {
		return LayoutStatusResult{}, err
	}
	var header struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return LayoutStatusResult{}, err
	}
	if header.SchemaVersion == layoutSchema {
		marker, err := strictLayout(data)
		if err != nil {
			return LayoutStatusResult{}, err
		}
		return LayoutStatusResult{Mode: "scoped", BackingRoot: marker.Holder.BackingRoot}, nil
	}
	lease, exists, err := readLease(path)
	if err != nil {
		return LayoutStatusResult{}, err
	}
	if exists && lease.SchemaVersion != SchemaVersion {
		return LayoutStatusResult{}, fmt.Errorf("unsupported legacy landlease schema %q", lease.SchemaVersion)
	}
	return LayoutStatusResult{Mode: "legacy"}, nil
}

func scopedRoot(backingRoot string, key ResourceKey) string {
	if key.Kind == ResourceProof {
		return filepath.Join(backingRoot, "landlease", "proof", "canonical-full")
	}
	sum := sha256.Sum256([]byte(key.TargetRef))
	return filepath.Join(backingRoot, "landlease", "landing", key.RepositoryFingerprint, hex.EncodeToString(sum[:]))
}

// selectedCall reads the one-way layout under the legacy flock, then releases
// it before entering a scoped resource transaction. Unrelated resources must
// not contend on a global lock while a holder finalizes. If a legacy selection
// races marker publication, the old lease decoder fails closed on the marker.
func selectedCall[T any](r *Resource, fn func(string) (T, error)) (T, error) {
	var zero T
	if r == nil {
		return zero, fmt.Errorf("landlease resource is nil")
	}
	if r.initErr != nil {
		return zero, r.initErr
	}
	var root string
	err := withLock(r.home, func() error {
		var selectErr error
		var scoped bool
		root, scoped, selectErr = selectedRootLocked(r.home, r.key)
		if selectErr == nil {
			root, selectErr = r.selectProofRoot(root, scoped)
		}
		return selectErr
	})
	if err != nil {
		return zero, err
	}
	return fn(root)
}

// selectedReadCall resolves the published layout without creating state.
func selectedReadCall[T any](r *Resource, fn func(string) (T, error)) (T, error) {
	var zero T
	if r == nil {
		return zero, fmt.Errorf("landlease resource is nil")
	}
	if r.initErr != nil {
		return zero, r.initErr
	}
	layout, err := LayoutStatus(r.home)
	if err != nil {
		return zero, err
	}
	if layout.Mode == "legacy" {
		root, err := r.selectProofRoot(r.home, false)
		if err != nil {
			return zero, err
		}
		return fn(root)
	}
	root, err := r.selectProofRoot(scopedRoot(layout.BackingRoot, r.key), true)
	if err != nil {
		return zero, err
	}
	return fn(root)
}

func (r *Resource) Status(now time.Time) (StatusResult, error) {
	return selectedCall(r, func(root string) (StatusResult, error) { return statusBound(root, now, r.binding(root, false)) })
}

func (r *Resource) StatusReadOnly(now time.Time) (StatusResult, error) {
	return selectedReadCall(r, func(root string) (StatusResult, error) { return statusReadOnlyBound(root, now, r.binding(root, false)) })
}

func (r *Resource) Acquire(holder string, ttl time.Duration, now time.Time) (Lease, error) {
	return selectedCall(r, func(root string) (Lease, error) {
		if err := r.rejectBarrierlessProof(); err != nil {
			return Lease{}, err
		}
		out, err := acquireWithProofIdentityPrepared(root, holder, ttl, now, nil, r.proofIdentity, r.binding(root, true))
		return out.Lease, err
	})
}

func (r *Resource) AcquireWithOutcomePrepared(holder string, ttl time.Duration, now time.Time, prepare func(AcquireOutcome) error) (AcquireOutcome, error) {
	return selectedCall(r, func(root string) (AcquireOutcome, error) {
		if err := r.rejectBarrierlessProof(); err != nil {
			return AcquireOutcome{}, err
		}
		return acquireWithProofIdentityPrepared(root, holder, ttl, now, prepare, r.proofIdentity, r.binding(root, true))
	})
}

type BarrierAcquire struct {
	Outcome AcquireOutcome
	Barrier *ExecutionBarrier
}

func (r *Resource) AcquireWithExecutionBarrierPrepared(holder string, ttl time.Duration, now time.Time, prepare func(AcquireOutcome) error) (AcquireOutcome, *ExecutionBarrier, error) {
	out, err := selectedCall(r, func(root string) (BarrierAcquire, error) {
		o, b, err := r.acquireProofBarrier(root, holder, ttl, now, prepare)
		return BarrierAcquire{Outcome: o, Barrier: b}, err
	})
	return out.Outcome, out.Barrier, err
}

func (r *Resource) Refresh(holder string, ttl time.Duration, now time.Time) (Lease, error) {
	return selectedCall(r, func(root string) (Lease, error) { return refreshBound(root, holder, ttl, now, r.binding(root, false)) })
}

func (r *Resource) RearmExpiredExact(expected StatusResult, now time.Time) (Lease, error) {
	return selectedCall(r, func(root string) (Lease, error) {
		return rearmExpiredExactBound(root, expected, now, r.binding(root, false))
	})
}

func (r *Resource) Release(holder string, now time.Time) (ReleaseResult, error) {
	return selectedCall(r, func(root string) (ReleaseResult, error) {
		return releaseBound(root, holder, now, r.binding(root, false))
	})
}

func (r *Resource) ReleaseAfter(holder string, now time.Time, finalize func() error) (ReleaseResult, error) {
	return selectedCall(r, func(root string) (ReleaseResult, error) {
		return releaseAfterBound(root, holder, now, finalize, r.binding(root, false))
	})
}

func (r *Resource) StealExpectedWithExecutionBarrierPrepared(newHolder, expectedPriorHolder, reason string, now time.Time, prepare func(Lease) error) (Lease, *ExecutionBarrier, error) {
	out, err := selectedCall(r, func(root string) (struct {
		Lease   Lease
		Barrier *ExecutionBarrier
	}, error) {
		l, b, err := r.stealProofBarrier(root, newHolder, expectedPriorHolder, reason, now, prepare)
		return struct {
			Lease   Lease
			Barrier *ExecutionBarrier
		}{l, b}, err
	})
	return out.Lease, out.Barrier, err
}

func (r *Resource) Steal(newHolder, reason string, now time.Time) (Lease, error) {
	return selectedCall(r, func(root string) (Lease, error) {
		if err := r.rejectBarrierlessProof(); err != nil {
			return Lease{}, err
		}
		return stealWithProofIdentityExpected(root, newHolder, "", reason, now, false, nil, nil, r.proofIdentity, r.binding(root, true))
	})
}

// StealExpected binds a governed takeover to the exact predecessor observed
// before approval; another holder cannot be silently substituted at execution.
func (r *Resource) StealExpected(newHolder, expectedPriorHolder, reason string, now time.Time) (Lease, error) {
	return selectedCall(r, func(root string) (Lease, error) {
		if err := r.rejectBarrierlessProof(); err != nil {
			return Lease{}, err
		}
		return stealWithProofIdentityExpected(root, newHolder, expectedPriorHolder, reason, now, true, nil, nil, r.proofIdentity, r.binding(root, true))
	})
}

// StealObserved binds an exceptional transfer to the durable lease snapshot
// that was reviewed. A release/reacquire by the same holder invalidates it.
func (r *Resource) StealObserved(newHolder string, expected StatusResult, reason string, now time.Time) (Lease, error) {
	if strings.TrimSpace(expected.Holder) == "" {
		return Lease{}, fmt.Errorf("land-lease observed holder required")
	}
	return selectedCall(r, func(root string) (Lease, error) {
		if err := r.rejectBarrierlessProof(); err != nil {
			return Lease{}, err
		}
		return stealWithProofIdentityExpected(root, newHolder, expected.Holder, reason, now, true, nil, &expected, r.proofIdentity, r.binding(root, true))
	})
}

func (r *Resource) AttestExecutionTerminated(holder string) (ExecutionTerminationAttestation, error) {
	return selectedCall(r, func(root string) (ExecutionTerminationAttestation, error) {
		return attestExecutionTerminatedBound(root, holder, r.binding(root, false))
	})
}

func (r *Resource) ExecutionPending(holder string) (bool, error) {
	return selectedCall(r, func(root string) (bool, error) { return executionPendingBound(root, holder, r.binding(root, false)) })
}

func (r *Resource) RequestExecutionTermination(expectedHolder, requestedBy, reason string, now time.Time) (Lease, error) {
	return selectedCall(r, func(root string) (Lease, error) {
		return requestExecutionTerminationBound(root, expectedHolder, requestedBy, reason, now, r.binding(root, false))
	})
}

func (r *Resource) WaitUntilFree(ctx context.Context, clk Clock, timeout, poll time.Duration) (StatusResult, error) {
	root, err := selectedReadCall(r, func(root string) (string, error) { return root, nil })
	if err != nil {
		return StatusResult{}, err
	}
	status, err := WaitUntilFree(ctx, root, clk, timeout, poll)
	if err != nil {
		return status, err
	}
	// A layout transition during the wait cannot grant a stale resource.
	current, err := r.StatusReadOnly(clk.Now())
	if err != nil {
		return StatusResult{}, err
	}
	if current.Holder != status.Holder || current.Held != status.Held {
		return StatusResult{}, fmt.Errorf("landlease layout or holder changed during wait")
	}
	return current, nil
}

func (r *Resource) AcquireWait(holder string, ttl time.Duration, clk Clock, timeout, poll time.Duration) (Lease, error) {
	if clk == nil {
		return Lease{}, fmt.Errorf("landlease acquire wait requires clock")
	}
	if poll <= 0 {
		poll = time.Second
	}
	deadline := clk.Now().Add(timeout)
	for {
		lease, err := r.Acquire(holder, ttl, clk.Now())
		if err == nil {
			return lease, nil
		}
		var held *HeldError
		if !errors.As(err, &held) || !clk.Now().Before(deadline) {
			return Lease{}, err
		}
		bound := poll
		if remain := deadline.Sub(clk.Now()); remain < bound {
			bound = remain
		}
		clk.Sleep(bound)
	}
}

// MigrateLegacyLayout is an explicit one-way cutover for one home. The owner
// validator must inspect pending proof completion and active legacy attempts.
// It runs under the old flock, before any marker is published.
func MigrateLegacyLayout(home, backingRoot string, validate func() error) error {
	if validate == nil {
		return fmt.Errorf("landlease migration requires owner validation")
	}
	if !filepath.IsAbs(backingRoot) {
		return fmt.Errorf("landlease backing root must be absolute")
	}
	return withLock(home, func() error {
		state, err := statePath(home)
		if err != nil {
			return err
		}
		if _, exists, err := readLease(state); err != nil {
			return fmt.Errorf("legacy landlease state must be readable and unmigrated: %w", err)
		} else if exists {
			return fmt.Errorf("legacy landlease record must be released before migration, including expired records")
		}
		if err := ensureNoLiveLegacyBarriers(home); err != nil {
			return err
		}
		if err := validate(); err != nil {
			return fmt.Errorf("landlease owner validation: %w", err)
		}
		if err := os.MkdirAll(backingRoot, 0o755); err != nil {
			return err
		}
		canonical, err := filepath.EvalSymlinks(backingRoot)
		if err != nil {
			return err
		}
		canonical, err = filepath.Abs(canonical)
		if err != nil {
			return err
		}
		for _, sub := range []string{"landlease/proof", "landlease/landing"} {
			dir := filepath.Join(canonical, sub)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := syncDirectory(dir); err != nil {
				return err
			}
		}
		marker := layoutMarker{SchemaVersion: layoutSchema}
		marker.Holder.Layout = "scoped"
		marker.Holder.BackingRoot = canonical
		return writeLayoutMarkerAtomic(state, marker)
	})
}

func ensureNoLiveLegacyBarriers(home string) error {
	dir := filepath.Join(home, executionBarrierRelPath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("legacy execution barrier %s is not regular", entry.Name())
		}
		file, err := os.OpenFile(filepath.Join(dir, entry.Name()), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return fmt.Errorf("legacy execution barrier %s still has live descendants", entry.Name())
		}
		if err == nil {
			err = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		}
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func writeLayoutMarkerAtomic(path string, marker layoutMarker) error {
	body, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if _, err := readLeaseFromBytesForMarkerTest(body); err == nil {
		return fmt.Errorf("layout marker must be unreadable by legacy Lease decoder")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "land-lease-layout-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func readLeaseFromBytesForMarkerTest(body []byte) (Lease, error) {
	var lease Lease
	err := json.Unmarshal(body, &lease)
	return lease, err
}
