package work

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
)

func TestProofResourceTimingHistorySeparatesRepositoriesAndPreservesUnknowns(t *testing.T) {
	s := newStore(t)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < proofResourceTimingsLimit+2; i++ {
		reservation := ProofResourceReservation{
			ID: fmt.Sprintf("pfr_sample_%d", i), WorkID: "work_sample",
			Profile: ProofResourceProfile{Repo: "/synthetic/repo", SourceSnapshot: &gitrepository.Snapshot{RepositoryFingerprint: fmt.Sprint(i % 2)}},
		}
		s.observeProofAdmission(reservation, time.Second, now, false)
	}
	samples, err := ReadProofResourceTimings(s.home, "1", 3)
	if err != nil || len(samples) != 3 {
		t.Fatalf("samples=%+v err=%v", samples, err)
	}
	for _, sample := range samples {
		if sample.RepositoryFingerprint != "1" || sample.ObservedReservedMS != nil || sample.QueueWaitMS == nil || *sample.QueueWaitMS != 0 {
			t.Fatalf("unexpected sample %+v", sample)
		}
	}
	id := samples[2].ReservationID
	s.observeProofRelease(id, now.Add(5*time.Second))
	s.observeProofRelease(id, now.Add(10*time.Second))
	samples, err = ReadProofResourceTimings(s.home, "1", 3)
	if err != nil || len(samples) != 3 || samples[2].ObservedReservedMS == nil || *samples[2].ObservedReservedMS != 5000 {
		t.Fatalf("release retry extended/duplicated timing: %+v %v", samples, err)
	}
	s.observeProofRelease("pfr_missing_admission", now)
	all, err := ReadProofResourceTimings(s.home, "", 100)
	if err != nil || len(all) != proofResourceTimingsLimit {
		t.Fatalf("history limit: %d %v", len(all), err)
	}
	missing := all[len(all)-1]
	if missing.AdmissionMS != nil || missing.QueueWaitMS != nil || missing.ObservedReservedMS != nil {
		t.Fatalf("missing observations became zero samples: %+v", missing)
	}
}

func TestProofResourceTimingFailureCannotChangeAdmissionOrRelease(t *testing.T) {
	s := newStore(t)
	// A directory in place of the advisory file makes reads and writes fail,
	// without interfering with any authority path.
	if err := os.Mkdir(filepath.Join(s.home, proofResourceTimingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	w := claimedProofWork(t, s, "proof despite telemetry failure", proofActorAlpha, fixedNow)
	acquired, err := reserveProofResource(t, s, w.ID, ProofResourceReserveInput{
		Actor: proofActorAlpha, TTL: time.Minute, Workspace: "/synthetic/repo-alpha",
		Command: "compound proof-check --out /synthetic/report.json",
	}, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := s.CompleteProofResource(w.ID, ProofResourceCompleteInput{
		Actor: proofActorAlpha, ReservationID: acquired.Reservation.ID, FailureReason: "synthetic runner failure",
	}, fixedNow.Add(2*time.Second))
	if err != nil || !completed.ResourceReleased || completed.Result.Status != ProofResourceFailed {
		t.Fatalf("advisory failure changed authoritative failure/release: %+v %v", completed, err)
	}
}

func TestReadProofResourceTimingsDoesNotCreateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	samples, err := ReadProofResourceTimings(root, "", 20)
	if err != nil || len(samples) != 0 {
		t.Fatalf("empty history: %+v %v", samples, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read created state: %v", err)
	}
}

func TestProofResourceTimingLockContentionDoesNotBlockAuthority(t *testing.T) {
	s := newStore(t)
	path := filepath.Join(s.home, "lock/proof-resource-timings.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	done := make(chan error, 1)
	go func() { done <- s.updateProofResourceTiming("pfr_contended", func(*ProofResourceTiming) {}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("contended advisory writer must skip its observation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("telemetry lock blocked its caller")
	}
}
