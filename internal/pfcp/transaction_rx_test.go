package pfcp

import (
	"net"
	"testing"
	"time"

	"github.com/free5gc/smf/pkg/factory"
)

func TestRxTimerPanicAbortsOnlyCurrentTransaction(t *testing.T) {
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.retransTimeout = time.Hour
	s.maxRetrans = 0
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.41"), Port: PfcpPort}

	failed := s.newRxTransaction(peer, 1)
	unrelated := s.newRxTransaction(peer, 2)
	t.Cleanup(unrelated.stop)
	generation := rxGeneration(failed)

	panicRxTimer(failed, generation)

	if _, found := s.loadRxTransaction(failed.id); found {
		t.Fatal("panicking Rx timer transaction was not removed")
	}
	failed.mu.Lock()
	failedDone, failedTimer := failed.done, failed.timer
	failed.mu.Unlock()
	if !failedDone || failedTimer != nil {
		t.Fatalf("failed transaction state: done=%t timer=%v, want done with no timer", failedDone, failedTimer)
	}
	if got, found := s.loadRxTransaction(unrelated.id); !found || got != unrelated {
		t.Fatal("unrelated Rx transaction was removed by timer panic recovery")
	}
}

func TestStaleRxTimerPanicDoesNotAbortResetGeneration(t *testing.T) {
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.retransTimeout = time.Hour
	s.maxRetrans = 0
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.42"), Port: PfcpPort}
	rx := s.newRxTransaction(peer, 1)
	t.Cleanup(rx.stop)
	staleGeneration := rxGeneration(rx)

	rx.mu.Lock()
	rx.resetTimerLocked()
	currentGeneration := rx.timerGeneration
	rx.mu.Unlock()
	if currentGeneration == staleGeneration {
		t.Fatal("reset did not advance timer generation")
	}

	panicRxTimer(rx, staleGeneration)

	if got, found := s.loadRxTransaction(rx.id); !found || got != rx {
		t.Fatal("stale timer panic removed the current Rx transaction generation")
	}
	rx.mu.Lock()
	done := rx.done
	rx.mu.Unlock()
	if done {
		t.Fatal("stale timer panic marked the current Rx transaction done")
	}
}

func TestOldRxTimerPanicDoesNotDeleteReplacementTransaction(t *testing.T) {
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.retransTimeout = time.Hour
	s.maxRetrans = 0
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.43"), Port: PfcpPort}
	old := s.newRxTransaction(peer, 1)
	generation := rxGeneration(old)

	replacement := &RxTransaction{server: s, destAddr: peer, seq: old.seq, id: old.id}
	s.rxTrans.Store(old.id, replacement)
	t.Cleanup(func() { s.rxTrans.CompareAndDelete(replacement.id, replacement) })

	panicRxTimer(old, generation)

	if got, found := s.loadRxTransaction(old.id); !found || got != replacement {
		t.Fatal("old timer panic deleted the replacement Rx transaction")
	}
}

func rxGeneration(rx *RxTransaction) uint64 {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	return rx.timerGeneration
}

func panicRxTimer(rx *RxTransaction, generation uint64) {
	defer rx.recoverTimerPanic(generation)
	panic("injected Rx timer failure")
}
