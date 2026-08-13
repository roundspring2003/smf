package pfcp

import (
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/wmnsk/go-pfcp/message"
)

type RcvPfcpMsg struct {
	RemoteAddr net.UDPAddr
	Msg        message.Message
	Rx         *RxTransaction
}

type TransmitMessage struct {
	Msg        message.Message
	RemoteAddr *net.UDPAddr
	TrType     TransType
	RspCh      chan RcvPfcpMsg
	Done       chan error
}

type TxTransaction struct {
	mu sync.Mutex

	server         *PfcpServer
	destAddr       *net.UDPAddr
	seq            uint32
	id             string
	retransTimeout time.Duration
	maxRetrans     uint8
	req            message.Message
	rspCh          chan RcvPfcpMsg
	msgBuf         []byte
	timer          *time.Timer
	retransCount   uint8
	done           bool
}

type RxTransaction struct {
	mu sync.Mutex

	server          *PfcpServer
	destAddr        *net.UDPAddr
	seq             uint32
	id              string
	timeout         time.Duration
	rsp             message.Message
	msgBuf          []byte
	timer           *time.Timer
	timerGeneration uint64
	handling        bool
	done            bool
}

// TransactionID follows Saviah's PFCP peer identity: remote IP plus sequence.
// The UDP port is deliberately ignored so a response can still match its
// request if the peer's source port changes.
func TransactionID(remote *net.UDPAddr, sequence uint32) string {
	if remote == nil {
		return fmt.Sprintf("<nil>-%#x", sequence)
	}
	return fmt.Sprintf("%s-%#x", remote.IP.String(), sequence)
}

func (s *PfcpServer) newTxTransaction(
	destination *net.UDPAddr,
	response chan RcvPfcpMsg,
) (*TxTransaction, error) {
	sequence, err := s.seqAlloc.Allocate()
	if err != nil {
		return nil, fmt.Errorf("allocate PFCP sequence: %w", err)
	}
	tx := &TxTransaction{
		server:         s,
		destAddr:       destination,
		seq:            sequence,
		id:             TransactionID(destination, sequence),
		retransTimeout: s.retransTimeout,
		maxRetrans:     s.maxRetrans,
		rspCh:          response,
	}
	s.txTrans.Store(tx.id, tx)
	return tx, nil
}

func (s *PfcpServer) deleteTxTransaction(tx *TxTransaction) {
	s.txTrans.Delete(tx.id)
	s.seqAlloc.Free(tx.seq)
}

func (tx *TxTransaction) send(request message.Message) error {
	request.SetSequenceNumber(tx.seq)
	packet := make([]byte, request.MarshalLen())
	if err := request.MarshalTo(packet); err != nil {
		return fmt.Errorf("marshal PFCP request transaction %q: %w", tx.id, err)
	}

	tx.mu.Lock()
	if tx.done {
		tx.mu.Unlock()
		return fmt.Errorf("PFCP request transaction %q is already complete", tx.id)
	}
	tx.req = request
	tx.msgBuf = packet
	tx.timer = tx.startTimer()
	tx.mu.Unlock()

	if _, err := tx.server.conn.WriteToUDP(packet, tx.destAddr); err != nil {
		return fmt.Errorf("send PFCP request transaction %q: %w", tx.id, err)
	}
	return nil
}

func (tx *TxTransaction) complete(notification RcvPfcpMsg) {
	tx.mu.Lock()
	if tx.done {
		tx.mu.Unlock()
		return
	}
	tx.done = true
	if tx.timer != nil {
		tx.timer.Stop()
		tx.timer = nil
	}
	response := tx.rspCh
	tx.rspCh = nil
	tx.mu.Unlock()

	tx.server.deleteTxTransaction(tx)
	if response != nil {
		response <- notification
		close(response)
	}
}

func (tx *TxTransaction) handleTimeout() {
	tx.mu.Lock()
	if tx.done {
		tx.mu.Unlock()
		return
	}
	if tx.req == nil || tx.retransCount >= tx.maxRetrans {
		tx.mu.Unlock()
		tx.complete(RcvPfcpMsg{Msg: nil})
		return
	}

	tx.retransCount++
	packet := append([]byte(nil), tx.msgBuf...)
	destination := tx.destAddr
	tx.timer = tx.startTimer()
	tx.mu.Unlock()

	if _, err := tx.server.conn.WriteToUDP(packet, destination); err != nil {
		tx.server.log.Errorf("retransmit PFCP request %q (#%d): %v", tx.id, tx.retransCount, err)
	}
}

func (tx *TxTransaction) startTimer() *time.Timer {
	return time.AfterFunc(tx.retransTimeout, func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				tx.server.log.Fatalf("panic in PFCP request timer: %v\n%s", recovered, debug.Stack())
			}
		}()
		tx.server.transactionTimeout(TX, tx.id)
	})
}

func (s *PfcpServer) transactionTimeout(transactionType TransType, id string) {
	if transactionType == TX {
		tx, found := s.loadTxTransaction(id)
		if !found {
			return
		}
		if isServerStopped(s.stopCh) {
			tx.complete(RcvPfcpMsg{Msg: nil})
			return
		}
		tx.handleTimeout()
	}
}

func (s *PfcpServer) newRxTransaction(destination *net.UDPAddr, sequence uint32) *RxTransaction {
	rx := &RxTransaction{
		server:   s,
		destAddr: destination,
		seq:      sequence,
		id:       TransactionID(destination, sequence),
		timeout:  s.retransTimeout * time.Duration(s.maxRetrans+1),
	}
	s.rxTrans.Store(rx.id, rx)
	rx.mu.Lock()
	rx.resetTimerLocked()
	rx.mu.Unlock()
	return rx
}

func (s *PfcpServer) deleteRxTransaction(rx *RxTransaction) {
	// A delayed callback belonging to an old transaction must not delete a
	// replacement that later reused the same remote-IP/sequence key.
	s.rxTrans.CompareAndDelete(rx.id, rx)
}

// beginHandling claims a queued request for exactly one dispatcher worker. If
// its queue timer has already expired, the stale request must not execute.
func (rx *RxTransaction) beginHandling() bool {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	if rx.done || rx.handling {
		return false
	}
	rx.handling = true
	rx.invalidateTimerLocked()
	return true
}

// finishHandling keeps an unanswered request only for one more response-cache
// interval. A successful send already reset the timer from response time.
func (rx *RxTransaction) finishHandling() {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	if rx.done || !rx.handling {
		return
	}
	rx.handling = false
	rx.resetTimerLocked()
}

func (rx *RxTransaction) send(response message.Message) error {
	packet := make([]byte, response.MarshalLen())
	if err := response.MarshalTo(packet); err != nil {
		return fmt.Errorf("marshal PFCP response transaction %q: %w", rx.id, err)
	}

	rx.mu.Lock()
	if rx.done {
		rx.mu.Unlock()
		return fmt.Errorf("PFCP response transaction %q is already complete", rx.id)
	}
	rx.rsp = response
	rx.msgBuf = packet
	rx.handling = false
	rx.resetTimerLocked()
	rx.mu.Unlock()

	if _, err := rx.server.conn.WriteToUDP(packet, rx.destAddr); err != nil {
		return fmt.Errorf("send PFCP response transaction %q: %w", rx.id, err)
	}
	return nil
}

// recv returns true only for the first request. A duplicate request reuses the
// cached response and must not run the handler again.
func (rx *RxTransaction) recv(found bool) (bool, error) {
	if !found {
		return true, nil
	}

	rx.mu.Lock()
	if rx.done {
		rx.mu.Unlock()
		return false, nil
	}
	packet := append([]byte(nil), rx.msgBuf...)
	destination := rx.destAddr
	rx.mu.Unlock()
	if len(packet) == 0 {
		return false, nil
	}
	if _, err := rx.server.conn.WriteToUDP(packet, destination); err != nil {
		return false, fmt.Errorf("retransmit PFCP response transaction %q: %w", rx.id, err)
	}
	return false, nil
}

func (rx *RxTransaction) stop() {
	rx.mu.Lock()
	if rx.done {
		rx.mu.Unlock()
		return
	}
	rx.done = true
	rx.handling = false
	rx.invalidateTimerLocked()
	rx.mu.Unlock()
	rx.server.deleteRxTransaction(rx)
}

func (rx *RxTransaction) expire(generation uint64) {
	shouldDelete := false
	func() {
		rx.mu.Lock()
		defer rx.mu.Unlock()
		if rx.done || rx.handling || generation != rx.timerGeneration {
			return
		}
		rx.done = true
		rx.timer = nil
		shouldDelete = true
	}()
	if shouldDelete {
		rx.server.deleteRxTransaction(rx)
	}
}

// recoverTimerPanic contains a timer failure within the transaction that owns
// the callback. A single Rx cache/queue cleanup failure does not invalidate the
// PFCP server or unrelated SMF sessions.
func (rx *RxTransaction) recoverTimerPanic(generation uint64) {
	if recovered := recover(); recovered != nil {
		rx.server.log.Errorf("panic in PFCP response timer transaction %q: %v\n%s",
			rx.id, recovered, debug.Stack())
		rx.abortTimerGeneration(generation)
	}
}

// abortTimerGeneration performs best-effort cleanup after a timer panic. The
// generation and handling checks prevent an obsolete callback from deleting a
// reset response cache or a request already claimed by a worker.
func (rx *RxTransaction) abortTimerGeneration(generation uint64) {
	shouldDelete := false
	func() {
		rx.mu.Lock()
		defer rx.mu.Unlock()
		if rx.handling || generation != rx.timerGeneration {
			return
		}
		rx.done = true
		rx.handling = false
		rx.invalidateTimerLocked()
		shouldDelete = true
	}()
	if shouldDelete {
		rx.server.deleteRxTransaction(rx)
	}
}

func (rx *RxTransaction) invalidateTimerLocked() {
	rx.timerGeneration++
	if rx.timer != nil {
		rx.timer.Stop()
		rx.timer = nil
	}
}

func (rx *RxTransaction) resetTimerLocked() {
	rx.invalidateTimerLocked()
	generation := rx.timerGeneration
	rx.timer = time.AfterFunc(rx.timeout, func() {
		defer rx.recoverTimerPanic(generation)
		rx.expire(generation)
	})
}
