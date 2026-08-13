package pfcp

import (
	"fmt"
	"net"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

func (s *PfcpServer) handleHeartbeatRequest(req *message.HeartbeatRequest) *message.HeartbeatResponse {
	return message.NewHeartbeatResponse(
		req.Sequence(),
		ie.NewRecoveryTimeStamp(s.RecoveryTime()),
	)
}

// SendHeartbeatRequest sends an SMF-initiated Heartbeat Request and waits for
// the response matched by the PFCP transaction layer. UPF association state is
// intentionally updated by the caller, not by this transport-level procedure.
func (s *PfcpServer) SendHeartbeatRequest(addr *net.UDPAddr) (*message.HeartbeatResponse, error) {
	if s == nil {
		return nil, fmt.Errorf("send PFCP Heartbeat Request: nil PFCP server")
	}
	if addr == nil || addr.IP == nil || addr.IP.IsUnspecified() {
		return nil, fmt.Errorf("send PFCP Heartbeat Request: no destination IP address")
	}

	request := message.NewHeartbeatRequest(
		0, // TxTransaction assigns the final 24-bit sequence number.
		ie.NewRecoveryTimeStamp(s.RecoveryTime()),
		nil,
	)
	responseChannel := s.SendPfcpMsg(request, addr)
	if responseChannel == nil {
		return nil, fmt.Errorf("send PFCP Heartbeat Request to %v: PFCP server is stopped", addr)
	}

	var received RcvPfcpMsg
	var ok bool
	select {
	case <-s.stopCh:
		return nil, fmt.Errorf("send PFCP Heartbeat Request to %v: PFCP server is stopped", addr)
	case received, ok = <-responseChannel:
	}
	if !ok || received.Msg == nil {
		if isServerStopped(s.stopCh) {
			return nil, fmt.Errorf("send PFCP Heartbeat Request to %v: PFCP server is stopped", addr)
		}
		return nil, fmt.Errorf("PFCP Heartbeat Request to %v timed out", addr)
	}

	response, ok := received.Msg.(*message.HeartbeatResponse)
	if !ok {
		return nil, fmt.Errorf("received unexpected response %T for PFCP Heartbeat Request", received.Msg)
	}
	if response.RecoveryTimeStamp == nil {
		return nil, fmt.Errorf("PFCP Heartbeat Response from %v is missing Recovery Time Stamp", addr)
	}
	if _, err := response.RecoveryTimeStamp.RecoveryTimeStamp(); err != nil {
		return nil, fmt.Errorf("decode PFCP Heartbeat Response Recovery Time Stamp from %v: %w", addr, err)
	}
	return response, nil
}
