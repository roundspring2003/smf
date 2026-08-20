package udp

import (
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	legacyPfcp "github.com/free5gc/pfcp"
	"github.com/free5gc/pfcp/pfcpUdp"
	"github.com/wmnsk/go-pfcp/message"

	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/logger"
)

const MaxPfcpUdpDataSize = 1024

// Transport is the subset of the new PFCP runtime used by procedures that
// still build legacy free5gc/pfcp messages. The adapter converts at the wire
// boundary; the new server remains the sole owner of UDP and transactions.
type Transport interface {
	SendRequest(message.Message, *net.UDPAddr) (message.Message, error)
	SendPfcpResponse(message.Message, *net.UDPAddr) error
}

var (
	// Server is retained only for legacy unit tests. Production configures
	// runtimeTransport and never starts this free5gc/pfcp UDP server.
	Server *pfcpUdp.PfcpServer

	ServerStartTime time.Time

	transportMu      sync.RWMutex
	runtimeTransport Transport
)

func UseTransport(transport Transport, recoveryTime time.Time) {
	transportMu.Lock()
	runtimeTransport = transport
	ServerStartTime = recoveryTime
	transportMu.Unlock()
}

func ClearTransport() {
	transportMu.Lock()
	runtimeTransport = nil
	transportMu.Unlock()
}

func getTransport() Transport {
	transportMu.RLock()
	defer transportMu.RUnlock()
	return runtimeTransport
}

// Run starts the legacy server for tests that have not yet migrated. Production
// startup uses pfcp.PfcpServer directly and does not call this function.
func Run(dispatch func(*pfcpUdp.Message)) {
	defer func() {
		if p := recover(); p != nil {
			logger.PfcpLog.Fatalf("panic: %v\n%s", p, string(debug.Stack()))
		}
	}()

	smfContext := smf_context.GetSelf()
	serverIP := smfContext.ListenIP().To4()
	Server = pfcpUdp.NewPfcpServer(serverIP.String())

	if err := Server.Listen(); err != nil {
		logger.PfcpLog.Errorf("Failed to listen: %v", err)
		return
	}

	logger.PfcpLog.Infof("Listen on %s", Server.Conn.LocalAddr().String())

	go func(p *pfcpUdp.PfcpServer) {
		defer func() {
			if p := recover(); p != nil {
				logger.PfcpLog.Fatalf("panic: %v\n%s", p, string(debug.Stack()))
			}
		}()

		for {
			msg, errReadFrom := p.ReadFrom()
			if errReadFrom != nil {
				if errReadFrom == pfcpUdp.ErrReceivedResentRequest {
					logger.PfcpLog.Infoln(errReadFrom)
				} else if strings.Contains(errReadFrom.Error(), "use of closed network connection") {
					continue
				} else {
					logger.PfcpLog.Warnf("Read PFCP error: %v, msg: [%v]", errReadFrom, msg)
					select {
					case <-smfContext.PfcpContext.Done():
						return
					default:
						continue
					}
				}
				continue
			}

			if msg.PfcpMessage.IsRequest() {
				go dispatch(msg)
			}
		}
	}(Server)

	ServerStartTime = time.Now()
	logger.PfcpLog.Infof("Pfcp running... [%v]", ServerStartTime)
}

func SendPfcpResponse(sndMsg *legacyPfcp.Message, addr *net.UDPAddr) {
	if transport := getTransport(); transport != nil {
		msg, err := toGoPFCPMessage(sndMsg)
		if err == nil {
			err = transport.SendPfcpResponse(msg, addr)
		}
		if err != nil {
			logger.PfcpLog.Errorf("send legacy PFCP response through new transport: %v", err)
		}
		return
	}
	if Server == nil {
		logger.PfcpLog.Error("send PFCP response: no PFCP transport is configured")
		return
	}
	Server.WriteResponseTo(sndMsg, addr)
}

func SendPfcpRequest(
	sndMsg *legacyPfcp.Message,
	addr *net.UDPAddr,
) (*pfcpUdp.Message, error) {
	if addr == nil || addr.IP == nil || addr.IP.IsUnspecified() {
		return nil, errors.New("no destination IP address is specified")
	}
	if transport := getTransport(); transport != nil {
		request, err := toGoPFCPMessage(sndMsg)
		if err != nil {
			return nil, fmt.Errorf("convert legacy PFCP request: %w", err)
		}
		response, err := transport.SendRequest(request, addr)
		if err != nil {
			return nil, err
		}
		return toLegacyPFCPMessage(response, addr)
	}
	if Server == nil {
		return nil, errors.New("no PFCP transport is configured")
	}
	return Server.WriteRequestTo(sndMsg, addr)
}

func toGoPFCPMessage(legacyMessage *legacyPfcp.Message) (message.Message, error) {
	if legacyMessage == nil {
		return nil, errors.New("nil legacy PFCP message")
	}
	packet, err := legacyMessage.Marshal()
	if err != nil {
		return nil, err
	}
	return message.Parse(packet)
}

func toLegacyPFCPMessage(goMessage message.Message, addr *net.UDPAddr) (*pfcpUdp.Message, error) {
	if goMessage == nil {
		return nil, errors.New("nil go-pfcp message")
	}
	packet := make([]byte, goMessage.MarshalLen())
	if err := goMessage.MarshalTo(packet); err != nil {
		return nil, err
	}
	legacyMessage := &legacyPfcp.Message{}
	if err := legacyMessage.Unmarshal(packet); err != nil {
		return nil, err
	}
	return pfcpUdp.NewMessage(addr, legacyMessage), nil
}

func ClosePfcp() error {
	smf_context.GetSelf().PfcpCancelFunc()
	if Server == nil {
		return nil
	}

	closeErr := Server.Close()
	if closeErr != nil {
		logger.PfcpLog.Errorf("Pfcp close err: %+v", closeErr)
	} else {
		logger.PfcpLog.Infof("Pfcp server closed")
	}
	return closeErr
}
