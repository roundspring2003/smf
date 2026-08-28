package processor

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
	"github.com/free5gc/smf/pkg/factory"
)

func installAssociationReleasePFCPContext(t *testing.T) context.Context {
	t.Helper()
	smfSelf := smf_context.GetSelf()
	previousContext := smfSelf.PfcpContext
	previousCancel := smfSelf.PfcpCancelFunc
	pfcpContext, cancelPFCP := context.WithCancel(context.Background())
	smfSelf.PfcpContext = pfcpContext
	smfSelf.PfcpCancelFunc = cancelPFCP
	t.Cleanup(func() {
		cancelPFCP()
		smfSelf.PfcpContext = previousContext
		smfSelf.PfcpCancelFunc = previousCancel
	})
	return pfcpContext
}

func TestAssociationReleaseContextFollowsPFCPParent(t *testing.T) {
	parentContext, cancelParent := context.WithCancel(context.Background())
	releaseContext, cancelRelease := associationReleaseContext(parentContext, nil)
	t.Cleanup(cancelRelease)

	cancelParent()
	select {
	case <-releaseContext.Done():
	case <-time.After(time.Second):
		t.Fatal("PFCP parent cancellation did not cancel Association Release context")
	}
}

func initAssociationReleaseTestContext(t *testing.T) {
	t.Helper()
	err := smf_context.InitSmfContext(&factory.Config{
		Info: &factory.Info{Version: "1.0.7"},
		Configuration: &factory.Configuration{
			Sbi: &factory.Sbi{Scheme: "http", BindingIPv4: "127.0.0.1"},
			UserPlaneInformation: factory.UserPlaneInformation{
				UPNodes: map[string]*factory.UPNode{},
				Links:   []*factory.UPLink{},
			},
		},
	})
	if err != nil {
		t.Fatalf("InitSmfContext() error: %v", err)
	}
	installAssociationReleasePFCPContext(t)
}

func TestProcessorPassiveAssociationStateUsesConfiguredUPF(t *testing.T) {
	nodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.81").To4(),
	}
	upf := smf_context.NewUPF(&nodeID, nil)
	t.Cleanup(func() { smf_context.RemoveUPFNodeByNodeID(nodeID) })
	peer := nodeID
	p := &Processor{}

	cause, afterResponse := p.SetupAssociation(peer, time.Now())
	if cause != ie.CauseRequestAccepted {
		t.Fatalf("SetupAssociation() cause = %d, want Request Accepted", cause)
	}
	if afterResponse != nil {
		t.Fatal("SetupAssociation() returned unexpected recovery work")
	}

	upf.EstablishAssociation(context.Background())
	upf.RecoveryTimeStamp = time.Now()
	if cause = p.ReleaseAssociation(peer); cause != ie.CauseRequestAccepted {
		t.Fatalf("ReleaseAssociation() cause = %d, want Request Accepted", cause)
	}
	if err := upf.IsAssociated(); err == nil {
		t.Fatal("UPF remains associated after passive Association Release")
	}
	if !upf.RecoveryTimeStamp.IsZero() {
		t.Fatalf("RecoveryTimeStamp = %v, want zero", upf.RecoveryTimeStamp)
	}
}

func TestProcessorPassiveAssociationUpdateStoresFeatures(t *testing.T) {
	nodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.83").To4(),
	}
	upf := smf_context.NewUPF(&nodeID, nil)
	t.Cleanup(func() { smf_context.RemoveUPFNodeByNodeID(nodeID) })
	upf.EstablishAssociation(context.Background())
	t.Cleanup(upf.CancelAssociation)
	p := &Processor{}
	featureIE := ie.NewUPFunctionFeatures(0x21, 0x43)

	cause, afterResponse := p.UpdateAssociation(nodeID,
		message.NewAssociationUpdateRequest(1, featureIE))
	if cause != ie.CauseRequestAccepted {
		t.Fatalf("UpdateAssociation() cause = %d, want Request Accepted", cause)
	}
	if afterResponse != nil {
		t.Fatal("feature-only update returned unexpected release work")
	}
	featureIE.Payload[0] = 0xff
	got := upf.UPFunctionFeatures()
	if len(got) != 2 || got[0] != 0x21 || got[1] != 0x43 {
		t.Fatalf("stored UP Function Features = %v, want [0x21 0x43]", got)
	}
}

func TestProcessorPassiveAssociationUpdateURSSDoesNotRelease(t *testing.T) {
	nodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.90").To4(),
	}
	upf := smf_context.NewUPF(&nodeID, nil)
	t.Cleanup(func() { smf_context.RemoveUPFNodeByNodeID(nodeID) })
	upf.EstablishAssociation(context.Background())
	t.Cleanup(upf.CancelAssociation)
	p := &Processor{}

	cause, afterResponse := p.UpdateAssociation(nodeID, message.NewAssociationUpdateRequest(
		1, ie.NewPFCPAssociationReleaseRequest(0, 1),
	))
	if cause != ie.CauseRequestAccepted {
		t.Fatalf("UpdateAssociation() cause = %d, want Request Accepted", cause)
	}
	if afterResponse != nil {
		t.Fatal("URSS-only Association Update unexpectedly started release work")
	}
	if err := upf.IsAssociated(); err != nil {
		t.Fatalf("URSS-only Association Update changed association state: %v", err)
	}
}

func TestProcessorPassiveAssociationUpdateReleasesAfterResponse(t *testing.T) {
	pfcpContext := installAssociationReleasePFCPContext(t)
	nodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.84").To4(),
	}
	upf := smf_context.NewUPF(&nodeID, nil)
	t.Cleanup(func() { smf_context.RemoveUPFNodeByNodeID(nodeID) })
	associationContext := upf.EstablishAssociation(pfcpContext)
	upf.RecoveryTimeStamp = time.Now()
	t.Cleanup(upf.CancelAssociation)
	releaseCalled := false
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		releaseCtx: func(ctx context.Context, addr *net.UDPAddr) (*message.AssociationReleaseResponse, error) {
			if ctx != associationContext {
				t.Errorf("Association Release Request did not use the UPF association context")
			}
			releaseCalled = true
			if got, want := addr.IP.String(), "192.0.2.84"; got != want {
				t.Fatalf("release destination IP = %s, want %s", got, want)
			}
			if got, want := addr.Port, pfcpPeerPort; got != want {
				t.Fatalf("release destination port = %d, want %d", got, want)
			}
			return message.NewAssociationReleaseResponse(
				1,
				ie.NewNodeIDHeuristic("192.0.2.84"),
				ie.NewCause(ie.CauseRequestAccepted),
			), nil
		},
	})

	cause, afterResponse := p.UpdateAssociation(nodeID, message.NewAssociationUpdateRequest(
		1, ie.NewPFCPAssociationReleaseRequest(1, 0),
	))
	if cause != ie.CauseRequestAccepted || afterResponse == nil {
		t.Fatalf("UpdateAssociation() cause = %d, afterResponse present = %t; want accepted release work",
			cause, afterResponse != nil)
	}
	if releaseCalled {
		t.Fatal("Association Release was sent before afterResponse")
	}
	if err := upf.IsAssociated(); err != nil {
		t.Fatalf("association was canceled before afterResponse: %v", err)
	}
	afterResponse()
	if !releaseCalled {
		t.Fatal("Association Release was not sent by afterResponse")
	}
	if err := upf.IsAssociated(); err == nil {
		t.Fatal("UPF remains associated after Association Release")
	}
	if !upf.RecoveryTimeStamp.IsZero() {
		t.Fatalf("RecoveryTimeStamp = %v, want zero", upf.RecoveryTimeStamp)
	}
}

func TestAssociationUpdateDeletesAffectedSessionsBeforeRelease(t *testing.T) {
	initAssociationReleaseTestContext(t)
	targetNodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.86").To4(),
	}
	otherNodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.87").To4(),
	}
	upf := smf_context.NewUPF(&targetNodeID, nil)
	otherUPF := smf_context.NewUPF(&otherNodeID, nil)
	t.Cleanup(func() {
		smf_context.RemoveUPFNodeByNodeID(targetNodeID)
		smf_context.RemoveUPFNodeByNodeID(otherNodeID)
	})
	upf.EstablishAssociation(context.Background())
	upf.RecoveryTimeStamp = time.Now()
	t.Cleanup(upf.CancelAssociation)
	otherUPF.EstablishAssociation(context.Background())
	t.Cleanup(otherUPF.CancelAssociation)

	first := smf_context.NewSMContext("imsi-association-release-1", 1)
	second := smf_context.NewSMContext("imsi-association-release-2", 2)
	t.Cleanup(func() {
		smf_context.RemoveSMContext(first.Ref)
		smf_context.RemoveSMContext(second.Ref)
	})
	first.PFCPContext[targetNodeID.String()] = &smf_context.PFCPSessionContext{
		NodeID: targetNodeID, LocalSEID: 501, RemoteSEID: 601,
	}
	first.PFCPContext[otherNodeID.String()] = &smf_context.PFCPSessionContext{
		NodeID: otherNodeID, LocalSEID: 502, RemoteSEID: 602,
	}
	second.PFCPContext[targetNodeID.String()] = &smf_context.PFCPSessionContext{
		NodeID: targetNodeID, LocalSEID: 503, RemoteSEID: 603,
	}

	var mu sync.Mutex
	deletedRemoteSEIDs := make(map[uint64]bool)
	releaseCalled := false
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		deleteCtx: func(
			ctx context.Context,
			request *message.SessionDeletionRequest,
			addr *net.UDPAddr,
			localSEID uint64,
		) (*message.SessionDeletionResponse, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !upf.IsAssociationReleasing() {
				t.Error("Session Deletion was sent before UPF started Association Release")
			}
			gotIP := addr.IP.String()
			if gotIP != targetNodeID.String() && gotIP != otherNodeID.String() {
				t.Errorf("unexpected Session Deletion destination = %s", gotIP)
			}
			mu.Lock()
			deletedRemoteSEIDs[request.SEID()] = true
			mu.Unlock()
			return message.NewSessionDeletionResponse(
				0, 0, localSEID, request.Sequence(), 0,
				ie.NewCause(ie.CauseRequestAccepted),
				ie.NewUsageReportWithinSessionDeletionResponse(
					ie.NewURRID(uint32(localSEID)),
					ie.NewUsageReportTrigger(0, 0x08),
					ie.NewVolumeMeasurement(0x07, 300, 100, 200, 0, 0, 0),
				),
			), nil
		},
		release: func(*net.UDPAddr) (*message.AssociationReleaseResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(deletedRemoteSEIDs) != 3 || !deletedRemoteSEIDs[601] ||
				!deletedRemoteSEIDs[602] || !deletedRemoteSEIDs[603] {
				t.Errorf("Association Release ran before complete PDU Session deletion: %v", deletedRemoteSEIDs)
			}
			if len(first.UrrReports) != 2 || len(second.UrrReports) != 1 {
				t.Errorf("Association Release ran before final reports were collected: first=%d second=%d",
					len(first.UrrReports), len(second.UrrReports))
			}
			releaseCalled = true
			return message.NewAssociationReleaseResponse(
				1, ie.NewNodeIDHeuristic(targetNodeID.String()), ie.NewCause(ie.CauseRequestAccepted),
			), nil
		},
	})

	cause, afterResponse := p.UpdateAssociation(targetNodeID, message.NewAssociationUpdateRequest(
		1, ie.NewPFCPAssociationReleaseRequest(1, 0),
	))
	if cause != ie.CauseRequestAccepted || afterResponse == nil {
		t.Fatalf("UpdateAssociation() cause = %d, afterResponse present = %t",
			cause, afterResponse != nil)
	}
	afterResponse()
	if !releaseCalled {
		t.Fatal("Association Release was not sent after PFCP Session Deletion")
	}
	if got := first.PFCPContext[targetNodeID.String()].RemoteSEID; got != 0 {
		t.Errorf("first target RemoteSEID = %d, want 0", got)
	}
	if got := second.PFCPContext[targetNodeID.String()].RemoteSEID; got != 0 {
		t.Errorf("second target RemoteSEID = %d, want 0", got)
	}
	if got := first.PFCPContext[otherNodeID.String()].RemoteSEID; got != 0 {
		t.Errorf("other UPF RemoteSEID = %d, want 0", got)
	}
	if got := len(first.UrrReports); got != 2 {
		t.Errorf("first Usage Reports = %d, want 2", got)
	}
	if got := len(second.UrrReports); got != 1 {
		t.Errorf("second Usage Reports = %d, want 1", got)
	}
}

func TestAssociationReleaseDeadlineCancelsDeletionBeforeRelease(t *testing.T) {
	initAssociationReleaseTestContext(t)
	nodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.88").To4(),
	}
	upf := smf_context.NewUPF(&nodeID, nil)
	t.Cleanup(func() { smf_context.RemoveUPFNodeByNodeID(nodeID) })
	upf.EstablishAssociation(context.Background())
	t.Cleanup(upf.CancelAssociation)
	smContext := smf_context.NewSMContext("imsi-association-release-deadline", 3)
	t.Cleanup(func() { smf_context.RemoveSMContext(smContext.Ref) })
	smContext.PFCPContext[nodeID.String()] = &smf_context.PFCPSessionContext{
		NodeID: nodeID, LocalSEID: 701, RemoteSEID: 801,
	}

	deletionCanceled := false
	releaseCalled := false
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		deleteCtx: func(
			ctx context.Context,
			_ *message.SessionDeletionRequest,
			_ *net.UDPAddr,
			_ uint64,
		) (*message.SessionDeletionResponse, error) {
			<-ctx.Done()
			deletionCanceled = true
			return nil, ctx.Err()
		},
		release: func(*net.UDPAddr) (*message.AssociationReleaseResponse, error) {
			if !deletionCanceled {
				t.Error("Association Release ran before deadline canceled Session Deletion")
			}
			releaseCalled = true
			return message.NewAssociationReleaseResponse(
				1, ie.NewNodeIDHeuristic(nodeID.String()), ie.NewCause(ie.CauseRequestAccepted),
			), nil
		},
	})
	period := 2 * time.Second
	_, afterResponse := p.UpdateAssociation(nodeID, message.NewAssociationUpdateRequest(
		1,
		ie.NewPFCPAssociationReleaseRequest(1, 0),
		ie.NewGracefulReleasePeriod(period),
	))
	start := time.Now()
	afterResponse()
	if !releaseCalled {
		t.Fatal("Association Release was not sent after deadline")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Association Release deadline took %s, want less than 500ms", elapsed)
	}
}

func TestAssociationReleaseDeadlineIncludesInFlightSessionWork(t *testing.T) {
	initAssociationReleaseTestContext(t)
	nodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.89").To4(),
	}
	upf := smf_context.NewUPF(&nodeID, nil)
	t.Cleanup(func() { smf_context.RemoveUPFNodeByNodeID(nodeID) })
	upf.EstablishAssociation(context.Background())
	t.Cleanup(upf.CancelAssociation)
	smContext := smf_context.NewSMContext("imsi-association-release-in-flight", 4)
	t.Cleanup(func() { smf_context.RemoveSMContext(smContext.Ref) })
	smContext.PFCPContext[nodeID.String()] = &smf_context.PFCPSessionContext{
		NodeID: nodeID, LocalSEID: 901, RemoteSEID: 902,
	}

	_, finishSessionWork, err := upf.BeginSessionWork()
	if err != nil {
		t.Fatalf("BeginSessionWork() error: %v", err)
	}
	deletionCalled := make(chan struct{}, 1)
	releaseCalled := make(chan struct{}, 1)
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		deleteCtx: func(
			context.Context,
			*message.SessionDeletionRequest,
			*net.UDPAddr,
			uint64,
		) (*message.SessionDeletionResponse, error) {
			deletionCalled <- struct{}{}
			return nil, context.DeadlineExceeded
		},
		release: func(*net.UDPAddr) (*message.AssociationReleaseResponse, error) {
			releaseCalled <- struct{}{}
			return message.NewAssociationReleaseResponse(
				1, ie.NewNodeIDHeuristic(nodeID.String()), ie.NewCause(ie.CauseRequestAccepted),
			), nil
		},
	})
	period := time.Duration(0)
	_, afterResponse := p.UpdateAssociation(nodeID, message.NewAssociationUpdateRequest(
		1,
		ie.NewPFCPAssociationReleaseRequest(1, 0),
		ie.NewGracefulReleasePeriod(period),
	))
	afterDone := make(chan struct{})
	go func() {
		afterResponse()
		close(afterDone)
	}()

	deadline := time.Now().Add(time.Second)
	for !upf.IsAssociationReleasing() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !upf.IsAssociationReleasing() {
		t.Fatal("UPF did not start Association Release while waiting for in-flight session work")
	}
	// A zero PFCP Graceful Release Period represents an immediate deadline.
	// Keep the existing work held until the release path is waiting on it.
	finishSessionWork()

	select {
	case <-afterDone:
	case <-time.After(time.Second):
		t.Fatal("association release did not finish after in-flight work completed")
	}
	select {
	case <-deletionCalled:
		t.Fatal("Session Deletion started after graceful-release deadline expired")
	default:
	}
	select {
	case <-releaseCalled:
	default:
		t.Fatal("Association Release was not sent after graceful-release deadline")
	}
}

func TestProcessorPassiveAssociationUpdateRequiresEstablishedAssociation(t *testing.T) {
	nodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.85").To4(),
	}
	smf_context.NewUPF(&nodeID, nil)
	t.Cleanup(func() { smf_context.RemoveUPFNodeByNodeID(nodeID) })
	p := &Processor{}

	cause, afterResponse := p.UpdateAssociation(nodeID, message.NewAssociationUpdateRequest(1))
	if cause != ie.CauseNoEstablishedPFCPAssociation || afterResponse != nil {
		t.Fatalf("UpdateAssociation() cause = %d, afterResponse present = %t; want No Established Association",
			cause, afterResponse != nil)
	}
}

func TestProcessorPassiveAssociationRejectsUnknownUPF(t *testing.T) {
	p := &Processor{}
	peer := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.82").To4(),
	}
	cause, _ := p.SetupAssociation(peer, time.Now())
	if cause != ie.CauseRequestRejected {
		t.Fatalf("SetupAssociation() cause = %d, want Request Rejected", cause)
	}
	if cause = p.ReleaseAssociation(peer); cause != ie.CauseNoEstablishedPFCPAssociation {
		t.Fatalf("ReleaseAssociation() cause = %d, want No Established Association", cause)
	}
}
