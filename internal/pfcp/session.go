package pfcp

import (
	"fmt"
	"net"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

// SendSessionEstablishmentRequest sends one already-built concrete go-pfcp
// request through the server-owned transaction layer. A rejected response is a
// valid protocol result and is returned to the processor; malformed responses
// are transport/protocol errors.
func (s *PfcpServer) SendSessionEstablishmentRequest(
	request *message.SessionEstablishmentRequest,
	addr *net.UDPAddr,
	localSEID uint64,
) (*message.SessionEstablishmentResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("send PFCP Session Establishment Request: nil PFCP server")
	}
	if request == nil {
		return nil, fmt.Errorf("send PFCP Session Establishment Request: nil request")
	}
	if addr == nil || addr.IP == nil || addr.IP.IsUnspecified() {
		return nil, fmt.Errorf("send PFCP Session Establishment Request: no destination IP address")
	}
	if localSEID == 0 {
		return nil, fmt.Errorf("send PFCP Session Establishment Request: local SEID is zero")
	}

	received, err := s.SendRequest(request, addr)
	if err != nil {
		return nil, fmt.Errorf("PFCP Session Establishment Request to %v: %w", addr, err)
	}
	response, ok := received.(*message.SessionEstablishmentResponse)
	if !ok {
		return nil, fmt.Errorf(
			"received unexpected response %T for PFCP Session Establishment Request", received,
		)
	}
	if response.SEID() != localSEID {
		return nil, fmt.Errorf(
			"PFCP Session Establishment Response from %v has SEID %d, want %d",
			addr, response.SEID(), localSEID,
		)
	}
	if response.NodeID == nil || response.Cause == nil {
		return nil, fmt.Errorf(
			"PFCP Session Establishment Response from %v is missing mandatory IE(s)", addr,
		)
	}
	if _, err = nodeIDFromIE(response.NodeID); err != nil {
		return nil, fmt.Errorf(
			"decode PFCP Session Establishment Response Node ID from %v: %w", addr, err,
		)
	}
	cause, err := response.Cause.Cause()
	if err != nil {
		return nil, fmt.Errorf(
			"decode PFCP Session Establishment Response Cause from %v: %w", addr, err,
		)
	}
	if cause == ie.CauseRequestAccepted {
		if response.UPFSEID == nil {
			return nil, fmt.Errorf(
				"accepted PFCP Session Establishment Response from %v is missing UP F-SEID", addr,
			)
		}
		if _, err = response.UPFSEID.FSEID(); err != nil {
			return nil, fmt.Errorf(
				"decode PFCP Session Establishment Response UP F-SEID from %v: %w", addr, err,
			)
		}
	}
	return response, nil
}
