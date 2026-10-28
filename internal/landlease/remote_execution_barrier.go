package landlease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RemoteExecutionRequest identifies immutable work-owned request bytes.
// Landlease owns only exclusion; the work owner validates policy, transport,
// remote execution and the terminal receipt.
type RemoteExecutionRequest struct {
	ID     string `json:"request_id"`
	Digest string `json:"request_digest"`
}

func (r RemoteExecutionRequest) validate() error {
	if len(r.ID) == 0 || len(r.ID) > 128 || !proofIdentityDigest(r.Digest) {
		return fmt.Errorf("exact remote request identity and SHA-256 digest required")
	}
	for _, c := range r.ID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return fmt.Errorf("invalid remote request identity")
		}
	}
	return nil
}

const remoteFenceSchema = "compound.remote_execution_fence.v0.1"

type remoteExecutionFence struct {
	Coordination  ProofCoordinationIdentity `json:"proof_coordination"`
	SchemaVersion string                    `json:"schema_version"`
	Holder        string                    `json:"holder"`
	Owner         string                    `json:"proof_holder"`
	Request       RemoteExecutionRequest    `json:"request"`
	State         string                    `json:"state"`
	ReceiptDigest string                    `json:"terminal_receipt_digest,omitempty"`
}

func remoteFenceDir(root, holder string) string {
	return filepath.Join(root, executionBarrierRelPath, executionBarrierID(holder)+".remote")
}
func remoteFenceName(requestID string) string {
	digest := sha256.Sum256([]byte(requestID))
	return hex.EncodeToString(digest[:]) + ".json"
}
func readRemoteFence(path string) (remoteExecutionFence, bool, error) {
	var out remoteExecutionFence
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return out, false, fmt.Errorf("invalid remote fence file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return out, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return out, false, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return out, false, fmt.Errorf("remote fence trailing JSON")
	}
	if out.SchemaVersion != remoteFenceSchema || strings.TrimSpace(out.Holder) == "" || strings.TrimSpace(out.Owner) == "" {
		return out, false, fmt.Errorf("invalid remote fence identity")
	}
	if err := out.Coordination.Validate(); err != nil {
		return out, false, err
	}
	if out.Holder != out.Owner && out.Holder != proofRepositoryHolder(&out.Coordination) {
		return out, false, fmt.Errorf("remote fence barrier identity mismatch")
	}
	if err := out.Request.validate(); err != nil {
		return out, false, err
	}
	if filepath.Base(path) != remoteFenceName(out.Request.ID) || filepath.Base(filepath.Dir(path)) != executionBarrierID(out.Holder)+".remote" {
		return out, false, fmt.Errorf("remote fence path disagrees with identity")
	}
	if out.State != "pending" && out.State != "quiescent" || out.State == "pending" && out.ReceiptDigest != "" || out.State == "quiescent" && !proofIdentityDigest(out.ReceiptDigest) {
		return out, false, fmt.Errorf("invalid remote fence disposition")
	}
	return out, true, nil
}

func remoteFenceDirectoryPending(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("invalid remote fence directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	pending := false
	for _, entry := range entries {
		fence, exists, err := readRemoteFence(filepath.Join(dir, entry.Name()))
		if err != nil {
			return false, err
		}
		if !exists {
			return false, fmt.Errorf("remote fence disappeared during observation")
		}
		pending = pending || fence.State == "pending"
	}
	return pending, nil
}

func remoteExecutionPending(root, holder string) (bool, error) {
	return remoteFenceDirectoryPending(remoteFenceDir(root, holder))
}

func ensureNoUnresolvedRemoteFences(root string) error {
	entries, err := os.ReadDir(filepath.Join(root, executionBarrierRelPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".remote") {
			continue
		}
		pending, err := remoteFenceDirectoryPending(filepath.Join(root, executionBarrierRelPath, entry.Name()))
		if err != nil {
			return err
		}
		if pending {
			return fmt.Errorf("proof domain has unresolved remote execution")
		}
	}
	return nil
}

type remoteFenceLocation struct{ root, holder string }

func (r *Resource) withRemoteFenceLocks(holder string, request RemoteExecutionRequest, historical bool, fn func([]remoteFenceLocation) error) error {
	if err := request.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(holder) == "" {
		return fmt.Errorf("exact proof holder required")
	}
	_, err := selectedCall(r, func(root string) (struct{}, error) {
		if r.proofIdentity == nil {
			return struct{}{}, fmt.Errorf("remote execution requires coordinated proof identity")
		}
		manifest, _, exists, err := proofDomainForHome(r.home)
		if err != nil {
			return struct{}{}, err
		}
		if !exists {
			return struct{}{}, fmt.Errorf("remote execution requires a proof domain")
		}
		if err := requireFencedProofDomain(manifest); err != nil {
			return struct{}{}, err
		}
		binding := r.binding(root, false)
		if historical {
			binding.historicalHolder = holder
		}
		err = withLeaseBindingLock(root, binding, func() error {
			return withLock(binding.repositoryRoot, func() error {
				return fn([]remoteFenceLocation{{root, holder}, {binding.repositoryRoot, proofRepositoryHolder(r.proofIdentity)}})
			})
		})
		return struct{}{}, err
	})
	return err
}

func exactRemoteFence(location remoteFenceLocation, holder string, request RemoteExecutionRequest, identity *ProofCoordinationIdentity) (remoteExecutionFence, bool, string, error) {
	path := filepath.Join(remoteFenceDir(location.root, location.holder), remoteFenceName(request.ID))
	fence, exists, err := readRemoteFence(path)
	if err != nil {
		return fence, exists, path, err
	}
	if exists && (fence.Holder != location.holder || fence.Owner != holder || fence.Request != request || !sameProofCoordination(&fence.Coordination, identity)) {
		return fence, exists, path, fmt.Errorf("remote request identity/digest conflicts with retained fence")
	}
	return fence, exists, path, nil
}

// PrepareRemoteExecution must finish before ANY transport side effect. Both the
// exact slot-holder and repository fence are durable before success. A partial
// preparation stays blocking and is reconciled using the same request. The
// caller validates current governed request authority while locks are held.
func (r *Resource) PrepareRemoteExecution(holder string, request RemoteExecutionRequest, validate func() error) error {
	return r.prepareRemoteExecution(holder, request, validate, nil)
}

func (r *Resource) prepareRemoteExecution(holder string, request RemoteExecutionRequest, validate, fault func() error) error {
	if validate == nil {
		return fmt.Errorf("remote request authority validator required")
	}
	return r.withRemoteFenceLocks(holder, request, false, func(locations []remoteFenceLocation) error {
		path, err := statePath(locations[0].root)
		if err != nil {
			return err
		}
		lease, exists, err := readLease(path)
		if err != nil {
			return err
		}
		if !exists || lease.Holder != holder || !lease.live(time.Now()) {
			return fmt.Errorf("remote preparation requires exact current proof holder")
		}
		for _, location := range locations {
			fence, exists, _, err := exactRemoteFence(location, holder, request, r.proofIdentity)
			if err != nil {
				return err
			}
			if exists && fence.State == "quiescent" {
				return fmt.Errorf("terminal remote request cannot be started again")
			}
			live, err := localExecutionBarrierPending(location.root, location.holder)
			if err != nil {
				return err
			}
			if !live {
				return fmt.Errorf("remote preparation requires both live local execution barriers")
			}
		}
		if err := validate(); err != nil {
			return err
		}
		// Repository first: even a crash before the slot marker fences other slots.
		for i := len(locations) - 1; i >= 0; i-- {
			location := locations[i]
			fence, exists, path, err := exactRemoteFence(location, holder, request, r.proofIdentity)
			if err != nil {
				return err
			}
			if !exists {
				fence = remoteExecutionFence{Coordination: *r.proofIdentity, SchemaVersion: remoteFenceSchema, Holder: location.holder, Owner: holder, Request: request, State: "pending"}
				if err := writeProofDomainJSON(path, fence); err != nil {
					return err
				}
			}
			if i == 1 && fault != nil {
				if err := fault(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ResolveRemoteExecution clears only this exact request after the work owner
// authenticates its content-addressed terminal receipt, including positive
// remote quiescence OR a durable cancel-before-start tombstone. Local exit,
// a missing remote request and caller prose are not termination evidence.
// Receipt validation runs under the same locks as both marker transitions.
func (r *Resource) ResolveRemoteExecution(holder string, request RemoteExecutionRequest, receiptDigest string, validate func() error) error {
	return r.resolveRemoteExecution(holder, request, receiptDigest, validate, nil)
}

func (r *Resource) resolveRemoteExecution(holder string, request RemoteExecutionRequest, receiptDigest string, validate, fault func() error) error {
	if validate == nil || !proofIdentityDigest(receiptDigest) {
		return fmt.Errorf("exact terminal receipt digest and validator required")
	}
	return r.withRemoteFenceLocks(holder, request, true, func(locations []remoteFenceLocation) error {
		found := false
		for _, location := range locations {
			fence, exists, _, err := exactRemoteFence(location, holder, request, r.proofIdentity)
			if err != nil {
				return err
			}
			found = found || exists
			if exists && fence.State == "quiescent" && fence.ReceiptDigest != receiptDigest {
				return fmt.Errorf("terminal remote receipt conflicts with retained reconciliation")
			}
		}
		if !found {
			return fmt.Errorf("remote request has no prepared fence")
		}
		if err := validate(); err != nil {
			return err
		}
		// Slot first: repository remains blocking across an interrupted resolution.
		for i, location := range locations {
			fence, exists, path, err := exactRemoteFence(location, holder, request, r.proofIdentity)
			if err != nil {
				return err
			}
			if !exists || fence.State != "quiescent" {
				fence = remoteExecutionFence{Coordination: *r.proofIdentity, SchemaVersion: remoteFenceSchema, Holder: location.holder, Owner: holder, Request: request, State: "quiescent", ReceiptDigest: receiptDigest}
				if err := writeProofDomainJSON(path, fence); err != nil {
					return err
				}
			}
			if i == 0 && fault != nil {
				if err := fault(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
