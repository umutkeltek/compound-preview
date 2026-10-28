package mem_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	homepkg "compound/internal/home"
	"compound/internal/mem"
	"compound/internal/routeevidence"
)

func TestEngineDoctorAcceptsRealRouteEvidenceExchangeCommit(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	engine := coordinatedOwnerEngine(t, homePath, publication.prepare, publication.beginAndCommit)

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err != nil {
		t.Fatalf("real rooted-CAS exchange+Commit must be owner-attributed: %v; report=%+v", err, report)
	}
	if report.RetryCount != 1 || len(report.EngineViolations) != 0 {
		t.Fatalf("exchange+Commit did not complete through one clean retry: %+v", report)
	}
}

func TestEngineDoctorTreatsPreparedSwapAsPendingUntilRealExchangeResolves(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	engine := coordinatedOwnerEngine(t, homePath, nil, publication.prepare, publication.beginAndCommit)

	report, err := store.EngineDoctor("ledger", 10, engine)
	if !errors.Is(err, mem.ErrLedgerBusy) || len(report.EngineViolations) != 0 {
		t.Fatalf("prepared→exchange→Commit across both attempts must be busy, not engine-attributed: %v; report=%+v", err, report)
	}
}

func TestEngineDoctorAcceptsCommitWhenDoctorStartsPostExchange(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	engine := coordinatedOwnerEngine(t, homePath, publication.prepareAndBegin, publication.commit)

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err != nil || report.RetryCount != 1 || len(report.EngineViolations) != 0 {
		t.Fatalf("post-exchange Commit must retain owner attribution: %v; report=%+v", err, report)
	}
}

func TestEngineDoctorAcceptsRealFirstRouteEvidenceLinkCommit(t *testing.T) {
	homePath, store := openDoctorStore(t)
	postimage := routeEvidenceRow(t, "req_first")
	publication := prepareRoutePublication(t, homePath, nil, postimage, false)
	engine := coordinatedOwnerEngine(t, homePath, publication.prepare, publication.beginAndCommit)

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err != nil {
		t.Fatalf("real rooted-CAS absent-canonical link+Commit must be owner-attributed: %v; report=%+v", err, report)
	}
	if report.RetryCount != 1 || len(report.EngineViolations) != 0 {
		t.Fatalf("first publication did not complete through one clean retry: %+v", report)
	}
}

func TestEngineDoctorAcceptsCommitWhenDoctorStartsInFirstPublicationLinkState(t *testing.T) {
	homePath, store := openDoctorStore(t)
	postimage := routeEvidenceRow(t, "req_first")
	publication := prepareRoutePublication(t, homePath, nil, postimage, false)
	engine := coordinatedOwnerEngine(t, homePath, publication.prepareAndBegin, publication.commit)

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err != nil || report.RetryCount != 1 || len(report.EngineViolations) != 0 {
		t.Fatalf("first-publication linked Commit must retain owner attribution: %v; report=%+v", err, report)
	}
}

func TestEngineDoctorRejectsStableUnresolvedRealRouteEvidencePublication(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	engine := coordinatedOwnerEngine(t, homePath, publication.prepareAndBegin, func() error { return nil })

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("stable publication artifact must fail closed, got %v; report=%+v", err, report)
	}
}

func TestEngineDoctorRejectsPendingPublicationBeforeBusyReturn(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	engine := coordinatedOwnerEngine(t, homePath, publication.prepare,
		publication.begin,
		func() error {
			return appendOwnerJSONL(filepath.Join(homePath, "ledger", "owner-activity.jsonl"), []byte("{\"producer\":\"compound\"}\n"))
		},
	)

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err == nil || errors.Is(err, mem.ErrLedgerBusy) || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("busy path must not hide a still-pending publication: %v; report=%+v", err, report)
	}
}

func TestEngineDoctorRejectsStableSwapAcrossSustainedGovernedWrites(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	activityPath := filepath.Join(homePath, "ledger", "owner-activity.jsonl")
	writes := 0
	engine := coordinatedOwnerEngine(t, homePath, publication.prepareAndBegin,
		func() error {
			writes++
			return appendOwnerJSONL(activityPath, []byte(fmt.Sprintf("{\"producer\":\"compound\",\"write\":%d}\n", writes)))
		},
		func() error {
			writes++
			return appendOwnerJSONL(activityPath, []byte(fmt.Sprintf("{\"producer\":\"compound\",\"write\":%d}\n", writes)))
		},
	)

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err == nil || errors.Is(err, mem.ErrLedgerBusy) || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("stable exact swap must not hide behind sustained governed writes: %v; report=%+v", err, report)
	}
}

func TestEngineDoctorRejectsPendingPublicationBeforeStableRetryReturn(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	engine := coordinatedOwnerEngine(t, homePath, publication.prepare, publication.begin, func() error { return nil })

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("stable retry must not certify a still-pending publication: %v; report=%+v", err, report)
	}
}

func TestEngineDoctorRejectsExactCASLookalikeWithInvalidRouteEvidence(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	invalidRow := []byte("{\"schema_version\":\"not-the-route-evidence-contract\"}\n")
	postimage := append(append([]byte(nil), preimage...), invalidRow...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	engine := coordinatedOwnerEngine(t, homePath, publication.prepare, publication.beginAndCommit)

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err == nil || len(report.EngineViolations) == 0 {
		t.Fatalf("exact name/hash/CAS identity must not bypass canonical row validation: %v; report=%+v", err, report)
	}
}

func TestEngineDoctorRejectsExactLookalikeWithoutRootedCASIdentity(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	canonicalPath := filepath.Join(homePath, "ledger", publication.canonicalName)
	swapPath := filepath.Join(homePath, "ledger", publication.swapName)
	if err := publication.prepare(); err != nil {
		t.Fatal(err)
	}
	engine := unlockedOwnerEngine(t, func() error {
		if err := os.WriteFile(canonicalPath, postimage, 0o644); err != nil {
			return err
		}
		return os.Remove(swapPath)
	})

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err == nil || len(report.EngineViolations) == 0 {
		t.Fatalf("matching name/hash/bytes without rooted-CAS identity must stay red: %v; report=%+v", err, report)
	}
}

func TestEngineDoctorRejectsRenameOverwriteWithoutOwnerLock(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	publication := prepareRoutePublication(t, homePath, preimage, postimage, true)
	canonicalPath := filepath.Join(homePath, "ledger", publication.canonicalName)
	swapPath := filepath.Join(homePath, "ledger", publication.swapName)
	if err := publication.prepare(); err != nil {
		t.Fatal(err)
	}
	engine := unlockedOwnerEngine(t, func() error { return os.Rename(swapPath, canonicalPath) })

	report, err := store.EngineDoctor("ledger", 10, engine)
	if err == nil || len(report.EngineViolations) == 0 {
		t.Fatalf("plain rename-overwrite must not impersonate rooted-CAS owner publication: %v; report=%+v", err, report)
	}
}

func TestLedgerDoctorRejectsExactSwapSymlinkLeaf(t *testing.T) {
	homePath, store := openDoctorStore(t)
	preimage := routeEvidenceRow(t, "req_before")
	postimage := append(append([]byte(nil), preimage...), routeEvidenceRow(t, "req_after")...)
	canonicalPath := filepath.Join(homePath, "ledger", "execution-route-evidence.jsonl")
	if err := os.WriteFile(canonicalPath, preimage, 0o644); err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(homePath, "outside-route-postimage.jsonl")
	if err := os.WriteFile(targetPath, postimage, 0o644); err != nil {
		t.Fatal(err)
	}
	swapName := fmt.Sprintf(
		".execution-route-evidence.jsonl.%d.%s.%s.swap",
		os.Getpid(),
		doctorSHA256(preimage)[:12],
		doctorSHA256(postimage)[:12],
	)
	if err := os.Symlink(targetPath, filepath.Join(homePath, "ledger", swapName)); err != nil {
		t.Fatal(err)
	}

	if _, err := store.LedgerDigest(); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("exact swap symlink leaf must fail rooted snapshot read, got %v", err)
	}
}

func TestLedgerDoctorRejectsSymlinkLedgerRoot(t *testing.T) {
	homePath, store := openDoctorStore(t)
	ledgerPath := filepath.Join(homePath, "ledger")
	realLedgerPath := filepath.Join(homePath, "ledger-real")
	if err := os.Rename(ledgerPath, realLedgerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realLedgerPath, ledgerPath); err != nil {
		t.Fatal(err)
	}

	if _, err := store.LedgerDigest(); err == nil || !strings.Contains(err.Error(), "ledger root must be a real directory") {
		t.Fatalf("symlink ledger root must fail rooted snapshot read, got %v", err)
	}
}

func openDoctorStore(t *testing.T) (string, *mem.Store) {
	t.Helper()
	homePath := t.TempDir()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	return homePath, store
}

func routeEvidenceRow(t *testing.T, requestID string) []byte {
	t.Helper()
	ev, err := routeevidence.Build(routeevidence.BuildInput{
		CreatedAt:     time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC),
		RequestID:     requestID,
		ClientProfile: "doctor-test",
		Protocol:      "chat_completions",
		Provider:      "provider",
		Model:         "model",
		Status:        "completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeevidence.Decode(body); err != nil {
		t.Fatalf("fixture must satisfy canonical route-evidence Decode/Validate: %v", err)
	}
	return append(body, '\n')
}

type rootedRoutePublication struct {
	root            *os.Root
	canonicalName   string
	swapName        string
	expectedData    []byte
	expectedInfo    os.FileInfo
	replacementData []byte
	replacementInfo os.FileInfo
	publication     *homepkg.RootedFileCAS
}

func prepareRoutePublication(t *testing.T, homePath string, preimage, postimage []byte, canonicalPresent bool) *rootedRoutePublication {
	t.Helper()
	ledgerPath := filepath.Join(homePath, "ledger")
	root, err := os.OpenRoot(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	p := &rootedRoutePublication{
		root:            root,
		canonicalName:   "execution-route-evidence.jsonl",
		expectedData:    append([]byte(nil), preimage...),
		replacementData: append([]byte(nil), postimage...),
	}
	p.swapName = fmt.Sprintf(
		".%s.%d.%s.%s.swap",
		p.canonicalName,
		os.Getpid(),
		doctorSHA256(preimage)[:12],
		doctorSHA256(postimage)[:12],
	)
	if canonicalPresent {
		writeRootedFile(t, root, p.canonicalName, preimage)
		p.expectedInfo, err = root.Lstat(p.canonicalName)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if p.publication != nil && p.publication.State() == homepkg.RootedFileCASPublished {
			_ = p.publication.Commit()
		}
		_ = root.Close()
	})
	return p
}

func (p *rootedRoutePublication) begin() error {
	publication, err := homepkg.BeginRootedFileCAS(
		p.root,
		p.canonicalName,
		p.swapName,
		p.expectedData,
		p.expectedInfo,
		p.replacementData,
		p.replacementInfo,
	)
	p.publication = publication
	return err
}

func (p *rootedRoutePublication) prepare() error {
	if err := writeRootedFileOwned(p.root, p.swapName, p.replacementData); err != nil {
		return err
	}
	info, err := p.root.Lstat(p.swapName)
	if err != nil {
		return err
	}
	p.replacementInfo = info
	return nil
}

func (p *rootedRoutePublication) prepareAndBegin() error {
	if err := p.prepare(); err != nil {
		return err
	}
	return p.begin()
}

func (p *rootedRoutePublication) commit() error {
	if p.publication == nil {
		return fmt.Errorf("route publication not begun")
	}
	return p.publication.Commit()
}

func (p *rootedRoutePublication) beginAndCommit() error {
	if err := p.begin(); err != nil {
		return err
	}
	return p.publication.Commit()
}

func writeRootedFile(t *testing.T, root *os.Root, name string, data []byte) {
	t.Helper()
	if err := writeRootedFileOwned(root, name, data); err != nil {
		t.Fatal(err)
	}
}

func writeRootedFileOwned(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func doctorSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type coordinatedEngine struct {
	base     mem.LexicalEngine
	request  chan struct{}
	response chan error
}

func coordinatedOwnerEngine(t *testing.T, homePath string, setup func() error, steps ...func() error) *coordinatedEngine {
	t.Helper()
	return newCoordinatedEngine(t, homePath, true, setup, steps...)
}

func unlockedOwnerEngine(t *testing.T, steps ...func() error) *coordinatedEngine {
	t.Helper()
	return newCoordinatedEngine(t, "", false, nil, steps...)
}

func newCoordinatedEngine(t *testing.T, homePath string, locked bool, setup func() error, steps ...func() error) *coordinatedEngine {
	t.Helper()
	engine := &coordinatedEngine{request: make(chan struct{}), response: make(chan error)}
	stop := make(chan struct{})
	ready := make(chan error, 1)
	go func() {
		run := func() error {
			if setup != nil {
				if err := setup(); err != nil {
					ready <- err
					return err
				}
			}
			ready <- nil
			step := 0
			for {
				select {
				case <-engine.request:
					var err error
					if step < len(steps) {
						err = steps[step]()
						step++
					}
					select {
					case engine.response <- err:
					case <-stop:
						return nil
					}
				case <-stop:
					return nil
				}
			}
		}
		if locked {
			if err := homepkg.WithAutonomyAdmissionLock(homePath, run); err != nil {
				select {
				case ready <- err:
				default:
				}
			}
		} else {
			_ = run()
		}
	}()
	if err := <-ready; err != nil {
		close(stop)
		t.Fatalf("start coordinated owner: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
	})
	return engine
}

func (e *coordinatedEngine) Capabilities() mem.EngineCapabilities {
	caps := e.base.Capabilities()
	caps.Name = "coordinated-owner-fixture"
	return caps
}

func (e *coordinatedEngine) Index(claims []mem.Claim, gen int64) error {
	return e.base.Index(claims, gen)
}

func (e *coordinatedEngine) Query(text, gate string, allowlist []string, k int) ([]mem.Hit, error) {
	e.request <- struct{}{}
	if err := <-e.response; err != nil {
		return nil, err
	}
	return e.base.Query(text, gate, allowlist, k)
}

func appendOwnerJSONL(path string, row []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(row); err != nil {
		return err
	}
	return file.Sync()
}
