package work

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// reserveProofWithTakeoverObserver keeps the fixture's cooperative predecessor
// alive through reservation preparation and its termination grace. The caller
// owns transfer/rejection/terminality assertions; this helper owns lifetime only.
func reserveProofWithTakeoverObserver(
	reserve func() (ProofResourceReserveResult, error),
	observe func() (bool, error),
) (result ProofResourceReserveResult, err error) {
	stop := make(chan struct{})
	ready := make(chan struct{})
	done := make(chan struct{})
	var observerErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		close(ready)
		for {
			select {
			case <-stop:
				observerErr = errors.New("proof takeover observer stopped before holder transfer")
				return
			default:
			}
			observed, observeErr := observe()
			if observed || observeErr != nil {
				observerErr = observeErr
				return
			}
			select {
			case <-stop:
				observerErr = errors.New("proof takeover observer stopped before holder transfer")
				return
			case <-ticker.C:
			}
		}
	}()
	<-ready
	defer func() {
		close(stop)
		<-done
		err = errors.Join(err, observerErr)
	}()
	return reserve()
}

func TestProofTakeoverObserverSurvivesReservationPreparation(t *testing.T) {
	t.Parallel()
	firstPoll := make(chan struct{})
	acknowledged := make(chan struct{})
	var transferred atomic.Bool
	// Hold preparation beyond the old independent observer deadline. No host
	// load or production timeout is changed: the reservation owns this delay.
	result, err := reserveProofWithTakeoverObserver(func() (ProofResourceReserveResult, error) {
		<-firstPoll
		time.Sleep(2100 * time.Millisecond)
		transferred.Store(true)
		select {
		case <-acknowledged:
			return ProofResourceReserveResult{Reservation: ProofResourceReservation{ID: "pfr_observer_test"}}, nil
		case <-time.After(3 * time.Second):
			return ProofResourceReserveResult{}, errors.New("predecessor did not acknowledge after preparation")
		}
	}, func() (bool, error) {
		select {
		case <-firstPoll:
		default:
			close(firstPoll)
		}
		if !transferred.Load() {
			return false, nil
		}
		close(acknowledged)
		return true, nil
	})
	if err != nil || result.Reservation.ID != "pfr_observer_test" {
		t.Fatalf("observer must survive reservation preparation: result=%+v err=%v", result, err)
	}
}

func TestProofTakeoverObserverCancelsWhenReservationReturns(t *testing.T) {
	t.Parallel()
	for _, reserveErr := range []error{nil, errors.New("reservation refused before transfer")} {
		t.Run(fmtObserverReturnCase(reserveErr), func(t *testing.T) {
			polled := make(chan struct{})
			_, err := reserveProofWithTakeoverObserver(func() (ProofResourceReserveResult, error) {
				<-polled
				return ProofResourceReserveResult{}, reserveErr
			}, func() (bool, error) {
				select {
				case <-polled:
				default:
					close(polled)
				}
				return false, nil
			})
			if err == nil || !strings.Contains(err.Error(), "stopped before holder transfer") {
				t.Fatalf("missing transfer must cancel and join observer: %v", err)
			}
			if reserveErr != nil && !errors.Is(err, reserveErr) {
				t.Fatalf("reservation refusal lost: %v", err)
			}
		})
	}
}

func fmtObserverReturnCase(err error) string {
	if err != nil {
		return "reservation error"
	}
	return "reservation returned without transfer"
}

func TestProofTakeoverObserverJoinsAndPreservesErrors(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	observerErr := errors.New("predecessor remained nonterminal")
	reserveErr := errors.New("reservation rejected")
	var finished atomic.Bool
	_, err := reserveProofWithTakeoverObserver(func() (ProofResourceReserveResult, error) {
		<-entered
		close(release)
		return ProofResourceReserveResult{}, reserveErr
	}, func() (bool, error) {
		close(entered)
		<-release
		defer finished.Store(true)
		return false, observerErr
	})
	if !finished.Load() || !errors.Is(err, observerErr) || !errors.Is(err, reserveErr) {
		t.Fatalf("must join predecessor and retain both errors: finished=%v err=%v", finished.Load(), err)
	}
}

func TestProofTakeoverObserverJoinsOnReservationPanic(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	defer func() {
		if recovered := recover(); recovered != "reservation panic" || !finished.Load() {
			t.Fatalf("panic must join predecessor: recovered=%v finished=%v", recovered, finished.Load())
		}
	}()
	_, _ = reserveProofWithTakeoverObserver(func() (ProofResourceReserveResult, error) {
		<-entered
		close(release)
		panic("reservation panic")
	}, func() (bool, error) {
		close(entered)
		<-release
		defer finished.Store(true)
		return true, nil
	})
}
