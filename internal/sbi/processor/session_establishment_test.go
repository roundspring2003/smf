package processor

import (
	stdcontext "context"
	"net"
	"strings"
	"testing"

	"github.com/free5gc/smf/internal/pfcp/pfcptype"
	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	"github.com/free5gc/smf/internal/context"
)

func TestApplyCreatedPDRsUpdatesUPFAllocatedFTEID(t *testing.T) {
	pdr := &context.PDR{PDRID: 7}
	sessionContext := &context.PFCPSessionContext{
		PDRs: map[uint16]*context.PDR{7: pdr},
	}
	createdPDR := ie.NewCreatedPDR(
		ie.NewPDRID(7),
		ie.NewFTEID(0x01, 0x10203040, net.ParseIP("192.0.2.20").To4(), nil, 0),
	)

	if err := applyCreatedPDRs([]*ie.IE{createdPDR}, sessionContext, nil); err != nil {
		t.Fatalf("applyCreatedPDRs() error: %v", err)
	}
	got := pdr.PDI.LocalFTeid
	if got == nil {
		t.Fatal("LocalFTeid was not updated")
	}
	if got.Teid != 0x10203040 || !got.V4 || got.V6 {
		t.Fatalf("LocalFTeid = %+v, want IPv4 TEID %#x", got, uint32(0x10203040))
	}
	if !got.Ipv4Address.Equal(net.ParseIP("192.0.2.20")) {
		t.Fatalf("LocalFTeid IPv4 = %v, want 192.0.2.20", got.Ipv4Address)
	}
}

func TestApplyCreatedPDRsAllowsNoAllocatedFTEID(t *testing.T) {
	pdr := &context.PDR{PDRID: 7}
	sessionContext := &context.PFCPSessionContext{
		PDRs: map[uint16]*context.PDR{7: pdr},
	}
	if err := applyCreatedPDRs(
		[]*ie.IE{ie.NewCreatedPDR(ie.NewPDRID(7))}, sessionContext, nil,
	); err != nil {
		t.Fatalf("applyCreatedPDRs() error: %v", err)
	}
	if pdr.PDI.LocalFTeid != nil {
		t.Fatalf("LocalFTeid = %+v, want unchanged nil", pdr.PDI.LocalFTeid)
	}
}

func TestApplyCreatedPDRsRejectsUnknownPDR(t *testing.T) {
	sessionContext := &context.PFCPSessionContext{PDRs: make(map[uint16]*context.PDR)}
	err := applyCreatedPDRs([]*ie.IE{
		ie.NewCreatedPDR(
			ie.NewPDRID(99),
			ie.NewFTEID(0x01, 1, net.ParseIP("192.0.2.20").To4(), nil, 0),
		),
	}, sessionContext, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown PDR ID 99") {
		t.Fatalf("applyCreatedPDRs() error = %v, want unknown PDR error", err)
	}
}

type fakeRollbackPFCPClient struct {
	delete func(
		*message.SessionDeletionRequest, *net.UDPAddr, uint64,
	) (*message.SessionDeletionResponse, error)
}

func (f *fakeRollbackPFCPClient) SendAssociationSetupRequest(
	stdcontext.Context, *net.UDPAddr,
) (*message.AssociationSetupResponse, error) {
	panic("unexpected Association Setup")
}

func (f *fakeRollbackPFCPClient) SendHeartbeatRequest(
	stdcontext.Context, *net.UDPAddr,
) (*message.HeartbeatResponse, error) {
	panic("unexpected Heartbeat")
}

func (f *fakeRollbackPFCPClient) SendSessionDeletionRequest(
	_ stdcontext.Context,
	request *message.SessionDeletionRequest,
	addr *net.UDPAddr,
	localSEID uint64,
) (*message.SessionDeletionResponse, error) {
	return f.delete(request, addr, localSEID)
}

func rollbackTestUPF(ip string) *context.UPF {
	upf := &context.UPF{
		NodeID: pfcptype.NodeID{
			NodeIdType: pfcptype.NodeIdTypeIpv4Address,
			IP:         net.ParseIP(ip).To4(),
		},
	}
	upf.EstablishAssociation(stdcontext.Background())
	return upf
}

func TestWaitAllPfcpRspReportsFailureWithoutCallback(t *testing.T) {
	results := make(chan SendPfcpResult, 2)
	results <- SendPfcpResult{Status: context.SessionEstablishSuccess}
	results <- SendPfcpResult{Status: context.SessionEstablishFailed}

	if waitAllPfcpRsp(&context.SMContext{}, 2, results, nil) {
		t.Fatal("waitAllPfcpRsp() = success, want failure")
	}
}

func TestRollbackEstablishedPfcpSessionsDeletesOnlyCreatedSessions(t *testing.T) {
	const createdIP = "192.0.2.10"
	const failedIP = "192.0.2.11"
	const localSEID uint64 = 101
	const remoteSEID uint64 = 201
	type deletionCall struct {
		request   *message.SessionDeletionRequest
		addr      *net.UDPAddr
		localSEID uint64
	}
	calls := make(chan deletionCall, 2)
	client := &fakeRollbackPFCPClient{
		delete: func(
			request *message.SessionDeletionRequest,
			addr *net.UDPAddr,
			gotLocalSEID uint64,
		) (*message.SessionDeletionResponse, error) {
			calls <- deletionCall{request: request, addr: addr, localSEID: gotLocalSEID}
			return message.NewSessionDeletionResponse(
				0, 0, gotLocalSEID, request.Sequence(), 0,
				ie.NewCause(ie.CauseRequestAccepted),
			), nil
		},
	}
	processor := &Processor{}
	processor.SetActivePFCPClient(client)
	createdSession := &context.PFCPSessionContext{LocalSEID: localSEID, RemoteSEID: remoteSEID}
	failedSession := &context.PFCPSessionContext{LocalSEID: 102}
	smContext := &context.SMContext{PFCPContext: map[string]*context.PFCPSessionContext{
		createdIP: createdSession,
		failedIP:  failedSession,
	}}
	targets := map[string]*PFCPState{
		createdIP: {upf: rollbackTestUPF(createdIP)},
		failedIP:  {upf: rollbackTestUPF(failedIP)},
	}

	if err := processor.rollbackEstablishedPfcpSessions(smContext, targets); err != nil {
		t.Fatalf("rollbackEstablishedPfcpSessions() error: %v", err)
	}
	call := <-calls
	if call.request.SEID() != remoteSEID {
		t.Errorf("deletion request SEID = %d, want %d", call.request.SEID(), remoteSEID)
	}
	if call.localSEID != localSEID {
		t.Errorf("deletion local SEID = %d, want %d", call.localSEID, localSEID)
	}
	if got := call.addr.IP.String(); got != createdIP {
		t.Errorf("deletion destination IP = %s, want %s", got, createdIP)
	}
	select {
	case extra := <-calls:
		t.Fatalf("unexpected deletion for %v", extra.addr)
	default:
	}
	if createdSession.RemoteSEID != 0 {
		t.Errorf("accepted deletion left RemoteSEID = %d, want 0", createdSession.RemoteSEID)
	}
}

func TestRollbackEstablishedPfcpSessionsKeepsSEIDWhenDeletionRejected(t *testing.T) {
	const upfIP = "192.0.2.20"
	const remoteSEID uint64 = 301
	client := &fakeRollbackPFCPClient{
		delete: func(
			request *message.SessionDeletionRequest,
			_ *net.UDPAddr,
			localSEID uint64,
		) (*message.SessionDeletionResponse, error) {
			return message.NewSessionDeletionResponse(
				0, 0, localSEID, request.Sequence(), 0,
				ie.NewCause(ie.CauseNoResourcesAvailable),
			), nil
		},
	}
	processor := &Processor{}
	processor.SetActivePFCPClient(client)
	session := &context.PFCPSessionContext{LocalSEID: 202, RemoteSEID: remoteSEID}
	smContext := &context.SMContext{PFCPContext: map[string]*context.PFCPSessionContext{upfIP: session}}

	err := processor.rollbackEstablishedPfcpSessions(smContext, map[string]*PFCPState{
		upfIP: {upf: rollbackTestUPF(upfIP)},
	})
	if err == nil || !strings.Contains(err.Error(), "Deletion rejected") {
		t.Fatalf("rollbackEstablishedPfcpSessions() error = %v, want rejection", err)
	}
	if session.RemoteSEID != remoteSEID {
		t.Errorf("rejected deletion changed RemoteSEID = %d, want %d", session.RemoteSEID, remoteSEID)
	}
}
