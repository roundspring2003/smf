package pfcp

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

func TestSendSessionEstablishmentRequest(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	const localSEID uint64 = 0x0102030405060708
	const remoteSEID uint64 = 0x1112131415161718
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		request, ok := received.(*message.SessionEstablishmentRequest)
		if !ok {
			return nil, fmt.Errorf("received %T, want *message.SessionEstablishmentRequest", received)
		}
		if request.NodeID == nil || request.CPFSEID == nil {
			return nil, fmt.Errorf("Session Establishment Request is missing mandatory IE(s)")
		}
		return message.NewSessionEstablishmentResponse(
			0, 0, localSEID, request.Sequence(), 0,
			ie.NewNodeIDHeuristic("127.0.0.2"),
			ie.NewCause(ie.CauseRequestAccepted),
			ie.NewFSEID(remoteSEID, net.ParseIP("127.0.0.2").To4(), nil),
		), nil
	})

	request := message.NewSessionEstablishmentRequest(
		0, 0, 0, 0, 0,
		ie.NewNodeIDHeuristic("127.0.0.1"),
		ie.NewFSEID(localSEID, net.ParseIP("127.0.0.1").To4(), nil),
	)
	response, err := s.SendSessionEstablishmentRequest(request, peerAddr, localSEID)
	if err != nil {
		t.Fatalf("SendSessionEstablishmentRequest() error: %v", err)
	}
	if err = <-peerDone; err != nil {
		t.Fatalf("UPF peer error: %v", err)
	}
	fields, err := response.UPFSEID.FSEID()
	if err != nil {
		t.Fatalf("decode UP F-SEID: %v", err)
	}
	if fields.SEID != remoteSEID {
		t.Fatalf("remote SEID = %d, want %d", fields.SEID, remoteSEID)
	}
}

func TestSendSessionEstablishmentRequestReturnsRejectedResponse(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	const localSEID uint64 = 41
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		return message.NewSessionEstablishmentResponse(
			0, 0, localSEID, received.Sequence(), 0,
			ie.NewNodeIDHeuristic("127.0.0.2"),
			ie.NewCause(ie.CauseNoResourcesAvailable),
		), nil
	})

	response, err := s.SendSessionEstablishmentRequest(
		message.NewSessionEstablishmentRequest(0, 0, 0, 0, 0),
		peerAddr,
		localSEID,
	)
	if err != nil {
		t.Fatalf("rejected response returned transport error: %v", err)
	}
	if err = <-peerDone; err != nil {
		t.Fatalf("UPF peer error: %v", err)
	}
	assertCause(t, response.Cause, ie.CauseNoResourcesAvailable)
}

func TestSendSessionEstablishmentRequestRejectsWrongSEID(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		return message.NewSessionEstablishmentResponse(
			0, 0, 99, received.Sequence(), 0,
			ie.NewNodeIDHeuristic("127.0.0.2"),
			ie.NewCause(ie.CauseRequestAccepted),
			ie.NewFSEID(77, net.ParseIP("127.0.0.2").To4(), nil),
		), nil
	})

	_, err := s.SendSessionEstablishmentRequest(
		message.NewSessionEstablishmentRequest(0, 0, 0, 0, 0),
		peerAddr,
		42,
	)
	if err == nil || !strings.Contains(err.Error(), "has SEID 99, want 42") {
		t.Fatalf("SendSessionEstablishmentRequest() error = %v, want SEID mismatch", err)
	}
	if peerErr := <-peerDone; peerErr != nil {
		t.Fatalf("UPF peer error: %v", peerErr)
	}
}

func TestSendSessionEstablishmentRequestRequiresUPFSEIDWhenAccepted(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	const localSEID uint64 = 42
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		return message.NewSessionEstablishmentResponse(
			0, 0, localSEID, received.Sequence(), 0,
			ie.NewNodeIDHeuristic("127.0.0.2"),
			ie.NewCause(ie.CauseRequestAccepted),
		), nil
	})

	_, err := s.SendSessionEstablishmentRequest(
		message.NewSessionEstablishmentRequest(0, 0, 0, 0, 0),
		peerAddr,
		localSEID,
	)
	if err == nil || !strings.Contains(err.Error(), "missing UP F-SEID") {
		t.Fatalf("SendSessionEstablishmentRequest() error = %v, want missing UP F-SEID", err)
	}
	if peerErr := <-peerDone; peerErr != nil {
		t.Fatalf("UPF peer error: %v", peerErr)
	}
}

func TestSendSessionDeletionRequest(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	const localSEID uint64 = 0x0102030405060708
	const remoteSEID uint64 = 0x1112131415161718
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		request, ok := received.(*message.SessionDeletionRequest)
		if !ok {
			return nil, fmt.Errorf("received %T, want *message.SessionDeletionRequest", received)
		}
		if request.SEID() != remoteSEID {
			return nil, fmt.Errorf("request SEID = %d, want %d", request.SEID(), remoteSEID)
		}
		return message.NewSessionDeletionResponse(
			0, 0, localSEID, request.Sequence(), 0,
			ie.NewCause(ie.CauseRequestAccepted),
		), nil
	})

	response, err := s.SendSessionDeletionRequest(
		message.NewSessionDeletionRequest(0, 0, remoteSEID, 0, 0),
		peerAddr,
		localSEID,
	)
	if err != nil {
		t.Fatalf("SendSessionDeletionRequest() error: %v", err)
	}
	if err = <-peerDone; err != nil {
		t.Fatalf("UPF peer error: %v", err)
	}
	assertCause(t, response.Cause, ie.CauseRequestAccepted)
}

func TestSendSessionDeletionRequestRequiresCause(t *testing.T) {
	s, _ := startTestPfcpServer(t)
	const localSEID uint64 = 42
	peerAddr, peerDone := startAssociationPeer(t, func(received message.Message) (message.Message, error) {
		return message.NewSessionDeletionResponse(
			0, 0, localSEID, received.Sequence(), 0,
		), nil
	})

	_, err := s.SendSessionDeletionRequest(
		message.NewSessionDeletionRequest(0, 0, 77, 0, 0),
		peerAddr,
		localSEID,
	)
	if err == nil || !strings.Contains(err.Error(), "missing Cause") {
		t.Fatalf("SendSessionDeletionRequest() error = %v, want missing Cause", err)
	}
	if peerErr := <-peerDone; peerErr != nil {
		t.Fatalf("UPF peer error: %v", peerErr)
	}
}
