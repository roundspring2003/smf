package pfcp

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

type fakeAssociationState struct {
	mu           sync.Mutex
	setupCause   uint8
	updateCause  uint8
	releaseCause uint8
	setupPeer    pfcptype.NodeID
	updatePeer   pfcptype.NodeID
	releasePeer  pfcptype.NodeID
	update       *message.AssociationUpdateRequest
	updateCalled bool
	recoveryTime time.Time
	after        func()
}

func (f *fakeAssociationState) SetupAssociation(
	peer pfcptype.NodeID,
	recoveryTime time.Time,
) (uint8, func()) {
	f.mu.Lock()
	f.setupPeer = peer
	f.recoveryTime = recoveryTime
	f.mu.Unlock()
	return f.setupCause, f.after
}

func (f *fakeAssociationState) UpdateAssociation(
	peer pfcptype.NodeID,
	request *message.AssociationUpdateRequest,
) (uint8, func()) {
	f.mu.Lock()
	f.updatePeer = peer
	f.update = request
	f.updateCalled = true
	f.mu.Unlock()
	return f.updateCause, f.after
}

func (f *fakeAssociationState) ReleaseAssociation(peer pfcptype.NodeID) uint8 {
	f.mu.Lock()
	f.releasePeer = peer
	f.mu.Unlock()
	return f.releaseCause
}

func TestHandleAssociationSetupRequestMissingMandatoryIE(t *testing.T) {
	s := NewPfcpServer(nil, "127.0.0.1")
	response, afterResponse := s.handleAssociationSetupRequest(message.NewAssociationSetupRequest(7))
	if afterResponse != nil {
		t.Fatal("afterResponse is non-nil for an invalid request")
	}
	assertCause(t, response.Cause, ie.CauseMandatoryIEMissing)
	if response.Sequence() != 7 {
		t.Fatalf("response sequence = %d, want 7", response.Sequence())
	}
}

func TestHandleAssociationSetupRequestWithoutLocalNodeIDDoesNotPanic(t *testing.T) {
	s := NewPfcpServer(nil, "")
	request := message.NewAssociationSetupRequest(
		8,
		ie.NewNodeIDHeuristic("192.0.2.10"),
		ie.NewRecoveryTimeStamp(time.Now()),
	)
	response, _ := s.handleAssociationSetupRequest(request)
	if response == nil {
		t.Fatal("handler returned nil response")
	}
	if response.NodeID != nil {
		t.Fatal("response unexpectedly contains a local Node ID")
	}
}

func TestHandleAssociationSetupRequestUsesStateManager(t *testing.T) {
	recoveryTime := time.Now().Add(-time.Minute).Truncate(time.Second)
	afterCalled := false
	state := &fakeAssociationState{
		setupCause: ie.CauseRequestAccepted,
		after:      func() { afterCalled = true },
	}
	s := NewPfcpServer(nil, "127.0.0.1")
	s.SetAssociationStateManager(state)
	request := message.NewAssociationSetupRequest(
		9,
		ie.NewNodeIDHeuristic("192.0.2.10"),
		ie.NewRecoveryTimeStamp(recoveryTime),
	)

	response, afterResponse := s.handleAssociationSetupRequest(request)
	assertCause(t, response.Cause, ie.CauseRequestAccepted)
	if afterResponse == nil {
		t.Fatal("accepted setup did not return afterResponse")
	}
	if afterCalled {
		t.Fatal("afterResponse ran before the caller invoked it")
	}
	if got := state.setupPeer.String(); got != "192.0.2.10" {
		t.Fatalf("setup peer = %q, want 192.0.2.10", got)
	}
	if got, want := state.recoveryTime.Unix(), recoveryTime.Unix(); got != want {
		t.Fatalf("recovery time = %d, want %d", got, want)
	}
	afterResponse()
	if !afterCalled {
		t.Fatal("afterResponse did not run")
	}
}

func TestAssociationSetupDispatchRunsAfterResponse(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	afterCalled := make(chan struct{}, 1)
	s.SetAssociationStateManager(&fakeAssociationState{
		setupCause: ie.CauseRequestAccepted,
		after:      func() { afterCalled <- struct{}{} },
	})

	peer, err := net.DialUDP("udp", nil, s.LocalAddr())
	if err != nil {
		t.Fatalf("DialUDP() error: %v", err)
	}
	defer peer.Close()
	request := message.NewAssociationSetupRequest(
		11,
		ie.NewNodeIDHeuristic("127.0.0.2"),
		ie.NewRecoveryTimeStamp(time.Now()),
	)
	packet, err := request.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if _, err = peer.Write(packet); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if err = peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	responsePacket := make([]byte, 1500)
	n, err := peer.Read(responsePacket)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	parsed, err := message.Parse(responsePacket[:n])
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	response, ok := parsed.(*message.AssociationSetupResponse)
	if !ok {
		t.Fatalf("response = %T, want *message.AssociationSetupResponse", parsed)
	}
	assertCause(t, response.Cause, ie.CauseRequestAccepted)
	select {
	case <-afterCalled:
	case <-time.After(time.Second):
		t.Fatal("afterResponse was not run after response send")
	}
}

func TestHandleAssociationUpdateRequestMissingNodeID(t *testing.T) {
	s := NewPfcpServer(nil, "127.0.0.1")
	response, afterResponse := s.handleAssociationUpdateRequest(
		message.NewAssociationUpdateRequest(12),
	)
	if afterResponse != nil {
		t.Fatal("afterResponse is non-nil for an invalid request")
	}
	assertCause(t, response.Cause, ie.CauseMandatoryIEMissing)
	if response.Sequence() != 12 {
		t.Fatalf("response sequence = %d, want 12", response.Sequence())
	}
}

func TestHandleAssociationUpdateRequestUsesStateManager(t *testing.T) {
	period := 30 * time.Second
	afterCalled := false
	state := &fakeAssociationState{
		updateCause: ie.CauseRequestAccepted,
		after:       func() { afterCalled = true },
	}
	s := NewPfcpServer(nil, "127.0.0.1")
	s.SetAssociationStateManager(state)
	request := message.NewAssociationUpdateRequest(
		14,
		ie.NewNodeIDHeuristic("192.0.2.20"),
		ie.NewUPFunctionFeatures(0x11, 0x22),
		ie.NewPFCPAssociationReleaseRequest(1, 1),
		ie.NewGracefulReleasePeriod(period),
		ie.NewPFCPAUReqFlags(0x01),
	)

	response, afterResponse := s.handleAssociationUpdateRequest(request)
	assertCause(t, response.Cause, ie.CauseRequestAccepted)
	if response.NodeID == nil || response.CPFunctionFeatures == nil {
		t.Fatal("accepted response is missing local Node ID or CP Function Features")
	}
	if afterResponse == nil {
		t.Fatal("accepted release update did not return afterResponse")
	}
	if afterCalled {
		t.Fatal("afterResponse ran before the caller invoked it")
	}
	if got := state.updatePeer.String(); got != "192.0.2.20" {
		t.Fatalf("update peer = %q, want 192.0.2.20", got)
	}
	if state.update != request {
		t.Fatal("state manager did not receive the original go-pfcp Association Update Request")
	}
	features, err := state.update.UPFunctionFeatures.UPFunctionFeatures()
	if err != nil || len(features) != 2 || features[0] != 0x11 || features[1] != 0x22 {
		t.Fatalf("UP Function Features = %v, err=%v; want [0x11 0x22]", features, err)
	}
	if !state.update.PFCPAssociationReleaseRequest.HasSARR() ||
		!state.update.PFCPAssociationReleaseRequest.HasURSS() ||
		!state.update.PFCPAUReqFlags.HasPARPS() {
		t.Fatal("state manager request is missing SARR, URSS, or PARPS")
	}
	gotPeriod, err := state.update.GracefulReleasePeriod.GracefulReleasePeriod()
	if err != nil || gotPeriod != period {
		t.Fatalf("Graceful Release Period = %v, err=%v; want %s", gotPeriod, err, period)
	}
	afterResponse()
	if !afterCalled {
		t.Fatal("afterResponse did not run")
	}
}

func TestHandleAssociationUpdateRequestRejectsGracePeriodWithoutReleaseIE(t *testing.T) {
	state := &fakeAssociationState{updateCause: ie.CauseRequestAccepted}
	s := NewPfcpServer(nil, "127.0.0.1")
	s.SetAssociationStateManager(state)
	response, afterResponse := s.handleAssociationUpdateRequest(
		message.NewAssociationUpdateRequest(
			15,
			ie.NewNodeIDHeuristic("192.0.2.21"),
			ie.NewGracefulReleasePeriod(time.Minute),
		),
	)
	if afterResponse != nil {
		t.Fatal("afterResponse is non-nil for an invalid request")
	}
	assertCause(t, response.Cause, ie.CauseConditionalIEMissing)
	if state.updateCalled {
		t.Fatal("state manager was called for invalid request")
	}
}

func TestAssociationUpdateDispatchRunsAfterResponse(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	afterCalled := make(chan struct{}, 1)
	s.SetAssociationStateManager(&fakeAssociationState{
		updateCause: ie.CauseRequestAccepted,
		after:       func() { afterCalled <- struct{}{} },
	})

	peer, err := net.DialUDP("udp", nil, s.LocalAddr())
	if err != nil {
		t.Fatalf("DialUDP() error: %v", err)
	}
	defer peer.Close()
	request := message.NewAssociationUpdateRequest(
		16,
		ie.NewNodeIDHeuristic("127.0.0.2"),
		ie.NewPFCPAssociationReleaseRequest(1, 0),
	)
	packet, err := request.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	if _, err = peer.Write(packet); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if err = peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	responsePacket := make([]byte, 1500)
	n, err := peer.Read(responsePacket)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	parsed, err := message.Parse(responsePacket[:n])
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	response, ok := parsed.(*message.AssociationUpdateResponse)
	if !ok {
		t.Fatalf("response = %T, want *message.AssociationUpdateResponse", parsed)
	}
	assertCause(t, response.Cause, ie.CauseRequestAccepted)
	select {
	case <-afterCalled:
	case <-time.After(time.Second):
		t.Fatal("afterResponse was not run after response send")
	}
}

func TestHandleAssociationReleaseRequest(t *testing.T) {
	state := &fakeAssociationState{releaseCause: ie.CauseRequestAccepted}
	s := NewPfcpServer(nil, "127.0.0.1")
	s.SetAssociationStateManager(state)
	response := s.handleAssociationReleaseRequest(message.NewAssociationReleaseRequest(
		13,
		ie.NewNodeIDHeuristic("upf.example.net"),
	))
	assertCause(t, response.Cause, ie.CauseRequestAccepted)
	if got := state.releasePeer.String(); got != "upf.example.net" {
		t.Fatalf("release peer = %q, want upf.example.net", got)
	}
}

func TestSendAssociationSetupRequest(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	upfRecoveryTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		request, ok := received.(*message.AssociationSetupRequest)
		if !ok {
			return nil, fmt.Errorf("received %T, want *message.AssociationSetupRequest", received)
		}
		if request.NodeID == nil || request.RecoveryTimeStamp == nil {
			return nil, fmt.Errorf("Association Setup Request is missing mandatory IE(s)")
		}
		return message.NewAssociationSetupResponse(
			request.Sequence(),
			ie.NewNodeIDHeuristic("127.0.0.2"),
			ie.NewCause(ie.CauseRequestAccepted),
			ie.NewRecoveryTimeStamp(upfRecoveryTime),
		), nil
	})

	response, err := s.SendAssociationSetupRequest(context.Background(), peerAddr)
	if err != nil {
		t.Fatalf("SendAssociationSetupRequest() error: %v", err)
	}
	if err = <-peerDone; err != nil {
		t.Fatalf("UPF peer error: %v", err)
	}
	recoveryTime, err := response.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		t.Fatalf("decode Recovery Time Stamp: %v", err)
	}
	if got, want := recoveryTime.Unix(), upfRecoveryTime.Unix(); got != want {
		t.Fatalf("UPF recovery time = %d, want %d", got, want)
	}
}

func TestSendAssociationSetupRequestRejectsMissingMandatoryIE(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		return message.NewAssociationSetupResponse(
			received.Sequence(),
			ie.NewNodeIDHeuristic("127.0.0.2"),
			ie.NewCause(ie.CauseRequestAccepted),
		), nil
	})
	_, err := s.SendAssociationSetupRequest(context.Background(), peerAddr)
	if err == nil || !strings.Contains(err.Error(), "missing mandatory") {
		t.Fatalf("SendAssociationSetupRequest() error = %v, want mandatory IE error", err)
	}
	if peerErr := <-peerDone; peerErr != nil {
		t.Fatalf("UPF peer error: %v", peerErr)
	}
}

func TestSendAssociationReleaseRequest(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		request, ok := received.(*message.AssociationReleaseRequest)
		if !ok {
			return nil, fmt.Errorf("received %T, want *message.AssociationReleaseRequest", received)
		}
		return message.NewAssociationReleaseResponse(
			request.Sequence(),
			ie.NewNodeIDHeuristic("127.0.0.2"),
			ie.NewCause(ie.CauseRequestAccepted),
		), nil
	})
	if _, err := s.SendAssociationReleaseRequest(context.Background(), peerAddr); err != nil {
		t.Fatalf("SendAssociationReleaseRequest() error: %v", err)
	}
	if err := <-peerDone; err != nil {
		t.Fatalf("UPF peer error: %v", err)
	}
}

func TestNodeIDFromIE(t *testing.T) {
	tests := []struct {
		name string
		ie   *ie.IE
		want string
	}{
		{name: "IPv4", ie: ie.NewNodeIDHeuristic("192.0.2.1"), want: "192.0.2.1"},
		{name: "IPv6", ie: ie.NewNodeIDHeuristic("2001:db8::1"), want: "2001:db8::1"},
		{name: "FQDN", ie: ie.NewNodeIDHeuristic("upf.example.net"), want: "upf.example.net"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nodeID, err := nodeIDFromIE(test.ie)
			if err != nil {
				t.Fatalf("nodeIDFromIE() error: %v", err)
			}
			if got := nodeID.String(); got != test.want {
				t.Fatalf("node ID = %q, want %q", got, test.want)
			}
		})
	}
}

func startAssociationPeer(
	t *testing.T,
	buildResponse func(message.Message) (message.Message, error),
) (*net.UDPAddr, <-chan error) {
	t.Helper()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP() error: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	done := make(chan error, 1)
	go func() {
		packet := make([]byte, 1500)
		n, smfAddr, readErr := peer.ReadFromUDP(packet)
		if readErr != nil {
			done <- readErr
			return
		}
		request, parseErr := message.Parse(packet[:n])
		if parseErr != nil {
			done <- parseErr
			return
		}
		response, buildErr := buildResponse(request)
		if buildErr != nil {
			done <- buildErr
			return
		}
		responsePacket := make([]byte, response.MarshalLen())
		if marshalErr := response.MarshalTo(responsePacket); marshalErr != nil {
			done <- marshalErr
			return
		}
		_, writeErr := peer.WriteToUDP(responsePacket, smfAddr)
		done <- writeErr
	}()
	return peer.LocalAddr().(*net.UDPAddr), done
}

func assertCause(t *testing.T, causeIE *ie.IE, want uint8) {
	t.Helper()
	if causeIE == nil {
		t.Fatal("Cause IE is nil")
	}
	got, err := causeIE.Cause()
	if err != nil {
		t.Fatalf("Cause() error: %v", err)
	}
	if got != want {
		t.Fatalf("Cause = %d, want %d", got, want)
	}
}
