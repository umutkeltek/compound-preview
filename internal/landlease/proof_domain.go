package landlease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const proofDomainSchema = "compound.proof_domain.v0.2"
const proofDomainFencedSchema = "compound.proof_domain.v0.3"

// ProofDomainRevision is an explicit, one-way owner-authorized cutover target.
type ProofDomainRevision string

const (
	ProofDomainLocalV2  ProofDomainRevision = proofDomainSchema
	ProofDomainFencedV3 ProofDomainRevision = proofDomainFencedSchema
)

func requireFencedProofDomain(manifest proofDomainManifest) error {
	if manifest.SchemaVersion != proofDomainFencedSchema {
		return fmt.Errorf("proof domain requires explicit v0.3 quiescent cutover before additional slots or remote execution")
	}
	return nil
}

func proofDomainCapacity(manifest proofDomainManifest, class ProofCapacityClass) int {
	if manifest.SchemaVersion == proofDomainSchema {
		return 1
	}
	return proofClassCapacity(class)
}

func validateProofDomainSlots(manifest proofDomainManifest, slots map[ProofCapacityClass][]StatusResult) error {
	if manifest.SchemaVersion != proofDomainSchema {
		return nil
	}
	for _, statuses := range slots {
		for slot, status := range statuses {
			if slot != 0 && status.Holder != "" {
				return fmt.Errorf("v0.2 proof domain contains an unsupported nonzero slot; exact recovery and cutover required")
			}
		}
	}
	return nil
}

type proofDomainManifest struct {
	SchemaVersion string   `json:"schema_version"`
	Generation    string   `json:"generation"`
	Phase         string   `json:"phase"`
	Homes         []string `json:"homes"`
}

func proofDomainManifestPath(backing string) string {
	return filepath.Join(backing, "landlease", "proof", "domain.json")
}
func readProofDomain(backing string) (proofDomainManifest, bool, error) {
	var manifest proofDomainManifest
	body, err := os.ReadFile(proofDomainManifestPath(backing))
	if errors.Is(err, os.ErrNotExist) {
		return manifest, false, nil
	}
	if err != nil {
		return manifest, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return manifest, false, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return manifest, false, fmt.Errorf("proof domain has trailing JSON")
	}
	if (manifest.SchemaVersion != proofDomainSchema && manifest.SchemaVersion != proofDomainFencedSchema) || !proofIdentityDigest(manifest.Generation) || (manifest.Phase != "prepared" && manifest.Phase != "active") || len(manifest.Homes) == 0 || !sort.StringsAreSorted(manifest.Homes) {
		return manifest, false, fmt.Errorf("invalid proof coordination manifest")
	}
	prior := ""
	for _, home := range manifest.Homes {
		if !filepath.IsAbs(home) || filepath.Clean(home) != home || home == prior {
			return manifest, false, fmt.Errorf("invalid proof domain home enrollment")
		}
		prior = home
	}
	if manifest.Phase == "active" {
		if err := validateProofFence(backing, manifest.Generation); err != nil {
			return manifest, false, err
		}
	}
	return manifest, true, nil
}
func proofDomainForHome(home string) (proofDomainManifest, string, bool, error) {
	layout, err := LayoutStatus(home)
	if err != nil {
		return proofDomainManifest{}, "", false, err
	}
	if layout.Mode != "scoped" {
		return proofDomainManifest{}, "", false, nil
	}
	manifest, exists, err := readProofDomain(layout.BackingRoot)
	if err != nil || !exists {
		return manifest, layout.BackingRoot, exists, err
	}
	if err := validateProofEnrollment(home, manifest); err != nil {
		return manifest, layout.BackingRoot, true, err
	}
	return manifest, layout.BackingRoot, true, nil
}
func validateProofEnrollment(home string, manifest proofDomainManifest) error {
	if manifest.Phase != "active" {
		return fmt.Errorf("proof coordination cutover is prepared; admissions and lifecycle mutations are fenced until recovery")
	}
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		return err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return err
	}
	i := sort.SearchStrings(manifest.Homes, canonical)
	if i == len(manifest.Homes) || manifest.Homes[i] != canonical {
		return fmt.Errorf("home is not enrolled in proof coordination generation")
	}
	return nil
}
func proofClassRoot(backing, generation string, class ProofCapacityClass) string {
	return filepath.Join(backing, "landlease", "proof", generation, "classes", string(class))
}

// proofSlotRoot keeps slot 0 at the original class root (fence semantics
// untouched); slot k is a sibling at the same depth.
func proofSlotRoot(backing, generation string, class ProofCapacityClass, slot int) string {
	if slot == 0 {
		return proofClassRoot(backing, generation, class)
	}
	return filepath.Join(backing, "landlease", "proof", generation, "classes", fmt.Sprintf("%s-slot-%d", class, slot))
}
func proofRepositoryRoot(backing, generation string) string {
	return filepath.Join(backing, "landlease", "proof", generation, "repositories")
}
func proofRepositoryHolder(identity *ProofCoordinationIdentity) string {
	return "repository:" + identity.RepositoryBarrier
}

// proofRepositoryOccupied reports whether identity's repository already owns
// proof execution: its repository barrier is held, or a lease record for its
// fingerprint sits in any class slot. An expired record still counts, because
// expiry does not recover a predecessor's repository ownership. It reads atomic
// lease images only and takes no class lock, so the queue lock order holds.
func proofRepositoryOccupied(backing string, identity *ProofCoordinationIdentity, now time.Time) (bool, error) {
	pending, err := executionBarrierPending(proofRepositoryRoot(backing, identity.Generation), proofRepositoryHolder(identity))
	if err != nil || pending {
		return pending, err
	}
	for _, class := range []ProofCapacityClass{ProofCapacitySystemFull, ProofCapacityNativeConsumer} {
		expected := *identity
		expected.Class = class
		for slot := 0; slot <= proofSlotMax; slot++ {
			slotted, err := expected.WithSlot(slot)
			if err != nil {
				return false, err
			}
			status, err := proofAdmissionSnapshot(proofSlotRoot(backing, identity.Generation, class, slot), now, &leaseBinding{identity: &slotted, acquisition: true})
			if err != nil {
				return false, err
			}
			if status.Holder != "" && status.ProofCoordination != nil && status.ProofCoordination.RepositoryFingerprint == identity.RepositoryFingerprint {
				return true, nil
			}
		}
	}
	return false, nil
}

// ProofCoordinationFor returns nil only for an unmigrated historical singleton.
// The work owner derives class from sealed canonical source, never a CLI flag.
func ProofCoordinationFor(home string, class ProofCapacityClass, repository string) (*ProofCoordinationIdentity, error) {
	manifest, _, exists, err := proofDomainForHome(home)
	if err != nil || !exists {
		return nil, err
	}
	identity, err := NewProofCoordinationIdentity(manifest.Generation, class, repository)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

// ProofResourceForIdentity routes historical records by their missing identity.
// Current configuration must never reinterpret an old reservation.
func ProofResourceForIdentity(home string, identity *ProofCoordinationIdentity) *Resource {
	r := ProofResource(home)
	r.proofIdentity = cloneProofCoordination(identity)
	if identity != nil {
		r.initErr = identity.Validate()
	}
	return r
}
func (r *Resource) selectProofRoot(root string, scoped bool) (string, error) {
	if r.key.Kind != ResourceProof {
		return root, nil
	}
	if !scoped {
		if r.proofIdentity != nil {
			return "", fmt.Errorf("coordinated proof requires enrolled scoped home")
		}
		return root, nil
	}
	backing := filepath.Dir(filepath.Dir(filepath.Dir(root)))
	manifest, exists, err := readProofDomain(backing)
	if err != nil {
		return "", err
	}
	if !exists {
		if r.proofIdentity != nil {
			return "", fmt.Errorf("proof identity has no installed coordination generation")
		}
		return root, nil
	}
	if err := validateProofEnrollment(r.home, manifest); err != nil {
		return "", err
	}
	if r.proofIdentity == nil {
		return root, nil
	} // Permanent legacy endpoint fence remains authoritative.
	if err := r.proofIdentity.Validate(); err != nil {
		return "", err
	}
	if r.proofIdentity.Generation != manifest.Generation {
		return "", fmt.Errorf("proof coordination generation changed")
	}
	if r.proofIdentity.Slot != 0 {
		if err := requireFencedProofDomain(manifest); err != nil {
			return "", err
		}
	}
	return proofSlotRoot(backing, manifest.Generation, r.proofIdentity.Class, r.proofIdentity.Slot), nil
}

// ProofIdentity returns the exact slot-bearing identity this resource routes by.
func (r *Resource) ProofIdentity() *ProofCoordinationIdentity {
	if r == nil {
		return nil
	}
	return cloneProofCoordination(r.proofIdentity)
}
func (r *Resource) binding(root string, acquisition bool) *leaseBinding {
	b := &leaseBinding{identity: r.proofIdentity, acquisition: acquisition, nonblocking: acquisition && r.proofIdentity != nil}
	if r.proofIdentity != nil {
		// slot root is generation/classes/<slot dir>; repository barriers share generation.
		b.repositoryRoot = filepath.Join(filepath.Dir(filepath.Dir(root)), "repositories")
	}
	return b
}

type ProofAdmissionBusyError struct{ Holder, Reason string }

func (e *ProofAdmissionBusyError) Error() string {
	return fmt.Sprintf("proof admission waits for %s: %s", e.Holder, e.Reason)
}

type proofAdmissionGuard struct {
	active, admitted   bool
	draining, inFlight bool
	mu                 sync.Mutex
	done               chan struct{}
	ctx                context.Context
	deadline           time.Time
	now                func() time.Time
}

func (g *proofAdmissionGuard) check() error {
	if g == nil {
		return fmt.Errorf("proof admission guard required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checkLocked()
}
func (g *proofAdmissionGuard) checkLocked() error {
	if g == nil || !g.active || g.admitted {
		return fmt.Errorf("coordinated proof acquisition requires its live FIFO admission callback")
	}
	if err := g.ctx.Err(); err != nil {
		return err
	}
	if !g.now().Before(g.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// ProofAdmission owns only FIFO position and callback-scoped admission. It is
// neither a second execution journal nor authority to run a proof.
type ProofAdmission struct {
	home, backing string
	identity      ProofCoordinationIdentity
	ticket        *ProofTicket
}

func BeginProofAdmission(home string, identity ProofCoordinationIdentity, deadline time.Time) (*ProofAdmission, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	manifest, backing, exists, err := proofDomainForHome(home)
	if err != nil {
		return nil, err
	}
	if !exists || manifest.Generation != identity.Generation {
		return nil, fmt.Errorf("proof admission requires exact current coordination generation")
	}
	queue, err := newProofQueue(backing, identity.Generation)
	if err != nil {
		return nil, err
	}
	ticket, err := queue.enqueue(identity, deadline)
	if err != nil {
		return nil, err
	}
	return &ProofAdmission{home: home, backing: backing, identity: identity, ticket: ticket}, nil
}
func (a *ProofAdmission) Cancel() error {
	if a == nil {
		return nil
	}
	return a.ticket.Cancel()
}
func (a *ProofAdmission) Identity() *ProofCoordinationIdentity {
	if a == nil {
		return nil
	}
	return cloneProofCoordination(&a.identity)
}

// WaitingStatus is an advisory atomic class snapshot, never admission. It
// preserves this ticket's generation while accepting a foreign repository's
// same-class holder. It takes no class lock, so a finalizer cannot block waiting
// cancellation. WithTurn must still revalidate every admission fence.
// It reports this repository's pinned slot if any, else the first free or
// expired slot (not Held, so the caller tries admission), else slot 0 (Held).
func (a *ProofAdmission) WaitingStatus(now time.Time) (StatusResult, error) {
	if a == nil || a.ticket == nil {
		return StatusResult{}, fmt.Errorf("proof waiting status requires an admission ticket")
	}
	manifest, backing, exists, err := proofDomainForHome(a.home)
	if err != nil {
		return StatusResult{}, err
	}
	if !exists || backing != a.backing || manifest.Generation != a.identity.Generation {
		return StatusResult{}, fmt.Errorf("proof coordination changed after enqueue")
	}
	observed, err := a.observeProofSlots(now)
	if err != nil {
		return StatusResult{}, err
	}
	if err := validateProofDomainSlots(manifest, observed); err != nil {
		return StatusResult{}, err
	}
	slots := observed[a.identity.Class]
	for _, status := range slots {
		if status.Holder != "" && status.ProofCoordination != nil && status.ProofCoordination.RepositoryFingerprint == a.identity.RepositoryFingerprint {
			return status, nil
		}
	}
	for _, status := range slots[:proofDomainCapacity(manifest, a.identity.Class)] {
		if status.Holder == "" || !status.Held {
			return status, nil
		}
	}
	return slots[0], nil
}

// observeProofSlots snapshots every slot root of both classes for this
// generation. Absent roots read empty; each binding expects its own slot.
func (a *ProofAdmission) observeProofSlots(now time.Time) (map[ProofCapacityClass][]StatusResult, error) {
	out := map[ProofCapacityClass][]StatusResult{}
	for _, class := range []ProofCapacityClass{ProofCapacitySystemFull, ProofCapacityNativeConsumer} {
		expected := a.identity
		expected.Class = class
		for slot := 0; slot <= proofSlotMax; slot++ {
			identity, err := expected.WithSlot(slot)
			if err != nil {
				return nil, err
			}
			status, err := proofAdmissionSnapshot(proofSlotRoot(a.backing, a.identity.Generation, class, slot), now, &leaseBinding{identity: &identity, acquisition: true})
			if err != nil {
				return nil, err
			}
			out[class] = append(out[class], status)
		}
	}
	return out, nil
}

// chooseProofSlot picks the one slot this turn may use. Same-repository records
// pin recovery to their exact root; otherwise a free slot, then an expired one,
// then (steal only) a held one, each lowest first.
func (a *ProofAdmission) chooseProofSlot(slots map[ProofCapacityClass][]StatusResult, capacity int, steal bool, prefer func(string) bool) (int, error) {
	mine := func(status StatusResult) bool {
		return status.Holder != "" && status.ProofCoordination != nil && status.ProofCoordination.RepositoryFingerprint == a.identity.RepositoryFingerprint
	}
	for class, statuses := range slots {
		if class == a.identity.Class {
			continue
		}
		for _, status := range statuses {
			if mine(status) {
				// Expiry does not recover a predecessor's repository ownership.
				return 0, &ProofAdmissionBusyError{Holder: status.Holder, Reason: "same repository requires exact predecessor recovery"}
			}
		}
	}
	own := slots[a.identity.Class]
	pinned := -1
	for slot, status := range own {
		if !mine(status) {
			continue
		}
		if pinned >= 0 {
			return 0, &ProofAdmissionBusyError{Holder: status.Holder, Reason: "same repository holds multiple slots; exact recovery required"}
		}
		pinned = slot
	}
	if pinned >= 0 {
		if own[pinned].Held && !steal {
			return 0, &ProofAdmissionBusyError{Holder: own[pinned].Holder, Reason: "same repository proof running"}
		}
		return pinned, nil
	}
	if steal && prefer != nil {
		for slot, status := range own[:capacity] {
			if status.Holder != "" && prefer(status.Holder) {
				return slot, nil
			}
		}
	}
	for slot, status := range own[:capacity] {
		if status.Holder == "" {
			return slot, nil
		}
	}
	for slot, status := range own[:capacity] {
		if !status.Held {
			return slot, nil
		}
	}
	if steal {
		return 0, nil
	}
	return 0, &ProofAdmissionBusyError{Holder: own[0].Holder, Reason: fmt.Sprintf("capacity occupied (%d/%d)", capacity, capacity)}
}

func (a *ProofAdmission) WithTurn(ctx context.Context, steal bool, fn func(*Resource) error) error {
	return a.WithTurnFor(ctx, steal, nil, fn)
}

// WithTurnFor admits into one class slot chosen under the queue turn. prefer
// lets an audited steal target the exact predecessor's slot.
func (a *ProofAdmission) WithTurnFor(ctx context.Context, steal bool, prefer func(holder string) bool, fn func(*Resource) error) error {
	if a == nil || fn == nil {
		return fmt.Errorf("proof admission and callback required")
	}
	return a.ticket.queue.withTurn(ctx, a.ticket, func() error {
		manifest, backing, exists, err := proofDomainForHome(a.home)
		if err != nil {
			return err
		}
		if !exists || backing != a.backing || manifest.Generation != a.identity.Generation {
			return fmt.Errorf("proof coordination changed after enqueue")
		}
		slots, err := a.observeProofSlots(a.ticket.queue.now())
		if err != nil {
			return err
		}
		if err := validateProofDomainSlots(manifest, slots); err != nil {
			return err
		}
		chosen, err := a.chooseProofSlot(slots, proofDomainCapacity(manifest, a.identity.Class), steal, prefer)
		if err != nil {
			return err
		}
		pending, err := executionBarrierPending(proofRepositoryRoot(a.backing, a.identity.Generation), proofRepositoryHolder(&a.identity))
		if err != nil {
			return err
		}
		if pending && !steal {
			return &ProofAdmissionBusyError{Holder: a.identity.RepositoryFingerprint, Reason: "repository descendants remain live"}
		}
		deadline, _ := time.Parse(time.RFC3339Nano, a.ticket.record.Deadline)
		guard := &proofAdmissionGuard{active: true, ctx: ctx, deadline: deadline, now: a.ticket.queue.now}
		defer guard.close()
		identity, err := a.identity.WithSlot(chosen)
		if err != nil {
			return err
		}
		resource := ProofResourceForIdentity(a.home, &identity)
		resource.proofAdmission = guard
		return fn(resource)
	})
}
func (r *Resource) requireProofBarrierAdmission() error {
	if r.proofIdentity == nil {
		return nil
	}
	return r.proofAdmission.check()
}
func (r *Resource) rejectBarrierlessProof() error {
	if r.proofIdentity != nil {
		return fmt.Errorf("coordinated proof requires inherited capacity and repository barriers")
	}
	return nil
}
func (r *Resource) acquireProofBarrier(root, holder string, ttl time.Duration, now time.Time, prepare func(AcquireOutcome) error) (AcquireOutcome, *ExecutionBarrier, error) {
	if err := r.requireProofBarrierAdmission(); err != nil {
		return AcquireOutcome{}, nil, err
	}
	if prepare == nil {
		return AcquireOutcome{}, nil, fmt.Errorf("proof prepare callback required")
	}
	success := false
	if r.proofIdentity != nil {
		finish, err := r.proofAdmission.begin()
		if err != nil {
			return AcquireOutcome{}, nil, err
		}
		defer func() { finish(success) }()
	}

	var repository *ExecutionBarrier
	outcome, barrier, err := acquireWithProofIdentityAndBarrierPrepared(root, holder, ttl, now, func(outcome AcquireOutcome) error {
		if r.proofIdentity != nil {
			if err := r.proofAdmission.check(); err != nil {
				return err
			}
			var err error
			repository, err = lockExecutionBarrier(r.binding(root, false).repositoryRoot, proofRepositoryHolder(r.proofIdentity))
			if err != nil {
				return err
			}
		}
		return prepare(outcome)
	}, r.proofIdentity, r.binding(root, true))
	if err != nil {
		if repository != nil {
			_ = repository.Close()
		}
		return AcquireOutcome{}, nil, err
	}
	if repository != nil {
		if err := barrier.include(repository); err != nil {
			_ = repository.Close()
			_ = barrier.Close()
			return AcquireOutcome{}, nil, err
		}
		success = true
	}
	return outcome, barrier, nil
}
func (r *Resource) stealProofBarrier(root, newHolder, prior, reason string, now time.Time, prepare func(Lease) error) (Lease, *ExecutionBarrier, error) {
	if err := r.requireProofBarrierAdmission(); err != nil {
		return Lease{}, nil, err
	}
	if prepare == nil {
		return Lease{}, nil, fmt.Errorf("proof steal prepare callback required")
	}
	success := false
	if r.proofIdentity != nil {
		finish, err := r.proofAdmission.begin()
		if err != nil {
			return Lease{}, nil, err
		}
		defer func() { finish(success) }()
	}

	var repository *ExecutionBarrier
	lease, barrier, err := stealWithProofIdentityAndBarrierPrepared(root, newHolder, prior, reason, now, func(lease Lease) error {
		if r.proofIdentity != nil {
			if err := r.proofAdmission.check(); err != nil {
				return err
			}
			var err error
			repository, err = lockExecutionBarrier(r.binding(root, false).repositoryRoot, proofRepositoryHolder(r.proofIdentity))
			if err != nil {
				return err
			}
		}
		return prepare(lease)
	}, r.proofIdentity, r.binding(root, true))
	if err != nil {
		if repository != nil {
			_ = repository.Close()
		}
		return Lease{}, nil, err
	}
	if repository != nil {
		if err := barrier.include(repository); err != nil {
			_ = repository.Close()
			_ = barrier.Close()
			return Lease{}, nil, err
		}
		success = true
	}
	return lease, barrier, nil
}

func (g *proofAdmissionGuard) begin() (func(bool), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.checkLocked(); err != nil {
		return nil, err
	}
	if g.draining || g.inFlight {
		return nil, fmt.Errorf("proof admission is draining or already acquiring")
	}
	g.inFlight = true
	g.done = make(chan struct{})
	return func(success bool) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.admitted = g.admitted || success
		g.inFlight = false
		close(g.done)
	}, nil
}
func (g *proofAdmissionGuard) close() {
	g.mu.Lock()
	g.draining = true
	done := g.done
	pending := g.inFlight
	g.mu.Unlock()
	if pending {
		<-done
	}
	g.mu.Lock()
	g.active = false
	g.mu.Unlock()
}

func ProofCoordinationGeneration(home string) (string, error) {
	manifest, _, exists, err := proofDomainForHome(home)
	if err != nil || !exists {
		return "", err
	}
	return manifest.Generation, nil
}

type ProofClassStatus struct {
	Class  ProofCapacityClass `json:"class,omitempty"`
	Slot   int                `json:"slot"`
	Status StatusResult       `json:"status"`
}

func ProofResourceStatuses(home string, now time.Time) ([]ProofClassStatus, error) {
	manifest, backing, exists, err := proofDomainForHome(home)
	if err != nil {
		return nil, err
	}
	if !exists {
		status, err := ProofResource(home).StatusReadOnly(now)
		if err != nil {
			return nil, err
		}
		return []ProofClassStatus{{Status: status}}, nil
	}
	var out []ProofClassStatus
	// One row per slot within capacity, plus any slot beyond it that still
	// holds a lease (capacity may shrink below historical slots).
	for _, class := range []ProofCapacityClass{ProofCapacitySystemFull, ProofCapacityNativeConsumer} {
		base, _ := NewProofCoordinationIdentity(manifest.Generation, class, strings.Repeat("0", 64))
		for slot := 0; slot <= proofSlotMax; slot++ {
			identity, err := base.WithSlot(slot)
			if err != nil {
				return nil, err
			}
			status, err := statusReadOnlyBound(proofSlotRoot(backing, manifest.Generation, class, slot), now, &leaseBinding{identity: &identity, acquisition: true})
			if err != nil {
				return nil, err
			}
			if slot >= proofDomainCapacity(manifest, class) && status.Holder == "" {
				continue
			}
			out = append(out, ProofClassStatus{Class: class, Slot: slot, Status: status})
		}
	}
	return out, nil
}

// AdmissionStatus observes the class predecessor only from its live FIFO
// callback. Lifecycle mutation methods continue requiring exact stored identity.
func (r *Resource) AdmissionStatus(now time.Time) (StatusResult, error) {
	if err := r.requireProofBarrierAdmission(); err != nil {
		return StatusResult{}, err
	}
	return selectedReadCall(r, func(root string) (StatusResult, error) {
		return proofAdmissionSnapshot(root, now, r.binding(root, true))
	})
}

// Admissions already hold the domain lock: no other class acquisition can race.
// Finalizers can atomically remove a lease; a stale snapshot only defers admission.
// Reading the atomic lease image avoids waiting on a long finalizer while holding
// the domain. Actual class acquisition uses a nonblocking bound class flock.
func proofAdmissionSnapshot(root string, now time.Time, binding *leaseBinding) (StatusResult, error) {
	path, err := statePath(root)
	if err != nil {
		return StatusResult{}, err
	}
	current, exists, err := readLease(path)
	if err != nil {
		return StatusResult{}, err
	}
	if err := binding.validate(current); err != nil {
		return StatusResult{}, err
	}
	return leaseStatusResult(current, exists, now), nil
}
