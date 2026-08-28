package pfcp

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/pkg/factory"
)

func startDispatcherRecoveryServer(t *testing.T, workers int) *PfcpServer {
	t.Helper()
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	s.port = 0
	s.dispatchWorkers = workers
	s.retransTimeout = 5 * time.Second
	s.maxRetrans = 0

	var wg sync.WaitGroup
	if err := s.Run(&wg); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	t.Cleanup(func() {
		s.Stop()
		wg.Wait()
	})
	return s
}

func TestDispatcherRequestPanicKeepsWorkerAvailable(t *testing.T) {
	s := startDispatcherRecoveryServer(t, 1)
	peer := &net.UDPAddr{IP: net.ParseIP("192.0.2.61"), Port: PfcpPort}
	secondHandled := make(chan struct{}, 1)

	s.SetDispatch(func(msg message.Message, _ *net.UDPAddr) {
		if msg.Sequence() == 1 {
			panic("injected request handler failure")
		}
		if msg.Sequence() == 2 {
			secondHandled <- struct{}{}
		}
	})

	s.transactionHandler(message.NewHeartbeatRequest(
		1, ie.NewRecoveryTimeStamp(time.Now()), nil,
	), peer)
	firstID := TransactionID(peer, 1)
	deadline := time.Now().Add(time.Second)
	for {
		if _, found := s.loadRxTransaction(firstID); !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("panicking request transaction was not aborted")
		}
		time.Sleep(time.Millisecond)
	}

	s.transactionHandler(message.NewHeartbeatRequest(
		2, ie.NewRecoveryTimeStamp(time.Now()), nil,
	), peer)
	select {
	case <-secondHandled:
	case <-time.After(time.Second):
		t.Fatal("the fixed dispatcher worker did not process a request after recovering from panic")
	}
}

func TestDispatcherPanicAfterResponseKeepsCacheAndWorker(t *testing.T) {
	s := startDispatcherRecoveryServer(t, 1)
	var dispatchCount atomic.Int32
	sendErrors := make(chan error, 2)
	s.SetDispatch(func(msg message.Message, addr *net.UDPAddr) {
		dispatchCount.Add(1)
		err := s.SendPfcpResponse(message.NewHeartbeatResponse(
			msg.Sequence(), ie.NewRecoveryTimeStamp(s.RecoveryTime()),
		), addr)
		if err != nil {
			sendErrors <- err
			return
		}
		if msg.Sequence() == 71 {
			panic("injected after-response failure")
		}
	})

	peer, err := net.DialUDP("udp", nil, s.LocalAddr())
	if err != nil {
		t.Fatalf("DialUDP() error: %v", err)
	}
	defer func() {
		if closeErr := peer.Close(); closeErr != nil {
			t.Errorf("Close() error: %v", closeErr)
		}
	}()

	firstPacket := marshalDispatcherHeartbeatRequest(t, 71)
	sendAndReadDispatcherHeartbeatResponse(t, peer, firstPacket, 71)

	// The panic happened after rx.send cached the response. A duplicate request
	// must reuse that cache instead of executing the handler again.
	sendAndReadDispatcherHeartbeatResponse(t, peer, firstPacket, 71)
	if got := dispatchCount.Load(); got != 1 {
		t.Fatalf("dispatch count after duplicate = %d, want 1", got)
	}

	// With exactly one worker this proves that the same fixed worker survived.
	sendAndReadDispatcherHeartbeatResponse(t, peer, marshalDispatcherHeartbeatRequest(t, 72), 72)
	if got := dispatchCount.Load(); got != 2 {
		t.Fatalf("dispatch count after next request = %d, want 2", got)
	}
	select {
	case err = <-sendErrors:
		t.Fatalf("SendPfcpResponse() error: %v", err)
	default:
	}
}

func TestDispatchIterationDoesNotStartWorkAfterStop(t *testing.T) {
	s := NewPfcpServer(&fakeSmf{cfg: &factory.Config{}}, "127.0.0.1")
	peer := net.UDPAddr{IP: net.ParseIP("192.0.2.62"), Port: PfcpPort}
	rx := s.newRxTransaction(&peer, 1)
	t.Cleanup(rx.stop)
	var dispatched atomic.Bool
	s.SetDispatch(func(message.Message, *net.UDPAddr) { dispatched.Store(true) })
	s.dispCh <- RcvPfcpMsg{
		RemoteAddr: peer,
		Msg: message.NewHeartbeatRequest(
			1, ie.NewRecoveryTimeStamp(time.Now()), nil,
		),
		Rx: rx,
	}

	s.Stop()
	if stopped := s.dispatchIteration(); !stopped {
		t.Fatal("dispatch iteration did not stop after stopCh was closed")
	}
	if dispatched.Load() {
		t.Fatal("dispatcher started a handler after stopCh was closed")
	}
}

func marshalDispatcherHeartbeatRequest(t *testing.T, sequence uint32) []byte {
	t.Helper()
	packet, err := message.NewHeartbeatRequest(
		sequence, ie.NewRecoveryTimeStamp(time.Now()), nil,
	).Marshal()
	if err != nil {
		t.Fatalf("marshal Heartbeat Request: %v", err)
	}
	return packet
}

func sendAndReadDispatcherHeartbeatResponse(
	t *testing.T,
	peer *net.UDPConn,
	packet []byte,
	sequence uint32,
) {
	t.Helper()
	if _, err := peer.Write(packet); err != nil {
		t.Fatalf("Write() sequence %d: %v", sequence, err)
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() sequence %d: %v", sequence, err)
	}
	buffer := make([]byte, 1500)
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatalf("Read() sequence %d: %v", sequence, err)
	}
	parsed, err := message.Parse(buffer[:n])
	if err != nil {
		t.Fatalf("parse response sequence %d: %v", sequence, err)
	}
	response, ok := parsed.(*message.HeartbeatResponse)
	if !ok {
		t.Fatalf("response sequence %d = %T, want *message.HeartbeatResponse", sequence, parsed)
	}
	if response.Sequence() != sequence {
		t.Fatalf("response sequence = %d, want %d", response.Sequence(), sequence)
	}
}
