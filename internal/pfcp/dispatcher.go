package pfcp

import (
	"net"
	"runtime/debug"
	"sync"

	legacyPfcp "github.com/free5gc/pfcp"
	"github.com/free5gc/pfcp/pfcpUdp"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/internal/logger"
	"github.com/free5gc/smf/internal/pfcp/handler"
)

// Dispatch is the legacy free5gc/pfcp dispatcher. It remains until the SMF
// runtime and all request handlers have moved to PfcpServer.Dispatch.
func Dispatch(msg *pfcpUdp.Message) {
	switch msg.PfcpMessage.Header.MessageType {
	case legacyPfcp.PFCP_HEARTBEAT_REQUEST:
		handler.HandlePfcpHeartbeatRequest(msg)
	case legacyPfcp.PFCP_PFD_MANAGEMENT_REQUEST:
		handler.HandlePfcpPfdManagementRequest(msg)
	case legacyPfcp.PFCP_ASSOCIATION_SETUP_REQUEST:
		handler.HandlePfcpAssociationSetupRequest(msg)
	case legacyPfcp.PFCP_ASSOCIATION_UPDATE_REQUEST:
		handler.HandlePfcpAssociationUpdateRequest(msg)
	case legacyPfcp.PFCP_ASSOCIATION_RELEASE_REQUEST:
		handler.HandlePfcpAssociationReleaseRequest(msg)
	case legacyPfcp.PFCP_NODE_REPORT_REQUEST:
		handler.HandlePfcpNodeReportRequest(msg)
	case legacyPfcp.PFCP_SESSION_SET_DELETION_REQUEST:
		handler.HandlePfcpSessionSetDeletionRequest(msg)
	case legacyPfcp.PFCP_SESSION_REPORT_REQUEST:
		handler.HandlePfcpSessionReportRequest(msg)
	default:
		logger.PfcpLog.Errorf("Unknown PFCP message type: %d", msg.PfcpMessage.Header.MessageType)
	}
}

// SetDispatch replaces the request callback used by the bounded dispatcher
// workers. NewPfcpServer installs PfcpServer.Dispatch by default; tests and
// embedders may replace it before Run.
func (s *PfcpServer) SetDispatch(dispatch func(message.Message, *net.UDPAddr)) {
	s.dispatchMu.Lock()
	s.dispatch = dispatch
	s.dispatchMu.Unlock()
}

// Dispatch routes concrete go-pfcp request messages to SMF handlers.
func (s *PfcpServer) Dispatch(msg message.Message, addr *net.UDPAddr) {
	var response message.Message
	var afterResponse func()
	switch request := msg.(type) {
	case *message.HeartbeatRequest:
		response = s.handleHeartbeatRequest(request)
	case *message.AssociationSetupRequest:
		response, afterResponse = s.handleAssociationSetupRequest(request)
	case *message.AssociationReleaseRequest:
		response = s.handleAssociationReleaseRequest(request)
	default:
		s.log.Warnf("unsupported PFCP request %T from %v", msg, addr)
		return
	}

	if response != nil {
		if err := s.SendPfcpResponse(response, addr); err != nil {
			s.log.Errorf("send %s to %v: %v", response.MessageTypeName(), addr, err)
			return
		}
	}
	if afterResponse != nil {
		afterResponse()
	}
}

// dispatcher is one fixed worker in the bounded PFCP request pool.
func (s *PfcpServer) dispatcher(wg *sync.WaitGroup) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.log.Fatalf("panic in PFCP dispatcher: %v\n%s", recovered, debug.Stack())
		}
		s.log.Infoln("PFCP dispatcher stopped")
		wg.Done()
	}()

	for {
		select {
		case <-s.stopCh:
			return
		case received := <-s.dispCh:
			if received.Msg == nil {
				continue
			}
			s.dispatchMu.RLock()
			dispatch := s.dispatch
			s.dispatchMu.RUnlock()
			if dispatch == nil {
				if received.Rx != nil {
					received.Rx.stop()
				}
				continue
			}
			if received.Rx == nil || !received.Rx.beginHandling() {
				s.log.Debugf("dropping stale queued PFCP request %s sequence %#x from %v",
					received.Msg.MessageTypeName(), received.Msg.Sequence(), &received.RemoteAddr)
				continue
			}

			// Synchronous execution is intentional: spawning here would turn
			// blocked handlers into an unbounded goroutine backlog. The small
			// closure scopes the defer to this request rather than the worker.
			func() {
				defer received.Rx.finishHandling()
				dispatch(received.Msg, &received.RemoteAddr)
			}()
		}
	}
}
