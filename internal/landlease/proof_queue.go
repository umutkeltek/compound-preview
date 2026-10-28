package landlease

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const proofQueueSchema = "compound.proof_queue.v0.1"

// The queue owns ordering only. Existing class leases remain execution and
// recovery authority. Every operation shares this short admission flock.
type proofQueue struct {
	backing, root, generation string
	now                       func() time.Time
}

type proofQueueRecord struct {
	ID       string                    `json:"id"`
	Order    uint64                    `json:"order"`
	Deadline string                    `json:"deadline"`
	Identity ProofCoordinationIdentity `json:"identity"`
}

type proofQueueState struct {
	SchemaVersion string             `json:"schema_version"`
	Generation    string             `json:"generation"`
	Next          uint64             `json:"next"`
	Records       []proofQueueRecord `json:"records"`
}

// ProofTicket keeps a descriptor-backed liveness claim. Process death closes
// the descriptor; PID reuse cannot preserve a dead waiter's position.
type ProofTicket struct {
	queue  *proofQueue
	record proofQueueRecord
	file   *os.File
}

type ProofQueueBusyError struct{ EarlierTicket string }

func (e *ProofQueueBusyError) Error() string {
	return fmt.Sprintf("proof queue waits for earlier conflicting ticket %s", e.EarlierTicket)
}

func newProofQueue(backing, generation string) (*proofQueue, error) {
	if !filepath.IsAbs(backing) || filepath.Clean(backing) != backing || !proofIdentityDigest(generation) {
		return nil, fmt.Errorf("canonical proof backing and generation required")
	}
	return &proofQueue{backing: backing, root: filepath.Join(backing, "landlease", "proof", generation, "admission"), generation: generation, now: time.Now}, nil
}

func (q *proofQueue) path() string { return filepath.Join(q.root, "state", "proof-queue.json") }
func (q *proofQueue) ticketPath(id string) string {
	return filepath.Join(q.root, "ticket-liveness", id+".lock")
}
func (t *ProofTicket) ID() string {
	if t == nil {
		return ""
	}
	return t.record.ID
}

func validProofTicketID(id string) bool {
	if len(id) != 36 || id[:4] != "pqt_" {
		return false
	}
	_, err := hex.DecodeString(id[4:])
	return err == nil
}

func (q *proofQueue) readLocked() (proofQueueState, error) {
	var state proofQueueState
	body, err := os.ReadFile(q.path())
	if errors.Is(err, os.ErrNotExist) {
		return proofQueueState{SchemaVersion: proofQueueSchema, Generation: q.generation}, nil
	}
	if err != nil {
		return state, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return state, fmt.Errorf("proof queue has trailing JSON")
	}
	if state.SchemaVersion != proofQueueSchema || state.Generation != q.generation {
		return state, fmt.Errorf("proof queue schema or generation mismatch")
	}
	var previous uint64
	seen := map[string]bool{}
	for _, record := range state.Records {
		if !validProofTicketID(record.ID) || seen[record.ID] || record.Order <= previous || record.Order > state.Next {
			return state, fmt.Errorf("invalid proof queue append order")
		}
		if err := record.Identity.Validate(); err != nil {
			return state, err
		}
		if record.Identity.Generation != q.generation {
			return state, fmt.Errorf("ticket generation mismatch")
		}
		deadline, err := time.Parse(time.RFC3339Nano, record.Deadline)
		if err != nil || deadline.IsZero() {
			return state, fmt.Errorf("invalid ticket deadline")
		}
		seen[record.ID] = true
		previous = record.Order
	}
	return state, nil
}

func (q *proofQueue) writeLocked(state proofQueueState) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	dir := filepath.Dir(q.path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".proof-queue-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(append(body, '\n')); err != nil {
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
	if err := os.Rename(name, q.path()); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func (q *proofQueue) aliveLocked(record proofQueueRecord) (bool, error) {
	file, err := os.OpenFile(q.ticketPath(record.ID), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("proof ticket liveness: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("ticket liveness is not regular")
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func (q *proofQueue) pruneLocked(state *proofQueueState, now time.Time) error {
	kept := make([]proofQueueRecord, 0, len(state.Records))
	for _, record := range state.Records {
		deadline, _ := time.Parse(time.RFC3339Nano, record.Deadline)
		if !deadline.After(now) {
			continue
		}
		alive, err := q.aliveLocked(record)
		if err != nil {
			return err
		}
		if alive {
			kept = append(kept, record)
		}
	}
	removed := make([]proofQueueRecord, 0, len(state.Records)-len(kept))
	keepIDs := map[string]bool{}
	for _, record := range kept {
		keepIDs[record.ID] = true
	}
	for _, record := range state.Records {
		if !keepIDs[record.ID] {
			removed = append(removed, record)
		}
	}
	if len(removed) != 0 {
		state.Records = kept
		if err := q.writeLocked(*state); err != nil {
			return err
		}
		// Remove liveness names only after the durable queue no longer names them.
		for _, record := range removed {
			if err := os.Remove(q.ticketPath(record.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return q.reclaimOrphansLocked(*state)
}

func (q *proofQueue) enqueue(identity ProofCoordinationIdentity, deadline time.Time) (*ProofTicket, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if identity.Generation != q.generation {
		return nil, fmt.Errorf("queue generation mismatch")
	}
	if identity.Slot != 0 {
		return nil, fmt.Errorf("proof queue tickets are slot-less; the slot is chosen at the turn")
	}
	if !deadline.After(q.now()) {
		return nil, context.DeadlineExceeded
	}
	var ticket *ProofTicket
	err := withLock(q.root, func() error {
		now := q.now()
		if !deadline.After(now) {
			return context.DeadlineExceeded
		}
		state, err := q.readLocked()
		if err != nil {
			return err
		}
		if err := q.pruneLocked(&state, now); err != nil {
			return err
		}
		if state.Next == ^uint64(0) {
			return fmt.Errorf("proof queue order exhausted")
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id := "pqt_" + hex.EncodeToString(random[:])
		path := q.ticketPath(id)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		retained := false
		defer func() {
			if !retained {
				file.Close()
				// A failed directory sync may follow a successful rename. Read
				// the published queue before reclaiming the failed ticket.
				if current, err := q.readLocked(); err == nil {
					_ = q.reclaimOrphansLocked(current)
				}
			}
		}()
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
		if !deadline.After(q.now()) {
			return context.DeadlineExceeded
		}
		state.Next++
		record := proofQueueRecord{ID: id, Order: state.Next, Deadline: deadline.UTC().Format(time.RFC3339Nano), Identity: identity}
		state.Records = append(state.Records, record)
		if err := q.writeLocked(state); err != nil {
			return err
		}
		ticket = &ProofTicket{queue: q, record: record, file: file}
		retained = true
		return nil
	})
	return ticket, err
}

// proofTicketBlocks reports whether the earlier pending ticket keeps later in
// line. Same repository always does. Same class does unless the earlier
// ticket's own repository is occupied: then it waits for its repository, not
// for capacity, and must not hold a free slot idle. Read errors fail closed.
func (q *proofQueue) proofTicketBlocks(earlier, later proofQueueRecord) (bool, error) {
	if earlier.Identity.RepositoryFingerprint == later.Identity.RepositoryFingerprint {
		return true, nil
	}
	if earlier.Identity.Class != later.Identity.Class {
		return false, nil
	}
	manifest, exists, err := readProofDomain(q.backing)
	if err != nil {
		return true, err
	}
	// Existing v0.2 clients retain their original same-class FIFO semantics.
	if exists && manifest.SchemaVersion == proofDomainSchema {
		return true, nil
	}
	occupied, err := proofRepositoryOccupied(q.backing, &earlier.Identity, q.now())
	if err != nil {
		return true, err
	}
	return !occupied, nil
}

// withTurn does not consume the ticket. The owner must defer Cancel across its
// entire wait/admit operation, including preparation failure. Thus a crash after
// lease publication needs no second durable execution journal: the dead ticket
// is pruned while the existing class lease remains authoritative.
func (q *proofQueue) withTurn(ctx context.Context, ticket *ProofTicket, fn func() error) error {
	if ctx == nil || fn == nil {
		return fmt.Errorf("proof turn requires context and callback")
	}
	if ticket == nil || ticket.queue != q || ticket.file == nil {
		return fmt.Errorf("proof ticket is not live in this queue")
	}
	deadline, _ := time.Parse(time.RFC3339Nano, ticket.record.Deadline)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !deadline.After(q.now()) {
		return context.DeadlineExceeded
	}
	return withLock(q.root, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := q.now()
		if !deadline.After(now) {
			return context.DeadlineExceeded
		}
		opened, err := ticket.file.Stat()
		if err != nil {
			return err
		}
		named, err := os.Lstat(q.ticketPath(ticket.record.ID))
		if err != nil {
			return err
		}
		if !named.Mode().IsRegular() || !os.SameFile(opened, named) {
			return fmt.Errorf("ticket liveness identity changed")
		}
		state, err := q.readLocked()
		if err != nil {
			return err
		}
		if err := q.pruneLocked(&state, now); err != nil {
			return err
		}
		found := false
		for _, record := range state.Records {
			if record.ID == ticket.record.ID {
				if record != ticket.record {
					return fmt.Errorf("proof ticket changed after enqueue")
				}
				found = true
				break
			}
			blocks, err := q.proofTicketBlocks(record, ticket.record)
			if err != nil {
				return err
			}
			if blocks {
				return &ProofQueueBusyError{EarlierTicket: record.ID}
			}
		}
		if !found {
			return fmt.Errorf("proof ticket is no longer pending")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !deadline.After(q.now()) {
			return context.DeadlineExceeded
		}
		return fn()
	})
}

func (ticket *ProofTicket) Cancel() error {
	if ticket == nil || ticket.file == nil {
		return nil
	}
	q := ticket.queue
	err := withLock(q.root, func() error {
		state, err := q.readLocked()
		if err != nil {
			return err
		}
		kept := make([]proofQueueRecord, 0, len(state.Records))
		for _, record := range state.Records {
			if record.ID != ticket.record.ID {
				kept = append(kept, record)
			}
		}
		if len(kept) != len(state.Records) {
			state.Records = kept
			if err := q.writeLocked(state); err != nil {
				return err
			}
		}
		if err := os.Remove(q.ticketPath(ticket.record.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	closeErr := ticket.file.Close()
	ticket.file = nil
	return errors.Join(err, closeErr)
}

// Reclaim names left by a crash before queue publication. A live unreferenced
// descriptor is never unlinked. This directory contains ticket liveness only.
func (q *proofQueue) reclaimOrphansLocked(state proofQueueState) error {
	referenced := map[string]bool{}
	for _, record := range state.Records {
		referenced[record.ID] = true
	}
	entries, err := os.ReadDir(filepath.Join(q.root, "ticket-liveness"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 41 || name[36:] != ".lock" || !validProofTicketID(name[:36]) {
			return fmt.Errorf("invalid proof ticket liveness name %q", name)
		}
		id := name[:36]
		if referenced[id] {
			continue
		}
		alive, err := q.aliveLocked(proofQueueRecord{ID: id})
		if err != nil {
			return err
		}
		if !alive {
			if err := os.Remove(q.ticketPath(id)); err != nil {
				return err
			}
		}
	}
	return nil
}
