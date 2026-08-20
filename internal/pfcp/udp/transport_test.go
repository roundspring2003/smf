package udp_test

import (
	"net"
	"testing"
	"time"

	legacyPfcp "github.com/free5gc/pfcp"
	"github.com/free5gc/pfcp/pfcpType"
	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/internal/pfcp/udp"
)

type fakeNewTransport struct {
	request  message.Message
	response message.Message
}

func (f *fakeNewTransport) SendRequest(
	request message.Message,
	_ *net.UDPAddr,
) (message.Message, error) {
	f.request = request
	return f.response, nil
}

func (f *fakeNewTransport) SendPfcpResponse(message.Message, *net.UDPAddr) error {
	return nil
}

func TestLegacySessionRequestUsesNewTransport(t *testing.T) {
	transport := &fakeNewTransport{}
	transport.response = message.NewSessionDeletionResponse(
		0,
		0,
		0x1001,
		7,
		0,
		ie.NewCause(ie.CauseRequestAccepted),
	)
	recoveryTime := time.Now().Truncate(time.Second)
	udp.UseTransport(transport, recoveryTime)
	t.Cleanup(udp.ClearTransport)

	legacyRequest := &legacyPfcp.Message{
		Header: legacyPfcp.Header{
			Version:        legacyPfcp.PfcpVersion,
			S:              legacyPfcp.SEID_PRESENT,
			MessageType:    legacyPfcp.PFCP_SESSION_DELETION_REQUEST,
			SEID:           0x2002,
			SequenceNumber: 9,
		},
		Body: legacyPfcp.PFCPSessionDeletionRequest{},
	}
	response, err := udp.SendPfcpRequest(legacyRequest, &net.UDPAddr{
		IP:   net.ParseIP("192.0.2.10"),
		Port: 8805,
	})
	if err != nil {
		t.Fatalf("SendPfcpRequest() error: %v", err)
	}
	if _, ok := transport.request.(*message.SessionDeletionRequest); !ok {
		t.Fatalf("new transport request = %T, want *message.SessionDeletionRequest", transport.request)
	}
	if got, want := transport.request.SEID(), uint64(0x2002); got != want {
		t.Fatalf("new transport request SEID = %#x, want %#x", got, want)
	}

	legacyResponse, ok := response.PfcpMessage.Body.(legacyPfcp.PFCPSessionDeletionResponse)
	if !ok {
		t.Fatalf("legacy response body = %T, want PFCPSessionDeletionResponse", response.PfcpMessage.Body)
	}
	if legacyResponse.Cause == nil || legacyResponse.Cause.CauseValue != pfcpType.CauseRequestAccepted {
		t.Fatalf("legacy response Cause = %+v, want Request Accepted", legacyResponse.Cause)
	}
	if got := udp.ServerStartTime.Unix(); got != recoveryTime.Unix() {
		t.Fatalf("ServerStartTime = %d, want %d", got, recoveryTime.Unix())
	}
}
