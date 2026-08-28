package pfcp

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/pkg/factory"
)

type fakeSmf struct {
	cfg *factory.Config
}

func (f *fakeSmf) Config() *factory.Config { return f.cfg }

func startTestPfcpServer(t *testing.T) (*PfcpServer, *sync.WaitGroup) {
	t.Helper()

	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.port = 0
	s.dispatchWorkers = 2
	s.retransTimeout = 20 * time.Millisecond
	s.maxRetrans = 1

	var wg sync.WaitGroup
	if err := s.Run(&wg); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		wg.Wait()
	})
	return s, &wg
}

func TestPfcpServerHeartbeatRoundTrip(t *testing.T) {
	s, _ := startTestPfcpServer(t)

	peer, err := net.DialUDP("udp", nil, s.LocalAddr())
	if err != nil {
		t.Fatalf("DialUDP() error: %v", err)
	}
	defer peer.Close()

	req := message.NewHeartbeatRequest(1, ie.NewRecoveryTimeStamp(time.Now()), nil)
	b, err := req.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if _, err = peer.Write(b); err != nil {
		t.Fatalf("Write() error: %v", err)
	}

	if err = peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error: %v", err)
	}
	rbuf := make([]byte, 1500)
	n, err := peer.Read(rbuf)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	rsp, err := message.Parse(rbuf[:n])
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	heartbeatResponse, ok := rsp.(*message.HeartbeatResponse)
	if !ok {
		t.Fatalf("response = %T, want *message.HeartbeatResponse", rsp)
	}
	if heartbeatResponse.Sequence() != req.Sequence() {
		t.Fatalf("response sequence = %d, want %d", heartbeatResponse.Sequence(), req.Sequence())
	}
	if heartbeatResponse.RecoveryTimeStamp == nil {
		t.Fatal("Heartbeat Response is missing Recovery Time Stamp")
	}
	recoveryTime, err := heartbeatResponse.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		t.Fatalf("decode Recovery Time Stamp: %v", err)
	}
	if got, want := recoveryTime.Unix(), s.RecoveryTime().Unix(); got != want {
		t.Fatalf("Recovery Time Stamp = %d, want %d", got, want)
	}
}

func TestPfcpServerMatchesOutboundResponse(t *testing.T) {
	s, _ := startTestPfcpServer(t)

	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP() error: %v", err)
	}
	defer peer.Close()

	peerDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1500)
		n, addr, readErr := peer.ReadFromUDP(buf)
		if readErr != nil {
			peerDone <- readErr
			return
		}
		req, parseErr := message.Parse(buf[:n])
		if parseErr != nil {
			peerDone <- parseErr
			return
		}
		rsp := message.NewHeartbeatResponse(req.Sequence(), ie.NewRecoveryTimeStamp(time.Now()))
		b, marshalErr := rsp.Marshal()
		if marshalErr != nil {
			peerDone <- marshalErr
			return
		}
		_, writeErr := peer.WriteToUDP(b, addr)
		peerDone <- writeErr
	}()

	req := message.NewHeartbeatRequest(0, ie.NewRecoveryTimeStamp(time.Now()), nil)
	got, err := s.sendRequest(context.Background(), req, peer.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("sendRequest() error: %v", err)
	}
	if _, ok := got.(*message.HeartbeatResponse); !ok {
		t.Fatalf("response = %T, want *message.HeartbeatResponse", got)
	}
	if err = <-peerDone; err != nil {
		t.Fatalf("peer error: %v", err)
	}
}

func TestPfcpServerTxTimeout(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	blackHole := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	req := message.NewHeartbeatRequest(0, ie.NewRecoveryTimeStamp(time.Now()), nil)

	_, err := s.sendRequest(context.Background(), req, blackHole)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("sendRequest() error = %v, want timeout", err)
	}
}

func TestPfcpServerStopUnblocksPendingSenders(t *testing.T) {
	s, wg := startTestPfcpServer(t)
	blackHole := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	req := message.NewHeartbeatRequest(0, ie.NewRecoveryTimeStamp(time.Now()), nil)
	sendDone := make(chan error, 1)
	go func() {
		_, err := s.sendRequest(context.Background(), req, blackHole)
		sendDone <- err
	}()

	// Wait until the main loop has created the transaction before stopping.
	deadline := time.Now().Add(time.Second)
	for transactionCount(&s.txTrans) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if transactionCount(&s.txTrans) == 0 {
		t.Fatal("request transaction was not created")
	}

	s.Stop()
	wg.Wait()

	select {
	case err := <-sendDone:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("sendRequest() error after Stop = %v, want stopped", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop() did not unblock the pending sender")
	}

	// Stop is intentionally idempotent because app shutdown paths may converge.
	s.Stop()
}

func TestPfcpServerDuplicateRequestUsesCachedResponse(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	var dispatchCount atomic.Int32
	s.SetDispatch(func(msg message.Message, addr *net.UDPAddr) {
		dispatchCount.Add(1)
		if err := s.SendPfcpResponse(message.NewHeartbeatResponse(
			msg.Sequence(),
			ie.NewRecoveryTimeStamp(s.RecoveryTime()),
		), addr); err != nil {
			t.Errorf("SendPfcpResponse() error: %v", err)
		}
	})

	peer, err := net.DialUDP("udp", nil, s.LocalAddr())
	if err != nil {
		t.Fatalf("DialUDP() error: %v", err)
	}
	defer peer.Close()

	req := message.NewHeartbeatRequest(7, ie.NewRecoveryTimeStamp(time.Now()), nil)
	b, err := req.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	buf := make([]byte, 1500)
	for i := 0; i < 2; i++ {
		if _, err = peer.Write(b); err != nil {
			t.Fatalf("Write() #%d error: %v", i+1, err)
		}
		if err = peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err = peer.Read(buf); err != nil {
			t.Fatalf("Read() #%d error: %v", i+1, err)
		}
	}

	if got := dispatchCount.Load(); got != 1 {
		t.Fatalf("dispatch count = %d, want 1", got)
	}
}

func TestNewPfcpServerUsesConfiguredDispatchWorkers(t *testing.T) {
	cfg := &factory.Config{Configuration: &factory.Configuration{PFCP: &factory.PFCP{
		DispatchWorkerCount: 3,
	}}}
	s := NewPfcpServer(&fakeSmf{cfg: cfg}, "127.0.0.1")
	if got := s.dispatchWorkers; got != 3 {
		t.Fatalf("dispatch workers = %d, want 3", got)
	}
}

func TestPfcpServerDispatchConcurrencyIsBounded(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	// This test intentionally blocks all workers; keep queued jobs alive long
	// enough to verify the worker bound rather than the stale-request policy.
	s.retransTimeout = 5 * time.Second
	s.maxRetrans = 0
	workers := s.dispatchWorkers
	jobs := workers * 4
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	started := make(chan struct{}, jobs)
	var active atomic.Int32
	var maxActive atomic.Int32
	var completed atomic.Int32
	s.SetDispatch(func(message.Message, *net.UDPAddr) {
		current := active.Add(1)
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		completed.Add(1)
	})

	peer := net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: PfcpPort}
	for sequence := 1; sequence <= jobs; sequence++ {
		rx := s.newRxTransaction(&peer, uint32(sequence))
		s.dispCh <- RcvPfcpMsg{
			RemoteAddr: peer,
			Msg: message.NewHeartbeatRequest(
				uint32(sequence),
				ie.NewRecoveryTimeStamp(time.Now()),
				nil,
			),
			Rx: rx,
		}
	}

	for worker := 0; worker < workers; worker++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("only %d/%d dispatch workers started", worker, workers)
		}
	}
	select {
	case <-started:
		t.Fatalf("more than %d handlers ran concurrently", workers)
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	deadline := time.Now().Add(time.Second)
	for int(completed.Load()) != jobs && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := int(completed.Load()); got != jobs {
		t.Fatalf("completed handlers = %d, want %d", got, jobs)
	}
	if got := int(maxActive.Load()); got > workers {
		t.Fatalf("maximum concurrent handlers = %d, worker limit = %d", got, workers)
	}
}

func TestPfcpServerDropsStaleQueuedRequest(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.21"), Port: PfcpPort}
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	started := make(chan struct{}, s.dispatchWorkers)
	var dispatched atomic.Int32
	s.SetDispatch(func(message.Message, *net.UDPAddr) {
		dispatched.Add(1)
		started <- struct{}{}
		<-release
	})

	// Occupy every worker with claimed transactions. Claimed requests must not
	// expire even though their handler runs beyond the queue timeout.
	for sequence := uint32(1); sequence <= uint32(s.dispatchWorkers); sequence++ {
		s.transactionHandler(message.NewHeartbeatRequest(
			sequence, ie.NewRecoveryTimeStamp(time.Now()), nil,
		), peer)
	}
	for worker := 0; worker < s.dispatchWorkers; worker++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("only %d/%d dispatch workers started", worker, s.dispatchWorkers)
		}
	}

	staleSequence := uint32(s.dispatchWorkers + 1)
	s.transactionHandler(message.NewHeartbeatRequest(
		staleSequence, ie.NewRecoveryTimeStamp(time.Now()), nil,
	), peer)
	staleID := TransactionID(peer, staleSequence)
	deadline := time.Now().Add(time.Second)
	for {
		if _, found := s.loadRxTransaction(staleID); !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued request did not expire")
		}
		time.Sleep(time.Millisecond)
	}

	// The transactions already claimed by workers remain live while handlers
	// are running, instead of expiring under their feet.
	for sequence := uint32(1); sequence <= uint32(s.dispatchWorkers); sequence++ {
		if _, found := s.loadRxTransaction(TransactionID(peer, sequence)); !found {
			t.Fatalf("claimed request sequence %d expired during handling", sequence)
		}
	}

	releaseOnce.Do(func() { close(release) })
	deadline = time.Now().Add(time.Second)
	for len(s.dispCh) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.dispCh) != 0 {
		t.Fatal("stale request was not removed from the dispatch queue")
	}
	// Give the worker that dequeued the stale item time to perform the claim.
	time.Sleep(10 * time.Millisecond)
	if got, want := dispatched.Load(), int32(s.dispatchWorkers); got != want {
		t.Fatalf("dispatched handlers = %d, want %d; stale request executed", got, want)
	}
}

func TestPfcpServerDispatchOverflowReleasesRxTransaction(t *testing.T) {
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.dispCh = make(chan RcvPfcpMsg, 1)
	s.dispCh <- RcvPfcpMsg{
		Msg: message.NewHeartbeatRequest(1, ie.NewRecoveryTimeStamp(time.Now()), nil),
	}

	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: PfcpPort}
	req := message.NewHeartbeatRequest(2, ie.NewRecoveryTimeStamp(time.Now()), nil)
	s.transactionHandler(req, peer)

	if got := transactionCount(&s.rxTrans); got != 0 {
		t.Fatalf("Rx transactions after dispatch overflow = %d, want 0", got)
	}
}

func TestTransactionIDIgnoresUDPPort(t *testing.T) {
	first := &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 8805}
	second := &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 18805}

	firstID := TransactionID(first, 0x123456)
	secondID := TransactionID(second, 0x123456)
	if firstID != secondID {
		t.Fatalf("transaction IDs differ only because of UDP port: %q != %q", firstID, secondID)
	}

	otherPeer := &net.UDPAddr{IP: net.ParseIP("192.0.2.11"), Port: 8805}
	if firstID == TransactionID(otherPeer, 0x123456) {
		t.Fatal("transaction IDs for different peer IPs must differ")
	}
	if firstID == TransactionID(first, 0x123457) {
		t.Fatal("transaction IDs for different sequence numbers must differ")
	}
}

func transactionCount(transactions *sync.Map) int {
	count := 0
	transactions.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
