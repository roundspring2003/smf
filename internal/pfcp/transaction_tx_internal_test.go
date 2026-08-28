package pfcp

import (
	"net"
	"testing"
	"time"

	"github.com/free5gc/smf/pkg/factory"
)

func TestTxTimerPanicAbortsOnlyCurrentTransaction(t *testing.T) {
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.retransTimeout = time.Hour
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.51"), Port: PfcpPort}

	response := make(chan RcvPfcpMsg, 1)
	failed, err := s.newTxTransaction(peer, response)
	if err != nil {
		t.Fatalf("newTxTransaction() error: %v", err)
	}
	startTxTimer(failed)
	unrelated, err := s.newTxTransaction(peer, nil)
	if err != nil {
		t.Fatalf("new unrelated TxTransaction: %v", err)
	}
	t.Cleanup(func() { unrelated.complete(RcvPfcpMsg{Msg: nil}) })
	generation := txGeneration(failed)

	panicTxTimer(failed, generation)

	if _, found := s.loadTxTransaction(failed.id); found {
		t.Fatal("panicking Tx timer transaction was not removed")
	}
	if txSequenceInUse(s, failed.seq) {
		t.Fatal("panicking Tx timer transaction did not release its sequence")
	}
	select {
	case notification, ok := <-response:
		if !ok {
			t.Fatal("response channel closed without a timeout notification")
		}
		if notification.Msg != nil {
			t.Fatalf("panic notification Msg = %T, want nil", notification.Msg)
		}
	case <-time.After(time.Second):
		t.Fatal("Tx timer panic did not unblock the response waiter")
	}
	if got, found := s.loadTxTransaction(unrelated.id); !found || got != unrelated {
		t.Fatal("unrelated Tx transaction was removed by timer panic recovery")
	}
}

func TestStaleTxTimerPanicDoesNotAbortResetGeneration(t *testing.T) {
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.retransTimeout = time.Hour
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.52"), Port: PfcpPort}
	response := make(chan RcvPfcpMsg, 1)
	tx, err := s.newTxTransaction(peer, response)
	if err != nil {
		t.Fatalf("newTxTransaction() error: %v", err)
	}
	startTxTimer(tx)
	t.Cleanup(func() { tx.complete(RcvPfcpMsg{Msg: nil}) })
	staleGeneration := txGeneration(tx)

	tx.mu.Lock()
	tx.resetTimerLocked()
	currentGeneration := tx.timerGeneration
	tx.mu.Unlock()
	if currentGeneration == staleGeneration {
		t.Fatal("reset did not advance Tx timer generation")
	}

	panicTxTimer(tx, staleGeneration)

	if got, found := s.loadTxTransaction(tx.id); !found || got != tx {
		t.Fatal("stale Tx timer panic removed the current transaction generation")
	}
	tx.mu.Lock()
	done := tx.done
	tx.mu.Unlock()
	if done {
		t.Fatal("stale Tx timer panic marked the current transaction done")
	}
	select {
	case <-response:
		t.Fatal("stale Tx timer panic notified the response waiter")
	default:
	}
}

func TestOldTxTimerPanicDoesNotDeleteReplacementTransaction(t *testing.T) {
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.retransTimeout = time.Hour
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.53"), Port: PfcpPort}
	old, err := s.newTxTransaction(peer, make(chan RcvPfcpMsg, 1))
	if err != nil {
		t.Fatalf("newTxTransaction() error: %v", err)
	}
	startTxTimer(old)
	generation := txGeneration(old)

	replacement := &TxTransaction{server: s, destAddr: peer, seq: old.seq, id: old.id}
	s.txTrans.Store(old.id, replacement)
	t.Cleanup(func() { replacement.complete(RcvPfcpMsg{Msg: nil}) })

	panicTxTimer(old, generation)

	if got, found := s.loadTxTransaction(old.id); !found || got != replacement {
		t.Fatal("old Tx timer panic deleted the replacement transaction")
	}
	if !txSequenceInUse(s, old.seq) {
		t.Fatal("old Tx timer panic freed the replacement transaction sequence")
	}
}

func startTxTimer(tx *TxTransaction) {
	tx.mu.Lock()
	tx.resetTimerLocked()
	tx.mu.Unlock()
}

func txGeneration(tx *TxTransaction) uint64 {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	return tx.timerGeneration
}

func txSequenceInUse(s *PfcpServer, sequence uint32) bool {
	s.seqAlloc.mu.Lock()
	defer s.seqAlloc.mu.Unlock()
	_, found := s.seqAlloc.inUse[sequence]
	return found
}

func panicTxTimer(tx *TxTransaction, generation uint64) {
	defer tx.recoverTimerPanic(generation)
	panic("injected Tx timer failure")
}
