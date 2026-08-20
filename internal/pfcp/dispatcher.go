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
		if err := dispatchLegacyRequest(msg, addr); err != nil {
			s.log.Warnf("unsupported PFCP request %T from %v: %v", msg, addr, err)
		}
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

// dispatchLegacyRequest is a temporary procedure-level bridge. The datagram
// was parsed and transaction-managed by the new server; only the handler body
// is converted back to free5gc/pfcp until that message type is migrated.
func dispatchLegacyRequest(msg message.Message, addr *net.UDPAddr) error {
	packet := make([]byte, msg.MarshalLen())
	if err := msg.MarshalTo(packet); err != nil {
		return err
	}
	legacyMessage := &legacyPfcp.Message{}
	if err := legacyMessage.Unmarshal(packet); err != nil {
		return err
	}
	Dispatch(pfcpUdp.NewMessage(addr, legacyMessage))
	return nil
}

// dispatcher is one fixed worker in the bounded PFCP request pool.
func (s *PfcpServer) dispatcher(wg *sync.WaitGroup) {
	defer func() {
		s.log.Infoln("PFCP dispatcher stopped")
		wg.Done()
	}()

	for {
		if s.dispatchIteration() {
			return
		}
	}
}

// dispatchIteration gives infrastructure panics an iteration-sized failure
// boundary. The fixed worker remains available for the next request instead of
// terminating or taking the whole SMF down.
func (s *PfcpServer) dispatchIteration() (stopped bool) {
	var received *RcvPfcpMsg
	defer func() {
		if recovered := recover(); recovered != nil {
			if received != nil && received.Rx != nil {
				received.Rx.abortUnansweredAfterDispatchPanic()
			}
			s.log.Errorf("panic in PFCP dispatcher infrastructure: %v\n%s", recovered, debug.Stack())
			stopped = false
		}
	}()

	select {
	case <-s.stopCh:
		return true
	case queued, ok := <-s.dispCh:
		if !ok {
			return true
		}
		received = &queued
		// select does not prioritize stopCh when both cases are ready. Do not
		// begin another handler after shutdown has already started.
		if isServerStopped(s.stopCh) {
			if queued.Rx != nil {
				queued.Rx.stop()
			}
			return true
		}
		s.dispatchReceived(queued)
		return false
	}
}

func (s *PfcpServer) dispatchReceived(received RcvPfcpMsg) {
	if received.Msg == nil {
		if received.Rx != nil {
			received.Rx.stop()
		}
		return
	}

	s.dispatchMu.RLock()
	dispatch := s.dispatch
	s.dispatchMu.RUnlock()
	if dispatch == nil {
		if received.Rx != nil {
			received.Rx.stop()
		}
		return
	}
	if received.Rx == nil || !received.Rx.beginHandling() {
		s.log.Debugf("dropping stale queued PFCP request %s sequence %#x from %v",
			received.Msg.MessageTypeName(), received.Msg.Sequence(), &received.RemoteAddr)
		return
	}

	// Register lifecycle cleanup first so the later panic-recovery defer runs
	// before it. Recovery can abort an unanswered transaction; finishHandling
	// then observes done and does not accidentally restart its timer.
	defer received.Rx.finishHandling()
	defer func() {
		if recovered := recover(); recovered != nil {
			aborted := received.Rx.abortUnansweredAfterDispatchPanic()
			s.log.Errorf(
				"panic handling PFCP %s sequence %#x from %v (unanswered transaction aborted=%t): %v\n%s",
				received.Msg.MessageTypeName(), received.Msg.Sequence(), &received.RemoteAddr,
				aborted, recovered, debug.Stack(),
			)
		}
	}()

	// Synchronous execution is intentional: spawning here would turn blocked
	// handlers into an unbounded goroutine backlog.
	dispatch(received.Msg, &received.RemoteAddr)
}
